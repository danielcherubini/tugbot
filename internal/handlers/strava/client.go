// client.go — the Strava v3 API surface for the strava feature.
package strava

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The list endpoint returns at most 100 rows per page; a smaller page ends
// the loop.
const listPageSize = 100

const (
	athletePath = "/v3/athlete"
	tokenPath   = "/oauth/token"
)

// Summary is what the list endpoint returns per activity (a subset
// model — enough to gate the detail fetch on sport_type).
type Summary struct {
	ID        int64
	SportType string // "" when absent (unparsed device upload → pending, per spec)
	StartDate time.Time
}

// Activity is the full detail model.
// InProgressSet mirrors the SOURCE JSON: whether the "in_progress" key was
// present at all (the documented fallback: field absent → ready unless
// resource_state == -1, the official API reference's processing indicator).
type Activity struct {
	ID            int64
	SportType     string
	Title         string  // "" when unset
	Distance      float64 // meters
	StartDate     time.Time
	InProgress    bool
	InProgressSet bool
	ResourceState int // as returned; -1 == 'processing'
}

// Error classes (each with a distinct Error() text).
type ErrUnauthorized struct{ Why string }       // 401, or refresh 400 invalid_grant
type ErrRateLimited struct{ RetryAfter string } // 429 + X-RateLimit headers
type ErrTransient struct{ Cause error }         // 5xx / network / unexpected 4xx

func (e ErrUnauthorized) Error() string { return "strava: unauthorized: " + e.Why }

func (e ErrRateLimited) Error() string {
	if e.RetryAfter == "" {
		return "strava: rate limited (no retry-after)"
	}
	return "strava: rate limited (retry after " + e.RetryAfter + ")"
}

func (e ErrTransient) Error() string { return "strava: transient: " + e.Cause.Error() }

// StravaAPI is the seam the handler resolves through (tests stub it).
type StravaAPI interface {
	// ListActivities pages internally: loop while a full 100-row page returns.
	ListActivities(ctx context.Context, token string, after time.Time) ([]Summary, error)
	GetActivity(ctx context.Context, token string, id int64) (Activity, error)
	// RefreshToken performs POST /oauth/token (grant_type=refresh_token form:
	// client_id, client_secret, refresh_token). The ROTATED refresh_token is
	// part of the return — the caller MUST persist it (spec).
	RefreshToken(ctx context.Context, clientID, clientSecret, refreshToken string) (accessToken string, newRefreshToken string, expiresAt time.Time, err error)
}

type stravaClient struct{ base string }

// NewStravaAPI is the production constructor (base = https://www.strava.com);
// tests construct stravaClient directly with an httptest URL. No network I/O
// at construction — selftest-safe.
func NewStravaAPI() StravaAPI {
	return &stravaClient{base: "https://www.strava.com"}
}

func (c *stravaClient) ListActivities(ctx context.Context, token string, after time.Time) ([]Summary, error) {
	athleteID, err := c.athleteID(ctx, token)
	if err != nil {
		return nil, err
	}
	var out []Summary
	cursor := after
	for {
		endpoint := fmt.Sprintf("%s/v3/athletes/%d/activities?per_page=%d", c.base, athleteID, listPageSize)
		if !cursor.IsZero() {
			endpoint += "&after=" + strconv.FormatInt(cursor.Unix(), 10)
		}
		page, err := c.getList(ctx, endpoint, token)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		// Loop while a FULL page (exactly 100 rows) returned.
		if len(page) < listPageSize {
			break
		}
		cursor = page[len(page)-1].StartDate
	}
	return out, nil
}

func (c *stravaClient) GetActivity(ctx context.Context, token string, id int64) (Activity, error) {
	var raw activityWire
	if err := c.getJSON(ctx, fmt.Sprintf("%s/v3/activities/%d", c.base, id), token, &raw); err != nil {
		return Activity{}, err
	}
	a := Activity{
		ID:            raw.ID,
		SportType:     raw.SportType,
		Title:         raw.Title,
		Distance:      raw.Distance,
		StartDate:     raw.StartDate,
		ResourceState: raw.ResourceState,
	}
	if raw.InProgress != nil {
		a.InProgressSet = true
		a.InProgress = *raw.InProgress
	}
	return a, nil
}

