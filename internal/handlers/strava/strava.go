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
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
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
	app  *app.App
	api  StravaAPI // seam: production = NewStravaAPI(); nil until first use
	poll time.Duration

	// sendFn seam: func(threadID, msg string) error — production posts via
	// h.app.D.ChannelMessageSendComplex with mention parsing disabled (the
	// threads are plain discordgo channels).
	sendFn func(threadID string, msg string) error

	cfgWarned bool // log-once: configured-but-credentialed-missing preflight
}

// New builds the handler. NO network I/O here (selftest discipline — every
// handler is constructed offline). The API is lazy (nil until first use);
// the seam is wired via a CLOSURE (the house single-thread send shape).
func New(app *app.App) *Strava {
	s := &Strava{app: app}
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
	// lazy API (first use) — NewStravaAPI is network-free, selftest-safe.
	if s.api == nil {
		s.api = NewStravaAPI()
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
	// step-d UPDATE-to-posted), resolved at DISPOSITION time, never post-commit.
	// COALESCE at load means a target_thread_id of 0 falls through to the
	// shared thread; still 0 → the family activity is dispositioned 'skipped'.
	resolveTarget := func() int64 {
		if a.targetThreadID != 0 {
			return a.targetThreadID
		}
		return s.app.Cfg.StravaSharedThreadID
	}

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
					inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusSkipped})
					continue
				}
				var erl ErrRateLimited
				if errors.As(err, &erl) {
					slog.Info("strava detail (rate limited) → pending", "module", "strava", "id", sum.ID, "error", err)
				} else {
					slog.Warn("strava detail transient → pending", "module", "strava", "id", sum.ID, "error", err)
				}
				// the existing hold rule keeps the id in the window; d re-fetches
				// next cycles, and the 5-retry drop covers a stuck fetch.
				inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusPending})
				continue
			}
			if isProcessing(detail) {
				inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusPending})
				continue
			}
			target := resolveTarget()
			if target == 0 {
				slog.Warn("strava family activity has no post target → skipped", "module", "strava", "label", a.label, "id", sum.ID)
				inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusSkipped})
				continue
			}
			// the posted invariant: dispositioned 'posted' BEFORE posting.
			inserts = append(inserts, seenInsert{sum.ID, sum.StartDate, statusPosted})
			posts = append(posts, s.makePost(a.label, detail, target, sum.ID))
		}
	}

	// ---- d. pending retry (carried rows, re-fetched uniformily) ----
	for i := range carried {
		c := &carried[i]
		detail, err := s.api.GetActivity(ctx, access, c.activityID)
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
				updates = append(updates, seenUpdate{c.activityID, statusSkipped, nil})
				continue
			}
			var erl ErrRateLimited
			if errors.As(err, &erl) {
				slog.Info("strava detail (retry) rate limited", "module", "strava", "id", c.activityID, "retry_after", erl.RetryAfter)
			}
			// fetch failed again → increment; drop at the new value >= 5.
			newRetries := c.retries + 1
			if newRetries >= dropAfterRetries {
				updates = append(updates, seenUpdate{c.activityID, statusSkipped, &newRetries})
			} else {
				updates = append(updates, seenUpdate{c.activityID, "", &newRetries})
			}
			continue
		}
		if isProcessing(detail) {
			newRetries := c.retries + 1
			if newRetries >= dropAfterRetries {
				updates = append(updates, seenUpdate{c.activityID, statusSkipped, &newRetries})
			} else {
				updates = append(updates, seenUpdate{c.activityID, "", &newRetries})
			}
			continue
		}
		// ready
		if !family(detail.SportType) || resolveTarget() == 0 {
			// outside the family (or no target) → dispositioned 'skipped'.
			updates = append(updates, seenUpdate{c.activityID, statusSkipped, nil})
			continue
		}
		target := resolveTarget()
		updates = append(updates, seenUpdate{c.activityID, statusPosted, nil})
		posts = append(posts, s.makePost(a.label, detail, target, c.activityID))
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
// (always false — the reply lands in the thread); DeferResponse is present
// for parity with the Rust struct default `Option::None`.
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
		return Response{Content: "strava is not configured on this bot — the owner needs to set the client credentials first"}
	}
	// Extract the label: first option (if any), string value.
	var label string
	if data, ok := i.Data.(discordgo.ApplicationCommandInteractionData); ok && len(data.Options) > 0 {
		if v, ok := data.Options[0].Value.(string); ok {
			label = sanitizeLabel(v)
		}
	}
	state, err := newState()
	if err != nil {
		slog.Error("onboarding state generation failed", "module", "strava", "error", err)
		return Response{Content: "onboarding setup failed — try again"}
	}
	// thread_id: the interaction's ChannelID is a string; the column is
	// bigint. Empty/unparsable → the setup-failed reply (no row).
	threadID, err := strconv.ParseInt(i.ChannelID, 10, 64)
	if err != nil {
		slog.Error("onboarding channel id unparsable", "module", "strava", "channel_id", i.ChannelID, "error", err)
		return Response{Content: "onboarding setup failed — try again"}
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
		return Response{Content: "onboarding setup failed — try again"}
	}
	content := authorizeURL(s.app.Cfg.StravaClientID, state) + " — this link expires in an hour."
	// The disabled-clause (the command itself is NOT gated on the flag).
	if !features.IsEnabled(context.Background(), s.app.Pool, FeatureKey) {
		content += " (the strava feature is disabled — you'll be tracked once it's enabled)"
	}
	// Ephemeral stays false — the reply lands in the thread (where the
	// later confirm lands).
	return Response{Content: content}
}
