// The embed-title leg (a pure helper, no new seam): the observed
// survival vector is a Klipy gifv embed — the message text is a bare
// URL and the brand word lives in the embed TITLE. embedTitleText
// joins the non-empty MessageEmbed.Title of a message so the titles
// join the judged text (fast-path token union + {{EMBED}} prompt
// block), the same way referenced-message content already does.

package derpies

import (
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	// maxEmbedTitles — Discord's hard per-message embed cap; every
	// possible title is judged, no more.
	maxEmbedTitles = 10
	// maxTitleRunes — Discord titles are ~256 chars max; the cap is
	// budget hygiene, not semantics (RUNES — titles may be multibyte).
	maxTitleRunes = 200
)

// embedTitleText joins the non-empty MessageEmbed.Title of a message,
// one per line, in embed order. Capped at maxEmbedTitles titles, each
// trimmed to maxTitleRunes RUNES ([]rune slicing — a UTF-8 cut
// mid-codepoint must not produce invalid bytes into the prompt).
// Returns "" for a nil message or a message with no entitled embeds.
// e.Description / e.Provider / e.Footer are deliberately NOT included —
// the title is the observed vector; the payload stays minimal and
// byte-stable for the prompt tests.
func embedTitleText(m *discordgo.Message) string {
	if m == nil {
		return ""
	}
	var lines []string
	for _, e := range m.Embeds {
		if e == nil || e.Title == "" {
			continue
		}
		if len(lines) == maxEmbedTitles {
			break
		}
		title := e.Title
		if r := []rune(title); len(r) > maxTitleRunes {
			title = string(r[:maxTitleRunes])
		}
		lines = append(lines, title)
	}
	return strings.Join(lines, "\n")
}
