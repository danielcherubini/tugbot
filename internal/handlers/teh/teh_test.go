package teh

import (
	"strings"
	"testing"
)

// Port of Rust: `Features::is_enabled(&pool, "teh") && msg.content.to_lowercase().contains("teh")`
// (src/handlers/teh.rs:12). Trigger = lowercase substring "teh".
func TestTehTrigger(t *testing.T) {
	cases := []struct {
		content string
		want    bool
	}{
		{"teh", true},
		{"TEH", true},
		{"trace teh classic", true},
		{"the quick brown fox", false},
		{"test", false},
		{"", false},
	}
	for _, c := range cases {
		if got := containsTeh(c.content); got != c.want {
			t.Errorf("containsTeh(%q) = %v, want %v", c.content, got, c.want)
		}
	}
}

// Port of the three sequential reactions in teh.rs:14-25: 🇹, then 🇪, then 🇭.
func TestTehReactionEmojis(t *testing.T) {
	got := reactionEmojis()
	want := []string{"🇹", "🇪", "🇭"}
	if len(got) != len(want) {
		t.Fatalf("reactionEmojis() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reactionEmojis()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Rust logs a distinct message per emoji (teh.rs:15, 18, 21).
func TestTehReactionErrorMessages(t *testing.T) {
	got := reactionErrorMessages()
	want := []string{"Error reacting with emoji T", "Error reacting with emoji E", "Error reacting with emoji H"}
	if len(got) != len(want) {
		t.Fatalf("reactionErrorMessages() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reactionErrorMessages()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The feature flag key is "teh", byte-identical to Rust.
func TestTehFeatureKey(t *testing.T) {
	if strings.TrimSpace(FeatureKey) != "teh" {
		t.Errorf("FeatureKey = %q, want \"teh\"", FeatureKey)
	}
}
