package twitter

import "testing"

// Port of fx_rewriter's regex `https://(twitter.com|x.com)/.+/status/\d+` with the
// first capture group replaced by "girlcockx.com" (src/handlers/twitter.rs:30-42).
// Note (parity): the returned value is ONLY the matched URL substring with the
// domain rewritten — Rust's `channel.say` posts that substring, not the whole
// message.

func TestTwitterRewrite(t *testing.T) {
	got := rewrite("https://twitter.com/davidbcooper/status/1684840110259404802")
	want := "https://girlcockx.com/davidbcooper/status/1684840110259404802"
	if got != want {
		t.Errorf("rewrite(twitter.com status) = %q, want %q", got, want)
	}
}

func TestTwitterRewriteX(t *testing.T) {
	got := rewrite("https://x.com/davidbcooper/status/1684840110259404802")
	want := "https://girlcockx.com/davidbcooper/status/1684840110259404802"
	if got != want {
		t.Errorf("rewrite(x.com status) = %q, want %q", got, want)
	}
}

func TestTwitterRewriteWithinContent(t *testing.T) {
	// Rust's rewriter returns the matched substring, so surrounding text is
	// dropped from the posted message — parity with twitter.rs fx_rewriter.
	got := rewrite("check https://twitter.com/u/status/123 out")
	want := "https://girlcockx.com/u/status/123"
	if got != want {
		t.Errorf("rewrite(embedded) = %q, want %q", got, want)
	}
}

func TestTwitterNoMatch(t *testing.T) {
	cases := []string{
		"https://bluesky.app/profile/x",
		"https://duck.duck",
		"https://x.com/no-status-path",
		"https://x.com/u/status/abc", // status id must be digits
		"",
	}
	for _, c := range cases {
		if got := rewrite(c); got != "" {
			t.Errorf("rewrite(%q) = %q, want empty", c, got)
		}
	}
}

// Port of Rust's test expectations pinned in twitter.rs #[cfg(test)].
func TestTwitterBskyDoesNotMatch(t *testing.T) {
	if got := rewrite("https://bsky.app/profile/user/post/abc"); got != "" {
		t.Errorf("rewrite(bsky url) = %q, want empty", got)
	}
}
