package strava

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/config"
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

// TestScopeHasReadAll pins the exact-FIELD scope check (whitespace-split; a
// field must equal "activity:read_all" — NOT a substring match).
func TestScopeHasReadAll(t *testing.T) {
	cases := []struct {
		scope string
		want  bool
	}{
		{"activity:read_all read", true},
		{"read", false},
		{"", false},
		{"activity:read_all", true},
		{"activity:read_allx read", false},
	}
	for _, c := range cases {
		t.Run(c.scope, func(t *testing.T) {
			if got := scopeHasReadAll(c.scope); got != c.want {
				t.Errorf("scopeHasReadAll(%q) = %v, want %v", c.scope, got, c.want)
			}
		})
	}
}

// TestResolveLabel pins the display/fallback resolution: the first non-empty
// of (rowLabel, existingLabel, firstname, username), else "Athlete". An
// explicit label wins over everything; an omitted label preserves the stored
// label for an existing athlete; a new athlete falls to first name → username
// → "Athlete".
func TestResolveLabel(t *testing.T) {
	cases := []struct {
		name      string
		rowLabel  string
		existing  string
		firstname string
		username  string
		want      string
	}{
		{"explicit wins over everything", "Sam", "OldName", "Daniel", "daniel", "Sam"},
		{"omitted → stored label preserved", "", "OldName", "Daniel", "daniel", "OldName"},
		{"new athlete → first name", "", "", "Daniel", "daniel", "Daniel"},
		{"new athlete, empty first name → username", "", "", "", "daniel", "daniel"},
		{"all empty → Athlete", "", "", "", "", "Athlete"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveLabel(c.rowLabel, c.existing, c.firstname, c.username); got != c.want {
				t.Errorf("resolveLabel(%q, %q, %q, %q) = %q, want %q", c.rowLabel, c.existing, c.firstname, c.username, got, c.want)
			}
		})
	}
}

