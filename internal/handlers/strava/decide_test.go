// decide_test.go — unit tests for the pure decideActivity decision (no DB:
// a nil Pool is fine — decideActivity never touches the pool; the
// newTestStrava construction pattern, minus the pool/api/sendFn it never
// needs).
package strava

import (
	"testing"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/config"
)

// newDecideStrava builds the handler for the decideActivity unit tests: a
// *app.App with a Config (shared thread id as given) and a nil Pool.
func newDecideStrava(sharedThreadID int64) *Strava {
	return &Strava{app: &app.App{Cfg: &config.Config{StravaSharedThreadID: sharedThreadID}}}
}

// athWithTarget is one athleteRow: the label "Matt", the given
// target_thread_id (0 = the column is NULL / unset).
func athWithTarget(threadID int64) *athleteRow {
	return &athleteRow{label: "Matt", targetThreadID: threadID}
}

// readyDetail is a ready (non-processing) in-family detail.
func readyDetail() Activity {
	return Activity{ID: 123, SportType: "Run", Title: "Morning", Distance: 42300}
}

// TestDecideReadyInFamilyWithTarget pins the posted arm: a ready in-family
// detail with a per-athlete thread posts to the PER-ATHLETE thread (the
// per-athlete thread beats the shared config), with the 2-line post.
// The post LINK is pinned to the PASSED activity id (42), NOT the detail's
// echoed id (123) — the degenerate-detail case (ID == 0) must not post
// …/activities/0.
func TestDecideReadyInFamilyWithTarget(t *testing.T) {
	s := newDecideStrava(9001)
	d := s.decideActivity(athWithTarget(42), fetchReady(readyDetail()), 42)
	if d.status != statusPosted || d.reason != "" || d.post == nil {
		t.Fatalf("d = %+v, want posted / reason '' / non-nil post", d)
	}
	if d.post.threadID != 42 {
		t.Errorf("post.threadID = %d, want 42 (the per-athlete thread beats the shared config)", d.post.threadID)
	}
	// the 2-line post via the existing buildPost shape: label line + link —
	// the link uses the PASSED id (42), not the detail's echoed id (123).
	want := buildPost("Matt", `"Morning" (42.3 km Run)`, "42")
	if d.post.message != want {
		t.Errorf("post.message = %q, want %q", d.post.message, want)
	}
}

// TestDecideReadyDegenerateDetailID pins the degenerate case: a ready
// in-family detail whose ID is 0 (the API did not echo the id) + a passed
// actID of 42 → the post link is …/activities/42 (NOT …/activities/0 —
// the caller knows the real id the seen row was written with).
func TestDecideReadyDegenerateDetailID(t *testing.T) {
	s := newDecideStrava(9001)
	degenerate := Activity{ID: 0, SportType: "Run", Title: "Morning", Distance: 42300}
	d := s.decideActivity(athWithTarget(42), fetchReady(degenerate), 42)
	if d.status != statusPosted || d.reason != "" || d.post == nil {
		t.Fatalf("d = %+v, want posted / reason '' / non-nil post", d)
	}
	if d.post.threadID != 42 {
		t.Errorf("post.threadID = %d, want 42", d.post.threadID)
	}
	want := buildPost("Matt", `"Morning" (42.3 km Run)`, "42")
	if d.post.message != want {
		t.Errorf("post.message = %q, want %q (the passed actID, not the detail's zero id)", d.post.message, want)
	}
}

// TestDecideReadyInFamilySharedConfigTarget pins the shared-config fallback:
// no per-athlete thread (0) → the STRAVA_SHARED_THREAD_ID config is the
// target.
func TestDecideReadyInFamilySharedConfigTarget(t *testing.T) {
	s := newDecideStrava(9001)
	d := s.decideActivity(athWithTarget(0), fetchReady(readyDetail()), 42)
	if d.status != statusPosted || d.reason != "" || d.post == nil {
		t.Fatalf("d = %+v, want posted / reason '' / non-nil post", d)
	}
	if d.post.threadID != 9001 {
		t.Errorf("post.threadID = %d, want 9001 (the shared config fallback)", d.post.threadID)
	}
}

// TestDecideReadyInFamilyNoTarget pins the no-target skip: no per-athlete
// thread AND no shared config → skipped/"no-target", no post.
func TestDecideReadyInFamilyNoTarget(t *testing.T) {
	s := newDecideStrava(0)
	d := s.decideActivity(athWithTarget(0), fetchReady(readyDetail()), 42)
	if d.status != statusSkipped || d.reason != reasonNoTarget || d.post != nil {
		t.Fatalf("d = %+v, want skipped / reason %q / nil post", d, reasonNoTarget)
	}
}

// TestDecideReadyOutOfFamily pins the in-family gate: a ready detail whose
// sport type is NOT one of the 9 family values → skipped/"out-of-family".
func TestDecideReadyOutOfFamily(t *testing.T) {
	s := newDecideStrava(9001)
	d := s.decideActivity(athWithTarget(42), fetchReady(Activity{ID: 123, SportType: "Swim"}), 42)
	if d.status != statusSkipped || d.reason != reasonOutOfFamily || d.post != nil {
		t.Fatalf("d = %+v, want skipped / reason %q / nil post", d, reasonOutOfFamily)
	}
}

// TestDecideReadyEmptySportType pins the documented unification: an empty
// SportType is out-of-family (family("") is false) → skipped/
// "out-of-family" (matching the carried-pending retry's existing behavior
// — the poll's post-fetch path historically did NOT re-gate the detail).
func TestDecideReadyEmptySportType(t *testing.T) {
	s := newDecideStrava(9001)
	d := s.decideActivity(athWithTarget(42), fetchReady(Activity{ID: 123, SportType: ""}), 42)
	if d.status != statusSkipped || d.reason != reasonOutOfFamily || d.post != nil {
		t.Fatalf("d = %+v, want skipped / reason %q / nil post", d, reasonOutOfFamily)
	}
}

// TestDecideProcessing pins the still-processing arm: in_progress present
// and true → pending (no reason; the retries counter is the caller's).
func TestDecideProcessing(t *testing.T) {
	s := newDecideStrava(9001)
	processing := Activity{ID: 123, SportType: "Run", InProgress: true, InProgressSet: true}
	if !isProcessing(processing) {
		t.Fatalf("test setup: isProcessing(%+v) = false, want true", processing)
	}
	d := s.decideActivity(athWithTarget(42), fetchProcessing(processing), 42)
	if d.status != statusPending || d.reason != "" || d.post != nil {
		t.Fatalf("d = %+v, want pending / reason '' / nil post", d)
	}
}

// TestDecideGone pins the 404 arm: ErrGone → skipped/"gone" (the
// deterministic first-sight disposition; the caller leaves any existing
// terminal row untouched).
func TestDecideGone(t *testing.T) {
	s := newDecideStrava(9001)
	d := s.decideActivity(athWithTarget(42), fetchGone(), 42)
	if d.status != statusSkipped || d.reason != reasonGone || d.post != nil {
		t.Fatalf("d = %+v, want skipped / reason %q / nil post", d, reasonGone)
	}
}

// TestDecideTransient pins the 429/other-transient arm: a transient fetch
// error → pending (no reason; the retries counter is the caller's).
func TestDecideTransient(t *testing.T) {
	s := newDecideStrava(9001)
	d := s.decideActivity(athWithTarget(42), fetchTransient(), 42)
	if d.status != statusPending || d.reason != "" || d.post != nil {
		t.Fatalf("d = %+v, want pending / reason '' / nil post", d)
	}
}
