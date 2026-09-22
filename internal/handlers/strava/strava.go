// strava.go — task 4: the handler core + pure-logic units for the Strava
// activity posting feature (spec §3/§4). RunPoll is one 15-minute background
// loop (the EXACT gulag/loops.go:53-86 ticker shape); iteration is one pass.
// The DB mechanics are exercised by task 5's integration tests (stubbed
// StravaAPI + real PG); this file's unit surface is FeatureKey + the five
// decision helpers.
package strava

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
)

// FeatureKey is the features-table key this handler is gated on (registered
// by migration 000006 at `enabled=false`).
const FeatureKey = "strava"

// The fixed, non-configured run/cycling family (spec §1 — 9 values; unit
// test pins EVERY member + known exclusions).
var familySet = map[string]struct{}{
	"Run": {}, "TrailRun": {}, "VirtualRun": {}, "Ride": {}, "VirtualRide": {},
	"GravelRide": {}, "MountainBikeRide": {}, "EBikeRide": {}, "EMountainBikeRide": {},
}

// tokenRefreshLead is how far before token expiry a refresh fires.
const tokenRefreshLead = time.Hour

// dropAfterRetries is the full-cycle count at which a stuck 'pending' activity
// is dropped (skipped) — it never gets a post.
const dropAfterRetries = 5

// firstPollLookback is the lookback for a first-time poll (NULL cursor).
const firstPollLookback = 24 * time.Hour

// windowOverlap backdates the LIST window start exactly 1h for an athlete with
// a non-NULL cursor (pass step b) so a backdated/late-surfacing activity
// (a manual upload's backdate, a second device syncing late, an edited start
// time) surfaces on a subsequent pass. Re-listed ids are deduped by the seen
// table (a re-listed 'posted' row never re-posts), and the overlap widens
// ONLY the list window — the cursor ADVANCE math (step e) is unchanged.
const windowOverlap = time.Hour

// onboardingTTL is how long a /strava authorize link stays valid before the
// pending row expires (unused rows simply lapse).
const onboardingTTL = time.Hour

// The seen-activity dispositions.
const (
	statusPending = "pending"
	statusPosted  = "posted"
	statusSkipped = "skipped"
)

// dbTx is the minimal query surface shared by *pgxpool.Pool and pgx.Tx.
type dbTx interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

// Strava is the handler; a *app.App is injected (house pattern).
type Strava struct {
	app *app.App
	api StravaAPI // seam: production = NewStravaAPI() (built in New — never
	//             // nil at runtime); tests inject the stub
	poll time.Duration

	// sendFn seam: func(threadID, msg string) error — production posts via
	// h.app.D.ChannelMessageSendComplex with mention parsing disabled (the
	// threads are plain discordgo channels).
	sendFn func(threadID string, msg string) error

	cfgWarned bool // log-once: configured-but-credentialed-missing preflight

	// Webhook job queue (nil until the WebhookHandler factory starts the
	// workers once): a 32-cap channel drained by the 2 worker goroutines.
	jobs        chan webhookJob
	workerOnce  sync.Once
	workersDone chan struct{} // closed when both workers exit (serverCtx done)
	// workerFn seam: the job body — nil = the workerBody (the real body;
	// Task 3 replaced the stub). Tests install a capturing fn BEFORE the
	// first WebhookHandler() call.
	workerFn func(ctx context.Context, j webhookJob)
	// serverCtx is the Start-owned context; canceling it drains the workers
	// (the ≤10s shutdown grace bounds the drain).
	serverCtx    context.Context
	serverCancel context.CancelFunc
}

// New builds the handler. NO network I/O here (selftest discipline — every
// handler is constructed offline): NewStravaAPI is network-free. The API is
// built HERE (not lazily in iteration) so the onboarding callback goroutine
// can never touch a nil s.api on a flag-off bot — and so the HTTP goroutine
// and RunPoll's goroutine never race on the initialization write. The seam
// is wired via a CLOSURE (the house single-thread send shape).
func New(app *app.App) *Strava {
	s := &Strava{app: app, api: NewStravaAPI()}
	s.sendFn = func(threadID, msg string) error {
		// MessageAllowedMentions' Parse is deliberately NOT omitempty: an
		// empty Parse slice marshals parse: [] — complete mention suppression
		// (discordgo's stated purpose: sending untrusted user input; a zero
		// value would marshal a nil slice to parse: null, so the closure
		// passes the explicit empty slice). A regular bot message without
		// allowed_mentions has ALL mention types parsed by default, so an
		// athlete's activity title could otherwise ping a user (<@id>) or
		// roll @everyone/@here pending MENTION_EVERYONE.
		_, err := s.app.D.ChannelMessageSendComplex(threadID, &discordgo.MessageSend{
			Content:         msg,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		})
		return err
	}
	return s
}

// RunPoll is the 15-minute background loop — the EXACT gulag/loops.go:53-86
// shape: ticker + select; on ctx cancel return ctx.Err(); a per-iteration
// error logs and skips the iteration (never crashes). `poll` is set from
// app.Cfg.StravaPollMinutes here (first use), keeping New trivial.
func (s *Strava) RunPoll(ctx context.Context) error {
	if s.poll == 0 {
		s.poll = time.Duration(s.app.Cfg.StravaPollMinutes) * time.Minute
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.iteration(ctx); err != nil {
				slog.Error("error in strava iteration, skipping", "module", "strava", "error", err)
			}
		}
	}
}

// athleteRow is one strava_athletes row (target_thread_id read via COALESCE
// so a NULL column reads as 0 — no pgx NULL scan); last_polled_at is
// nullable (NULL = first poll = 24h lookback).
type athleteRow struct {
	id             int32
	label          string
	accessToken    string
	refreshToken   string
	tokenExpiresAt time.Time
	lastPolledAt   *time.Time
	targetThreadID int64
}

// seenInsert is a new seen row to apply in the per-athlete transaction.
type seenInsert struct {
	activityID int64
	startDate  time.Time
	status     string
}

// seenUpdate is a carried-pending resolution; empty status leaves status
// unchanged, nil retries leaves retries unchanged (COALESCE in the UPDATE).
type seenUpdate struct {
	activityID int64
	status     string // "" = leave as-is
	retries    *int
}

// postItem is a post deferred until AFTER the per-athlete transaction commits.
type postItem struct {
	threadID int64
	message  string
}

// iteration is one pass (spec §3, verbatim).
func (s *Strava) iteration(ctx context.Context) error {
	// Per-tick cleanup (hygiene, not feature work — runs even while the
	// flag is off): expire the lapsed onboarding rows. Non-fatal: a failure
	// logs and the tick continues.
	if _, err := s.app.Pool.Exec(ctx, `DELETE FROM strava_onboardings WHERE expires_at < now()`); err != nil {
		slog.Error("strava onboarding cleanup failed", "module", "strava", "error", err)
	}
	if !features.IsEnabled(ctx, s.app.Pool, FeatureKey) {
		return nil
	}
	// creds preflight (log-once optional) — call nothing when absent.
	if s.app.Cfg.StravaClientID == "" || s.app.Cfg.StravaClientSecret == "" {
		if !s.cfgWarned {
			slog.Info("strava configured but client credentials unset; no-op", "module", "strava")
			s.cfgWarned = true
		}
		return nil
	}

	athletes, err := s.loadAthletes(ctx)
	if err != nil {
		return err
	}
	if len(athletes) == 0 {
		return nil
	}
	// per athlete, in order: a pass error logs + continues to the next athlete
	// (the 401 class returns nil from the pass — abort for THIS athlete only).
	// A list-endpoint 429 is different: Strava's 15-min rate window is
	// APP-WIDE, so the remaining athletes are deferred to the next tick
	// entirely — the 15-min ticker IS the backoff (no cursor change for any
	// not-yet-processed athlete; they were simply never visited).
	for i := range athletes {
		err := s.passForAthlete(ctx, &athletes[i])
		if err == nil {
			continue
		}
		var lrl *errListRateLimited
		if errors.As(err, &lrl) {
			slog.Info("strava list rate limited; aborting iteration; remaining athletes deferred to the next tick",
				"module", "strava", "label", athletes[i].label, "retry_after", lrl.retryAfter)
			break
		}
		slog.Error("strava pass failed for athlete", "module", "strava", "label", athletes[i].label, "error", err)
	}
	return nil
}