// TestOnboardingThrottle pins the throttle's edge semantics with a BARE
// handler (no app, no pool — the throttle state is closure-local to
// OnboardingHandler and fires at the request edge before any app/pool
// access): 10 requests from one client pass (any status, but not 429), the
// 11th gets 429. Key = the LAST hop of X-Forwarded-For — caddy's
// reverse_proxy PRESERVES an inbound client-supplied XFF and APPENDS the
// trusted client IP at the END of the chain, so the first hop is
// attacker-controlled — falling back to RemoteAddr when the header is
// absent.
func TestOnboardingThrottle(t *testing.T) {
	s := &Strava{}
	h := s.OnboardingHandler()
	doReq := func(xff string) int {
		req := httptest.NewRequest(http.MethodGet, "/strava/callback", nil)
		req.RemoteAddr = "1.2.3.4:9999"
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	t.Run("RemoteAddr fallback (no XFF header)", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			if code := doReq(""); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (the 10/min budget)", i+1)
			}
		}
		if code := doReq(""); code != http.StatusTooManyRequests {
			t.Fatalf("request 11 got %d, want 429", code)
		}
	})
	t.Run("XFF last-hop keying", func(t *testing.T) {
		s2 := &Strava{}
		h2 := s2.OnboardingHandler()
		doReq2 := func(xff string) int {
			req := httptest.NewRequest(http.MethodGet, "/strava/callback", nil)
			req.RemoteAddr = "1.2.3.4:9999"
			req.Header.Set("X-Forwarded-For", xff)
			rec := httptest.NewRecorder()
			h2.ServeHTTP(rec, req)
			return rec.Code
		}
		// Two requests with different XFF values → separate buckets, both
		// pass.
		if code := doReq2("1.1.1.1"); code == http.StatusTooManyRequests {
			t.Fatalf("first 1.1.1.1 request got 429, want it to pass")
		}
		if code := doReq2("2.2.2.2"); code == http.StatusTooManyRequests {
			t.Fatalf("first 2.2.2.2 request got 429, want it to pass (a separate bucket)")
		}
		// 11 requests with the SAME XFF value → the 11th is 429.
		for i := 0; i < 10; i++ {
			if code := doReq2("3.3.3.3"); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (the 10/min budget)", i+1)
			}
		}
		if code := doReq2("3.3.3.3"); code != http.StatusTooManyRequests {
			t.Fatalf("request 11 got %d, want 429", code)
		}
	})
	t.Run("RemoteAddr fallback: rotating ports (port stripped)", func(t *testing.T) {
		// 11 requests, SAME host, 11 distinct ephemeral source ports → the
		// 11th is 429. The port is stripped: a raw RemoteAddr would be a
		// fresh bucket per connection and the 10/min limit would never
		// fire on the direct path.
		s4 := &Strava{}
		h4 := s4.OnboardingHandler()
		doReq4 := func(remote string) int {
			req := httptest.NewRequest(http.MethodGet, "/strava/callback", nil)
			req.RemoteAddr = remote
			rec := httptest.NewRecorder()
			h4.ServeHTTP(rec, req)
			return rec.Code
		}
		for i := 0; i < 10; i++ {
			if code := doReq4(fmt.Sprintf("1.2.3.4:%d", 40000+i)); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (the 10/min budget)", i+1)
			}
		}
		if code := doReq4("1.2.3.4:40010"); code != http.StatusTooManyRequests {
			t.Fatalf("request 11 got %d, want 429 (the port is stripped — a rotated port is NOT a fresh bucket)", code)
		}
	})
	t.Run("hard cap: 10 001 unique keys pass, the 10 002nd is 429", func(t *testing.T) {
		// A single-window burst of unique keys can't balloon the map: past
		// 10 000 stored entries a NEW key is throttled, not inserted (the
		// 60 s sweep bounds steady-state growth only).
		s5 := &Strava{}
		h5 := s5.OnboardingHandler()
		doReq5 := func(xff string) int {
			req := httptest.NewRequest(http.MethodGet, "/strava/callback", nil)
			req.Header.Set("X-Forwarded-For", xff)
			rec := httptest.NewRecorder()
			h5.ServeHTTP(rec, req)
			return rec.Code
		}
		for i := 0; i < 10001; i++ {
			if code := doReq5(fmt.Sprintf("10.0.%d.%d", i/256, i%256)); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (a fresh bucket under the cap)", i+1)
			}
		}
		if code := doReq5("10.9.9.9"); code != http.StatusTooManyRequests {
			t.Fatalf("request 10 002 got %d, want 429 (the hard cap — a new key is not inserted past 10 000 entries)", code)
		}
	})
	t.Run("XFF multi-hop: the LAST hop is the key", func(t *testing.T) {
		// A client rotating its own first hop (the inbound XFF caddy
		// preserves) must NOT get a fresh bucket per request — the
		// trusted proxy's appended hop (last) is the key. 11 requests,
		// distinct first hops, same last hop → the 11th is 429.
		s3 := &Strava{}
		h3 := s3.OnboardingHandler()
		doReq3 := func(xff string) int {
			req := httptest.NewRequest(http.MethodGet, "/strava/callback", nil)
			req.RemoteAddr = "1.2.3.4:9999"
			req.Header.Set("X-Forwarded-For", xff)
			rec := httptest.NewRecorder()
			h3.ServeHTTP(rec, req)
			return rec.Code
		}
		for i := 0; i < 10; i++ {
			if code := doReq3(fmt.Sprintf("10.0.0.%d, 4.4.4.4", i)); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (the 10/min budget)", i+1)
			}
		}
		if code := doReq3("10.0.0.99, 4.4.4.4"); code != http.StatusTooManyRequests {
			t.Fatalf("request 11 got %d, want 429 (the last hop is the key — a rotated first hop is NOT a fresh bucket)", code)
		}
	})
}

// TestStartBindFailure pins Start's fail-fast path: a bind failure
// (address in use) is returned promptly WITHOUT any ctx cancel — the
// errgroup arm's os.Exit(1) fires at startup, not at the next SIGTERM.
func TestStartBindFailure(t *testing.T) {
	// Occupy the port for the duration of the test.
	l, err := net.Listen("tcp", ":8643")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	s := &Strava{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Start() on a bound port = nil, want an error (not a panic)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not fail fast on a bound port (blocked without any ctx cancel)")
	}
}

