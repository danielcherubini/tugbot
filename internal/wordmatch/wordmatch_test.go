// Tests for the wordmatch package, written against its public API
// (FoldToASCII / WordValid) with wordmatch-qualified names.
package wordmatch_test

import (
	"strings"
	"testing"

	"github.com/danielcherubini/tugbot/internal/wordmatch"
)

// ---------------------------------------------------------------------------
// wordmatch.FoldToASCII
// ---------------------------------------------------------------------------

func TestFoldToASCII(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"świft", "swift"},
		{"žwift", "zwift"},
		{"źwift", "zwift"},
		{"SWIFT", "swift"}, // lowercasing included
		{"swift", "swift"}, // idempotent ASCII
		{"zzz", "zzz"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := wordmatch.FoldToASCII(tt.in); got != tt.want {
			t.Errorf("wordmatch.FoldToASCII(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// wordmatch.WordValid
// ---------------------------------------------------------------------------

func TestWordValid(t *testing.T) {
	tests := []struct {
		w    string
		want bool
	}{
		{"sw1ft", true},
		{"zswiftf", true},
		{strings.Repeat("a", 32), true},
		{strings.Repeat("a", 33), false},
		{"a", false},
		{"s-w1ft", false},
		{"swïft", false},
		{"sw1ft!", false},
	}
	for _, tt := range tests {
		if got := wordmatch.WordValid(tt.w); got != tt.want {
			t.Errorf("wordmatch.WordValid(%q) = %v, want %v", tt.w, got, tt.want)
		}
	}
}
