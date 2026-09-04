// Command tugbot is the Go port of the Rust bot's main.rs +
// src/handlers/mod.rs event surface (the single source for the event
// dispatch) and src/tugbot/servers.rs (the Ready three-way).
//
// Startup (mirrors main.rs, in order):
//  1. config.LoadConfig (godotenv first — Rust's dotenv().ok()),
//  2. db.NewPool (fatal on failure — Rust's expect panic → slog.Error +
//     non-zero exit),
//  3. pirpc.Start (NOT fatal on failure: "Failed to start pi RPC
//     subprocess — mention feature will not work"; App.Pi stays nil and
//     the mention feature degrades),
//  4. discordgo.New with EXACTLY the six privileged() intents from
//     config.rs: GUILD_MEMBERS, MESSAGE_CONTENT, GUILD_MESSAGES,
//     GUILD_MESSAGE_REACTIONS, GUILD_MESSAGE_POLLS, GUILD_PRESENCES,
//  5. the handler wiring below,
//  6. the two gulag background loops under an errgroup on a shared ctx,
//  7. d.Open() (the gateway; the bot lives here until SIGTERM).
//
// Shutdown (SIGTERM/SIGINT): cancel the shared ctx (drain the background
// loops) → Pi.Stop() (if non-nil) → pool close → gateway close → exit 0.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/config"
	"github.com/danielcherubini/tugbot/internal/db"
	"github.com/danielcherubini/tugbot/internal/handlers/aislop"
	"github.com/danielcherubini/tugbot/internal/handlers/bsky"
	"github.com/danielcherubini/tugbot/internal/handlers/cull"
	"github.com/danielcherubini/tugbot/internal/handlers/feat"
	"github.com/danielcherubini/tugbot/internal/handlers/gokupoll"
	"github.com/danielcherubini/tugbot/internal/handlers/gulag"
	"github.com/danielcherubini/tugbot/internal/handlers/instagram"
	"github.com/danielcherubini/tugbot/internal/handlers/mention"
	"github.com/danielcherubini/tugbot/internal/handlers/prefixhandler"
	"github.com/danielcherubini/tugbot/internal/handlers/teh"
	"github.com/danielcherubini/tugbot/internal/handlers/twitter"
	"github.com/danielcherubini/tugbot/internal/pirpc"
)

