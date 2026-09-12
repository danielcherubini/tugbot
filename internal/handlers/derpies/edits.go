// The edit flow (GUILD_MESSAGE_UPDATE events, gated like the message
// flow). The message flow (derpies.go) runs on MessageCreate only — so
// an edit of an already-posted message is a second look-free window:
// the LLM judged the ORIGINAL text, and the author can now rewrite that
// same message to carry a gimmick with no create-time check re-running
// (only gokupoll watches the update event today). Every update by a
// gated author re-runs the existing create flow on the updated content —
// fast path, images, one-hop reference, one slow-path ask, learning, and
// deletion all carry over unchanged (an edit is origin-agnostic: at most
// one list SELECT + one pi ask, same as a create).
// BARE update payloads (the mod.rs:126-136 quirk — a payload with no
// usable content) are fetched by channel+id through the
// channelMessageRetrieve seam; a fetch failure degrades (log + skip),
// never aborts. There is no echo-recursion concern — the bot never
// edits messages — and no message dedup cache exists, so simply
// re-running the flow on the updated message is correct.
package derpies

import (
	"context"
	"log/slog"

	"github.com/bwmarrin/discordgo"

	core "github.com/danielcherubini/tugbot/internal/handlers/gulag"
)

// MessageUpdate re-judges an edit by a gated author: the full create
// flow (fast path, images, one-hop reference, one slow
// ask, learn, delete) runs on the UPDATED content. The event thread is
// never held (same shape as MessageCreate).
func (h *Derpies) MessageUpdate(evt *discordgo.MessageUpdate) { go h.editFlow(evt) }

func (h *Derpies) editFlow(evt *discordgo.MessageUpdate) {
	// 0. Payload guard (the event carries *Message — nil guards are the
	//    house discipline; Author must exist for the gate below).
	if evt == nil || evt.Message == nil || evt.Author == nil {
		return
	}
	ctx := context.Background()
	// 1. Feature gate (silent flavour, first gate).
	if !h.store.featureEnabled(ctx, FeatureKey) {
		return
	}
	// 2. Guild guard.
	if evt.GuildID == "" {
		return
	}
	// 3. Author-ID gate (checked conversion — the house discipline).
	uid, err := core.DiscordID("user", evt.Author.ID)
	if err != nil {
		return
	}
	if _, ok := h.app.Cfg.DerpiesUserIDs[uid]; !ok {
		return
	}
	slog.Info("derpies edit from filtered user", "module", module, "user", evt.Author.ID, "message", evt.ID)
	// 4. Bare-payload guard — the mod.rs:126-136 fetch branch (the
	//    gokupoll port, with a STRICT trigger deviation — gokupoll
	//    fetches on empty content ALONE; here also require NO
	//    attachments AND NO embeds, so an image-capable payload — the
	//    flow's imageURLPlan reads attachments + embed image/thumbnail
	//    urls — is judged in place): a payload with no text and no
	//    image-bearing fields is fetched by channel+id through the seam.
	//    Fetch failure degrades: log + skip, never abort.
	m := evt.Message
	if m.Content == "" && len(m.Attachments) == 0 && len(m.Embeds) == 0 {
		fetched, err := h.ops.channelMessageRetrieve(m.ChannelID, m.ID)
		if err != nil {
			slog.Error("derpies edit fetch failed", "module", module, "message", m.ID, "error", err)
			return
		}
		// The fetched message replaces m and re-runs flow's gates — the
		// step-0 payload discipline applies to it too: discordgo
		// documents Message.Author as not guaranteed while flow step 3
		// dereferences m.Author.ID with no nil check, and a missing
		// GuildID would silently no-op flow's guild gate. Guard both —
		// log + skip, never abort, never panic.
		if fetched == nil || fetched.Author == nil {
			slog.Error("derpies edit fetched message has no author", "module", module, "message", m.ID)
			return
		}
		if fetched.GuildID == "" {
			slog.Error("derpies edit fetched message has no guild", "module", module, "message", m.ID)
			return
		}
		m = fetched
	}
	// 5. The full create flow (origin-agnostic; an edit costs at most one
	//    list SELECT + one pi ask). The flow's own gates re-run on m —
	//    idempotent and cheap; m already passed the event-level gates.
	h.flow(m)
}
