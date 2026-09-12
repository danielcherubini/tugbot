// The nickname flow (GUILD_MEMBER_UPDATE events, gated like the message
// flow). discordgo v0.29.0's MemberUpdate fires for ANY member change
// (role, mute, nick, ...) and its Member struct has neither an OldNick
// nor an ActorID field — so "did the nick change?" and "did the bot just
// write this?" cannot be answered from the event payload. Both run on the
// in-process last-nick cache: an event whose Nick equals the cache is
// skipped (no nick change — including the bot's own reset echo, the cache
// being written BEFORE the echo arrives), and a successful reset writes
// the reset value ("Derpies") into the cache before the echo arrives.
// No recursion, no wasted RPC for echoes.
package derpies

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	core "github.com/danielcherubini/tugbot/internal/handlers/gulag"
	"github.com/danielcherubini/tugbot/internal/wordmatch"
)

// derpiesNickReset — the fixed value the reset SETS (not clears) the
// guild nick to. This filter gates exactly one user; the bot has no
// API to modify another user's GLOBAL name, so the neutral value
// masks it (a null clear would expose it).
const derpiesNickReset = "Derpies"

// nicknameResetCooldown — the per-member 60s window: at most ONE reset
// attempt (success OR reset-failure mark the window — 429 discipline: a
// failed attempt is never retried inside the window). A const, not
// config (YAGNI).
const nicknameResetCooldown = 60 * time.Second

// nickKey — the state-map key.
func nickKey(guildID, memberID string) string {
	return guildID + "|" + memberID
}

// ---------------------------------------------------------------------------
// The flow
// ---------------------------------------------------------------------------

// MemberUpdate starts the goroutine, in the same shape as
// MessageCreate (the flow can block on the pi RPC).
func (h *Derpies) MemberUpdate(evt *discordgo.GuildMemberUpdate) { go h.nickFlow(evt) }

func (h *Derpies) nickFlow(evt *discordgo.GuildMemberUpdate) {
	ctx := context.Background()

	// 1. Feature gate (silent flavour, same as the message flow).
	if !h.store.featureEnabled(ctx, FeatureKey) {
		return
	}

	// 2. Payload guard (the main.go closure already nil-guards the
	//    payload; this makes the flow call-safe in its own right).
	if evt == nil || evt.Member == nil || evt.Member.User == nil {
		return
	}

	// 3. Guild guard.
	if evt.GuildID == "" {
		return
	}

	// 4. Author-ID gate (checked conversion, house discipline — same as
	//    step 3 of the message flow).
	uid, err := core.DiscordID("user", evt.User.ID)
	if err != nil {
		return
	}
	if _, ok := h.app.Cfg.DerpiesUserIDs[uid]; !ok {
		return
	}

	slog.Info("derpies nickname event", "module", module, "user", evt.User.ID, "guild", evt.GuildID, "nick", evt.Nick)

	key := nickKey(evt.GuildID, evt.User.ID)

	// 5. Empty-nick arm: the display is already the global name
	//    (nick removed/gone) — record and return, no list fetch.
	if evt.Nick == "" {
		h.saveNick(key, "")
		return
	}

	// Serialization + cooldown + change detection under one lock.
	h.nickMu.Lock()
	if h.busy[key] {
		h.nickMu.Unlock()
		slog.Info("derpies nickname busy — skipping", "module", module, "guild", evt.GuildID, "member", evt.User.ID)
		return
	}
	if last, ok := h.lastEdit[key]; ok && h.clock().Sub(last) < nicknameResetCooldown {
		h.nickMu.Unlock()
		slog.Info("derpies nickname cooldown — skipping", "module", module, "guild", evt.GuildID, "member", evt.User.ID)
		return
	}
	if cur, ok := h.lastNick[key]; ok && cur == evt.Nick {
		h.nickMu.Unlock()
		return
	}
	h.busy[key] = true
	h.nickMu.Unlock()
	defer func() {
		h.nickMu.Lock()
		h.busy[key] = false
		h.nickMu.Unlock()
	}()

	// 6. Fast path: one list SELECT, exact token match (same shape as
	//    step 4 of the message flow).
	list, err := h.store.listGimmicks(ctx)
	if err != nil {
		slog.Error("derpies nickname list fetch failed", "module", module, "error", err)
		return
	}
	toks := tokensForMatch(evt.Nick)
	for tok := range toks {
		if list[tok] {
			h.resetNow(key, evt, "fast", tok)
			return
		}
	}

	// 7. Slow path: pi unavailable -> silently return (same degradation;
	//    nicknames have no image, so no AskWithImages here).
	if h.app.Pi == nil {
		slog.Info("derpies pi RPC not available, skipping (nickname)", "module", module)
		return
	}

	// 8. One ask. Template fetching is with fallback, like the message
	//    flow (with an "(nickname)" suffix on the log line).
	tmpl, err := h.store.promptText(ctx)
	if err != nil || !validTemplate(tmpl) {
		tmpl = defaultPromptTemplate
		if err != nil {
			slog.Warn("derpies prompt template unavailable — using default (nickname)", "module", module, "error", err)
		}
	}
	content := "New nickname set by the user: " + evt.Nick
	// The nickname prompt has no embeds: embedTitles "" (nImages 0).
	prompt := gimmickPrompt(tmpl, content, sortedKeys(list), 0, "", "")
	text, askErr := h.app.Pi.Ask(ctx, prompt)
	if askErr != nil {
		slog.Error("derpies nickname ask failed", "module", module, "error", askErr)
		return
	}

	// 9. Parse the verdict (the cache write is making decisions explicit:
	//    terminal / same-nick arms cache the nick so subsequent role-only
	//    events do not re-judge it; the reset path caches the reset value
	//    — and its failure arm caches "" — via resetNow).
	kind, word := parseVerdict(text)
	switch kind {
	case "clean":
		slog.Info("derpies nickname verdict clean", "module", module, "guild", evt.GuildID, "member", evt.User.ID, "nick", evt.Nick)
		h.saveNick(key, evt.Nick)
		return
	case "unknown":
		slog.Warn("derpies nickname unrecognized verdict — doing nothing", "module", module, "verdict", strings.TrimSpace(text))
		h.saveNick(key, evt.Nick)
		return
	}

	// 10. Learn — the two-arm gate is UNCHANGED and now gates LEARNING
	//     ONLY (a recognized gimmick name is reset even when the word
	//     isn't learnable; what may enter the table is not loosened).
	fw := wordmatch.FoldToASCII(word)
	if wordmatch.WordValid(fw) && toks[fw] {
		if err := h.store.addGimmick(ctx, fw, SourceLLM); err != nil {
			slog.Error("derpies nickname add gimmick failed", "module", module, "word", fw, "error", err)
		}
	} else {
		slog.Warn("derpies nickname verdict word not learnable — name reset only", "module", module, "word", word, "nick", evt.Nick)
	}

	// 11. Reset (ALWAYS, on a GIMMICK verdict) — the name action no
	//     longer depends on the learning gate.
	h.resetNow(key, evt, "llm", fw)
}