// selftestDatabaseURL is the explicit compose-PG URL --selftest always
// uses (never a production .env value): the local docker-compose
// "postgres" service (the Makefile db-up target). docker-compose.yml
// pins those credentials (postgres:postgres, database "tugbot"), so
// the URL is valid with no env setup. The TUGBOT_TEST_SELFTEST_DATABASE_URL
// override is the integration-test seam (black-holing the compose-pg
// host to exercise the 30s pool deadline — see TestSelftestBoundedPoolStart).
func selftestURL() string {
	if u := os.Getenv("TUGBOT_TEST_SELFTEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://postgres:postgres@localhost:5432/tugbot?timezone=UTC"
}

// interactionAckBudget is the ACK-deadline fallback threshold: Discord
// requires an acknowledgement (reply or defer) within 3s of the event.
// Work that outlives this budget (2s, leaving 1s for the defer call
// itself) first ACKs via a matching defer, then delivers the result as
// a follow-up — the standard "bot is thinking…" pattern. Fast
// commands take the plain-reply path unchanged.
var interactionAckBudget = 2 * time.Second

// botIntents pins exactly the six privileged() intents from config.rs —
// NOT discordgo's default headless intents (which are missing
// GUILD_MEMBERS / GUILD_PRESENCES) and NOT GatewayIntents(1) (which is
// missing GUILD_MESSAGE_POLLS).
func botIntents() discordgo.Intent {
	return discordgo.IntentGuildMembers |
		discordgo.IntentMessageContent |
		discordgo.IntentGuildMessages |
		discordgo.IntentGuildMessageReactions |
		discordgo.IntentGuildMessagePolls |
		discordgo.IntentGuildPresences
}

// discordgoToken adapts the .env DISCORD_TOKEN to discordgo's contract.
// The Rust bot stores the RAW token (discord-rs builds
// "Authorization: Bot <token>" itself); discordgo instead uses the
// session token VERBATIM in the Authorization header, so it expects the
// "Bot " prefix already present. Both bots share the same .env
// convention (cutover runbook: one app, one server), so the raw token
// is prefixed here instead of editing .env (a prefixed .env would
// break the Rust rollback binary). A token that already carries the
// prefix passes through unchanged.
func discordgoToken(token string) string {
	if !strings.HasPrefix(token, "Bot ") {
		return "Bot " + token
	}
	return token
}

// handlers is the fully-wired handler set, constructed exactly once per
// process (shared by the production flow and --selftest).
type handlers struct {
	app       *app.App
	teh       *teh.Teh
	twitter   *twitter.Twitter
	bsky      *bsky.Bsky
	instagram *instagram.Instagram
	mention   *mention.Mention
	gokupoll  *gokupoll.GokuPoll
	gulag     *gulag.Gulag
	aiSlop    *aislop.AiSlop
	prefix    *prefixhandler.PrefixHandler
	feat      *feat.Feat
	cull      *cull.Cull

	// Test seams (mirroring the per-handler seam convention; nil = the
	// concrete Discord-session / gulag paths run unchanged in
	// production). They substitute the few Discord REST calls the main
	// wiring makes so the ready three-way and the per-guild command
	// registration can be integration-tested against a real PG without
	// opening a gateway.
	// checkGuildFu substitutes for the verify-arm d.Guild (Rust's
	// ctx.http.get_guild).
	checkGuildFu func(guildID string) (bool, error)
	// deferAckFu substitutes for the defer-ACK + follow-up message
	// pair of the ACK-deadline fallback (see finishInteraction).
	deferAckFu func(ephemeral bool) error
	// guildSetupFu substitutes for the four-shape
	// Gulag.SetupCommand(d) registration.
	guildSetupFu func() []error
	// applyShapeFu substitutes for the five non-gulag shape
	// ApplicationCommandCreate calls.
	applyShapeFu func(guildID, name string) error
}

// newHandlers constructs all eleven handlers (the selftest's "handler
// construction" step and main's wiring).
func newHandlers(a *app.App) *handlers {
	return &handlers{
		app:       a,
		teh:       teh.New(a),
		twitter:   twitter.New(a),
		bsky:      bsky.New(a),
		instagram: instagram.New(a),
		mention:   mention.New(a),
		gokupoll:  gokupoll.New(a),
		gulag:     gulag.New(a),
		aiSlop:    aislop.New(a),
		prefix:    prefixhandler.New(a),
		feat:      feat.New(a),
		cull:      cull.New(a),
	}
}

// serverRow is main's view of a servers-table row (raw-SQL mirror of
// the committed UNEXPORTED sqlc queries — see serversStore).
type serverRow struct {
	ID      int32
	GuildID int64
	GulagID int64
}

// serversStore is main's share of the SQL surface: raw SQL over the App
// pool (the committed-handler pattern — the Task-1 sqlc methods are
// UNEXPORTED (Queries.insert_server / delete_server_by_id /
// select_servers_all — see internal/db), so cmd/tugbot cannot route
// through them without widening the db package API; the raw-SQL mirror
// is the minimal-coupling choice). The statements MIRROR the committed
// .sql canons SEMANTICALLY (same tables/columns/ordering) with two
// deliberate differences: the .sql files use sqlc @-named placeholders
// while this raw-SQL port uses positional $n placeholders, and the
// INSERT canon ends with "RETURNING id;" while this port omits RETURNING
// (bootstrap rows' ids are never read back — the Rust bootstrap arm
// likewise discards the i32 id). The placeholder-style and missing
// RETURNING differences are intentional, not drift.
type serversStore struct {
	pool *pgxpool.Pool
}

// load mirrors the committed select_servers_all .sql (positionally:
// "SELECT id, guild_id, gulag_id FROM servers;" is the canon verbatim).
func (s serversStore) load(ctx context.Context) ([]serverRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, guild_id, gulag_id FROM servers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []serverRow
	for rows.Next() {
		var r serverRow
		if err := rows.Scan(&r.ID, &r.GuildID, &r.GulagID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// createServer mirrors the committed insert_server .sql semantically (the
// bootstrap arm): "INSERT INTO servers (guild_id, gulag_id) VALUES
// (@guild_id, @gulag_id) RETURNING id;" — this port drops the trailing
// "RETURNING id" (never read back) and uses positional placeholders.
func (s serversStore) createServer(ctx context.Context, guildID, roleID int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO servers (guild_id, gulag_id) VALUES ($1, $2)`, guildID, roleID)
	return err
}

// deleteServer mirrors the committed delete_server_by_id .sql (the
// stale-row cleanup arm): "DELETE FROM servers WHERE id = @id;" with a
// positional placeholder.
func (s serversStore) deleteServer(ctx context.Context, id int32) (int, error) {
	res, err := s.pool.Exec(ctx, `DELETE FROM servers WHERE id = $1`, id)
	if err != nil {
		return 0, err
	}
	return int(res.RowsAffected()), nil
}

func main() {
	selftest := flag.Bool("selftest", false,
		"Verify the CI surface and exit WITHOUT opening the Discord gateway: load the config (falling back to dummies for missing DISCORD_TOKEN/APPLICATION_ID), connect the database pool at postgres://postgres:postgres@localhost:5432/tugbot — the compose-PG URL, start its container with `make db-up` (alias: docker compose up -d postgres; docker/compose pins the postgres:postgres credentials on database tugbot) — construct the discordgo session and all eleven handlers")
	flag.Parse()

	if *selftest {
		os.Exit(runSelftest())
	}
	run()
}

// runSelftest: config → pool (explicit compose URL, never production) →
// discordgo construction → handler construction → exit 0. No gateway,
// no pi subprocess, no background loops.
func runSelftest() int {
	// CI-friendliness: the selftest must never depend on production
	// .env values. DATABASE_URL is ALWAYS the explicit compose-PG URL
	// (or the test override, which the production value never uses);
	// missing credential env falls back to dummies (the session is
	// never opened, so the values are never used).
	os.Setenv("DATABASE_URL", selftestURL())
	if os.Getenv("DISCORD_TOKEN") == "" {
		os.Setenv("DISCORD_TOKEN", "selftest-dummy-token")
	}
	if os.Getenv("APPLICATION_ID") == "" {
		os.Setenv("APPLICATION_ID", "1")
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		slog.Error("Failed to load config", "module", "main", "error", err)
		return 1
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))
	slog.Info("selftest: config loaded", "module", "main")

	ctx := context.Background()
	// 30s deadline mirroring r2d2's 30s acquire timeout (pgxpool has no
	// connection_timeout equivalent — see internal/db).
	poolStartCtx, poolStartCancel := context.WithTimeout(ctx, 30*time.Second)
	defer poolStartCancel()
	pool, err := db.NewPool(poolStartCtx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("Failed to create database pool", "module", "main", "error", err)
		return 1
	}
	defer pool.Close()
	slog.Info("selftest: database pool connected", "module", "main")

	d, err := discordgo.New(discordgoToken(cfg.Token))
	if err != nil {
		slog.Error("Failed to create Discord session", "module", "main", "error", err)
		return 1
	}
	d.Identify.Intents = botIntents()

	a := app.NewApp(cfg, pool, d)
	_ = newHandlers(a)
	slog.Info("selftest: Discord session and all eleven handlers constructed", "module", "main")
	return 0
}

// checkGuild is the verify arm's REST call: the injected seam when set,
// else the concrete session's Guild() (Rust: ctx.http.get_guild).
func (h *handlers) checkGuild(gid string) error {
	if h.checkGuildFu != nil {
		_, err := h.checkGuildFu(gid)
		return err
	}
	_, err := h.app.D.Guild(gid)
	return err
}

// guildSetup runs the four-shape gulag registration: the injected seam
// when set, else the concrete Gulag.SetupCommand.
func (h *handlers) guildSetup() []error {
	if h.guildSetupFu != nil {
		return h.guildSetupFu()
	}
	return h.gulag.SetupCommand(h.app.D, h.app.Cfg.ApplicationID)
}

// applyShape runs one non-gulag shape registration: the injected seam
// when set, else the concrete ApplicationCommandCreate with
// CONFIG's APPLICATION_ID (an empty app id would 404 on
// /applications//guilds/<id>/commands).
func (h *handlers) applyShape(gid string, cmd *discordgo.ApplicationCommand) error {
	if h.applyShapeFu != nil {
		return h.applyShapeFu(gid, cmd.Name)
	}
	_, err := h.app.D.ApplicationCommandCreate(h.app.Cfg.ApplicationID, gid, cmd)
	return err
}

// run is the production flow (main.rs order).
func run() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg, err := config.LoadConfig()
	if err != nil {
		slog.Error("Failed to load config", "module", "main", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	slog.Info("Tugbot starting", "module", "main")

	// 2. Database pool (fatal — Rust's expect path). Bounded by a 30s
	// deadline mirroring r2d2's 30s acquire timeout (pgxpool has no
	// connection_timeout equivalent — see internal/db): a down/slow PG
	// must not hang startup indefinitely.
	poolStartCtx, poolStartCancel := context.WithTimeout(ctx, 30*time.Second)
	defer poolStartCancel()
	pool, err := db.NewPool(poolStartCtx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("Failed to create database pool", "module", "main", "error", err)
		os.Exit(1)
	}

	// 3. pi RPC startup is NON-fatal (Rust mod.rs:267-279): a spawn
	// failure logs and the mention feature degrades (App.Pi stays nil).
	var piVal app.PiBackend
	if piRpc, perr := pirpc.Start(ctx, pirpc.StartConfig{SkillsDir: cfg.SkillsDir, Logger: logger}); perr != nil {
		slog.Error("Failed to start pi RPC subprocess — mention feature will not work", "module", "main", "error", perr)
	} else {
		piVal = piRpc
		slog.Info("pi RPC subprocess started", "module", "main")
	}

	// 4. The Discord session with exactly the six privileged() intents.
	d, derr := discordgo.New(discordgoToken(cfg.Token))
	if derr != nil {
		slog.Error("Failed to create Discord session", "module", "main", "error", derr)
		os.Exit(1)
	}
	d.Identify.Intents = botIntents()

	a := app.NewApp(cfg, pool, d)
	a.Pi = piVal
	h := newHandlers(a)

	// OnMessageCreate → teh, twitter, bsky, instagram, mention in that
	// order (Rust mod.rs dispatch order — the commented-out TikTok arm
	// is likewise absent). NON-BLOCKING: one goroutine per message runs
	// the five MessageCreate calls serially (the handlers' own
	// internal goroutines — e.g. mention's 300 s pi-ask flow — return
	// promptly; the event thread is never held).
	d.AddHandler(func(_ *discordgo.Session, evt *discordgo.MessageCreate) {
		if evt.Message == nil {
			return
		}
		m := evt.Message
		go func() {
			h.teh.MessageCreate(m)
			h.twitter.MessageCreate(m)
			h.bsky.MessageCreate(m)
			h.instagram.MessageCreate(m)
			h.mention.MessageCreate(m)
		}()
	})

	// OnMessageUpdate → gokupoll (the mod.rs:126-136 empty-payload
	// fetch lives inside the handler). NON-BLOCKING.
	d.AddHandler(func(_ *discordgo.Session, evt *discordgo.MessageUpdate) {
		go h.gokupoll.MessageUpdate(evt)
	})

	// OnGuildMemberAdd → the gulag join/rejoin arm.
	d.AddHandler(func(_ *discordgo.Session, evt *discordgo.GuildMemberAdd) {
		if evt.Member == nil || evt.Member.User == nil {
			return
		}
		h.gulag.JoinRejoin(evt.Member)
	})

	// OnReactionAdd / OnReactionRemove → the gulag reaction handler ONLY.
	d.AddHandler(func(_ *discordgo.Session, evt *discordgo.MessageReactionAdd) {
		h.gulag.ReactionAdd(evt.MessageReaction)
	})
	d.AddHandler(func(_ *discordgo.Session, evt *discordgo.MessageReactionRemove) {
		h.gulag.ReactionRemove(evt.MessageReaction)
	})

	// OnApplicationCommandCreate — discordgo v0.29.0 exposes this as the
	// INTERACTION_CREATE event (*discordgo.InteractionCreate); the Rust
	// `if let Interaction::Command` guard is the Type check below;
	// dispatch by InteractionData.Name in the pinned Rust order. One
	// goroutine per interaction: the handler arms do REST work.
	d.AddHandler(func(_ *discordgo.Session, evt *discordgo.InteractionCreate) {
		i := evt.Interaction
		if i == nil || i.Type != discordgo.InteractionApplicationCommand || i.Data == nil {
			return
		}
		// The Rust Command arm only matches application-command data.
		data, ok := i.Data.(discordgo.ApplicationCommandInteractionData)
		if !ok {
			return
		}
		// LOG ENGAGEMENT: one line per command so an un-responded
		// interaction is visible in journalctl (the Rust bot was silent
		// here; this is a Go-side observability addition).
		slog.Info("command received", "module", "main", "name", data.Name, "guild", i.GuildID)
		go func() {
			workStart := time.Now()
			h.finishInteraction(i, h.dispatchCommand(i), workStart)
		}()
	})

	// OnReady — Rust ready(): the servers three-way + the per-guild
	// command registration (9 shapes). The pi RPC spawn is main's
	// instead (startup step 3); the ready-arm parity is the rest. The
	// ready three-way's row slice is REUSED for the registration (Rust
	// ready() iterates the same get_servers() result — one load, not
	// one per call site).
	d.AddHandler(func(_ *discordgo.Session, evt *discordgo.Ready) {
		if evt.User != nil {
			slog.Info(evt.User.Username+" is connected!", "module", "main")
		}
		servers := h.readyThreeWay(ctx)
		if servers == nil {
			return
		}
		h.registerCommands(ctx, servers)
	})

	// The background loops (Rust mod.rs:263-264: spawned before the
	// per-guild loop; here before Open). Iteration errors are
	// logged-and-continue (inside the loops); a loop exiting on hard
	// failure is surfaced here.
	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		if err := h.gulag.RunReleaseCheck(egCtx); err != nil && !isContextErr(err) {
			slog.Error("release check loop terminated", "module", "main", "error", err)
		}
		return nil
	})
	eg.Go(func() error {
		if err := h.gulag.RunVoteCheck(egCtx); err != nil && !isContextErr(err) {
			slog.Error("vote check loop terminated", "module", "main", "error", err)
		}
		return nil
	})

	openErr := make(chan error, 1)
	go func() { openErr <- d.Open() }()

	// Block until SIGTERM/SIGINT cancels the shared ctx.
	<-ctx.Done()
	slog.Info("shutdown signal received — draining background loops", "module", "main")
	if werr := eg.Wait(); werr != nil && !isContextErr(werr) {
		// The loop arms log hard failures themselves; a non-ctx error
		// here (e.g. a panic in a loop) is surfaced at shutdown.
		slog.Warn("background loops did not drain cleanly", "module", "main", "error", werr)
	}
	if piVal != nil {
		piVal.Stop()
	}
	pool.Close()
	if cerr := d.Close(); cerr != nil {
		slog.Warn("Discord session close", "module", "main", "error", cerr)
	}
	<-openErr
	slog.Info("Tugbot stopped", "module", "main")
}

// finishInteraction applies the ACK-deadline rule (see
// interactionAckBudget): a defer response (cull's contract) ACKs on
// its own; a fresh result is sent as the plain reply; a stale result
// first ACKs via a matching (ephemeral-preserving) defer, then sends
// the result as a follow-up. Without it the heaviest command (the
// /gulag-release chain: seven REST round-trips from the prod host ≈
// 4.4s) outlives the 3s deadline and dies with 10062 "Unknown
// interaction" (proven live at cutover).
func (h *handlers) finishInteraction(i *discordgo.Interaction, r reply, workStart time.Time) {
	if r.deferResp {
		h.respond(i, r)
		return
	}
	if time.Since(workStart) > interactionAckBudget {
		slog.Warn("slash command outlived its ACK budget — deferring, then follow-up", "module", "main", "ms", time.Since(workStart).Milliseconds())
		if h.deferAckFu != nil {
			if err := h.deferAckFu(r.ephemeral); err != nil {
				slog.Error("Cannot defer slash command (ACK)", "module", "main", "error", err)
			}
			return
		}
		ack := &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{},
		}
		if r.ephemeral {
			ack.Data.Flags = discordgo.MessageFlagsEphemeral
		}
		if err := h.app.D.InteractionRespond(i, ack); err != nil {
			slog.Error("Cannot defer slash command (ACK)", "module", "main", "error", err)
			return
		}
		// WebhookParams.Flags may carry MessageFlagsEphemeral ONLY via
		// this Followup-Message-Create endpoint (the deferred
		// interaction is what makes the visibility stick).
		fm := &discordgo.WebhookParams{Content: r.content}
		if r.ephemeral {
			fm.Flags = discordgo.MessageFlagsEphemeral
		}
		if _, err := h.app.D.FollowupMessageCreate(i, false, fm); err != nil {
			slog.Error("Cannot follow up slash command", "module", "main", "error", err)
		}
		return
	}
	h.respond(i, r)
}

// isContextErr folds the loop-drain arms: a ctx-canceled loop is a
// clean shutdown, not a failure.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// reply is this binary's share of Rust's HandlerResponse shape
// (Content + Ephemeral + DeferResponse — cull is the one handler whose
// responses defer; none of the nine commands carry components, so the
// shape has no components field).
type reply struct {
	content   string
	ephemeral bool
	deferResp bool
}

// dispatchCommand mirrors Rust's interaction_create (mod.rs:214-253) in
// the pinned name order: gulag, gulag-release, gulag-list, Add Gulag
// Vote (message-kind, target_id = message id → outific author), AI Slop
// (message-kind, first resolved message), phony, horny (both via
// prefixhandler), feature (Feat), cull. Fallthrough: ephemeral "Not
// Implemented" WITHOUT a defer (Rust defer_response: None).
func (h *handlers) dispatchCommand(i *discordgo.Interaction) reply {
	name := ""
	if data, ok := i.Data.(discordgo.ApplicationCommandInteractionData); ok {
		name = data.Name
	}
	switch name {
	// The three slash arms + the message-kind arm all live in the
	// canonical gulag handler (its HandleCommandCreate dispatches by
	// name internally).
	case "gulag":
		r := h.gulag.HandleCommandCreate(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral}
	case "gulag-release":
		r := h.gulag.HandleCommandCreate(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral}
	case "gulag-list":
		r := h.gulag.HandleCommandCreate(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral}
	case "Add Gulag Vote":
		r := h.gulag.HandleCommandCreate(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral}
	case "AI Slop":
		r := h.aiSlop.HandleInteraction(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral}
	case "phony":
		r := h.prefix.HandleInteraction(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral}
	case "horny":
		r := h.prefix.HandleInteraction(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral}
	case "feature":
		r := h.feat.HandleInteraction(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral}
	case "cull":
		r := h.cull.HandleInteraction(i)
		return reply{content: r.Content, ephemeral: r.Ephemeral, deferResp: r.DeferResponse}
	default:
		return reply{content: "Not Implemented", ephemeral: true}
	}
}

// readyThreeWay port of servers.rs Servers::get_servers: (a) empty DB →
// bootstrap from the guild-API pages of 10 with the "gulag" role
// filter + create_server; (b) non-empty → verify each row against the
// guild API and delete stale rows; both with the "found in DB"
// logging. Returns nil on the hard-failure arms (Rust: a logged
// `return Vec::new()`).
func (h *handlers) readyThreeWay(ctx context.Context) []serverRow {
	st := serversStore{pool: h.app.Pool}

	rows, err := st.load(ctx)
	if err != nil {
		slog.Error("Database error loading servers", "module", "main", "error", err)
		return nil
	}

	if len(rows) == 0 {
		slog.Info("Nothing found in DB", "module", "main")
		guilds, err := h.app.D.UserGuilds(10, "", "1", false)
		if err != nil {
			slog.Error("Failed to get guilds", "module", "main", "error", err)
			return nil
		}
		var out []serverRow
		for _, gu := range guilds {
			g, err := strconv.ParseInt(gu.ID, 10, 64)
			if err != nil {
				slog.Error("Guild ID overflow", "module", "main", "error", err)
				continue
			}
			roles, err := h.app.D.GuildRoles(gu.ID)
			if err != nil {
				slog.Error("Failed to get roles for guild "+gu.ID, "module", "main", "error", err)
				continue
			}
			for _, role := range roles {
				if role.Name != "gulag" {
					continue
				}
				rid, err := strconv.ParseInt(role.ID, 10, 64)
				if err != nil {
					slog.Error("Guild ID overflow", "module", "main", "error", err)
					continue
				}
				if err := st.createServer(ctx, g, rid); err != nil {
					slog.Error("Failed to create server in DB", "module", "main", "error", err)
					continue
				}
				out = append(out, serverRow{GuildID: g, GulagID: rid})
			}
		}
		return out
	}

	slog.Info("found in DB", "module", "main")
	var verified []serverRow
	for _, s := range rows {
		// Rust servers.rs:142-156 parity: u64::try_from fails for a
		// NEGATIVE guild_id / gulag_id — log the conversion error and
		// KEEP the row (continue: no REST verify, no delete). A
		// negative id would 404 through the REST arm and the buggy
		// delete path would delete the row.
		if s.GuildID <= 0 {
			slog.Error("Guild ID conversion error for server "+strconv.FormatInt(int64(s.ID), 10), "module", "main")
			continue
		}
		if s.GulagID <= 0 {
			slog.Error("Gulag ID conversion error for server "+strconv.FormatInt(int64(s.ID), 10), "module", "main")
			continue
		}
		gid := strconv.FormatInt(s.GuildID, 10)
		if err := h.checkGuild(gid); err != nil {
			slog.Error("Couldn't connect to server with guild_id "+gid, "module", "main", "error", err)
			switch n, derr := st.deleteServer(ctx, s.ID); {
			case derr == nil:
				slog.Info("Deleted stale server "+strconv.FormatInt(int64(s.ID), 10)+" from database ("+strconv.Itoa(n)+" rows)", "module", "main")
			default:
				slog.Error("Database error during delete", "module", "main", "error", derr)
			}
			continue
		}
		verified = append(verified, s)
	}
	return verified
}

// registerCommands pins the per-guild registration: EXACTLY the nine
// command shapes (7 slash + 2 message-kind; there is no "goku"). The
// four gulag shapes (gulag, gulag-release, gulag-list, Add Gulag Vote)
// go through the canonical gulag.SetupCommand — registered onto every
// configured guild at once — then the remaining five shapes (AI Slop,
// phony, horny, feature, cull) in the Rust ready() vector order, per
// guild. The per-guild iteration consumes the ready three-way's row
// slice (the caller's readyThreeWay result, passed in) — ONE load,
// Rust parity (ready() iterates the single get_servers() result; no
// second read here). Registration failures are logged, not fatal (a
// missing guild registration never skips the others).
func (h *handlers) registerCommands(ctx context.Context, servers []serverRow) {
	for _, e := range h.guildSetup() {
		slog.Error("gulag command registration", "module", "main", "error", e)
	}
	for _, s := range servers {
		gid := strconv.FormatInt(s.GuildID, 10)
		// Rust ready() vector order (after the four gulag shapes):
		// AI Slop, horny, phony, feature, cull.
		shapes := []*discordgo.ApplicationCommand{
			h.aiSlop.SetupCommand(),
			h.prefix.SetupCommand("horny", "Mark yourself as horny/lfg"),
			h.prefix.SetupCommand("phony", "Mark yourself as phony/watching"),
			h.feat.SetupCommand(),
			h.cull.SetupCommand(),
		}
		for _, cmd := range shapes {
			if err := h.applyShape(gid, cmd); err != nil {
				slog.Error("register "+cmd.Name+" in guild "+gid+" failed", "module", "main", "error", err)
			}
		}
	}

	slog.Info("I now have the following guild slash commands:", "module", "main")
	for _, n := range []string{"gulag", "gulag-release", "gulag-list", "Add Gulag Vote", "AI Slop", "horny", "phony", "feature", "cull"} {
		slog.Info(n, "module", "main")
	}
}

// respond mirrors Rust's interaction_create reply logic (mod.rs:236-253)
// + the error logging: defer arm (defer_response == Some(true) → Defer
// with the ephemeral flag — cull's path); otherwise the message arm
// (honoring the ephemeral flag; none of the nine commands carry
// components). Send failures log "Cannot respond to slash command".
func (h *handlers) respond(i *discordgo.Interaction, r reply) {
	d := h.app.D
	var resp *discordgo.InteractionResponse
	if r.deferResp {
		// Rust: CreateInteractionResponse::Defer(
		// CreateInteractionResponseMessage::new().ephemeral(true)).
		resp = &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Flags: discordgo.MessageFlagsEphemeral,
			},
		}
	} else {
		data := &discordgo.InteractionResponseData{Content: r.content}
		if r.ephemeral {
			data.Flags = discordgo.MessageFlagsEphemeral
		}
		resp = &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: data,
		}
	}
	if err := d.InteractionRespond(i, resp); err != nil {
		slog.Error("Cannot respond to slash command", "module", "main", "error", err)
	}
}
