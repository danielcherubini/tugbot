// Package wordmatch is the shared word contract for the derpies_gimmicks
// stored-word space: a message token is matched by folding it
// (FoldToASCII) to the lowercased, combining-mark-free ASCII form, and a
// stored word is valid (WordValid) only if it is a pure
// /^[a-z0-9]{2,32}$/ token. FoldToASCII performs NO punctuation trim —
// that trim happens at match time only (tokensForMatch), never on stored
// words; a trailing punctuation (e.g. "Swift." folding to "swift.") must
// be REJECTED by WordValid, not trimmed to "swift".
package wordmatch

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// FoldToASCII lowers and reduces a token to ASCII: NFD decompose, drop
// combining marks (Mn). "świft" -> "swift", "žwift" -> "zwift". Letters
// that don't decompose to a base (e.g. Cyrillic lookalikes) stay — they
// remain wordValid-ineligible and are only catchable via the LLM naming an
// ASCII form.
func FoldToASCII(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// WordValid: ^[a-z0-9]{2,32}$ — token charset only (no punctuation, no
// unicode), 2..32 chars. Precompiled regexp.
var wordRe = regexp.MustCompile(`^[a-z0-9]{2,32}$`)

func WordValid(w string) bool {
	return wordRe.MatchString(w)
}