// TestStartNilOnCancel pins Start's clean-cancel contract (the
// mcp.Server.Start template, mirrored by mcp's TestStartNilOnCancel): a
// clean ctx-cancel shutdown returns nil — NOT context.Canceled, NOT
// http.ErrServerClosed — well under the 10 s shutdown grace.
func TestStartNilOnCancel(t *testing.T) {
	s := &Strava{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	// Let the listener come up (:8643 must accept before cancel).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", "127.0.0.1:8643")
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	start := time.Now()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start() after cancel = %v (%T), want nil", err, err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("Start() took %v after cancel, want well under the 10 s shutdown grace", elapsed)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Start() did not return within 12s of cancel (shutdown grace must be ≤10s)")
	}
}

// webhookTestStrava is the unit-test construction: a real *app.App with a
// real *config.Config but a NIL pool — loadAthleteByStravaID's nil-pool
// guard returns an explicit error (a nil *pgxpool.Pool receiver would
// PANIC on QueryRow), so every DB-touching arm takes the graceful
// DB-failure arm (log + 200, no job).
func webhookTestStrava(verifyToken string) *Strava {
	return &Strava{app: &app.App{Cfg: &config.Config{StravaWebhookVerifyToken: verifyToken}}}
}

// TestWebhookRouteGuard pins the webhook handler's route guard: only
// /strava/webhook proceeds (both methods — Strava POSTs events and GETs the
// verification challenge); anything else 404s (the onboarding route is NOT
// served by the webhook handler).
func TestWebhookRouteGuard(t *testing.T) {
	h := webhookTestStrava("").WebhookHandler()
	do := func(method, target string) int {
		req := httptest.NewRequest(method, target, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(http.MethodGet, "/"); code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404", code)
	}
	if code := do(http.MethodPost, "/strava/callback"); code != http.StatusNotFound {
		t.Errorf("POST /strava/callback = %d, want 404 (the webhook handler serves only /strava/webhook)", code)
	}
	if code := do(http.MethodPost, "/"); code != http.StatusNotFound {
		t.Errorf("POST / = %d, want 404", code)
	}
	if code := do(http.MethodDelete, "/strava/webhook"); code != http.StatusNotFound {
		t.Errorf("DELETE /strava/webhook = %d, want 404 (only POST events + GET verification)", code)
	}
}

// TestRoutesMux pins the routes() mux: /strava/callback is the onboarding
// handler (its GET-only guard 404s a POST — behavior-preserving),
// /strava/webhook is the webhook handler (a create event 200s via the
// nil-pool DB-failure arm), and everything else 404s (mux default).
func TestRoutesMux(t *testing.T) {
	m := webhookTestStrava("").routes()
	do := func(method, target, body string) int {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, target, nil)
		} else {
			r = httptest.NewRequest(method, target, strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := do(http.MethodPost, "/strava/callback", ""); code != http.StatusNotFound {
		t.Errorf("POST /strava/callback = %d, want 404 (the onboarding handler's GET-only guard is unchanged)", code)
	}
	if code := do(http.MethodPost, "/strava/webhook", `{"aspect_type":"create","object_type":"activity","object_id":1,"owner_id":1}`); code != http.StatusOK {
		t.Errorf("POST /strava/webhook = %d, want 200 (the nil-pool DB-failure arm — never 5xx on a webhook)", code)
	}
	if code := do(http.MethodGet, "/", ""); code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404 (mux default)", code)
	}
}

// TestWebhookVerifyTokenEcho pins the verification handshake's three arms:
// a matching non-empty token → 200 + the exact hub.challenge echoed as JSON;
// a mismatch → 403 with the challenge NOT echoed (no oracle); an empty
// config token → 403 (feature off — absent/empty = the verification GET
// 403s); a GET without hub.mode=subscribe → 404.
func TestWebhookVerifyTokenEcho(t *testing.T) {
	do := func(tokenCfg, target string) (int, string) {
		rec := httptest.NewRecorder()
		webhookTestStrava(tokenCfg).WebhookHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Code, rec.Body.String()
	}
	t.Run("matching token -> 200 + echoed challenge", func(t *testing.T) {
		code, body := do("sekret", "/strava/webhook?hub.mode=subscribe&hub.verify_token=sekret&hub.challenge=abc123")
		if code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		if body != `{"hub.challenge":"abc123"}` {
			t.Errorf("body = %q, want the exact echoed challenge JSON {\"hub.challenge\":\"abc123\"}", body)
		}
	})
	t.Run("mismatching token -> 403, no echo", func(t *testing.T) {
		code, body := do("sekret", "/strava/webhook?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=abc123")
		if code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", code)
		}
		if strings.Contains(body, "abc123") {
			t.Errorf("body = %q, want the challenge NOT echoed on a mismatch (no oracle)", body)
		}
	})
	t.Run("empty config -> 403 (feature off)", func(t *testing.T) {
		code, body := do("", "/strava/webhook?hub.mode=subscribe&hub.verify_token=&hub.challenge=abc123")
		if code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (absent token = verification disabled — an empty match is NOT enabled)", code)
		}
		if strings.Contains(body, "abc123") {
			t.Errorf("body = %q, want no echo", body)
		}
	})
	t.Run("GET without hub.mode=subscribe -> 404", func(t *testing.T) {
		code, _ := do("sekret", "/strava/webhook?hub.verify_token=sekret&hub.challenge=abc123")
		if code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", code)
		}
	})
}

