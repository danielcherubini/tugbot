package mention

import (
	"fmt"
	"time"
)

// cooldownDecision mirrors Rust step 8's comparison:
//
//	elapsed = SystemTime::now().duration_since(last_used_at)
//	            .unwrap_or_default().as_secs()
//
// i.e. integer seconds (floor), clamped at 0 under clock skew (a future
// last_used_at counts as 0 elapsed, exactly like Rust's
// unwrap_or_default()). Blocking when elapsed < limit, with
// remaining = limit - elapsed on whole seconds.
func cooldownDecision(lastUse, now time.Time, limit time.Duration) (blocking bool, remaining time.Duration) {
	// now arrives as time.Now().UTC() from the caller: lastUse is a
	// NAIVE DB timestamp that pgx decodes as a UTC-labeled wall clock
	// ("timestamp without time zone" — the Rust SystemTime frame is
	// likewise the UTC wall clock), so the subtraction must use the
	// UTC-labeled now or the CEST host offset (2h at cutover) leaks
	// into every cooldown. time.Since(lastUse) did exactly that.
	elapsed := now.Sub(lastUse)
	secs := int64(elapsed / time.Second) // truncates towards zero for negatives → floor for positive elapsed
	if secs < 0 {
		secs = 0
	}
	limitSecs := int64(limit / time.Second)
	if secs < limitSecs {
		return true, time.Duration(limitSecs-secs) * time.Second
	}
	return false, 0
}

// formatRemaining mirrors Rust's format_remaining(seconds: u64), ported to
// time.Duration operating on whole seconds (the cooldown always lands on a
// whole-second boundary after cooldownDecision). Distinct from the GULAG
// FormatDuration (Task 4 core) — both keep their Rust names so a reader can
// map back. Avoids the "0m" bug when fewer than 60 seconds remain.
func formatRemaining(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs < 0 {
		secs = 0
	}
	if secs >= 3600 {
		hours := secs / 3600
		mins := (secs % 3600) / 60
		if mins > 0 {
			return fmt.Sprintf("%dh %dm", hours, mins)
		}
		return fmt.Sprintf("%dh", hours)
	}
	if secs >= 60 {
		return fmt.Sprintf("%dm", secs/60)
	}
	return fmt.Sprintf("%ds", secs)
}
