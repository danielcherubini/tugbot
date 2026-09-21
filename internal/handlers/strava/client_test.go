package strava

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// statusServer returns an httptest server that responds to every request
// with the given status (and optional headers) — for the error-mapping
// subtests.
func statusServer(t *testing.T, code int, h http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range h {
			for _, v := range vs {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
}

func TestListActivitiesPagination(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	type shape struct {
		ID        int64     `json:"id"`
		SportType string    `json:"sport_type"`
		StartDate time.Time `json:"start_date"`
	}
	sportFor := func(id int64) string {
		if id%2 == 0 {
			return "Run"
		}
		return ""
	}

	t.Run("merged 106 across descending pages, id-deduplicated, fixed after", func(t *testing.T) {
		var calls int
		var afterVals, pageVals []string
		// Page 1 = 100 rows (ids 106..7, newest first); page 2 = a full page
		// whose ids 106 and 104..7 ALL overlap page 1 (99 duplicated ids — 106
		// plus the whole descent 104..7, only 105 is page-1-only) followed by
		// ids 6..2, then 1; ids 6..1 are page-2-only, yet the id-deduplicated
		// merge still yields 106 UNIQUE ids; page 3 (and any further page the
		// walk should never reach) = a FULL 100-row page in which EVERY id
		// was already seen on page 1 — len(rows) < 100 can NOT terminate
		// the walk, so ONLY the zero NEW ids guard (fresh == 0) can (no
		// infinite loop on degenerate repeated full pages).
		pageRows := func(id int64) shape {
			return shape{ID: id, SportType: sportFor(id), StartDate: base.Add(time.Duration(id) * time.Second)}
		}
		rowsFor := func(page int) []shape {
			var out []shape
			switch page {
			case 1:
				for i := 0; i < 100; i++ {
					out = append(out, pageRows(int64(106-i))) // 106..7, newest first
				}
			case 2:
				out = append(out, pageRows(106)) // one of the 99 ids overlapping page 1
				for i := 0; i < 103; i++ {
					out = append(out, pageRows(int64(104-i))) // 104..2
				}
				out = append(out, pageRows(1)) // 1 — the oldest row, page 2 only
			default:
				for i := 0; i < 100; i++ {
					out = append(out, pageRows(int64(106-i))) // FULL 100-row page; every id already seen (fresh == 0 is the ONLY stop)
				}
			}
			return out
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v3/athlete/activities":
				calls++
				q := r.URL.Query()
				if a := q.Get("after"); a != "" {
					afterVals = append(afterVals, a)
				}
				page := 1
				if p := q.Get("page"); p != "" {
					if n, err := strconv.Atoi(p); err == nil {
						page = n
					}
					pageVals = append(pageVals, p)
				}
				// Safety cap: a buggy client paginating the SAME page forever
				// gets 200 calls, not an infinite loop.
				var list []shape
				if calls > 200 {
					list = []shape{}
				} else {
					list = rowsFor(page)
				}
				if err := json.NewEncoder(w).Encode(list); err != nil {
					t.Errorf("encode page: %v", err)
				}
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		c := &stravaClient{base: srv.URL}
		got, err := c.ListActivities(context.Background(), "tok", base)
		if err != nil {
			t.Fatalf("ListActivities: %v", err)
		}

		// Exactly three pages: the full page 1, page 2 (of which 99 ids
		// overlap page 1), and the zero-new-id page 3 that must terminate
		// the walk.
		// Page 3 is a FULL 100-row page of already-seen ids — a SHORT page
		// (len(rows) < 100) would also terminate the walk, so this shape
		// independently exercises the fresh == 0 guard.
		if calls != 3 {
			t.Errorf("activities endpoint called %d times, want 3", calls)
		}
		if len(pageVals) != 3 || pageVals[0] != "1" || pageVals[1] != "2" || pageVals[2] != "3" {
			t.Errorf("page params = %v, want [1 2 3]", pageVals)
		}
		// The FIXED-after invariant: the after anchor is IDENTICAL on every
		// page in particular on page 1 and page 2 — it must NOT be moved
		// (Strava's list is newest-first, so a last-row cursor walks the
		// wrong direction).
		wantAfter := strconv.FormatInt(base.Unix(), 10)
		if len(afterVals) != 3 {
			t.Fatalf("got %d 'after' params, want 3: %v", len(afterVals), afterVals)
		}
		for i, a := range afterVals {
			if a != wantAfter {
				t.Errorf("page %d after = %s, want %s (fixed anchor)", i+1, a, wantAfter)
			}
		}
		if afterVals[0] != afterVals[1] {
			t.Errorf("after moved between page 1 (%s) and page 2 (%s)", afterVals[0], afterVals[1])
		}

		// All 106 UNIQUE ids arrive in one call, deduplicated across pages.
		if len(got) != 106 {
			t.Fatalf("got %d summaries, want 106", len(got))
		}
		seenIDs := make(map[int64]bool, len(got))
		for i, s := range got {
			if seenIDs[s.ID] {
				t.Errorf("got[%d].ID = %d is duplicated in the result", i, s.ID)
			}
			seenIDs[s.ID] = true
			// The client returns (StartDate, ID)-sorted order.
			id := int64(i + 1)
			if s.ID != id {
				t.Errorf("got[%d].ID = %d, want %d", i, s.ID, id)
			}
			if s.SportType != sportFor(id) {
				t.Errorf("got[%d].SportType = %q, want %q", i, s.SportType, sportFor(id))
			}
			want := base.Add(time.Duration(id) * time.Second)
			if !s.StartDate.Equal(want) {
				t.Errorf("got[%d].StartDate = %v, want %v", i, s.StartDate, want)
			}
		}
	})

	t.Run("429 maps to ErrRateLimited", func(t *testing.T) {
		srv := statusServer(t, http.StatusTooManyRequests, http.Header{
			"X-RateLimit-Retry-After": []string{"123"},
		})
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, err := c.ListActivities(context.Background(), "tok", base)
		var rl ErrRateLimited
		if !errors.As(err, &rl) {
			t.Fatalf("want ErrRateLimited, got %v", err)
		}
		if rl.RetryAfter != "123" {
			t.Errorf("RetryAfter = %q, want \"123\"", rl.RetryAfter)
		}
	})

	t.Run("429 plain Retry-After header is extracted", func(t *testing.T) {
		srv := statusServer(t, http.StatusTooManyRequests, http.Header{
			"Retry-After": []string{"900"},
		})
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, err := c.ListActivities(context.Background(), "tok", base)
		var rl ErrRateLimited
		if !errors.As(err, &rl) {
			t.Fatalf("want ErrRateLimited, got %v", err)
		}
		if rl.RetryAfter != "900" {
			t.Errorf("RetryAfter = %q, want \"900\"", rl.RetryAfter)
		}
	})

	t.Run("429 X-RateLimit-Retry-After takes precedence over plain Retry-After", func(t *testing.T) {
		srv := statusServer(t, http.StatusTooManyRequests, http.Header{
			"X-RateLimit-Retry-After": []string{"123"},
			"Retry-After":             []string{"900"},
		})
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, err := c.ListActivities(context.Background(), "tok", base)
		var rl ErrRateLimited
		if !errors.As(err, &rl) {
			t.Fatalf("want ErrRateLimited, got %v", err)
		}
		if rl.RetryAfter != "123" {
			t.Errorf("RetryAfter = %q, want \"123\" (X-RateLimit must win)", rl.RetryAfter)
		}
	})

	t.Run("429 any-other-X-RateLimit-* header is extracted as fallback RetryAfter", func(t *testing.T) {
		// A 429 whose ONLY header is X-RateLimit-Remaining: statusServer
		// copies it via Header().Set, so http.Header STORES it canonically as
		// "X-Ratelimit-Remaining" (Go lowercases all but the first letter of
		// each dash-separated word). Tiers 1 (X-RateLimit-Retry-After) and 2
		// (plain Retry-After) are both empty, so the third tier must match
		// the canonical stored form and surface the value.
		srv := statusServer(t, http.StatusTooManyRequests, http.Header{
			"X-RateLimit-Remaining": []string{"42"},
		})
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, err := c.ListActivities(context.Background(), "tok", base)
		var rl ErrRateLimited
		if !errors.As(err, &rl) {
			t.Fatalf("want ErrRateLimited, got %v", err)
		}
		if rl.RetryAfter != "42" {
			t.Errorf("RetryAfter = %q, want \"42\" (the other X-RateLimit-* fallback tier)", rl.RetryAfter)
		}
	})

	t.Run("401 maps to ErrUnauthorized", func(t *testing.T) {
		srv := statusServer(t, http.StatusUnauthorized, nil)
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, err := c.ListActivities(context.Background(), "tok", base)
		var u ErrUnauthorized
		if !errors.As(err, &u) {
			t.Fatalf("want ErrUnauthorized, got %v", err)
		}
	})

	t.Run("503 maps to ErrTransient", func(t *testing.T) {
		srv := statusServer(t, http.StatusServiceUnavailable, nil)
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, err := c.ListActivities(context.Background(), "tok", base)
		var tr ErrTransient
		if !errors.As(err, &tr) {
			t.Fatalf("want ErrTransient, got %v", err)
		}
	})

	t.Run("404 maps to ErrTransient (an app/role problem, not a per-activity outcome)", func(t *testing.T) {
		srv := statusServer(t, http.StatusNotFound, nil)
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, err := c.ListActivities(context.Background(), "tok", base)
		var tr ErrTransient
		if !errors.As(err, &tr) {
			t.Fatalf("want ErrTransient (a list-endpoint 404 is NEVER the per-activity ErrGone), got %v", err)
		}
	})
}

func TestGetActivitySignals(t *testing.T) {
	detail := func(t *testing.T, body string) Activity {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			if r.URL.Path != "/api/v3/activities/7" {
				t.Errorf("path = %s, want /api/v3/activities/7", r.URL.Path)
			}
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		a, err := c.GetActivity(context.Background(), "tok", 7)
		if err != nil {
			t.Fatalf("GetActivity: %v", err)
		}
		return a
	}

	wantFields := func(t *testing.T, a Activity) {
		t.Helper()
		if a.ID != 7 {
			t.Errorf("ID = %d, want 7", a.ID)
		}
		if a.SportType != "Swim" {
			t.Errorf("SportType = %q, want \"Swim\"", a.SportType)
		}
		if a.Title != "Morning Swim" {
			t.Errorf("Title = %q, want \"Morning Swim\"", a.Title)
		}
		if a.Distance != 2500.5 {
			t.Errorf("Distance = %v, want 2500.5", a.Distance)
		}
		want := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
		if !a.StartDate.Equal(want) {
			t.Errorf("StartDate = %v, want %v", a.StartDate, want)
		}
	}

	t.Run("in_progress present true", func(t *testing.T) {
		body := `{"id":7,"sport_type":"Swim","title":"Morning Swim","distance":2500.5,"start_date":"2026-02-03T04:05:06Z","in_progress":true,"resource_state":0}`
		a := detail(t, body)
		wantFields(t, a)
		if !a.InProgress {
			t.Errorf("InProgress = false, want true")
		}
		if !a.InProgressSet {
			t.Errorf("InProgressSet = false, want true (key present)")
		}
	})

	t.Run("key absent with resource_state -1", func(t *testing.T) {
		body := `{"id":7,"sport_type":"Swim","title":"Morning Swim","distance":2500.5,"start_date":"2026-02-03T04:05:06Z","resource_state":-1}`
		a := detail(t, body)
		wantFields(t, a)
		if a.InProgress {
			t.Errorf("InProgress = true, want false")
		}
		if a.InProgressSet {
			t.Errorf("InProgressSet = true, want false (key absent)")
		}
		if a.ResourceState != -1 {
			t.Errorf("ResourceState = %d, want -1", a.ResourceState)
		}
	})

	t.Run("neither signal is the ready class", func(t *testing.T) {
		body := `{"id":7,"sport_type":"Swim","title":"Morning Swim","distance":2500.5,"start_date":"2026-02-03T04:05:06Z","resource_state":0}`
		a := detail(t, body)
		wantFields(t, a)
		if a.InProgress {
			t.Errorf("InProgress = true, want false")
		}
		if a.InProgressSet {
			t.Errorf("InProgressSet = true, want false (key absent)")
		}
		if a.ResourceState == -1 {
			t.Errorf("ResourceState = -1, want != -1 (ready class)")
		}
	})

	t.Run("404 maps to ErrGone (the detail endpoint only — a listed id that 404s is gone)", func(t *testing.T) {
		srv := statusServer(t, http.StatusNotFound, nil)
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, err := c.GetActivity(context.Background(), "tok", 7)
		var g ErrGone
		if !errors.As(err, &g) {
			t.Fatalf("want ErrGone, got %v", err)
		}
		if g.Error() != "strava activity gone (404)" {
			t.Errorf("Error() = %q, want \"strava activity gone (404)\"", g.Error())
		}
	})

	t.Run("refresh 400 invalid_grant maps to ErrUnauthorized", func(t *testing.T) {
		// The mapping keys off `invalid_grant` in the 400 body on the refresh
		// route.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
				t.Errorf("want POST /oauth/token, got %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}))
		defer srv.Close()
		c := &stravaClient{base: srv.URL}
		_, _, _, err := c.RefreshToken(context.Background(), "cid", "sec", "rt_old")
		var u ErrUnauthorized
		if !errors.As(err, &u) {
			t.Fatalf("want ErrUnauthorized, got %v", err)
		}
	})
}

func TestRefreshTokenRotation(t *testing.T) {
	const epoch = 1780000000

	var (
		gotMethod  string
		gotPath    string
		gotValues  map[string]string
		gotContent string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContent = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		gotValues = map[string]string{
			"client_id":     r.PostForm.Get("client_id"),
			"client_secret": r.PostForm.Get("client_secret"),
			"grant_type":    r.PostForm.Get("grant_type"),
			"refresh_token": r.PostForm.Get("refresh_token"),
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at_new",
			"refresh_token": "rt_new",
			"expires_at":    float64(epoch),
		})
	}))
	defer srv.Close()

	c := &stravaClient{base: srv.URL}
	at, rt, exp, err := c.RefreshToken(context.Background(), "cid", "sec", "rt_old")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if at != "at_new" {
		t.Errorf("accessToken = %q, want \"at_new\"", at)
	}
	if rt != "rt_new" {
		t.Errorf("newRefreshToken = %q, want \"rt_new\" (the ROTATED token)", rt)
	}
	if !exp.Equal(time.Unix(epoch, 0).UTC()) {
		t.Errorf("expiresAt = %v, want %v", exp, time.Unix(epoch, 0).UTC())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/oauth/token" {
		t.Errorf("path = %s, want /oauth/token", gotPath)
	}
	if !strings.HasPrefix(gotContent, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", gotContent)
	}
	if gotValues["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q, want \"refresh_token\"", gotValues["grant_type"])
	}
	if gotValues["client_id"] != "cid" {
		t.Errorf("client_id = %q, want \"cid\"", gotValues["client_id"])
	}
	if gotValues["client_secret"] != "sec" {
		t.Errorf("client_secret = %q, want \"sec\"", gotValues["client_secret"])
	}
	if gotValues["refresh_token"] != "rt_old" {
		t.Errorf("refresh_token = %q, want \"rt_old\"", gotValues["refresh_token"])
	}
}