// resetNow — the single action arm: attempt the reset (a SET of the
// fixed neutral value — see derpiesNickReset), mark the window
// (marking it on FAILURE too — 429 discipline), and — only on success —
// write the reset value into the cache before the gateway's echo
// arrives (the echo event then arrives with Nick equal to the cache and
// is skipped by the cur == evt.Nick change-detection: no self-rejudge,
// no recursion — the echo carries Nick != "", so this skip happens
// via the change-detection check, NOT the empty-nick arm). The
// success-path saveNick(key, derpiesNickReset) establishes the
// spec-prescribed cache state; the FAILURE-arm saveNick(key, "")
// normalizes the cache to explicit "" so the post-window same-nick
// retry stays working (the nick is unchanged on failure; today an absent
// entry is treated identically by the ok && check, but this makes the
// state explicit and self-describing — and spec-proof against any
// future arm that could leave the nick cached before a reset). The
// plan-specified leading ctx parameter was dropped: the body and its
// seams take no ctx here.
func (h *Derpies) resetNow(key string, evt *discordgo.GuildMemberUpdate, path, word string) {
	if err := h.ops.setNickname(evt.GuildID, evt.User.ID, derpiesNickReset); err != nil {
		slog.Error("derpies nickname reset ("+path+") failed", "module", module, "word", word, "guild", evt.GuildID, "member", evt.User.ID, "from", evt.Nick, "error", err)
		// Failed attempt: markEdit bounds the window at exactly the point
		// the retry is declared over (no retry INSIDE it — 429 discipline),
		// and saveNick(key, "") normalizes the cache to explicit "" so the
		// post-window same-nick retry stays working (today an absent entry
		// is treated identically by the ok && check, but this makes the
		// state explicit and self-describing — and spec-proof against any
		// future arm that could leave the nick cached before a reset).
		h.markEdit(key)
		h.saveNick(key, "")
		return
	}
	h.markEdit(key)
	h.saveNick(key, derpiesNickReset)
	slog.Info("derpies nickname reset ("+path+")", "module", module, "word", word, "guild", evt.GuildID, "member", evt.User.ID, "from", evt.Nick)
}

// ---------------------------------------------------------------------------
// State helpers (each locks nickMu, sets one map, unlocks)
// ---------------------------------------------------------------------------

// saveNick sets the per-member last-known nickname.
func (h *Derpies) saveNick(key, value string) {
	h.nickMu.Lock()
	h.lastNick[key] = value
	h.nickMu.Unlock()
}

// markEdit records the last reset attempt (success OR reset-failure mark
// the window).
func (h *Derpies) markEdit(key string) {
	h.nickMu.Lock()
	h.lastEdit[key] = h.clock()
	h.nickMu.Unlock()
}