func (c *stravaClient) RefreshToken(ctx context.Context, clientID, clientSecret, refreshToken string) (string, string, time.Time, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", time.Time{}, ErrTransient{Cause: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", time.Time{}, ErrTransient{Cause: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", time.Time{}, ErrTransient{Cause: err}
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", time.Time{}, classifyStatus(resp.StatusCode, resp.Header, body)
	}
	var tok tokenWire
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", "", time.Time{}, ErrTransient{Cause: fmt.Errorf("decode token response: %w", err)}
	}
	// expires_at is epoch seconds in the API response.
	return tok.AccessToken, tok.RefreshToken, time.Unix(int64(tok.ExpiresAt), 0).UTC(), nil
}

// athleteID resolves the authenticated athlete's ID; the client never
// decodes the access token (server-side endpoint only).
func (c *stravaClient) athleteID(ctx context.Context, token string) (int, error) {
	var ath struct {
		ID int `json:"id"`
	}
	if err := c.getJSON(ctx, c.base+athletePath, token, &ath); err != nil {
		return 0, err
	}
	return ath.ID, nil
}

// getJSON runs a Bearer-authed GET and decodes the 200 body into out; any
// non-200 status is mapped through classifyStatus, and transport errors
// (dial/TLS/timeout) are returned as ErrTransient.
func (c *stravaClient) getJSON(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ErrTransient{Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ErrTransient{Cause: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ErrTransient{Cause: err}
	}
	if resp.StatusCode != http.StatusOK {
		return classifyStatus(resp.StatusCode, resp.Header, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return ErrTransient{Cause: fmt.Errorf("decode response: %w", err)}
	}
	return nil
}

func (c *stravaClient) getList(ctx context.Context, endpoint, token string) ([]Summary, error) {
	var raw []summaryWire
	if err := c.getJSON(ctx, endpoint, token, &raw); err != nil {
		return nil, err
	}
	out := make([]Summary, len(raw))
	for i, s := range raw {
		out[i] = Summary(s)
	}
	return out, nil
}

// classifyStatus is the exact error mapping: any 401 → ErrUnauthorized; a 400
// whose body contains invalid_grant → ErrUnauthorized; any 429 →
// ErrRateLimited (RetryAfter populated from the X-RateLimit-* headers when
// present, else ""); other 4xx and any 5xx → ErrTransient.
func classifyStatus(status int, hdr http.Header, body []byte) error {
	switch status {
	case http.StatusUnauthorized:
		return ErrUnauthorized{Why: "http 401"}
	case http.StatusTooManyRequests:
		ra := hdr.Get("X-RateLimit-Retry-After")
		if ra == "" {
			for k, vs := range hdr {
				if strings.HasPrefix(k, "X-RateLimit-") && len(vs) > 0 {
					ra = vs[0]
					break
				}
			}
		}
		return ErrRateLimited{RetryAfter: ra}
	case http.StatusBadRequest:
		if bytes.Contains(body, []byte("invalid_grant")) {
			return ErrUnauthorized{Why: "invalid_grant"}
		}
		return ErrTransient{Cause: fmt.Errorf("unexpected 400 response")}
	}
	return ErrTransient{Cause: fmt.Errorf("unexpected status %d", status)}
}

// summaryWire mirrors the list-endpoint JSON (sport_type may be absent for
// unparsed uploads → "" in the model).
type summaryWire struct {
	ID        int64     `json:"id"`
	SportType string    `json:"sport_type"`
	StartDate time.Time `json:"start_date"`
}

// activityWire keeps InProgress a pointer so the ABSENCE of the key is
// distinguishable from a false value.
type activityWire struct {
	ID            int64     `json:"id"`
	SportType     string    `json:"sport_type"`
	Title         string    `json:"title"`
	Distance      float64   `json:"distance"`
	StartDate     time.Time `json:"start_date"`
	InProgress    *bool     `json:"in_progress"`
	ResourceState int       `json:"resource_state"`
}

type tokenWire struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresAt    float64 `json:"expires_at"`
}
