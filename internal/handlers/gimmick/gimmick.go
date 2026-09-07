// Package gimmick is the /gimmick slash command: add|list|delete on
// the derpies_gimmicks word list.
//
// Ungated by design (docs/decisions/0003-gimmick-command-ungated.md,
// reviewer-verified): there is NO feature-flag check — the list is
// pre-seedable while the derpies feature is off. The instant role
// gate (Highly Regarded or admin) covers the write cost, and the
// log line with the invoker is the trace.
//
// Word contract: entries are normalized with wordmatch.FoldToASCII
// (NO punctuation trim — "Swift." folds to "swift.", which is
// INVALID) and validated with wordmatch.WordValid
// (^[a-z0-9]{2,32}$). Writes insert with source 'manual' (distinct
// from 'seed' = migration-seeded and 'llm' = runtime-learnt) via
// idempotent ON CONFLICT (word) DO NOTHING — an existing row's
// source is never modified. Deletion is an exact WHERE word = $1 on
// the folded word; there is no cascade (no FK on the table) and it
// takes effect on the next message (the derpies filter re-reads the
// list per event). All responses are ephemeral.
package gimmick

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/handlers/cull"
	"github.com/danielcherubini/tugbot/internal/handlers/gulag"
	"github.com/danielcherubini/tugbot/internal/wordmatch"
)

// Module — the slog module tag for this package's log lines.
const Module = "gimmick"

// SourceManual — the source column value the command's writes carry
// (varchar(8) fits; distinct from 'seed' and 'llm').
const SourceManual = "manual"

// gimmickRow is the command's view of a derpies_gimmicks row
// (created_at is not part of the command surface).
type gimmickRow struct {
	Word   string
	Source string
}

// Response — the module's share of the handler response shape.
// Contract: when Chunks is non-empty, Content == Chunks[0] (the
// [cmd/tugbot/main.go] delivery set carries the full message set;
// content mirrors chunk 0 for the fast path).
type Response struct {
	Content   string
	Ephemeral bool
	Chunks    []string
}

// store is the command's SQL seam (derpies pattern).
type store interface {
	listGimmicks(ctx context.Context) ([]gimmickRow, error)
	addGimmick(ctx context.Context, word, source string) (int64, error)
	deleteGimmick(ctx context.Context, word string) (int64, error)
}

// gateOps is the member / role verification seam.
type gateOps interface {
	guildMember(guildID, userID string) (*discordgo.Member, error)
	memberHasAnyRole(ctx context.Context, guildID string, m *discordgo.Member) bool
}

// realGateOps is the production gate (discord REST + the canonical
// gulag core).
type realGateOps struct {
	session *discordgo.Session
	g       *gulag.Gulag
}

func (r *realGateOps) guildMember(guildID, userID string) (*discordgo.Member, error) {
	return r.session.GuildMember(guildID, userID)
}

// memberHasAnyRole delegates to the core scan; the roles come from
// cull.WhitelistRoles() (the single source of truth — the two gates
// cannot drift).
func (r *realGateOps) memberHasAnyRole(ctx context.Context, guildID string, m *discordgo.Member) bool {
	return r.g.MemberHasAnyRole(ctx, guildID, m, cull.WhitelistRoles()...)
}

// Gimmick handles the /gimmick add|list|delete command.
type Gimmick struct {
	app   *app.App
	store store
	gate  gateOps
}

// New builds the handler (the cull mirror: its own gulag.New(a)
// member — never re-declares any of the core's helpers).
func New(a *app.App) *Gimmick {
	return &Gimmick{
		app:   a,
		store: &poolStore{pool: a.Pool},
		gate:  &realGateOps{session: a.D, g: gulag.New(a)},
	}
}

// poolStore is the production store (raw SQL over the shared pool;
// the three pinned statements, mirroring derpies.poolStore).
type poolStore struct{ pool *pgxpool.Pool }

func (p *poolStore) listGimmicks(ctx context.Context) ([]gimmickRow, error) {
	rows, err := p.pool.Query(ctx, `SELECT word, source FROM derpies_gimmicks ORDER BY word`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []gimmickRow
	for rows.Next() {
		var r gimmickRow
		if err := rows.Scan(&r.Word, &r.Source); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *poolStore) addGimmick(ctx context.Context, word, source string) (int64, error) {
	res, err := p.pool.Exec(ctx,
		`INSERT INTO derpies_gimmicks (word, source) VALUES ($1, $2) ON CONFLICT (word) DO NOTHING`,
		word, source)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

func (p *poolStore) deleteGimmick(ctx context.Context, word string) (int64, error) {
	res, err := p.pool.Exec(ctx, `DELETE FROM derpies_gimmicks WHERE word = $1`, word)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// ephemeral is the every-arm response helper (all gimmick responses
// are ephemeral).
func ephemeral(content string) Response {
	return Response{Content: content, Ephemeral: true}
}

// SetupCommand — the registered command shape (the pin test
// TestSetupCommandShape pins it field-by-field).
func (h *Gimmick) SetupCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "gimmick",
		Description: "Manage the gimmick word list (derpies filter)",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "add",
				Description: "Add a gimmick word",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionString,
						Name:        "word",
						Description: "The respelling to add (e.g. sw1ft)",
						Required:    true,
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "list",
				Description: "List all gimmick words",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "delete",
				Description: "Delete a gimmick word",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionString,
						Name:        "word",
						Description: "The word to delete",
						Required:    true,
					},
				},
			},
		},
	}
}