// TestWebhookEventClassification pins the POST classification arms with the
// nil-pool construction (loadAthleteByStravaID returns the explicit
// nil-guard error → the graceful DB-failure arm): create/update → 200 + no
// job; delete → 200 no-op (out of scope); unparseable body / unknown
// object_type → 200 no-op (never 5xx — Strava retries non-200s up to 3
// times; we want the event dropped, not replayed). The full enqueue
// assertions live in the PG integration tests (they need a real athlete
// row to get past the DB-failure arm).
func TestWebhookEventClassification(t *testing.T) {
	captured := make(chan webhookJob)
	s := webhookTestStrava("")
	s.workerFn = func(_ context.Context, j webhookJob) { captured <- j }
	h := s.WebhookHandler()
	do := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/strava/webhook", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	noJob := func() {
		select {
		case j := <-captured:
			t.Errorf("job captured: %+v, want none (the nil-pool DB-failure arm enqueues nothing)", j)
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Run("create -> 200 + no job (nil-pool DB-failure arm)", func(t *testing.T) {
		if code := do(`{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		noJob()
	})
	t.Run("update -> 200 + no job", func(t *testing.T) {
		if code := do(`{"aspect_type":"update","object_type":"activity","object_id":555,"owner_id":777}`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		noJob()
	})
	t.Run("delete -> 200 + no job (out of scope)", func(t *testing.T) {
		if code := do(`{"aspect_type":"delete","object_type":"activity","object_id":555,"owner_id":777}`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		noJob()
	})
	t.Run("unparseable body -> 200 + no job", func(t *testing.T) {
		if code := do(`{not json`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (never 5xx on a webhook)", code)
		}
		noJob()
	})
	t.Run("unknown object_type -> 200 + no job", func(t *testing.T) {
		if code := do(`{"aspect_type":"create","object_type":"bogus","object_id":555,"owner_id":777}`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		noJob()
	})
}

// TestWebhookStringShapedIDs pins the LENIENT acceptance: a STRING-shaped
// id (not the documented numeric shape) still parses — a string create
// event for a known owner reaches the nil-pool DB-failure arm (200 + no
// job) rather than the unparseable-id arm: the flexID decode of the string
// shape is what lets a string id through (asserted via the log stream —
// the DB-failure arm's "athlete lookup failed" line is present and NO
// unparseable-id line is).
func TestWebhookStringShapedIDs(t *testing.T) {
	var logBuf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	captured := make(chan webhookJob)
	s := webhookTestStrava("")
	s.workerFn = func(_ context.Context, j webhookJob) { captured <- j }
	h := s.WebhookHandler()
	req := httptest.NewRequest(http.MethodPost, "/strava/webhook",
		strings.NewReader(`{"aspect_type":"create","object_type":"activity","object_id":"555","owner_id":"777"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("string-shaped ids = %d, want 200 (a string id still parses — the lenient acceptance)", rec.Code)
	}
	select {
	case j := <-captured:
		t.Errorf("job captured: %+v, want none (the nil-pool DB-failure arm enqueues nothing)", j)
	case <-time.After(200 * time.Millisecond):
	}
	// The parse SUCCEEDED — the classification reached the nil-pool
	// DB-failure arm ("athlete lookup failed"), NOT the unparseable-id arm
	// (a string-shaped id decodes to a real value; a non-parsing value
	// would decode to 0 → the unparseable arm).
	if !strings.Contains(logBuf.String(), "athlete lookup failed") {
		t.Errorf("log = %q, want the DB-failure arm (\"athlete lookup failed\") — not the unparseable-id arm", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "unparseable") {
		t.Errorf("log = %q, want NO unparseable-id line (the string id parsed)", logBuf.String())
	}
}

// TestFlexIDShapes pins the flexID edge shapes the fix claims decode to 0
// WITHOUT erroring the whole body: a JSON number, a JSON string, a literal
// 0, and the unparseable shapes (a non-numeric string, a bool, null, an
// object, a float) all decode to 0 with a nil error — a bad id never fails
// the body (only genuinely malformed JSON fails the top-level Decode and
// takes the 200 no-op arm). The raw wire bytes are captured too (the
// unparseable-id log shows the raw value, not the always-0 parsed value).
func TestFlexIDShapes(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int64
		wantRaw string
	}{
		{"number", `555`, 555, `555`},
		{"string", `"555"`, 555, `"555"`},
		{"zero", `0`, 0, `0`},
		{"garbage-string", `"abc"`, 0, `"abc"`},
		{"bool", `true`, 0, `true`},
		{"null", `null`, 0, `null`},
		{"object", `{}`, 0, `{}`},
		{"float", `134815.0`, 0, `134815.0`},
		{"empty-string", `""`, 0, `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f flexID
			if err := json.Unmarshal([]byte(tc.in), &f); err != nil {
				t.Fatalf("json.Unmarshal(%s) = %v, want nil (a bad id never fails the body)", tc.in, err)
			}
			if got := f.Int64(); got != tc.want {
				t.Errorf("flexID = %d, want %d", got, tc.want)
			}
			if got := f.raw; got != tc.wantRaw {
				t.Errorf("flexID.raw = %q, want %q (the raw wire value is captured for the no-op log)", got, tc.wantRaw)
			}
		})
	}
	t.Run("empty-input-direct-call", func(t *testing.T) {
		var f flexID
		// A direct call with empty input must not panic (b[0] on empty input
		// would panic; unreachable via encoding/json, which never passes an
		// empty slice) and decodes to 0.
		if err := f.UnmarshalJSON(nil); err != nil {
			t.Fatalf("UnmarshalJSON(nil) = %v, want nil", err)
		}
		if got := f.Int64(); got != 0 {
			t.Errorf("flexID = %d, want 0", got)
		}
	})
}

// TestWebhookThrottle pins the webhook handler's OWN throttle (its own
// closure-local map — the onboarding handler's map stays untouched; two
// independent throttle maps, one per route) with the same edge semantics as
// TestOnboardingThrottle: 10 requests from one client pass (200 via the
// nil-pool DB-failure arm, but not 429), the 11th gets 429. Key = the LAST
// hop of X-Forwarded-For, falling back to the port-stripped RemoteAddr.
func TestWebhookThrottle(t *testing.T) {
	// ONE handler per subtest (the throttle state is closure-local to the
	// WebhookHandler factory — a fresh factory call is a fresh map, so the
	// 429 would never fire). The factory also starts the workers once
	// (workerOnce) — harmless here (no jobs are enqueued: nil pool).
	newReq := func(xff, remote string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/strava/webhook", strings.NewReader(`{"aspect_type":"create","object_type":"activity","object_id":1,"owner_id":1}`))
		if remote != "" {
			r.RemoteAddr = remote
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	doReq := func(h http.Handler, xff, remote string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReq(xff, remote))
		return rec.Code
	}
	t.Run("RemoteAddr fallback (no XFF header)", func(t *testing.T) {
		h := webhookTestStrava("").WebhookHandler()
		for i := 0; i < 10; i++ {
			if code := doReq(h, "", "1.2.3.4:9999"); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (the 10/min budget)", i+1)
			}
		}
		if code := doReq(h, "", "1.2.3.4:9999"); code != http.StatusTooManyRequests {
			t.Fatalf("request 11 got %d, want 429", code)
		}
	})
	t.Run("XFF last-hop keying", func(t *testing.T) {
		h := webhookTestStrava("").WebhookHandler()
		// Two requests with different XFF values → separate buckets, both pass.
		if code := doReq(h, "1.1.1.1", ""); code == http.StatusTooManyRequests {
			t.Fatalf("first 1.1.1.1 request got 429, want it to pass")
		}
		if code := doReq(h, "2.2.2.2", ""); code == http.StatusTooManyRequests {
			t.Fatalf("first 2.2.2.2 request got 429, want it to pass (a separate bucket)")
		}
		// 11 requests with the SAME XFF value → the 11th is 429.
		for i := 0; i < 10; i++ {
			if code := doReq(h, "3.3.3.3", ""); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (the 10/min budget)", i+1)
			}
		}
		if code := doReq(h, "3.3.3.3", ""); code != http.StatusTooManyRequests {
			t.Fatalf("request 11 got %d, want 429", code)
		}
	})
	t.Run("RemoteAddr fallback: rotating ports (port stripped)", func(t *testing.T) {
		// 11 requests, SAME host, 11 distinct ephemeral source ports → the
		// 11th is 429 (a raw RemoteAddr would be a fresh bucket per
		// connection and the 10/min limit would never fire on the direct
		// path).
		h := webhookTestStrava("").WebhookHandler()
		for i := 0; i < 10; i++ {
			if code := doReq(h, "", fmt.Sprintf("1.2.3.4:%d", 40000+i)); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (the 10/min budget)", i+1)
			}
		}
		if code := doReq(h, "", "1.2.3.4:40010"); code != http.StatusTooManyRequests {
			t.Fatalf("request 11 got %d, want 429 (the port is stripped — a rotated port is NOT a fresh bucket)", code)
		}
	})
	t.Run("hard cap: 10 001 unique keys pass, the 10 002nd is 429", func(t *testing.T) {
		// A single-window burst of unique keys can't balloon the map: past
		// 10 000 stored entries a NEW key is throttled, not inserted (the
		// 60 s sweep bounds steady-state growth only).
		h := webhookTestStrava("").WebhookHandler()
		for i := 0; i < 10001; i++ {
			if code := doReq(h, fmt.Sprintf("10.0.%d.%d", i/256, i%256), ""); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (a fresh bucket under the cap)", i+1)
			}
		}
		if code := doReq(h, "10.9.9.9", ""); code != http.StatusTooManyRequests {
			t.Fatalf("request 10 002 got %d, want 429 (the hard cap — a new key is not inserted past 10 000 entries)", code)
		}
	})
	t.Run("XFF multi-hop: the LAST hop is the key", func(t *testing.T) {
		// A client rotating its own first hop (the inbound XFF caddy
		// preserves) must NOT get a fresh bucket per request — the trusted
		// proxy's appended hop (last) is the key. 11 requests, distinct
		// first hops, same last hop → the 11th is 429.
		h := webhookTestStrava("").WebhookHandler()
		for i := 0; i < 10; i++ {
			if code := doReq(h, fmt.Sprintf("10.0.0.%d, 4.4.4.4", i), ""); code == http.StatusTooManyRequests {
				t.Fatalf("request %d got 429, want it to pass (the 10/min budget)", i+1)
			}
		}
		if code := doReq(h, "10.0.0.99, 4.4.4.4", ""); code != http.StatusTooManyRequests {
			t.Fatalf("request 11 got %d, want 429 (the last hop is the key — a rotated first hop is NOT a fresh bucket)", code)
		}
	})
}
