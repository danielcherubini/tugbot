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
		// Page 1 = 100 rows (ids 106..7, newest first); page 2 = 100 rows
		// (one OVERLAPPING id from page 1 — 106 — followed by ids 104..1
		// completing the descent; id 1, the OLDEST activity, exists ONLY on
		// page 2, so the result must be 106 UNIQUE ids); page 3 (and any
		// further page the walk should never reach) = a FULL page
		// re-returning the same 7 ids — zero NEW ids, so the zero-new-id
		// guard must terminate the walk (no infinite loop on degenerate
		// repeated pages).
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
				out = append(out, pageRows(106)) // overlapping id from page 1
				for i := 0; i < 99; i++ {
					out = append(out, pageRows(int64(104-i))) // 104..2
				}
				out = append(out, pageRows(1)) // 1 — the oldest row, page 2 only
			default:
				for i := 0; i < 7; i++ {
					out = append(out, pageRows(int64(7-i))) // 7..1, all seen → zero new ids
				}
			}
			return out
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v3/athlete":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
			case "/v3/athletes/1/activities":
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

		// Exactly three pages: the full page 1, page 2 (of which one id
		// overlaps page 1), and the zero-new-id page 3 that must terminate
		// the walk.
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
}

func TestGetActivitySignals(t *testing.T) {
	detail := func(t *testing.T, body string) Activity {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			if r.URL.Path != "/v3/activities/7" {
				t.Errorf("path = %s, want /v3/activities/7", r.URL.Path)
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