// passForAthlete is one athlete-per-pass (spec §3 sequence). It applies the
// new seen rows + the pending retries increments + the new last_polled_at in
// ONE transaction, then posts. The needs_reauth 401 UPDATE commits as its OWN
// statement (autocommit); only the seen/cursor transaction is the atomic unit.
func (s *Strava) passForAthlete(ctx context.Context, a *athleteRow) error {
	now := time.Now().UTC()
	clientID := s.app.Cfg.StravaClientID
	clientSecret := s.app.Cfg.StravaClientSecret

	// ---- a. token refresh (if near expiry) ----
	access := a.accessToken
	if a.tokenExpiresAt.Before(now.Add(tokenRefreshLead)) {
		newAccess, newRefresh, expiresAt, err := s.api.RefreshToken(ctx, clientID, clientSecret, a.refreshToken)
		if err != nil {
			var eua ErrUnauthorized
			if errors.As(err, &eua) {
				return s.abortReauth(ctx, a)
			}
			return err // transient/other: skip this athlete; cursor preserved (no commit)
		}
		if _, err := s.app.Pool.Exec(ctx,
			`UPDATE strava_athletes SET access_token=$1, refresh_token=$2, token_expires_at=$3 WHERE id=$4`,
			newAccess, newRefresh, expiresAt, a.id); err != nil {
			return err
		}
		access = newAccess
	}

	// ---- b. list the window ----
	// A NON-NULL cursor backdates the list window start by 1h (windowOverlap):
	// the recent boundary is re-listed so a backdated/late-surfacing activity
	// (a manual upload's backdate, a second device syncing late, an edited
	// start time) is not permanently produced by the all-time max cursor. The
	// re-listing makes it free — re-listed ids are deduped by the
	// state machine (step c's seen-skip) + seen table. The NULL-cursor first
	// activation 24h lookback gets no −1h: it is already wider. The overlap
	// widens only the LIST window; the cursor advance math (step e) is
	// unchanged.
	var windowStart time.Time
	if a.lastPolledAt != nil {
		windowStart = a.lastPolledAt.Add(-windowOverlap)
	} else {
		windowStart = now.Add(-firstPollLookback)
	}
	summaries, err := s.api.ListActivities(ctx, access, windowStart)
	if err != nil {
		var erl ErrRateLimited
		if errors.As(err, &erl) {
			// App-wide window (not per-athlete): signal the CALLER to abort the
			// WHOLE iteration. This athlete's pass simply ends — no tx commit,
			// cursor unchanged — and the remaining athletes' cursors are
			// implicitly preserved (never visited); the 15-min ticker is the
			// backoff.
			return &errListRateLimited{retryAfter: erl.RetryAfter}
		}
		var eua ErrUnauthorized
		if errors.As(err, &eua) {
			return s.abortReauth(ctx, a)
		}
		return err
	}

	// SNAPSHOT the pending-row set at pass start (before step c's inserts) —
	// the only discriminator a carried row is re-fetchable this pass.
	seenIDs, err := s.loadSeenIDs(ctx, a.id)
	if err != nil {
		return err
	}
	carried, err := s.loadCarriedPending(ctx, a.id)
	if err != nil {
		return err
	}

	// Target resolution (spec: applies to BOTH the step-c INSERT and the
	// step-d UPDATE-to-posted) moved to the (s *Strava).resolveTarget method
	// (decide.go) — decideActivity applies it at DISPOSITION time, never
	// post-commit.

	var inserts []seenInsert
	var updates []seenUpdate
	var posts []postItem

	// ---- c. per NEW listed id (a seen row of any status → no-op here) ----
	for i := range summaries {
		sum := &summaries[i]
		if _, ok := seenIDs[sum.ID]; ok {
			continue
		}
		switch {
		case sum.SportType == "":
			// unparsed device upload → 'pending' (NOT 'skipped'), re-fetched
			// uniformaly in d (the detail yields the final sport_type).
			inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusPending})
		case !family(sum.SportType):
			inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusSkipped})
		default: // family hit → detail
			detail, err := s.api.GetActivity(ctx, access, sum.ID)
			var out fetchOutcome
			if err != nil {
				var eua ErrUnauthorized
				if errors.As(err, &eua) {
					return s.abortReauth(ctx, a)
				}
				var egone ErrGone
				if errors.As(err, &egone) {
					// A 404 for an id the list just returned is a DETERMINISTIC
					// outcome (deleted / access revoked) — the same category as
					// no-target: dispose 'skipped' on first sight, no pending row,
					// never re-fetched; one counted line in the per-cycle log.
					slog.Warn("strava listed activity is gone (404) → skipped on first sight", "module", "strava", "label", a.label, "id", sum.ID)
					out = fetchGone()
				} else {
					var erl ErrRateLimited
					if errors.As(err, &erl) {
						slog.Info("strava detail (rate limited) → pending", "module", "strava", "id", sum.ID, "error", err)
					} else {
						slog.Warn("strava detail transient → pending", "module", "strava", "id", sum.ID, "error", err)
					}
					// the existing hold rule keeps the id in the window; d re-fetches
					// next cycles, and the 5-retry drop covers a stuck fetch.
					out = fetchTransient()
				}
			} else if isProcessing(detail) {
				out = fetchProcessing(detail)
			} else {
				out = fetchReady(detail)
			}
			// The per-activity decision (decide.go): the error arms + the
			// detail's in-family gate + the target resolution. NOTE the
			// documented unification: the detail's sport type is now gated in
			// THIS post-fetch path too (it was previously gated only on the
			// summary pre-fetch above) — see decideActivity.
			d := s.decideActivity(a, out, sum.ID)
			switch d.status {
			case statusSkipped:
				if d.reason == reasonNoTarget {
					slog.Warn("strava family activity has no post target → skipped", "module", "strava", "label", a.label, "id", sum.ID)
				}
				inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusSkipped})
			case statusPending:
				inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusPending})
			default: // statusPosted
				// the posted invariant: dispositioned 'posted' BEFORE posting.
				inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusPosted})
				posts = append(posts, *d.post)
			}
		}
	}

	// ---- d. pending retry (carried rows, re-fetched uniformily) ----
	for i := range carried {
		c := &carried[i]
		detail, err := s.api.GetActivity(ctx, access, c.activityID)
		var out fetchOutcome
		if err != nil {
			var eua ErrUnauthorized
			if errors.As(err, &eua) {
				return s.abortReauth(ctx, a)
			}
			var egone ErrGone
			if errors.As(err, &egone) {
				// Terminal state (like the off-family detail resolution): dispose
				// 'skipped' in the SAME UPDATE — do NOT increment retries; the
				// 5-cycle budget stays for genuinely stuck/transient fetches.
				slog.Warn("strava pending activity is gone (404) → skipped", "module", "strava", "label", a.label, "id", c.activityID)
				out = fetchGone()
			} else {
				var erl ErrRateLimited
				if errors.As(err, &erl) {
					slog.Info("strava detail (retry) rate limited", "module", "strava", "id", c.activityID, "retry_after", erl.RetryAfter)
				}
				// fetch failed again → increment; drop at the new value >= 5.
				out = fetchTransient()
			}
		} else if isProcessing(detail) {
			// still processing → increment; drop at the new value >= 5.
			out = fetchProcessing(detail)
		} else {
			out = fetchReady(detail)
		}
		d := s.decideActivity(a, out, c.activityID)
		switch d.status {
		case statusSkipped:
			// gone (404) or a ready detail that is out of the family / has no
			// target → 'skipped' in the SAME UPDATE, no retries increment.
			updates = append(updates, seenUpdate{c.activityID, statusSkipped, nil})
		case statusPending:
			// fetch failed again / still processing → increment; drop at the
			// new value >= 5.
			newRetries := c.retries + 1
			if newRetries >= dropAfterRetries {
				updates = append(updates, seenUpdate{c.activityID, statusSkipped, &newRetries})
			} else {
				updates = append(updates, seenUpdate{c.activityID, "", &newRetries})
			}
		default: // statusPosted
			updates = append(updates, seenUpdate{c.activityID, statusPosted, nil})
			posts = append(posts, *d.post)
		}
	}

	// ---- 3. ONE transaction: inserts + retries increments + cursor ----
	tx, err := s.app.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := tx.Rollback(ctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			slog.Error("strava tx deferred rollback failed", "module", "strava", "error", rerr)
		}
	}()

	for i := range inserts {
		ins := &inserts[i]
		if _, err := tx.Exec(ctx,
			`INSERT INTO strava_seen_activities (strava_athletes_id, strava_activity_id, start_date, status)
			   VALUES ($1, $2, $3, $4)
			   ON CONFLICT (strava_athletes_id, strava_activity_id) DO NOTHING`,
			a.id, ins.activityID, ins.startDate, ins.status); err != nil {
			return err
		}
	}
	for i := range updates {
		up := &updates[i]
		if _, err := tx.Exec(ctx,
			`UPDATE strava_seen_activities
			   SET status = COALESCE(NULLIF($2, ''), status),
			       retries = COALESCE($3, retries)
			   WHERE strava_athletes_id = $1 AND strava_activity_id = $4`,
			a.id, up.status, up.retries, up.activityID); err != nil {
			return err
		}
	}
	// cursor (step e): pending remain → hold at min(pending); else (nothing
	// pending, all-time dispositioned) → max(dispositioned); else unchanged.
	pendTimes, err := s.loadColumnTimes(ctx, tx, `status = 'pending'`, a.id)
	if err != nil {
		return err
	}
	dispTimes, err := s.loadColumnTimes(ctx, tx, `status <> 'pending'`, a.id)
	if err != nil {
		return err
	}
	next, advanced := nextCursor(pendTimes, dispTimes)
	// advanced → advance to max; or pending non-empty → hold at min.
	// next is zero only in the "else → unchanged" branch, so a zero write is
	// never issued.
	if (advanced || len(pendTimes) > 0) && !next.IsZero() {
		if _, err := tx.Exec(ctx, `UPDATE strava_athletes SET last_polled_at=$1 WHERE id=$2`, next, a.id); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// ---- THEN the posts (never re-posted on a send error) ----
	for i := range posts {
		p := &posts[i]
		if err := s.sendFn(strconv.FormatInt(p.threadID, 10), p.message); err != nil {
			slog.Error("strava post failed (no retry; activity is 'posted')", "module", "strava", "thread", strconv.FormatInt(p.threadID, 10), "error", err)
		}
	}
	return nil
}

// abortReauth commits the needs_reauth UPDATE as its OWN statement (autocommit
// — the per-athlete seen/cursor transaction is simply discarded, never run) and
// aborts this athlete's pass (the cursor is implicitly preserved). Returns nil
// so the caller continues to the next athlete.
func (s *Strava) abortReauth(ctx context.Context, a *athleteRow) error {
	if _, err := s.app.Pool.Exec(ctx, `UPDATE strava_athletes SET needs_reauth=true WHERE id=$1`, a.id); err != nil {
		return err
	}
	slog.Warn("strava re-auth required (invalid/deauthorized token); cursor preserved",
		"module", "strava", "label", a.label, "hint", "re-run the consent and clear needs_reauth")
	return nil
}

// errListRateLimited is the pass-error token for a list-endpoint 429: the
// Strava 15-min rate window is APP-WIDE, so the entire iteration (not just
// this athlete's pass) is aborted and the remaining athletes are deferred to
// the next tick. Distinct from a DETAIL-endpoint 429, which is per-activity
// and stays handled inside the pass (→ pending / retry increment) without
// stopping the other athletes.
type errListRateLimited struct {
	retryAfter string
}

func (e *errListRateLimited) Error() string { return "strava list rate limited (app-wide window)" }