func TestGetAthlete(t *testing.T) {
	var (
		gotAuth  string
		gotPath  string
		gotQuery string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/v3/athlete" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":        2703661,
			"firstname": "Daniel",
			"lastname":  "Cherubini",
			"username":  "danielcherubini",
		})
	}))
	defer srv.Close()

	t.Run("200 decodes the athlete struct", func(t *testing.T) {
		c := &stravaClient{base: srv.URL}
		a, err := c.GetAthlete(context.Background(), "tok")
		if err != nil {
			t.Fatalf("GetAthlete: %v", err)
		}
		if a.ID != 2703661 {
			t.Errorf("ID = %d, want 2703661", a.ID)
		}
		if a.Firstname != "Daniel" {
			t.Errorf("Firstname = %q, want \"Daniel\"", a.Firstname)
		}
		if a.Lastname != "Cherubini" {
			t.Errorf("Lastname = %q, want \"Cherubini\"", a.Lastname)
		}
		if a.Username != "danielcherubini" {
			t.Errorf("Username = %q, want \"danielcherubini\"", a.Username)
		}
		if gotPath != "/api/v3/athlete" {
			t.Errorf("path = %s, want /api/v3/athlete", gotPath)
		}
		if gotQuery != "" {
			t.Errorf("query = %q, want empty", gotQuery)
		}
		if gotAuth != "Bearer tok" {
			t.Errorf("Authorization = %q, want \"Bearer tok\"", gotAuth)
		}
	})

	t.Run("401 maps to ErrUnauthorized", func(t *testing.T) {
		srv401 := statusServer(t, http.StatusUnauthorized, nil)
		defer srv401.Close()
		c := &stravaClient{base: srv401.URL}
		_, err := c.GetAthlete(context.Background(), "tok")
		var u ErrUnauthorized
		if !errors.As(err, &u) {
			t.Fatalf("want ErrUnauthorized, got %v", err)
		}
	})

	t.Run("404 maps to ErrGone", func(t *testing.T) {
		srv404 := statusServer(t, http.StatusNotFound, nil)
		defer srv404.Close()
		c := &stravaClient{base: srv404.URL}
		_, err := c.GetAthlete(context.Background(), "tok")
		var g ErrGone
		if !errors.As(err, &g) {
			t.Fatalf("want ErrGone, got %v", err)
		}
	})
}

