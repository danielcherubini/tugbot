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
// combining marks (Mn) AND format characters (Cf — zero-width space,
// word joiner, ZWNJ: zero-width splices inside a token strip out, so
// "z\u200bwi\u200bft" folds to "zwift"), fold non-decomposable lookalike
// letters to their ASCII form (confusableFold), and lowercase the result.
// "świft" -> "swift", "zw\u0438ft" (и U+0438) -> "zwift",
// "l\u0663t" (Arabic-Indic ٣) -> "l3t", "sw\u00DFft" (ß) -> "sswift".
//
// Letters with NO Latin confusable (the rest of a non-Latin script)
// stay as-is: they remain wordValid-ineligible and are only catchable
// via the LLM naming an ASCII form (the LLM sees them in the prompt
// verbatim).
//
// confusableFold is a CONSERVATIVE, visually unambiguous map, built once
// at init: NFD already decomposes diacritic-Latin (ś->s+Mn, ø->o+Mn)
// and ligatures (ﬁ->f+i); every entry below is a single glyph a
// reasonable reader would read as the ASCII letter (or is an
// Arabic-Indic digit). Keyed by LOWERCASE runes only — FoldToASCII
// lowercases before lookup, so fullwidth uppercase Ｚ arrives as ｚ.
var confusableFold map[rune]string

func init() {
	m := map[rune]string{}
	// The map is keyed by LOWERCASE runes only (FoldToASCII lowercases
	// before lookup), so only lowercase keys + digit entries appear here.
	add := func(r rune, s string) { m[r] = s }
	// Cyrillic lookalikes
	add('\u0430', "a") // а
	add('\u0431', "b") // б
	add('\u0432', "b") // в
	add('\u0435', "e") // е
	add('\u0438', "i") // і (short i)
	add('\u043E', "o") // о
	add('\u0440', "p") // р
	add('\u0442', "t") // т
	add('\u0443', "y") // у
	add('\u0445', "x") // х
	add('\u0455', "s") // ѕ
	add('\u0456', "i") // і
	add('\u04D1', "e") // э
	// Greek lookalikes
	add('\u03B1', "a") // α
	add('\u03B2', "b") // β
	add('\u03B4', "d") // δ
	add('\u03B5', "e") // ε
	add('\u03B9', "i") // ι
	add('\u03BB', "l") // λ
	add('\u03BC', "m") // μ
	add('\u03BD', "v") // ν
	add('\u03BF', "o") // ο
	add('\u03C1', "p") // ρ
	add('\u03C3', "s") // σ
	add('\u03C2', "s") // ς
	add('\u03C4', "t") // τ
	add('\u03C5', "v") // υ
	add('\u03C6', "f") // φ
	add('\u03C7', "x") // χ
	add('\u03C9', "o") // ω
	// assorted Latin lookalikes
	add('\u0131', "i") // ı (dotless i)
	add('\u0142', "l") // ł
	add('\u0259', "e") // ə (schwa)
	add('\u025B', "e") // ɛ (e with hook — ToLower of Ɛ U+0190 lands here)
	m['ß'] = "ss"      // ß (NOT NFD-decomposable)
	// fullwidth: FF21-FF3A (A-Z), FF41-FF5A (a-z) — lowercased away
	// before lookup; FF10-FF19 (0-9).
	for i := 0; i < 26; i++ {
		m[rune(0xFF41+i)] = string(rune('a' + i))
	}
	for i := 0; i < 10; i++ {
		m[rune(0xFF10+i)] = string(rune('0' + i))
		m[rune(0x0660+i)] = string(rune('0' + i)) // Arabic-Indic digits
	}
	confusableFold = m
}

func FoldToASCII(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		lr := unicode.ToLower(r)
		if t, ok := confusableFold[lr]; ok {
			b.WriteString(t)
		} else {
			b.WriteRune(lr)
		}
	}
	return b.String()
}

// WordValid: ^[a-z0-9]{2,32}$ — token charset only (no punctuation, no
// unicode), 2..32 chars. Precompiled regexp.
var wordRe = regexp.MustCompile(`^[a-z0-9]{2,32}$`)

func WordValid(w string) bool {
	return wordRe.MatchString(w)
}