// makePost resolves the target (already) and builds the post message; the
// title branch composes noun = `"title" (distNoun)`.
func (s *Strava) makePost(label string, a Activity, threadID, activityID int64) postItem {
	// Newline-sanitize the title (and, defensively, the label) BEFORE
	// composition so the two-line post-format pin holds no matter what
	// Strava returns.
	label = strings.ReplaceAll(label, "\n", " ")
	title := strings.ReplaceAll(a.Title, "\n", " ")
	distNoun := formatNoun(a)
	noun := distNoun
	if title != "" {
		noun = `"` + title + `" (` + distNoun + `)`
	}
	return postItem{
		threadID: threadID,
		message:  buildPost(label, noun, strconv.FormatInt(activityID, 10)),
	}
}

// ---------------------------------------------------------------------------
// Load helpers (raw SQL, house style)
// ---------------------------------------------------------------------------

func (s *Strava) loadAthletes(ctx context.Context) ([]athleteRow, error) {
	rows, err := s.app.Pool.Query(ctx,
		`SELECT id, label, access_token, refresh_token, token_expires_at,
		       last_polled_at, COALESCE(target_thread_id, 0)
		 FROM strava_athletes
		 WHERE NOT needs_reauth
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []athleteRow
	for rows.Next() {
		var r athleteRow
		if err := rows.Scan(&r.id, &r.label, &r.accessToken, &r.refreshToken, &r.tokenExpiresAt, &r.lastPolledAt, &r.targetThreadID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadAthleteByStravaID loads ONE strava_athletes row by strava_athlete_id
// (the webhook's owner_id) — the DB row carries the token the worker needs
// (NOT the Strava HTTP API: the row is needed to OBTAIN the token). No
// needs_reauth filter: a flagged row is still fetched (the worker's 401→
// refresh path handles the token state; a genuinely revoked refresh fails
// the refresh and re-sets the flag as a no-op). The nil-pool guard is FIRST
// — a nil *pgxpool.Pool receiver PANICS on QueryRow, it does not return an
// error (the guard makes the unit tests' nil-pool construction genuinely
// safe and doubles as a defensive production guard). Returns (nil, nil)
// for an unknown owner (pgx.ErrNoRows); a real DB error returns (nil, err).
func (s *Strava) loadAthleteByStravaID(ctx context.Context, id int64) (*athleteRow, error) {
	if s.app == nil || s.app.Pool == nil {
		return nil, errors.New("strava: no pool")
	}
	var r athleteRow
	err := s.app.Pool.QueryRow(ctx,
		`SELECT id, label, access_token, refresh_token, token_expires_at,
		       last_polled_at, COALESCE(target_thread_id, 0)
		 FROM strava_athletes
		 WHERE strava_athlete_id = $1`, id).
		Scan(&r.id, &r.label, &r.accessToken, &r.refreshToken, &r.tokenExpiresAt, &r.lastPolledAt, &r.targetThreadID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // unknown owner — not an error
		}
		return nil, err
	}
	return &r, nil
}

// loadAthleteByID loads ONE strava_athletes row by its surrogate id — the
// webhook worker's re-load (workerBody's job-start re-load and the 401
// reauth arm's re-check; see those). Same column list as
// loadAthleteByStravaID; the nil-pool guard is FIRST (a nil *pgxpool.Pool
// receiver PANICS on QueryRow, it does not return an error). Returns
// (nil, nil) for a deleted row (pgx.ErrNoRows); a real DB error returns
// (nil, err).
func (s *Strava) loadAthleteByID(ctx context.Context, id int32) (*athleteRow, error) {
	if s.app == nil || s.app.Pool == nil {
		return nil, errors.New("strava: no pool")
	}
	var r athleteRow
	err := s.app.Pool.QueryRow(ctx,
		`SELECT id, label, access_token, refresh_token, token_expires_at,
		       last_polled_at, COALESCE(target_thread_id, 0)
		 FROM strava_athletes
		 WHERE id = $1`, id).
		Scan(&r.id, &r.label, &r.accessToken, &r.refreshToken, &r.tokenExpiresAt, &r.lastPolledAt, &r.targetThreadID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // deleted row — not an error
		}
		return nil, err
	}
	return &r, nil
}

func (s *Strava) loadSeenIDs(ctx context.Context, athleteID int32) (map[int64]struct{}, error) {
	rows, err := s.app.Pool.Query(ctx,
		`SELECT strava_activity_id FROM strava_seen_activities WHERE strava_athletes_id=$1`, athleteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// pendRow is a carried 'pending' row (snapshot at pass start).
type pendRow struct {
	activityID int64
	startDate  time.Time
	retries    int
}

func (s *Strava) loadCarriedPending(ctx context.Context, athleteID int32) ([]pendRow, error) {
	rows, err := s.app.Pool.Query(ctx,
		`SELECT strava_activity_id, start_date, retries FROM strava_seen_activities
		 WHERE strava_athletes_id=$1 AND status='pending'`, athleteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendRow
	for rows.Next() {
		var r pendRow
		if err := rows.Scan(&r.activityID, &r.startDate, &r.retries); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadColumnTimes returns the start_date of every seen row matching statusCond.
// `cond` is an internal constant (never caller input) — no injection surface.
func (s *Strava) loadColumnTimes(ctx context.Context, tx dbTx, cond string, athleteID int32) ([]time.Time, error) {
	rows, err := tx.Query(ctx,
		`SELECT start_date FROM strava_seen_activities WHERE strava_athletes_id=$1 AND `+cond, athleteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Pure decision functions (the unit-test surface)
// ---------------------------------------------------------------------------

// family reports whether sportType is one of the fixed 9 run/cycling family
// values (spec §1).
func family(sportType string) bool {
	_, ok := familySet[sportType]
	return ok
}

// isProcessing applies the documented fallback: a PRESENT in_progress value wins
// over the resource_state==-1 "processing" indicator — only the ABSENCE of the
// field falls through to resource_state.
func isProcessing(a Activity) bool {
	if a.InProgressSet {
		return a.InProgress
	}
	return a.ResourceState == -1
}

// nextCursor applies the cursor decision (spec step e):
//   - pending non-empty → (min(pending), false)          [hold the window open]
//   - else dispositioned non-empty → (max(dispositioned), true)
//   - else → (zero time, false)                          [unchanged]
//
// The CALLER loads `dispositioned` = start dates of EVERY seen row with status
// != 'pending' — ALL-TIME, not this-pass-only (step-d resolutions count once
// their status is updated to posted/skipped).
func nextCursor(pending, dispositioned []time.Time) (time.Time, bool) {
	if len(pending) > 0 {
		min := pending[0]
		for _, t := range pending[1:] {
			if t.Before(min) {
				min = t
			}
		}
		return min, false
	}
	if len(dispositioned) > 0 {
		max := dispositioned[0]
		for _, t := range dispositioned[1:] {
			if t.After(max) {
				max = t
			}
		}
		return max, true
	}
	return time.Time{}, false
}

// formatNoun returns `<distance> <SportType>`: meters → km. >= 100 km rounds
// HALF UP to an integer via int64(m/1000+0.5) (e.g. "142 km"); < 100 km is one
// decimal via %.1f (e.g. "42.3 km"). The branch flips at exactly 100.0 km. A
// non-finite (NaN, +/-Inf) or negative distance is treated as 0 — one check
// covers all three since NaN fails `>= 0` — so the post never carries
// garbage digits into a user-facing message.
func formatNoun(a Activity) string {
	if !(a.Distance >= 0) || math.IsInf(a.Distance, 0) {
		a.Distance = 0
	}
	var dist string
	if a.Distance >= 100000 {
		dist = strconv.FormatInt(int64(a.Distance/1000+0.5), 10) + " km"
	} else {
		dist = fmt.Sprintf("%.1f km", a.Distance/1000)
	}
	return dist + " " + a.SportType
}

// buildPost is the exact two-line post: the label line + the activity link.
func buildPost(label, noun, activityID string) string {
	return label + " finished " + noun + "\n" + "https://www.strava.com/activities/" + activityID
}

// ---------------------------------------------------------------------------
// The /strava command (onboarding): a member runs it in the thread they
// want posts to; the bot replies with a one-time, state-scoped authorize
// link and inserts a pending row (migration 000008).
// ---------------------------------------------------------------------------

// Response is this module's share of Rust's HandlerResponse shape: the
// command never defers, so the fields it uses are Content and Ephemeral
// (always true — the reply is visible ONLY to the invoker, binding the
// posted link to them); DeferResponse is present for parity with the
// Rust struct default `Option::None`.
type Response struct {
	Content       string
	Ephemeral     bool
	DeferResponse *bool
}

// sanitizeLabel trims the label, collapses newline sequences to single
// spaces, and caps at 32 runes (first 32). The newline order matters: "\r\n"
// is replaced as a UNIT first (one space, not two), then lone "\r", then
// lone "\n" — a naive two-pass ReplaceAll would turn "A\r\nB" into "A  B".
// Empty/whitespace-only input returns "".
func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > 32 {
		return string(r[:32])
	}
	return s
}

// newState is the one-time state token: 16 random bytes as 32 lowercase
// hex chars.
func newState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// authorizeURL is the exact Strava authorize URL. The redirect URI is a
// code constant (public, not a secret); the client_id is already public in
// the URL.
func authorizeURL(clientID, state string) string {
	return "https://www.strava.com/oauth/authorize?client_id=" + clientID +
		"&redirect_uri=https%3A%2F%2Ftugbot.wizards.town%2Fstrava%2Fcallback&response_type=code&scope=activity:read_all&state=" + state
}

// SetupCommand is the /strava registration: one optional string option
// "label" (the feat handler's option shape is the template).
func (s *Strava) SetupCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "strava",
		Description: "Add your Strava account — finished runs and rides post to this thread",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "label",
				Type:        discordgo.ApplicationCommandOptionString,
				Description: "Display name for posts (defaults to your Strava first name)",
				// Required left false — the label is optional.
			},
		},
	}
}

// HandleInteraction is the /strava command body. It does NOT gate on the
// feature flag (onboarding is valid while the feature is off — the row just
// sits until it's enabled) and does NOT dedupe (a second /strava in the same
// thread inserts a fresh row; the unused one expires). DB failures become an
// error Response — never a panic.
func (s *Strava) HandleInteraction(i *discordgo.Interaction) Response {
	// Creds preflight: without client credentials the link would be dead —
	// reply without inserting a row.
	if s.app.Cfg.StravaClientID == "" || s.app.Cfg.StravaClientSecret == "" {
		return Response{Content: "strava is not configured on this bot — the owner needs to set the client credentials first", Ephemeral: true}
	}
	// Extract the label: the first option whose name is "label" (a name
	// match, not Options[0] — a future second option must not be
	// silently misread; no matching option → empty label), string value.
	var label string
	if data, ok := i.Data.(discordgo.ApplicationCommandInteractionData); ok {
		for _, opt := range data.Options {
			if opt.Name != "label" {
				continue
			}
			if v, ok := opt.Value.(string); ok {
				label = sanitizeLabel(v)
			}
			break
		}
	}
	state, err := newState()
	if err != nil {
		slog.Error("onboarding state generation failed", "module", "strava", "error", err)
		return Response{Content: "onboarding setup failed — try again", Ephemeral: true}
	}
	// thread_id: the interaction's ChannelID is a string; the column is
	// bigint. Empty/unparsable → the setup-failed reply (no row).
	threadID, err := strconv.ParseInt(i.ChannelID, 10, 64)
	if err != nil {
		slog.Error("onboarding channel id unparsable", "module", "strava", "channel_id", i.ChannelID, "error", err)
		return Response{Content: "onboarding setup failed — try again", Ephemeral: true}
	}
	var labelArg any = nil
	if label != "" {
		labelArg = label
	}
	_, err = s.app.Pool.Exec(context.Background(),
		`INSERT INTO strava_onboardings (state, thread_id, label, status, created_at, expires_at)
		VALUES ($1, $2, $3, 'pending', now(), now() + $4::interval)`,
		state, threadID, labelArg, onboardingTTL.String())
	if err != nil {
		slog.Error("onboarding row insert failed", "module", "strava", "error", err)
		return Response{Content: "onboarding setup failed — try again", Ephemeral: true}
	}
	content := authorizeURL(s.app.Cfg.StravaClientID, state) + " — this link expires in an hour."
	// The disabled-clause (the command itself is NOT gated on the flag).
	// CheckEnabled, not IsEnabled: IsEnabled silently returns false on ANY
	// DB error, so a transient pool blip would append the clause on a
	// perfectly valid link. On a DB error append nothing — the link is
	// still valid and the row is still inserted (the command is ungated).
	enabled, flagErr := features.CheckEnabled(context.Background(), s.app.Pool, FeatureKey)
	switch {
	case flagErr != nil:
		slog.Warn("strava feature flag check failed", "module", "strava", "error", flagErr)
	case !enabled:
		content += " (the strava feature is disabled — you'll be tracked once it's enabled)"
	}
	// Ephemeral: the link is visible ONLY to the invoker (a thread member
	// can no longer consume it — the ownership check is impossible in a
	// plain browser redirect, so visibility IS the binding). The later
	// confirm is a SEPARATE, non-ephemeral channel message that posts to
	// the thread for everyone (the sendFn seam — unchanged).
	return Response{Content: content, Ephemeral: true}
}

// ---------------------------------------------------------------------------
// The onboarding callback (decision 0012): the :8643 in-process listener.
// Strava redirects the consenting athlete's browser to
// tugbot.wizards.town/strava/callback?code=…&state=…; this handler
// completes the flow and renders the "Done" page. Deliberately inside the
// bot process — consolidated liveness, one unit, confirm on the bot's own
// session.
// ---------------------------------------------------------------------------

// stravaOnboardAddr is the in-process callback listener address — a code
// constant (no env var, per the spec). Unexported: package main never
// references it — the address is hidden inside Start.
const stravaOnboardAddr = ":8643"

// scopeHasReadAll reports whether the space-separated scope string (the
// token response's `scope`, e.g. "activity:read_all read") contains the
// EXACT field "activity:read_all" — a field match, not a substring.
func scopeHasReadAll(scope string) bool {
	for _, f := range strings.Fields(scope) {
		if f == "activity:read_all" {
			return true
		}
	}
	return false
}

// resolveLabel is the display/fallback resolution: the first non-empty of
// (rowLabel, existingLabel, firstname, username), else "Athlete". An
// explicit command label overrides (new or existing athlete); an omitted
// label preserves the stored label for an existing athlete (a re-consent
// never silently relabels a custom label); a new athlete falls to first
// name → username → "Athlete".
func resolveLabel(rowLabel, existingLabel, firstname, username string) string {
	for _, v := range []string{rowLabel, existingLabel, firstname, username} {
		if v != "" {
			return v
		}
	}
	return "Athlete"
}

// onboardingPage renders the full HTML page (the same shape the retired
// sidecar rendered). Both fields are escaped. The page never carries
// secrets: the code is single-use and the tokens are never rendered.
func onboardingPage(title, body string) []byte {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html><head><meta charset=\"utf-8\">" +
		"<style>body{font-family:monospace}</style></head><body>\n")
	b.WriteString("<h2>" + html.EscapeString(title) + "</h2>\n")
	b.WriteString("<p>" + html.EscapeString(body) + "</p>\n")
	b.WriteString("</body></html>\n")
	return []byte(b.String())
}

// throttleEntry is one client's fixed 60 s window (10 per window; the
// entry resets fully when the window lapses).
type throttleEntry struct {
	count       int
	windowStart time.Time
}

// OnboardingHandler returns the single-route callback handler (GET
// /strava/callback; any other method or path 404s). The throttle state is
// created HERE (closure-local, mutex-protected, lazily pruned on access) —
// NEVER a struct field initialized in New(): the unit throttle test
// constructs a bare &Strava{} and the integration tests build the handler
// via struct literal; neither goes through New(), so a New()-initialized
// field would be a nil map and panic on first write.
func (s *Strava) OnboardingHandler() http.Handler {
	var (
		thMu    sync.Mutex
		thState = make(map[string]throttleEntry)
	)
	const (
		throttleLimit  = 10
		throttleWindow = 60 * time.Second
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Route guard: only GET /strava/callback.
		if r.Method != http.MethodGet || r.URL.Path != "/strava/callback" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(onboardingPage("Not found", ""))
			return
		}
		render := func(status int, title, body string) {
			w.WriteHeader(status)
			_, _ = w.Write(onboardingPage(title, body))
		}
		// 2. Throttle — 10 per fixed 60 s window (resets at lapse), BEFORE
		// any DB work. Key = the LAST hop of X-Forwarded-For — caddy's
		// default reverse_proxy PRESERVES an inbound client-supplied XFF
		// and APPENDS the trusted client IP at the END of the chain, so
		// the first hop is attacker-controlled (an internet client
		// rotating its own XFF would get a fresh bucket per request);
		// behind the proxy r.RemoteAddr is always caddy's own address, so
		// RemoteAddr alone would be one global bucket for every athlete.
		// Fall back to r.RemoteAddr when the header is absent. Guarantee:
		// the per-IP throttle holds BEHIND the trusted proxy (caddy
		// appends the client IP); on the direct (non-proxied) path the
		// hardening is ReadHeaderTimeout + the cheap route guard + the
		// port-stripped fallback bucket.
		key := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			hops := strings.Split(xff, ",")
			for i := len(hops) - 1; i >= 0; i-- {
				if hop := strings.TrimSpace(hops[i]); hop != "" {
					key = hop
					break
				}
			}
		} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			// Direct path: the source port is ephemeral — a raw
			// RemoteAddr would be a fresh bucket per connection and the
			// 10/min limit would never fire. Strip the port (fall back
			// to the raw RemoteAddr on error).
			key = host
		}
		now := time.Now()
		thMu.Lock()
		// Sweep: delete entries whose window has lapsed — without this
		// every unique key ever seen leaks an entry forever (attacker-
		// controlled unbounded growth on an unauthenticated port). The map
		// is small at onboarding scale, so a full sweep under the mutex is
		// cheap.
		for k, e := range thState {
			if now.Sub(e.windowStart) >= throttleWindow {
				delete(thState, k)
			}
		}
		e := thState[key]
		if e.windowStart.IsZero() || now.Sub(e.windowStart) >= throttleWindow {
			// Hard cap: a single-window burst of unique keys can balloon
			// the map past the sweep's steady-state bound — past 10 000
			// stored entries a NEW key is throttled, not inserted.
			if len(thState) > 10000 {
				thMu.Unlock()
				w.Header().Set("Retry-After", "60")
				render(http.StatusTooManyRequests, "Too many requests", "Too many requests — try again later")
				return
			}
			e = throttleEntry{windowStart: now} // lazy pruning: the stale entry resets
		}
		if e.count >= throttleLimit {
			thMu.Unlock()
			w.Header().Set("Retry-After", "60")
			render(http.StatusTooManyRequests, "Too many requests", "Too many requests — try again later")
			return
		}
		e.count++
		thState[key] = e
		thMu.Unlock()

		// 3. Parse. Strava's denial redirect (the athlete clicked Decline):
		// error=access_denied, no code → 200; the pending row is left
		// as-is (it expires harmlessly; the athlete may retry — a denial
		// is not a flow failure, so NO failed mark).
		q := r.URL.Query()
		if q.Get("error") != "" {
			render(http.StatusOK, "Declined", "consent was not granted — run /strava again")
			return
		}
		code := q.Get("code")
		state := q.Get("state")
		if code == "" || state == "" {
			render(http.StatusNotFound, "Not found", "missing code or state")
			return
		}
		// ctx provenance: the HTTP request context (client-abort-cancellable;
		// the 30 s stravaHTTP timeout bounds the Strava calls).
		ctx := r.Context()

		// 4. State lookup (label scans into *string — the column is
		// nullable; a NULL label becomes "" for resolveLabel).
		var (
			status   string
			expires  time.Time
			threadID int64
			rowLabel *string
		)
		pool := s.app.Pool
		if err := pool.QueryRow(ctx,
			`SELECT status, expires_at, thread_id, label FROM strava_onboardings WHERE state = $1`, state).
			Scan(&status, &expires, &threadID, &rowLabel); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				render(http.StatusNotFound, "Not found", "unknown or expired link — run /strava again")
				return
			}
			slog.Error("strava onboarding state lookup failed", "module", "strava", "state", state, "error", err)
			render(http.StatusInternalServerError, "Internal error", "internal error — try again")
			return
		}
		if status != statusPending {
			render(http.StatusConflict, "Already used", "this link was already used")
			return
		}
		if expires.Before(time.Now()) {
			if _, err := pool.Exec(ctx, `DELETE FROM strava_onboardings WHERE state = $1`, state); err != nil {
				slog.Error("strava onboarding expired-row delete failed", "module", "strava", "state", state, "error", err)
				render(http.StatusInternalServerError, "Internal error", "internal error — try again")
				return
			}
			render(http.StatusNotFound, "Expired", "expired — run /strava again")
			return
		}
		rowLabelStr := ""
		if rowLabel != nil {
			rowLabelStr = *rowLabel
		}
		// markFailed leaves the row 'pending' on a DB error — the 1h TTL +
		// the per-tick cleanup absorb it (no compensating rollback).
		markFailed := func(logMsg string) {
			if _, err := pool.Exec(ctx, `UPDATE strava_onboardings SET status = 'failed' WHERE state = $1 AND status = 'pending'`, state); err != nil {
				slog.Error(logMsg+" (failed-mark update errored)", "module", "strava", "state", state, "error", err)
			}
		}

		// 5. Exchange (never log the code or tokens).
		accessToken, refreshToken, expiresAt, scope, err := s.api.ExchangeCode(ctx, s.app.Cfg.StravaClientID, s.app.Cfg.StravaClientSecret, code)
		if err != nil {
			markFailed("strava onboarding exchange failed")

			slog.Warn("strava onboarding exchange failed", "module", "strava", "state", state, "error", err)
			render(http.StatusBadRequest, "Authorization failed", "authorization failed — run /strava again")
			return
		}
		// 6. Scope check.
		if !scopeHasReadAll(scope) {
			markFailed("strava onboarding scope missing")
			slog.Warn("strava onboarding scope missing", "module", "strava", "state", state, "reason", "consent lacked activity:read_all")
			render(http.StatusBadRequest, "Insufficient scope", "consent lacked activity:read_all — use the link /strava posts")
			return
		}
		// 7. Athlete fetch.
		athlete, err := s.api.GetAthlete(ctx, accessToken)
		if err != nil {
			markFailed("strava onboarding athlete fetch failed")
			slog.Warn("strava onboarding athlete fetch failed", "module", "strava", "state", state, "error", err)
			render(http.StatusBadRequest, "Profile fetch failed", "could not load the athlete profile — run /strava again")
			return
		}
		// 9. Existing-athlete pre-check (drives step 8's existingLabel and
		// the confirm text).
		var existingLabelPtr *string
		err = pool.QueryRow(ctx, `SELECT label FROM strava_athletes WHERE strava_athlete_id = $1`, athlete.ID).Scan(&existingLabelPtr)
		isNew := true
		existingLabel := ""
		switch {
		case err == nil:
			isNew = false
			if existingLabelPtr != nil {
				existingLabel = *existingLabelPtr
			}
		case errors.Is(err, pgx.ErrNoRows):
			// no row → new athlete
		default:
			slog.Error("strava onboarding existing-athlete pre-check failed", "module", "strava", "error", err)
			render(http.StatusInternalServerError, "Internal error", "internal error — try again")
			return
		}
		// 8. Label (the Go-side resolution decides the final value — the
		// upsert applies it unconditionally; no SQL-side COALESCE).
		label := resolveLabel(rowLabelStr, existingLabel, athlete.Firstname, athlete.Username)
		// 10+11. ONE transaction: the locked re-check + the athlete upsert
		// + the claim. The pre-exchange lookup (step 4) is only the fast
		// path (404/409/expired before burning the single-use code); THIS
		// re-check under SELECT ... FOR UPDATE is the arbiter — a
		// concurrent same-state replay blocks here until we commit, then
		// sees 'done' and gets 409. A zero-row claim is therefore
		// impossible: the confirm (step 12) fires only after a successful
		// commit, so a replay can never double-upsert or double-confirm.
		tx, err := pool.Begin(ctx)
		if err != nil {
			slog.Error("strava onboarding claim tx begin failed", "module", "strava", "state", state, "error", err)
			render(http.StatusInternalServerError, "Internal error", "internal error — try again")
			return
		}
		defer func() {
			if rerr := tx.Rollback(ctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
				slog.Error("strava onboarding claim tx deferred rollback failed", "module", "strava", "state", state, "error", rerr)
			}
		}()
		var (
			lockedStatus string
			lockedExpiry time.Time
		)
		if err := tx.QueryRow(ctx,
			`SELECT status, expires_at FROM strava_onboardings WHERE state = $1 FOR UPDATE`, state).
			Scan(&lockedStatus, &lockedExpiry); err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, pgx.ErrNoRows) {
				// Gone between step 4 and the lock (a concurrent expired-delete
				// or the per-tick sweep) — same 404 as the fast path.
				render(http.StatusNotFound, "Not found", "unknown or expired link — run /strava again")
				return
			}
			slog.Error("strava onboarding locked re-check failed", "module", "strava", "state", state, "error", err)
			render(http.StatusInternalServerError, "Internal error", "internal error — try again")
			return
		}
		if lockedStatus != statusPending {
			_ = tx.Rollback(ctx)
			render(http.StatusConflict, "Already used", "this link was already used")
			return
		}
		if lockedExpiry.Before(time.Now()) {
			// Expired in the sub-second window between step 4 and the lock:
			// delete + rollback (the per-tick sweep absorbs the row either
			// way) + 404.
			if _, err := tx.Exec(ctx, `DELETE FROM strava_onboardings WHERE state = $1`, state); err != nil {
				_ = tx.Rollback(ctx)
				slog.Error("strava onboarding expired-row delete failed", "module", "strava", "state", state, "error", err)
				render(http.StatusInternalServerError, "Internal error", "internal error — try again")
				return
			}
			_ = tx.Rollback(ctx)
			render(http.StatusNotFound, "Expired", "expired — run /strava again")
			return
		}
		// 10. Upsert (inside the claim transaction — the locked re-check
		// already verified pending, so no replay reaches this statement).
		if _, err := tx.Exec(ctx,
			`INSERT INTO strava_athletes (label, strava_athlete_id, access_token, refresh_token, token_expires_at, target_thread_id, needs_reauth)
			VALUES ($1, $2, $3, $4, $5, $6, false)
			ON CONFLICT (strava_athlete_id) DO UPDATE SET
			    access_token = EXCLUDED.access_token,
			    refresh_token = EXCLUDED.refresh_token,
			    token_expires_at = EXCLUDED.token_expires_at,
			    needs_reauth = false,
			    target_thread_id = EXCLUDED.target_thread_id,
			    label = EXCLUDED.label`,
			label, athlete.ID, accessToken, refreshToken, expiresAt, threadID); err != nil {
			_ = tx.Rollback(ctx)
			slog.Error("strava onboarding athlete upsert failed", "module", "strava", "error", err)
			render(http.StatusInternalServerError, "Internal error", "internal error — try again")
			return
		}
		// 11. Claim (the locked re-check already verified pending — the
		// status='pending' condition is no longer load-bearing).
		if _, err := tx.Exec(ctx, `UPDATE strava_onboardings SET status = 'done' WHERE state = $1`, state); err != nil {
			_ = tx.Rollback(ctx)
			slog.Error("strava onboarding claim failed", "module", "strava", "state", state, "error", err)
			render(http.StatusInternalServerError, "Internal error", "internal error — try again")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			slog.Error("strava onboarding claim tx commit failed", "module", "strava", "state", state, "error", err)
			render(http.StatusInternalServerError, "Internal error", "internal error — try again")
			return
		}
		// 12. Confirm via the handler's existing send (the posts'
		// ChannelMessageSendComplex parse: [] sender). Fires ONLY after a
		// successful commit (a zero-row claim is impossible — the locked
		// re-check is the arbiter). A send failure is logged — the flow
		// still succeeds (the page still says Done; the row is live).
		confirm := "✅ " + label + " is now tracked — finished runs and rides will post here."
		if !isNew {
			confirm = "🔄 " + label + "'s Strava authorization was refreshed."
		}
		if s.sendFn != nil {
			if err := s.sendFn(strconv.FormatInt(threadID, 10), confirm); err != nil {
				slog.Error("strava onboarding confirm failed", "module", "strava", "label", label, "error", err)
			}
		}
		// 13. Log + render.
		slog.Info("strava onboarding completed", "module", "strava", "label", label, "thread", threadID, "new", isNew)
		render(http.StatusOK, "Done", label+" is set up. You can close this tab.")
	})
}

// webhookJob is one enqueued webhook event (the worker fetches + decides +
// posts; Task 3 replaces the stub body). aspect is "create"/"update" — a
// log-only diagnostic (the seen table has no reason column to persist it).
type webhookJob struct {
	ath    *athleteRow
	actID  int64
	aspect string
}

// flexID is the lenient id field: it decodes a JSON NUMBER (the documented
// Strava event shape — `"object_id": 1360128428`, `"owner_id": 134815`)
// OR a JSON STRING (the lenient acceptance — a string-shaped id still
// parses); a value that parses neither decodes to 0 (NOT an error — the
// caller's unparseable-id no-op arm handles it: one bad id never
// hard-fails the whole body; genuinely malformed JSON still fails the
// top-level Decode and takes the 200 no-op arm).
type flexID struct {
	v   int64
	raw string // the raw wire bytes, captured in UnmarshalJSON (see below)
}

// Int64 returns the parsed id (0 when the value parsed to nothing).
func (f *flexID) Int64() int64 { return f.v }

// UnmarshalJSON implements json.Unmarshaler: a JSON number (via
// json.Number) or a JSON string (via strconv.ParseInt); null / empty /
// any other shape → 0. The raw wire bytes are always captured (even for a
// parsed value) so the unparseable-id no-op log can show WHICH producer
// sent a weird shape — the parsed value is always 0 in exactly those
// cases, so logging it would be useless.
func (f *flexID) UnmarshalJSON(b []byte) error {
	f.raw = string(b)
	switch {
	case len(b) == 0:
		return nil // empty input → 0 (unreachable via encoding/json, which
		// never passes an empty slice; a direct call must not panic on b[0])
	case string(b) == "null", string(b) == `""`:
		return nil // absent / empty → 0 (the caller's no-op arm)
	case b[0] == '"':
		s, err := strconv.Unquote(string(b))
		if err != nil {
			return nil // unparseable string → 0
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil // unparseable string → 0
		}
		f.v = v
		return nil
	default:
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return nil // not a JSON number → 0
		}
		v, err := n.Int64()
		if err != nil {
			return nil // non-int64 number (float / overflow) → 0
		}
		f.v = v
		return nil
	}
}

// webhookEvent is the Strava webhook POST payload (the fields we use; the
// updates map's values are decoded as any — the deauth arm requires the
// EXACT string "false" and treats any other shape as ambiguous). The ids
// are flexID: Strava's documented payload carries them as JSON NUMBERS —
// a bare string field would hard-fail the decode and silently 200-no-op
// every real delivery (the poll backstop would mask the dead feature) —
// and the string shape is accepted leniently.
type webhookEvent struct {
	AspectType string         `json:"aspect_type"`
	ObjectType string         `json:"object_type"`
	ObjectID   flexID         `json:"object_id"`
	OwnerID    flexID         `json:"owner_id"`
	Updates    map[string]any `json:"updates"`
}

// routes returns the :8643 mux: the onboarding handler (its existing route
// guard — GET /strava/callback only — stays untouched) + the webhook
// handler. Extracted so the mux is testable without binding a port.
func (s *Strava) routes() http.Handler {
	m := http.NewServeMux()
	m.Handle("/strava/callback", s.OnboardingHandler())
	m.Handle("/strava/webhook", s.WebhookHandler())
	return m
}

// WebhookHandler returns the webhook handler: POST /strava/webhook (the
// event stream) and GET /strava/webhook?hub.mode=subscribe (the
// verification handshake) — both methods on the same path; anything else
// 404s. The factory starts the workers ONCE (workerOnce). The throttle
// state is closure-local — its OWN map (the onboarding handler's map stays
// untouched; two independent throttle maps, one per route) with the same
// shape: last-XFF-hop keying, sweep-bounded map, 10/60 s window,
// Retry-After: 60, the 10 000 hard cap. The throttle runs BEFORE the
// verification GET (the GET is throttled too — an unthrottled echo arm
// would be an unlimited-rate guessing oracle for hub.verify_token on this
// unauthenticated port).
func (s *Strava) WebhookHandler() http.Handler {
	s.workerOnce.Do(s.startWorkers)
	var (
		thMu    sync.Mutex
		thState = make(map[string]throttleEntry)
	)
	const (
		throttleLimit  = 10
		throttleWindow = 60 * time.Second
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Route guard: only /strava/webhook (both methods — Strava POSTs
		// events and GETs the verification challenge on the same path).
		if r.URL.Path != "/strava/webhook" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// 2. Throttle — 10 per fixed 60 s window (resets at lapse), BEFORE
		// any verification or body work — the verification GET is
		// throttled too (an unthrottled echo arm would be an unlimited-rate
		// guessing oracle for hub.verify_token on this unauthenticated
		// port). Key = the LAST hop of X-Forwarded-For (caddy's
		// reverse_proxy preserves an inbound client-supplied XFF and
		// appends the trusted client IP at the END of the chain; fall back
		// to the port-stripped RemoteAddr when the header is absent) — the
		// onboarding handler's keying, verbatim, on its own map.
		key := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			hops := strings.Split(xff, ",")
			for i := len(hops) - 1; i >= 0; i-- {
				if hop := strings.TrimSpace(hops[i]); hop != "" {
					key = hop
					break
				}
			}
		} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			// Direct path: the source port is ephemeral — a raw RemoteAddr
			// would be a fresh bucket per connection and the 10/min limit
			// would never fire. Strip the port (fall back to the raw
			// RemoteAddr on error).
			key = host
		}
		now := time.Now()
		thMu.Lock()
		// Sweep: delete entries whose window has lapsed — without this every
		// unique key ever seen leaks an entry forever (attacker-controlled
		// unbounded growth on an unauthenticated port). The map is small at
		// webhook scale, so a full sweep under the mutex is cheap.
		for k, e := range thState {
			if now.Sub(e.windowStart) >= throttleWindow {
				delete(thState, k)
			}
		}
		e := thState[key]
		if e.windowStart.IsZero() || now.Sub(e.windowStart) >= throttleWindow {
			// Hard cap: a single-window burst of unique keys can balloon the
			// map past the sweep's steady-state bound — past 10 000 stored
			// entries a NEW key is throttled, not inserted.
			if len(thState) > 10000 {
				thMu.Unlock()
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			e = throttleEntry{windowStart: now} // lazy pruning: the stale entry resets
		}
		if e.count >= throttleLimit {
			thMu.Unlock()
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		e.count++
		thState[key] = e
		thMu.Unlock()

		// 3. Verification handshake (the subscribe GET). A matching
		// non-empty token → 200 + the exact hub.challenge echoed as JSON;
		// a mismatch or an empty config token → 403 with the challenge NOT
		// echoed (no oracle); a GET without hub.mode=subscribe → 404. The
		// token match is a constant-time compare (no timing side-channel
		// on the verify_token comparison).
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("hub.mode") != "subscribe" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var token string
			if s.app != nil && s.app.Cfg != nil {
				token = s.app.Cfg.StravaWebhookVerifyToken
			}
			if token != "" && subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("hub.verify_token")), []byte(token)) == 1 {
				body, _ := json.Marshal(map[string]string{"hub.challenge": r.URL.Query().Get("hub.challenge")})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(body)
				return
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// 4. Parse. NEVER 5xx on a webhook: a parse failure is a 200 no-op
		// (Strava retries non-200s up to 3 times; we want the event dropped,
		// not replayed).
		var ev webhookEvent
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ev); err != nil {
			slog.Warn("strava webhook: unparseable body — no-op", "module", "strava", "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		// 5. Classify (every arm is 200 — the ACK is the 2 s contract).
		switch {
		case ev.ObjectType == "activity" && (ev.AspectType == "create" || ev.AspectType == "update"):
			s.handleActivityEvent(w, r, ev)
		case ev.ObjectType == "athlete" && ev.AspectType == "update":
			s.handleAthleteUpdate(w, r, ev)
		case ev.AspectType == "delete":
			slog.Info("strava webhook: delete event — no-op (out of scope)", "module", "strava", "object_type", ev.ObjectType, "object_id", ev.ObjectID.Int64())
			w.WriteHeader(http.StatusOK)
		default:
			slog.Info("strava webhook: unhandled event — no-op", "module", "strava", "object_type", ev.ObjectType, "aspect_type", ev.AspectType)
			w.WriteHeader(http.StatusOK)
		}
	})
}

// handleActivityEvent is the create/update arm: load the athlete row by
// strava_athlete_id (= the payload's owner_id), enqueue the job
// non-blockingly, and 200 IMMEDIATELY — the ACK goes out BEFORE any worker
// work (Strava expects 200 within 2 s; the work is async). A DB error or an
// unknown owner_id is a log + 200 no-op (the poll backstop covers it).
func (s *Strava) handleActivityEvent(w http.ResponseWriter, r *http.Request, ev webhookEvent) {
	ack := func() { w.WriteHeader(http.StatusOK) }
	// flexID: a 0 here means the id parsed to nothing (a value that is
	// neither a JSON number nor a parsable string — or a literal 0) →
	// the unparseable-id no-op arm (one bad id never hard-fails the whole
	// body; the poll backstop covers the drop). The raw wire value is what
	// the no-op log shows (the parsed value is always 0 in exactly these
	// cases — logging it would be useless).
	ownerID := ev.OwnerID.Int64()
	if ownerID == 0 {
		slog.Warn("strava webhook: unparseable owner_id — no-op", "module", "strava", "owner_id_raw", ev.OwnerID.raw)
		ack()
		return
	}
	actID := ev.ObjectID.Int64()
	if actID == 0 {
		slog.Warn("strava webhook: unparseable object_id — no-op", "module", "strava", "object_id_raw", ev.ObjectID.raw)
		ack()
		return
	}
	ath, err := s.loadAthleteByStravaID(r.Context(), ownerID)
	if err != nil {
		slog.Error("strava webhook: athlete lookup failed — no-op (backstop covers it)", "module", "strava", "owner_id", ownerID, "error", err)
		ack()
		return
	}
	if ath == nil {
		slog.Warn("strava webhook: unknown owner_id — no-op", "module", "strava", "owner_id", ownerID)
		ack()
		return
	}
	// Enqueue non-blockingly: a full queue DROPS with a log (the poll
	// backstop covers the drop) — never block the 200 on the queue.
	select {
	case s.jobs <- webhookJob{ath: ath, actID: actID, aspect: ev.AspectType}:
	default:
		slog.Warn("strava webhook queue full — dropping (backstop covers it)", "module", "strava", "owner_id", ownerID, "activity", actID)
	}
	// The 200 is sent BEFORE the worker does anything — the ACK is the
	// contract (the work is async).
	ack()
}

// handleAthleteUpdate is the athlete-update (deauth) arm: a deauthorization
// (the updates payload carries authorized: "false") sets needs_reauth=TRUE
// (one small write, inside the 2 s budget) and 200s. An absent key, a
// non-string value, or any value other than the exact string "false" is an
// AMBIGUOUS shape: log + 200 no-op and let the poll's 401 arm handle it —
// we do NOT guess.
func (s *Strava) handleAthleteUpdate(w http.ResponseWriter, r *http.Request, ev webhookEvent) {
	ack := func() { w.WriteHeader(http.StatusOK) }
	// flexID: a 0 = the id parsed to nothing (the unparseable-id no-op
	// arm — one bad id never hard-fails the whole body). The raw wire
	// value is what the no-op log shows (the parsed value is always 0 in
	// exactly these cases — logging it would be useless).
	ownerID := ev.OwnerID.Int64()
	if ownerID == 0 {
		slog.Warn("strava webhook: unparseable owner_id — no-op", "module", "strava", "owner_id_raw", ev.OwnerID.raw)
		ack()
		return
	}
	v, ok := ev.Updates["authorized"]
	if !ok {
		slog.Warn("strava webhook: athlete update without an authorized field — no-op (the poll's 401 arm handles it)", "module", "strava", "owner_id", ownerID)
		ack()
		return
	}
	auth, isStr := v.(string)
	if !isStr || auth != "false" {
		slog.Warn("strava webhook: ambiguous authorized value — no-op (the poll's 401 arm handles it)", "module", "strava", "owner_id", ownerID, "value", v)
		ack()
		return
	}
	if s.app == nil || s.app.Pool == nil {
		slog.Error("strava: no pool (deauth flag update skipped)", "module", "strava", "owner_id", ownerID)
		ack()
		return
	}
	if _, err := s.app.Pool.Exec(r.Context(), `UPDATE strava_athletes SET needs_reauth = true WHERE strava_athlete_id = $1`, ownerID); err != nil {
		// The UPDATE failed — do NOT claim "needs_reauth set" (the log
		// stream must not claim success on a failed write): the error line
		// is the only line, and the 200 still goes out (the poll's 401 arm
		// is the backstop for the un-set flag).
		slog.Error("strava webhook: deauth flag update failed", "module", "strava", "owner_id", ownerID, "error", err)
		ack()
		return
	}
	slog.Warn("strava webhook: deauthorization — needs_reauth set", "module", "strava", "owner_id", ownerID)
	ack()
}

// startWorkers spawns the 2 worker goroutines (draining s.jobs until
// serverCtx is done) and the workersDone closer — once (workerOnce). Each
// worker calls s.workerFn (nil = workerBody; the test hook captures jobs
// without racing the live workers).
func (s *Strava) startWorkers() {
	s.jobs = make(chan webhookJob, 32)
	s.workersDone = make(chan struct{})
	// A nil serverCtx (a bare construction that never went through Start)
	// would PANIC on Done() — default to a never-cancelling context (the
	// workers simply idle; production always goes through Start, which sets
	// serverCtx before the factory runs).
	ctx := s.serverCtx
	if ctx == nil {
		ctx = context.Background()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case j := <-s.jobs:
					fn := s.workerFn
					if fn == nil {
						fn = s.workerBody
					}
					fn(ctx, j)
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(s.workersDone)
	}()
}

// workerBody is the webhook worker body (Task 3): re-load the athlete row
// (the job's snapshot is pre-enqueue — a concurrent rotation must not flag
// a healthy athlete), fetch the detail (with the 401 refresh-and-retry-once
// arm), decide via decideActivity (the pure
// shared function, decide.go), persist in ONE per-event transaction (the
// rows-affected gate is the concurrency-safe dedupe), and post AFTER the
// commit. It NEVER writes last_polled_at (the cursor is poll-only — the
// webhook must not advance the poll's watermark); a send failure is
// log-only (the seen row stays 'posted', so the poll's carried-pending does
// NOT re-post a 'posted' row — a lost send is a logged gap, consistent with
// the existing post-failure behavior in the poll path). It signals done by
// returning — the worker then drains the next job.
func (s *Strava) workerBody(ctx context.Context, j webhookJob) {
	// Re-load the athlete row: j.ath is the snapshot loaded in
	// handleActivityEvent (synchronous, pre-enqueue); a concurrent rotation
	// (the poll's step-a, another worker — the near-expiry window) while the
	// job is queued can rotate the pair, and a refresh with the rotated-away
	// snapshot token would 401 (invalid_grant) and the reauth arm would
	// spuriously flag a healthy athlete. The FRESH access_token /
	// refresh_token are used for the fetch and any refresh below. A reload
	// error or a deleted row ends the job (no flag, no row, no post — the
	// poll's backstop covers it).
	fresh, rerr := s.loadAthleteByID(ctx, j.ath.id)
	if rerr != nil {
		slog.Error("strava webhook: athlete re-load failed — job ends (no flag)",
			"module", "strava", "id", j.ath.id, "error", rerr)
		return
	}
	if fresh == nil {
		slog.Warn("strava webhook: athlete row gone — job ends (no flag)",
			"module", "strava", "id", j.ath.id)
		return
	}
	ath := fresh
	// ---- 1. fetch + the 401 refresh-and-retry-once arm ----
	// A 401 is NOT a bare flag: a merely EXPIRED token (the bot was down
	// >= 6h; Strava tokens last 6h) must refresh + retry, not permanently
	// disable the athlete until manual re-consent. A genuinely REVOKED
	// token (the refresh itself 401s, or the retry fetch 401s) sets the
	// flag — the poll's next tick sees it and pauses; a re-consent
	// self-heals. No further retries inside the worker (the next event or
	// the next poll tick is the retry).
	detail, err := s.api.GetActivity(ctx, ath.accessToken, j.actID)
	if err != nil {
		var eua ErrUnauthorized
		if errors.As(err, &eua) {
			newAccess, newRefresh, expiresAt, rerr := s.api.RefreshToken(
				ctx, s.app.Cfg.StravaClientID, s.app.Cfg.StravaClientSecret, ath.refreshToken)
			switch {
			case rerr == nil:
				// Success: persist the ROTATED pair (the existing step-a
				// pattern) — the ROTATED refresh token MUST be re-persisted
				// or the token chain dies — then retry the fetch ONCE with
				// the new access token.
				// Lost-update residual (shared with the poll's step-a — the same
				// last-writer-wins property; deliberately NOT serialized here: no
				// singleflight / FOR UPDATE): two concurrent refreshers of the same
				// athlete (two workers + the poll, the near-expiry window) can each
				// persist a different rotated pair; the loser's newly-issued refresh
				// token is silently discarded. The next 401 will surface it, and the
				// reauth arm's stored-token comparison (the UPDATE … WHERE
				// refresh_token = lastRefresh inside reauthWebhook) prevents the
				// spurious flag — the data-corruption-adjacent half. That check-and-set
				// is ATOMIC (the UPDATE's WHERE does the comparison, so a third
				// rotation landing between any earlier re-read and the UPDATE can no
				// longer flag a healthy athlete — the pre-fix SELECT-then-UPDATE was a
				// TOCTOU; the UPDATE-with-WHERE closes it).
				if _, uerr := s.app.Pool.Exec(ctx,
					`UPDATE strava_athletes SET access_token=$1, refresh_token=$2, token_expires_at=$3 WHERE id=$4`,
					newAccess, newRefresh, expiresAt, ath.id); uerr != nil {
					slog.Error("strava webhook: rotated-token persist failed — job ends",
						"module", "strava", "id", ath.id, "error", uerr)
					return
				}
				detail, err = s.api.GetActivity(ctx, newAccess, j.actID)
				var retry401 ErrUnauthorized
				if errors.As(err, &retry401) {
					// The retry fetch also 401s (the explicit loop guard) —
					// the refresh did not cure it: the same arm as a 401 refresh.
					// lastRefresh = newRefresh (the value this worker JUST
					// persisted — the stored token still equals it when no
					// concurrent rotation landed after the persist; the atomic
					// UPDATE … WHERE refresh_token = newRefresh inside reauthWebhook
					// gates the flag).
					s.reauthWebhook(ctx, ath, newRefresh)
					return
				}
			case isUnauthorizedErr(rerr):
				// The refresh ITSELF 401s — the refresh token is genuinely
				// revoked: flag + end (no seen row, no post; the poll's next
				// tick sees the flag and pauses; a re-consent self-heals).
				// lastRefresh = ath.refreshToken (the reloaded snapshot — the
				// FAILED refresh persisted nothing; the atomic UPDATE … WHERE
				// refresh_token = ath.refreshToken inside reauthWebhook gates the
				// flag on a concurrent rotation).
				s.reauthWebhook(ctx, ath, ath.refreshToken)
				return
			default:
				// A TRANSIENT refresh error (network / 5xx): no flag — the
				// token may be fine; the job ends and the next event or the
				// next poll tick retries (the poll's 401 arm stays the backstop).
				slog.Warn("strava webhook: token refresh transient — job ends (no flag)",
					"module", "strava", "id", ath.id, "error", rerr)
				return
			}
		}
	}
	// Map the fetch result to the outcome (the client.go typed errors — the
	// same arms the poll path's decideActivity consumes).
	var out fetchOutcome
	switch {
	case err != nil:
		var egone ErrGone
		if errors.As(err, &egone) {
			out = fetchGone() // 404: the activity is gone (deleted / access revoked)
		} else {
			out = fetchTransient() // 429 / other transient: retry on a later pass
		}
	case isProcessing(detail):
		out = fetchProcessing(detail) // still processing → pending
	default:
		out = fetchReady(detail)
	}
	// ---- 2. the start_date rule: a VALID detail only (fetchReady /
	// fetchProcessing) gets a row. A gone (404) / transient (429) outcome
	// has no detail — a zero start_date on a pending row would poison the
	// poll cursor's min(pending) hold and force a full re-walk from page 1
	// every tick. NO row: the poll's next tick creates the skipped/pending
	// row with the real summary start_date (the backstop, decision 0013).
	if out.err != nil {
		cls := "transient"
		var egone ErrGone
		if errors.As(out.err, &egone) {
			cls = "gone"
		}
		slog.Info("strava webhook: no valid detail — no row (the poll backstop creates the row with the real summary start_date)",
			"module", "strava", "athlete", ath.id, "activity", j.actID, "class", cls)
		return
	}
	// ---- 3. the decision (pure — decide.go; shared with the poll path) ----
	d := s.decideActivity(ath, out, j.actID)
	// ---- 4. the per-event transaction (the rows-affected gate — the
	// concurrency-safe dedupe) ----
	// The row check and the write happen in ONE transaction (a bare pre-tx
	// SELECT would race a concurrent duplicate).
	tx, err := s.app.Pool.Begin(ctx)
	if err != nil {
		slog.Error("strava webhook: tx begin failed", "module", "strava", "id", ath.id, "activity", j.actID, "error", err)
		return
	}
	defer func() {
		if rerr := tx.Rollback(ctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			slog.Error("strava webhook: tx deferred rollback failed", "module", "strava", "error", rerr)
		}
	}()
	var existingStatus string
	err = tx.QueryRow(ctx,
		`SELECT status FROM strava_seen_activities WHERE strava_athletes_id=$1 AND strava_activity_id=$2`,
		ath.id, j.actID).Scan(&existingStatus)
	// postGate: the write matched exactly one row (the gate — see the arms
	// below); the post fires only when postGate AND d.status == 'posted'.
	postGate := false
	switch {
	case err == nil:
		if existingStatus == statusPending {
			// pending → terminal (or pending → pending on a processing
			// re-notify): the status='pending' WHERE guard is what makes the
			// rows-affected dedupe work under READ COMMITTED — a concurrent
			// duplicate's committed pending→terminal update makes the second
			// worker's WHERE match 0 rows after waiting on the row lock (the
			// UPDATE re-evaluates the WHERE against the committed row).
			// retries / dispositioned_at untouched (set at first creation).
			tag, uerr := tx.Exec(ctx,
				`UPDATE strava_seen_activities
				   SET status = $3
				   WHERE strava_athletes_id = $1 AND strava_activity_id = $2 AND status = 'pending'`,
				ath.id, j.actID, d.status)
			if uerr != nil {
				slog.Error("strava webhook: seen-row update failed", "module", "strava", "id", ath.id, "activity", j.actID, "error", uerr)
				return
			}
			postGate = tag.RowsAffected() == 1
		} else {
			// An existing TERMINAL row (posted / skipped): ABSORBED — no
			// post, no re-evaluation (the no-re-post / no-re-evaluation
			// invariant; the terminal row is never overwritten).
			slog.Info("strava webhook: terminal row — absorbed",
				"module", "strava", "athlete", ath.id, "activity", j.actID, "status", existingStatus)
		}
	case errors.Is(err, pgx.ErrNoRows):
		// First sight: the unique constraint does the work, the
		// rows-affected check is the gate — a concurrent duplicate's
		// committed row makes the second worker's INSERT a DO NOTHING →
		// 0 rows → no double post. start_date = the detail's StartDate
		// (always a valid detail here, per the rule above); retries = 0
		// and dispositioned_at = DEFAULT now() (the first-creation
		// timestamp).
		tag, ierr := tx.Exec(ctx,
			`INSERT INTO strava_seen_activities (strava_athletes_id, strava_activity_id, start_date, status)
			   VALUES ($1, $2, $3, $4)
			   ON CONFLICT (strava_athletes_id, strava_activity_id) DO NOTHING`,
			ath.id, j.actID, out.detail.StartDate, d.status)
		if ierr != nil {
			slog.Error("strava webhook: seen-row insert failed", "module", "strava", "id", ath.id, "activity", j.actID, "error", ierr)
			return
		}
		postGate = tag.RowsAffected() == 1
	default:
		slog.Error("strava webhook: seen-row lookup failed", "module", "strava", "id", ath.id, "activity", j.actID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("strava webhook: tx commit failed", "module", "strava", "id", ath.id, "activity", j.actID, "error", err)
		return
	}
	// ---- 5. the disposition log. The reason is a LOG-ONLY diagnostic —
	// the seen table has NO reason column; it is never persisted.
	slog.Info("strava webhook disposition", "module", "strava", "athlete", ath.id, "activity", j.actID, "status", d.status, "reason", d.reason, "post", postGate)
	// ---- THEN the post (AFTER the commit — persist before post, never the
	// reverse; the existing ordering). A send failure is log-only (the
	// seen row stays 'posted', so the poll's carried-pending does NOT
	// re-post a 'posted' row; a lost send is a logged gap).
	if postGate && d.status == statusPosted && d.post != nil {
		if err := s.sendFn(strconv.FormatInt(d.post.threadID, 10), d.post.message); err != nil {
			slog.Error("strava webhook post failed (no retry; the row stays 'posted')",
				"module", "strava", "athlete", ath.id, "activity", j.actID, "error", err)
		}
	}
}

// reauthWebhook is the webhook worker's 401 arm (a genuinely REVOKED token
// — the refresh itself 401s, or the retry fetch 401s): the needs_reauth
// UPDATE (bound to ath.id — athleteRow has no strava_athlete_id field; id
// and stravaAthleteID identify the same row) + a log. The job ends (no seen
// row, no post); the poll's next tick sees the flag and pauses the athlete;
// a re-consent self-heals (the onboarding upsert clears the flag).
//
// The check-and-set is ATOMIC: the UPDATE's WHERE does the stored-token
// comparison (… AND refresh_token = lastRefresh). lastRefresh is the refresh
// token this worker last persisted (the reloaded snapshot when the refresh
// FAILED — nothing was persisted; the ROTATED newRefresh when the refresh
// SUCCEEDED and the retry fetch 401s). If the stored refresh token no longer
// equals it, a concurrent rotation (the poll's step-a, another worker) already
// cured the athlete and the 401 was a stale-snapshot artifact → the UPDATE
// matches 0 rows → end the job with NO flag (a spurious flag would disable a
// healthy athlete until manual re-consent: loadAthletes filters flagged rows
// out). Only set needs_reauth=TRUE when the stored token still equals
// lastRefresh (RowsAffected == 1). A nil pool (a bare construction that never
// went through Start) must NOT panic on the UPDATE and must NOT flag: the
// guard below takes the existing error path (log + no flag). A DB error on
// the UPDATE also ends with NO flag (the poll's 401 arm is the backstop).
func (s *Strava) reauthWebhook(ctx context.Context, ath *athleteRow, lastRefresh string) {
	// A nil pool would PANIC on the UPDATE (a nil *pgxpool.Pool receiver
	// does not return an error) — the guard is the existing error path
	// (log + no flag; the poll's 401 arm is the backstop).
	if s.app == nil || s.app.Pool == nil {
		slog.Error("strava webhook: reauth update skipped — no pool (backstop covers it)",
			"module", "strava", "id", ath.id)
		return
	}
	// ATOMIC check-and-set: the WHERE does the stored-token comparison, so a
	// concurrent rotation landing between any earlier re-read and this UPDATE
	// can no longer flag a healthy athlete (the UPDATE's WHERE closes the
	// pre-fix TOCTOU: the check and the set are one statement).
	tag, err := s.app.Pool.Exec(ctx,
		`UPDATE strava_athletes SET needs_reauth = true WHERE id = $1 AND refresh_token = $2`,
		ath.id, lastRefresh)
	if err != nil {
		slog.Error("strava webhook: needs_reauth update failed — no flag (backstop covers it)",
			"module", "strava", "id", ath.id, "error", err)
		return
	}
	if tag.RowsAffected() == 1 {
		slog.Warn("strava webhook: re-auth required (401; the refresh did not cure it)",
			"module", "strava", "label", ath.label, "hint", "re-run the consent and clear needs_reauth")
		return
	}
	// 0 rows: the stored token no longer matches lastRefresh — a concurrent
	// rotation landed (OR the row was deleted: a deleted row simply has no
	// matching row) → the athlete is healthy / gone → NO flag.
	slog.Info("strava webhook: token rotated concurrently — skipping reauth flag",
		"module", "strava", "id", ath.id)
}

// isUnauthorizedErr reports whether err is the 401 class (ErrUnauthorized —
// the refresh's invalid_grant routes here too, per client.go).
func isUnauthorizedErr(err error) bool {
	var eua ErrUnauthorized
	return errors.As(err, &eua)
}

// Start runs the onboarding callback + webhook listener on stravaOnboardAddr. It
// blocks until ctx cancels, then http.Server.Shutdown with a ≤10s grace
// (the mcp.Server.Start template, verbatim). A bind failure is returned
// immediately at boot (fail fast — the errgroup arm's os.Exit(1) fires at
// startup, not at the next SIGTERM); a clean cancel returns nil (never
// leaking context.Canceled or http.ErrServerClosed).
func (s *Strava) Start(ctx context.Context) error {
	// serverCtx: the Start-owned context; canceling it drains the webhook
	// workers (the ≤10s shutdown grace bounds the drain — a worker stuck in
	// a job body is released when the job's ctx is done).
	s.serverCtx, s.serverCancel = context.WithCancel(ctx)
	defer s.serverCancel()
	hs := &http.Server{
		Addr:    stravaOnboardAddr,
		Handler: s.routes(),
		// ReadHeaderTimeout: a slowloris against the all-interfaces :8643
		// directly (bypassing caddy) could otherwise hold connections
		// indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Capacity 1: the single ListenAndServe result (the bind error or the
	// server-closed termination) is exactly once, and a buffered send
	// never blocks the goroutine on any path.
	errCh := make(chan error, 1)
	go func() { errCh <- hs.ListenAndServe() }()
	select {
	case <-ctx.Done():
		// Clean-cancel path: drain the workers, then become the ≤10s
		// shutdown grace. ONE shared 10 s budget — the total stays ≤10s, and
		// if the workers don't exit within the grace the existing fail-fast
		// path applies (the shutdown deadline is already spent).
		s.serverCancel()
		grace, stopGrace := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopGrace()
		if s.workersDone != nil {
			select {
			case <-s.workersDone:
			case <-grace.Done():
			}
		}
		shutdownDeadline, _ := grace.Deadline()
		shutdownCtx, cancel := context.WithDeadline(context.Background(), shutdownDeadline)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
		if shutdownCtx.Err() != nil {
			// Deadline: active connections may still be in flight. A
			// previous bind failure already delivered its error to the
			// buffered channel, so a non-blocking read is exact.
			select {
			case err := <-errCh:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				// Mapped clean listen-terminator (nil / ErrServerClosed): nil.
				return nil
			default:
				return nil
			}
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		// ListenAndServe finished without us cancelling: the bind failure
		// (address in use — returned immediately at boot) or a clean
		// listen-terminator (nil / ErrServerClosed → nil).
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
