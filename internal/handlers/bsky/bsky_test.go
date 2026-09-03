package bsky

import "testing"

// Port of fx_rewriter's regex `https://(bsky.app)/.+` with the first capture
// group ("bsky.app") replaced by "bsyy.app" (src/handlers/bsky.rs:29-41).
// The returned value is ONLY the matched substring — Rust's `channel.say`
// posts exactly that, not the whole message (parity anchor).

// Mirrors bsky.rs test `bsky_rewrite`.
func TestBskyRewrite(t *testing.T) {
	got := rewrite("https://bsky.app/profile/radleybalko.bsky.social/post/3lb5nsfya6s2o")
	want := "https://bsyy.app/profile/radleybalko.bsky.social/post/3lb5nsfya6s2o"
	if got != want {
		t.Errorf("rewrite = %q, want %q", got, want)
	}
}

// Mirrors bsky.rs test `bsky_no_match`.
func TestBskyNoMatch(t *testing.T) {
	if got := rewrite("https://twitter.com/someone/status/123"); got != "" {
		t.Errorf("rewrite(twitter url) = %q, want empty", got)
	}
}

// Mirrors bsky.rs test `bsky_empty_string`.
func TestBskyEmptyString(t *testing.T) {
	if got := rewrite(""); got != "" {
		t.Errorf("rewrite(\"\") = %q, want empty", got)
	}
}

// Mirrors bsky.rs test `bsky_partial_url`: the regex needs at least one
// character after the domain slash.
func TestBskyPartialURL(t *testing.T) {
	if got := rewrite("https://bsky.app/"); got != "" {
		t.Errorf("rewrite(\"https://bsky.app/\") = %q, want empty", got)
	}
}

// Mirrors bsky.rs test `bsky_with_query_params`.
func TestBskyWithQueryParams(t *testing.T) {
	got := rewrite("https://bsky.app/profile/user?ref=share")
	want := "https://bsyy.app/profile/user?ref=share"
	if got != want {
		t.Errorf("rewrite = %q, want %q", got, want)
	}
}