func TestExchangeCode(t *testing.T) {
	const epoch = 1789864896

	var (
		gotMethod  string
		gotPath    string
		gotValues  map[string]string
		gotContent string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContent = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		gotValues = map[string]string{
			"client_id":     r.PostForm.Get("client_id"),
			"client_secret": r.PostForm.Get("client_secret"),
			"grant_type":    r.PostForm.Get("grant_type"),
			"code":          r.PostForm.Get("code"),
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "a",
			"refresh_token": "r",
			"expires_at":    float64(epoch),
			"scope":         "read activity:read_all",
		})
	}))
	defer srv.Close()

	t.Run("200 returns all five values", func(t *testing.T) {
		c := &stravaClient{base: srv.URL}
		at, rt, exp, scope, err := c.ExchangeCode(context.Background(), "cid", "sec", "code123")
		if err != nil {
			t.Fatalf("ExchangeCode: %v", err)
		}
		if at != "a" {
			t.Errorf("accessToken = %q, want \"a\"", at)
		}
		if rt != "r" {
			t.Errorf("refreshToken = %q, want \"r\"", rt)
		}
		if !exp.Equal(time.Unix(epoch, 0)) {
			t.Errorf("expiresAt = %v, want %v", exp, time.Unix(epoch, 0))
		}
		if scope != "read activity:read_all" {
			t.Errorf("scope = %q, want \"read activity:read_all\"", scope)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %s, want POST", gotMethod)
		}
		if gotPath != "/oauth/token" {
			t.Errorf("path = %s, want /oauth/token (the ROOT endpoint — NOT /api/…)", gotPath)
		}
		if !strings.HasPrefix(gotContent, "application/x-www-form-urlencoded") {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", gotContent)
		}
		if gotValues["grant_type"] != "authorization_code" {
			t.Errorf("grant_type = %q, want \"authorization_code\"", gotValues["grant_type"])
		}
		if gotValues["client_id"] != "cid" {
			t.Errorf("client_id = %q, want \"cid\"", gotValues["client_id"])
		}
		if gotValues["client_secret"] != "sec" {
			t.Errorf("client_secret = %q, want \"sec\"", gotValues["client_secret"])
		}
		if gotValues["code"] != "code123" {
			t.Errorf("code = %q, want \"code123\"", gotValues["code"])
		}
	})

	t.Run("400 maps to ErrInvalidCode (the code grant — NOT the refresh invalid_grant class)", func(t *testing.T) {
		srv400 := statusServer(t, http.StatusBadRequest, nil)
		defer srv400.Close()
		c := &stravaClient{base: srv400.URL}
		_, _, _, _, err := c.ExchangeCode(context.Background(), "cid", "sec", "code123")
		if !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("want ErrInvalidCode, got %v", err)
		}
	})

	t.Run("401 maps to ErrUnauthorized", func(t *testing.T) {
		srv401 := statusServer(t, http.StatusUnauthorized, nil)
		defer srv401.Close()
		c := &stravaClient{base: srv401.URL}
		_, _, _, _, err := c.ExchangeCode(context.Background(), "cid", "sec", "code123")
		var u ErrUnauthorized
		if !errors.As(err, &u) {
			t.Fatalf("want ErrUnauthorized, got %v", err)
		}
	})
}