// HandleInteraction — the gate order (all returns ephemeral):
// guild guard → role gate (no feature-flag check, ADR 0003) →
// subcommand dispatch.
func (h *Gimmick) HandleInteraction(i *discordgo.Interaction) Response {
	ctx := context.Background()

	// 1. Guild guard.
	if i.GuildID == "" {
		return ephemeral("This command can only be used in a guild")
	}

	// 2. Instant role gate (Highly Regarded or admin).
	invokerID := ""
	if i.Member != nil && i.Member.User != nil {
		invokerID = i.Member.User.ID
	} else if i.User != nil {
		invokerID = i.User.ID
	}
	member, err := h.gate.guildMember(i.GuildID, invokerID)
	if err != nil {
		return ephemeral("Error: Could not verify your permissions")
	}
	if !h.gate.memberHasAnyRole(ctx, i.GuildID, member) {
		return ephemeral("Error: You need Highly Regarded or admin role to use this command")
	}

	// 3. Subcommand (missing option / non-option → Unknown).
	data, ok := i.Data.(discordgo.ApplicationCommandInteractionData)
	if !ok || len(data.Options) == 0 || data.Options[0] == nil {
		return ephemeral("Unknown subcommand")
	}
	switch data.Options[0].Name {
	case "add":
		return h.add(ctx, i, data.Options[0], invokerID)
	case "list":
		return h.list(ctx)
	case "delete":
		return h.delete(ctx, i, data.Options[0], invokerID)
	}
	return ephemeral("Unknown subcommand")
}

// resolveWord is the shared option/normalization arm (add and delete
// arm identically): the word option (missing → the error text),
// FoldToASCII (NO punctuation trim), WordValid.
func resolveWord(sub *discordgo.ApplicationCommandInteractionDataOption) (string, *Response) {
	var o *discordgo.ApplicationCommandInteractionDataOption
	for _, opt := range sub.Options {
		if opt != nil && opt.Name == "word" {
			o = opt
		}
	}
	if o == nil {
		fail := ephemeral("Missing required option: word")
		return "", &fail
	}
	raw, _ := o.Value.(string)
	w := wordmatch.FoldToASCII(raw)
	if !wordmatch.WordValid(w) {
		fail := ephemeral(`Invalid word "` + w + `": 2-32 lowercase letters or digits only`)
		return "", &fail
	}
	return w, nil
}

// add — normalize, validate, the idempotent insert (source manual;
// the existing row's source is never modified).
func (h *Gimmick) add(ctx context.Context, i *discordgo.Interaction, sub *discordgo.ApplicationCommandInteractionDataOption, invokerID string) Response {
	w, fail := resolveWord(sub)
	if fail != nil {
		return *fail
	}
	n, err := h.store.addGimmick(ctx, w, SourceManual)
	if err != nil {
		slog.Error("gimmick add failed", "module", Module, "word", w, "user", invokerID, "guild", i.GuildID, "error", err)
		return ephemeral("Failed to add " + w)
	}
	if n == 1 {
		slog.Info("gimmick add", "module", Module, "word", w, "source", SourceManual, "user", invokerID, "guild", i.GuildID)
		return ephemeral("Added " + w + " to the gimmick list")
	}
	slog.Info("gimmick add skipped (already exists)", "module", Module, "word", w, "source", SourceManual, "user", invokerID, "guild", i.GuildID)
	return ephemeral(w + " is already in the list")
}

// list — the pre-chunked response (Task 2's delivery set carries
// the chunks).
func (h *Gimmick) list(ctx context.Context) Response {
	rows, err := h.store.listGimmicks(ctx)
	if err != nil {
		slog.Error("gimmick list failed", "module", Module, "error", err)
		return ephemeral("Failed to query gimmick words. Please try again later.")
	}
	if len(rows) == 0 {
		return ephemeral("No gimmick words yet.")
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%s (%s)", r.Word, r.Source))
	}
	chunks := chunkGimmickList(len(rows), lines)
	return Response{Content: chunks[0], Ephemeral: true, Chunks: chunks}
}

// delete — normalize, validate, the exact-word delete.
func (h *Gimmick) delete(ctx context.Context, i *discordgo.Interaction, sub *discordgo.ApplicationCommandInteractionDataOption, invokerID string) Response {
	w, fail := resolveWord(sub)
	if fail != nil {
		return *fail
	}
	n, err := h.store.deleteGimmick(ctx, w)
	if err != nil {
		slog.Error("gimmick delete failed", "module", Module, "word", w, "user", invokerID, "guild", i.GuildID, "error", err)
		return ephemeral("Failed to delete " + w)
	}
	if n == 1 {
		slog.Info("gimmick delete", "module", Module, "word", w, "user", invokerID, "guild", i.GuildID)
		return ephemeral("Deleted " + w + " from the gimmick list")
	}
	return ephemeral(w + " is not in the list")
}

// chunkGimmickList packs whole lines under the 2000-char limit
// (content is pure ASCII, so rune len == byte len). First chunk:
// "Gimmick words (N): " + lines (N = the total line count);
// continuation chunks: "Gimmick words (continued): " + lines. Splits
// at line boundaries only; a fresh chunk always fits (max line ≈ 44
// chars: 32-char word + " (" + 7-char source + ")").
func chunkGimmickList(total int, lines []string) []string {
	var chunks []string
	header := fmt.Sprintf("Gimmick words (%d): ", total)
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			chunks = append(chunks, header)
		} else {
			chunks = append(chunks, header+"\n"+strings.Join(cur, "\n"))
		}
		cur = nil
		header = "Gimmick words (continued): "
	}
	for _, l := range lines {
		next := header + l
		if len(cur) > 0 {
			next = header + "\n" + strings.Join(append(append([]string{}, cur...), l), "\n")
		}
		if len(next) <= 2000 {
			cur = append(cur, l)
		} else {
			flush()
			cur = append(cur, l)
		}
	}
	flush()
	return chunks
}
