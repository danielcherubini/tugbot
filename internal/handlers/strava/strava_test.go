package strava

import (
	"testing"
	"time"
)

// TestFeatureKey pins the feature flag key (task 4, spec — the features-table
// row the loop gates on; registered by migration 000006 at `enabled=false`).
func TestFeatureKey(t *testing.T) {
	if FeatureKey != "strava" {
		t.Errorf("FeatureKey = %q, want %q", FeatureKey, "strava")
	}
}

// TestFamilyPin pins the ENTIRE fixed run/cycling family (spec §1 — exactly 9
// values) plus known exclusions: not configurable anywhere.
func TestFamilyPin(t *testing.T) {
	familyValues := []string{
		"Run", "TrailRun", "VirtualRun", "Ride", "VirtualRide",
		"GravelRide", "MountainBikeRide", "EBikeRide", "EMountainBikeRide",
	}
	for _, s := range familyValues {
		if !family(s) {
			t.Errorf("family(%q) = false, want true", s)
		}
	}
	exclusions := []string{"Swim", "Walking", "Badminton", ""}
	for _, s := range exclusions {
		if family(s) {
			t.Errorf("family(%q) = true, want false", s)
		}
	}
}

// TestIsProcessing pins the documented fallback (spec — a present-false in_progress
// WINS over the resource_state==-1 indicator; only the absence of the field
// falls through to resource_state).
func TestIsProcessing(t *testing.T) {
	cases := []struct {
		name          string
		inProgressSet bool
		inProgress    bool
		resourceState int
		want          bool
	}{
		{"present true", true, true, 2, true},
		{"present false", true, false, 2, false},
		{"absent -1 (processing)", false, false, -1, true},
		{"absent 2 (done)", false, false, 2, false},
		{"present false wins over -1", true, false, -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Activity{InProgressSet: c.inProgressSet, InProgress: c.inProgress, ResourceState: c.resourceState}
			if got := isProcessing(a); got != c.want {
				t.Errorf("isProcessing(%+v) = %v, want %v", a, got, c.want)
			}
		})
	}
}

// TestNextCursor pins the cursor decision: hold the window open on any pending
// (min), advance to max(dispositioned) when nothing is pending, unchanged
// (zero) when there is nothing at all.
func TestNextCursor(t *testing.T) {
	ts := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	pending := []time.Time{ts("2026-01-01T06:00:00Z"), ts("2026-01-01T02:00:00Z")}       // min = 02:00
	dispositioned := []time.Time{ts("2026-01-01T08:00:00Z"), ts("2026-01-01T04:00:00Z")} // max = 08:00

	t.Run("hold priority: pending + dispositioned -> min(pending), false", func(t *testing.T) {
		next, advanced := nextCursor(pending, dispositioned)
		if advanced {
			t.Errorf("advanced = true, want false (hold)")
		}
		want := ts("2026-01-01T02:00:00Z")
		if !next.Equal(want) {
			t.Errorf("next = %v, want %v (min of pending)", next, want)
		}
	})

	t.Run("advance: dispositioned only -> max, true", func(t *testing.T) {
		next, advanced := nextCursor(nil, dispositioned)
		if !advanced {
			t.Errorf("advanced = false, want true")
		}
		want := ts("2026-01-01T08:00:00Z")
		if !next.Equal(want) {
			t.Errorf("next = %v, want %v (max of dispositioned)", next, want)
		}
	})

	t.Run("empty -> zero, false", func(t *testing.T) {
		next, advanced := nextCursor(nil, nil)
		if advanced {
			t.Errorf("advanced = true, want false")
		}
		if !next.IsZero() {
			t.Errorf("next = %v, want zero", next)
		}
	})
}

// TestFormatNoun pins the distance formatting branches: >= 100 km rounds HALF UP
// to an integer (int64(m/1000+0.5)); < 100 km one decimal (%.1f); the branch
// flips at EXACTLY 100.0 km (no decimals).
func TestFormatNoun(t *testing.T) {
	act := func(m float64, sport string) Activity { return Activity{Distance: m, SportType: sport} }
	cases := []struct {
		name string
		a    Activity
		want string
	}{
		{"42.3 km Run", act(42300, "Run"), "42.3 km Run"},
		{"142 km MountainBikeRide", act(142000, "MountainBikeRide"), "142 km MountainBikeRide"},
		{"143 km round half up", act(142500, "MountainBikeRide"), "143 km MountainBikeRide"},
		{"99.8 km Ride (under-100 branch)", act(99800, "Ride"), "99.8 km Ride"},
		{"exactly 100.0 km flips branch", act(100000, "Ride"), "100 km Ride"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatNoun(c.a); got != c.want {
				t.Errorf("formatNoun(%v) = %q, want %q", c.a.Distance, got, c.want)
			}
		})
	}
}

// TestBuildPost pins the exact two-line post shape (label line + activity link).
func TestBuildPost(t *testing.T) {
	got := buildPost("Matt", "42.3 km Run", "123")
	want := "Matt finished 42.3 km Run\nhttps://www.strava.com/activities/123"
	if got != want {
		t.Errorf("buildPost = %q, want %q", got, want)
	}
	// Title branch: the CALLER composes the noun; buildPost just embeds it.
	gotT := buildPost("Matt", `"Tuesday tempo" (42.3 km Run)`, "123")
	wantT := `Matt finished "Tuesday tempo" (42.3 km Run)` + "\nhttps://www.strava.com/activities/123"
	if gotT != wantT {
		t.Errorf("buildPost(title) = %q, want %q", gotT, wantT)
	}
}
