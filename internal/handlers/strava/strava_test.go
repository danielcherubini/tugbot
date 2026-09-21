package strava

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// TestMentionSuppressionPayload pins the zero-empty-JSON suppression that the
// production sendFn relies on: MessageAllowedMentions' Parse is deliberately NOT
// omitempty in discordgo — an EMPTY (non-nil) Parse slice marshals to
// "parse":[] — complete mention suppression. (A zero-value struct would
// marshal a nil slice to "parse":null, which is not the documented
// suppression form; the production closure therefore passes the explicit
// empty Parse.) A regular bot message WITHOUT allowed_mentions has ALL mention
// types parsed by default, so without this an activity title could ping a user
// (<@id>) or roll @everyone/@here pending MENTION_EVERYONE.
func TestMentionSuppressionPayload(t *testing.T) {
	b, err := json.Marshal(&discordgo.MessageSend{
		Content:         "x",
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"allowed_mentions":{"parse":[]`) {
		t.Errorf("marshal = %s, want it to contain %q (explicit-empty-JSON suppression)", b, `"allowed_mentions":{"parse":[]`)
	}
}

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
		// non-finite / negative → 0.0 (NaN, +Inf, negative all map to 0 metres).
		{"NaN → 0.0 km", act(math.NaN(), "Run"), "0.0 km Run"},
		{"+Inf → 0.0 km", act(math.Inf(1), "Ride"), "0.0 km Ride"},
		{"negative → 0.0 km (NOT -42.0)", act(-42000, "Run"), "0.0 km Run"},
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

// TestMakePostTitleSanitize pins the two-line post-format pin: a Strava title
// (or label) containing a newline is newlined to spaces BEFORE composition, so
// the post stays EXACTLY two lines.
func TestMakePostTitleSanitize(t *testing.T) {
	s := &Strava{}

	// Title with an embedded newline → "Tue Tempo"; post is exactly two lines.
	got := s.makePost("Matt", Activity{Distance: 42300, SportType: "Run", Title: "Tue\nTempo"}, 0, 123)
	want := `Matt finished "Tue Tempo" (42.3 km Run)` + "\nhttps://www.strava.com/activities/123"
	if got.message != want {
		t.Errorf("makePost(title) = %q, want exactly two lines %q", got.message, want)
	}

	// Label with an embedded newline (defensive) → also newlined, still two legs.
	gotL := s.makePost("Mat\nn", Activity{Distance: 42300, SportType: "Run", Title: ""}, 0, 123)
	wantL := "Mat n finished 42.3 km Run" + "\nhttps://www.strava.com/activities/123"
	if gotL.message != wantL {
		t.Errorf("makePost(label) = %q, want exactly two lines %q", gotL.message, wantL)
	}
}

// TestSanitizeLabel pins the /strava label sanitizer: trims, newlines
// collapse to single spaces, 32-rune cap (first 32 runes), empty for
// empty/whitespace-only input.
func TestSanitizeLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trims spaces", "  Sam  ", "Sam"},
		{"lone newline", "An\nna", "An na"},
		{"CRLF unit", "A\r\nB", "A B"},
		{"40-rune input caps at 32", strings.Repeat("a", 40), strings.Repeat("a", 32)},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"32-rune input unchanged", strings.Repeat("b", 32), strings.Repeat("b", 32)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeLabel(c.in); got != c.want {
				t.Errorf("sanitizeLabel(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSanitizeLabelCR pins the newline-ordering: \r\n is replaced as a
// UNIT first (one space, not two), then lone \r, then lone \n — a
// naive two-pass ReplaceAll would turn "A\r\nB" into "A  B".
func TestSanitizeLabelCR(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"CRLF as a unit -> one space", "A\r\nB", "A B"},
		{"lone CR", "A\rB", "A B"},
		{"lone LF", "A\nB", "A B"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeLabel(c.in); got != c.want {
				t.Errorf("sanitizeLabel(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNewState pins the onboarding state: exactly 32 lowercase hex chars,
// and 1000 consecutive calls produce 1000 distinct values (collision
// guard).
func TestNewState(t *testing.T) {
	for i := 0; i < 1000; i++ {
		s, err := newState()
		if err != nil {
			t.Fatalf("newState() call %d: %v", i, err)
		}
		if len(s) != 32 {
			t.Fatalf("newState() call %d = %q (len %d), want 32 chars", i, s, len(s))
		}
		for _, r := range s {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				t.Fatalf("newState() call %d = %q, want all [0-9a-f]", i, s)
			}
		}
	}
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		s, err := newState()
		if err != nil {
			t.Fatalf("newState() call %d: %v", i, err)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("newState() collision at call %d: %q", i, s)
		}
		seen[s] = struct{}{}
	}
}

// TestAuthorizeURL pins the exact authorize URL (code constant redirect URI
// — public, not a secret; the client_id is already public in the URL).
func TestAuthorizeURL(t *testing.T) {
	got := authorizeURL("280525", "abc123")
	want := "https://www.strava.com/oauth/authorize?client_id=280525&redirect_uri=https%3A%2F%2Ftugbot.wizards.town%2Fstrava%2Fcallback&response_type=code&scope=activity:read_all&state=abc123"
	if got != want {
		t.Errorf("authorizeURL = %q, want %q", got, want)
	}
}

// TestSetupCommandShape pins the /strava registration shape: name, exactly
// one option (label, string, optional — the feat handler's option shape is
// the template).
func TestSetupCommandShape(t *testing.T) {
	s := &Strava{}
	cmd := s.SetupCommand()
	if cmd.Name != "strava" {
		t.Fatalf("Name = %q, want %q", cmd.Name, "strava")
	}
	if len(cmd.Options) != 1 {
		t.Fatalf("Options = %d, want exactly 1", len(cmd.Options))
	}
	o := cmd.Options[0]
	if o.Name != "label" {
		t.Errorf("option Name = %q, want %q", o.Name, "label")
	}
	if o.Type != discordgo.ApplicationCommandOptionString {
		t.Errorf("option Type = %v, want string", o.Type)
	}
	if o.Required {
		t.Errorf("option Required = true, want false")
	}
}
