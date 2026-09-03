package instagram

import (
	"strings"
	"testing"
)

// Port of fx_rewriter's regex `https://(www\.)?(instagram\.com)/.+` with the
// SECOND capture group ("instagram.com") replaced by "kkinstagram.com" while
// the optional "www." prefix is preserved (src/handlers/instagram.rs:28-40).

// Mirrors instagram.rs test `instagram_rewrite`.
func TestInstagramRewrite(t *testing.T) {
	got := rewrite("https://www.instagram.com/reel/DCkUQSry42v/?igsh=MXNrMDFwbTEzZnFvMg==")
	want := "https://www.kkinstagram.com/reel/DCkUQSry42v/?igsh=MXNrMDFwbTEzZnFvMg=="
	if got != want {
		t.Errorf("rewrite = %q, want %q", got, want)
	}
}

// Mirrors instagram.rs test `instagram_rewrite_without_www`.
func TestInstagramRewriteWithoutWWW(t *testing.T) {
	got := rewrite("https://instagram.com/p/ABC123/")
	want := "https://kkinstagram.com/p/ABC123/"
	if got != want {
		t.Errorf("rewrite = %q, want %q", got, want)
	}
}

// Mirrors instagram.rs test `instagram_no_match`.
func TestInstagramNoMatch(t *testing.T) {
	if got := rewrite("https://twitter.com/someone"); got != "" {
		t.Errorf("rewrite(twitter url) = %q, want empty", got)
	}
}

// Mirrors instagram.rs test `instagram_empty_string`.
func TestInstagramEmptyString(t *testing.T) {
	if got := rewrite(""); got != "" {
		t.Errorf("rewrite(\"\") = %q, want empty", got)
	}
}

// Mirrors instagram.rs test `instagram_post_url`.
func TestInstagramPostURL(t *testing.T) {
	got := rewrite("https://www.instagram.com/p/ABC123/")
	want := "https://www.kkinstagram.com/p/ABC123/"
	if got != want {
		t.Errorf("rewrite = %q, want %q", got, want)
	}
}

// Mirrors instagram.rs test `instagram_story_url`.
func TestInstagramStoryURL(t *testing.T) {
	got := rewrite("https://www.instagram.com/stories/username/123456/")
	if got == "" {
		t.Fatal("rewrite(story url) = empty, want Some")
	}
	if !strings.Contains(got, "kkinstagram.com") {
		t.Errorf("rewrite(story url) = %q, want it to contain \"kkinstagram.com\"", got)
	}
}
