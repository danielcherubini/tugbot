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
		// Non-decomposable unicode lookalikes: NFD never folds these, so the
		// confusable map + Cf (format-character) stripping carry them to ASCII.
		{"zwиft", "zwift"},                          // and (U+0438, small i)
		{"ZWИFТ", "zwift"},                          // капитализация: И (И) + Т (Т)
		{"sоt", "sot"},                              // о (small O)
		{"z​wi​ft", "zwift"},                        // zero-width space (Cf) stripped
		{"z⁠wi⁠ft", "zwift"},                        // word joiner (Cf) stripped
		{"z‍wi‍ft", "zwift"},                        // ZWNJ (Cf) stripped
		{"\uFF5A\uFF57\uFF49\uFF46\uFF54", "zwift"}, // fullwidth lowercase
		{"\uFF3A\uFF37\uFF29\uFF26\uFF34", "zwift"}, // fullwidth uppercase
		{"sw٣ft", "sw3ft"},                          // Arabic-Indic ٣
		{"sw٢ft", "sw2ft"},                          // Arabic-Indic ٢
		{"swßft", "swssft"},                         // ß (not decomposable) -> ss
		{"\u0190wift", "ewift"},                     // e-with-hook (U+0190, ToLower -> ɛ) -> e
		{"\u0259wift", "ewift"},                     // schwa (U+0259) -> e
		{"swıft", "swift"},                          // dotless i (U+0131)
		{"\u0142owift", "lowift"},                   // ł (U+0142) -> l
		{"\u03B5wift", "ewift"},                     // greek epsilon
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
