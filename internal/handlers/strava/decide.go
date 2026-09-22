// decide.go — the per-activity decision, extracted from passForAthlete as a
// pure function the webhook worker (task 3) can share with the poll path.
// decideActivity takes the fetch OUTCOME (a ready detail or the fetch error
// class), not just a ready detail, so both callers share the same error
// arms. It NEVER persists — the poll path persists in its batched
// transaction; the webhook worker persists in its own per-event transaction.
package strava

import (
	"errors"
)

// The skipped reasons (the seen table's status CHECK values are pending /
// posted / skipped; the reason is the decision's diagnostic, empty for
// pending/posted — a decision value the caller persists, never a
// decideActivity output the poll path writes).
const (
	reasonGone        = "gone"
	reasonOutOfFamily = "out-of-family"
	reasonNoTarget    = "no-target"
)

// fetchOutcome is the result of one GetActivity fetch: a ready detail or
// the fetch error class (the client.go typed errors — no new error types).
type fetchOutcome struct {
	detail Activity
	err    error
}

// fetchReady is a ready (non-processing) detail (err nil).
func fetchReady(detail Activity) fetchOutcome { return fetchOutcome{detail: detail} }

// fetchGone is the 404 class (ErrGone): the activity is gone (deleted /
// access revoked).
func fetchGone() fetchOutcome { return fetchOutcome{err: ErrGone{}} }

// fetchTransient is the 429 / other transient class (ErrRateLimited /
// ErrTransient): retry on a later pass.
func fetchTransient() fetchOutcome { return fetchOutcome{err: ErrRateLimited{}} }

// fetchProcessing is a detail that is still processing (in_progress or
// resource_state == -1; err nil, isProcessing(detail) true).
func fetchProcessing(detail Activity) fetchOutcome { return fetchOutcome{detail: detail} }

// disposition is decideActivity's verdict for one activity: the
// seen-table status (the strava_seen_activities CHECK values) + the
// skipped reason (empty for pending/posted) + the post (non-nil only when
// posted — the same postItem the poll path's deferred posts slice holds).
type disposition struct {
	status string
	reason string
	post   *postItem
}

// resolveTarget is the post-target resolution (spec: applies to BOTH the
// step-c INSERT and the step-d UPDATE-to-posted): the per-athlete thread
// first (COALESCE at load means a NULL column reads as 0 and falls
// through), else the STRAVA_SHARED_THREAD_ID config, else 0 (no target —
// the family activity is dispositioned 'skipped').
func (s *Strava) resolveTarget(ath *athleteRow) int64 {
	if ath.targetThreadID != 0 {
		return ath.targetThreadID
	}
	return s.app.Cfg.StravaSharedThreadID
}

// decideActivity is the per-activity decision, shared by the poll pass
// (step c: per new listed id; step d: carried-pending retry) and the
// webhook worker (task 3). A method (not a bare function) because it
// needs s.app.Cfg (via resolveTarget) and s.makePost. PURE: no DB, no
// s.app.Pool access, no persistence — the caller persists (the poll path
// in its batched transaction; the webhook worker in its own per-event
// transaction).
//
// actID is the CALLER's activity id (the listed summary's id on step c;
// the carried-pending row's id on step d; the webhook job's actID) — the
// post LINK uses it, NOT the fetched detail's echoed id: a degenerate
// detail (ID == 0) must not post …/activities/0 for a row the caller
// knows the real id of.
//
// The arms:
//   - gone (404) → skipped/"gone" — a DETERMINISTIC first-sight outcome
//     (deleted / access revoked): the caller leaves any existing terminal
//     row untouched (the poll's seenIDs check; the webhook worker's
//     per-event transaction).
//   - 429 / any other transient class → pending (no reason; the retries
//     counter is the caller's concern — decideActivity never touches it).
//   - still processing → pending (no reason; isProcessing already true in
//     the outcome).
//   - ready → the in-family gate on the DETAIL's sport type:
//     out-of-family → skipped/"out-of-family"; in-family → resolveTarget
//     (per-athlete thread → STRAVA_SHARED_THREAD_ID → none): no target →
//     skipped/"no-target"; target present → posted + the post item.
//
// DOCUMENTED UNIFICATION: the poll's step c (the post-fetch path)
// historically did NOT re-gate the fetched detail's sport type (the family
// gate ran on the summary pre-fetch), while step d (the carried-pending
// retry) DID gate it; the unified function gates the detail's sport type
// in BOTH paths (an empty SportType is out-of-family — family("") is
// false — so a processing-then-ready detail with an empty sport type is
// skipped/"out-of-family", matching step d's existing behavior).
func (s *Strava) decideActivity(ath *athleteRow, out fetchOutcome, actID int64) disposition {
	if out.err != nil {
		var egone ErrGone
		if errors.As(out.err, &egone) {
			return disposition{status: statusSkipped, reason: reasonGone}
		}
		// 429 or any other transient class: retry on a later pass; the
		// retries counter is the caller's concern.
		return disposition{status: statusPending}
	}
	if isProcessing(out.detail) {
		return disposition{status: statusPending}
	}
	// Ready: the in-family gate on the DETAIL's sport type (the documented
	// unification above — both the post-fetch path and the carried-pending
	// retry gate it; an empty SportType is out-of-family).
	if !family(out.detail.SportType) {
		return disposition{status: statusSkipped, reason: reasonOutOfFamily}
	}
	if target := s.resolveTarget(ath); target != 0 {
		// the post link uses the CALLER's id (not the detail's echoed id —
		// a degenerate detail with ID == 0 must not post …/activities/0).
		p := s.makePost(ath.label, out.detail, target, actID)
		return disposition{status: statusPosted, post: &p}
	}
	return disposition{status: statusSkipped, reason: reasonNoTarget}
}
