// Package mcp is the Tugbot MCP Discord Bridge layer: an MCP (Model Context
// Protocol) server that exposes Discord operations as tools over Streamable
// HTTP. Everything hangs on ONE seam — the tools must be testable without a
// live Discord connection, but *app.App carries a concrete
// *discordgo.Session — so this package defines a small DiscordAPI interface
// covering exactly the session methods the bridge tools use: production
// wraps the real session (realDiscord), tests use a fake.
//
// Error-handling convention: a recoverable Discord API failure is NOT a Go
// error — it is mapped to an IsError tool result (wrapDiscordErr), and the
// IsError convention is established by toolErr ("<bot>: <message>"). wrapDiscordErr
// is the ONE place the real discordgo error types (*discordgo.RESTError /
// *discordgo.RateLimitError — there is no *discordgo.Error in v0.29.0) are
// understood; the tools just pass errors through it.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DiscordAPI is the seam: the exact surface of *discordgo.Session the bridge
// tools use. Production wraps App.D; tests use a fake.
type DiscordAPI interface {
	// State-backed (no REST):
	StateGuilds() []*discordgo.Guild
	// REST — the realDiscord impl passes discordgo.WithRetryOnRatelimit(false)
	// on every one of these, so 429s surface as *discordgo.RateLimitError
	// instead of being absorbed by the session's default block-and-retry.
	UserGuilds(limit int, before, after string, withCounts bool) ([]*discordgo.UserGuild, error)
	GuildChannels(id string) ([]*discordgo.Channel, error)
	ChannelMessages(id string, limit int, before, after, around string) ([]*discordgo.Message, error)
	ChannelMessageSend(channelID, content string) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error)
	// The seam name is the plan's; the underlying SESSION method is
	// MessageReactionAdd (v0.29.0 — there is no ChannelMessageReactionAdd).
	MessageReactionAdd(channelID, messageID, emoji string) error
}

// realDiscord wraps the production *discordgo.Session.
type realDiscord struct{ s *discordgo.Session }

// NewRealDiscord wraps the real session in the DiscordAPI seam (always the
// pointer form — the seam methods are declared on *realDiscord).
func NewRealDiscord(s *discordgo.Session) DiscordAPI { return &realDiscord{s: s} }

// StateGuilds returns a LOCK-AND-COPY snapshot of s.State.Guilds (the
// state is populated on READY with StateEnabled, discordgo's default). The
// copy is mandatory, not just locking: discordgo v0.29.0's gateway-event
// writers (GuildAdd and the update path's in-place `*g = *guild` rewrite of
// an already-referenced *Guild, state.go) write under the State's embedded
// sync.RWMutex, so handing over the live slice — even a slice-header read
// taken under RLock — still leaves the caller's unlocked g.Name/g.ID field
// reads racing once the lock is released. Each entry is therefore allocated
// fresh and copied (a shallow struct copy; callers read Name/ID only, so the
// shared inner slices are not a problem). A nil State (StateEnabled=false)
// yields nil.
func (d *realDiscord) StateGuilds() []*discordgo.Guild {
	if d.s.State == nil {
		return nil
	}
	d.s.State.RLock()
	defer d.s.State.RUnlock()
	out := make([]*discordgo.Guild, 0, len(d.s.State.Guilds))
	for _, guild := range d.s.State.Guilds {
		g := *guild
		out = append(out, &g)
	}
	return out
}

// Each REST method below delegates to the corresponding *discordgo.Session
// method with IDENTICAL semantics — plus
// discordgo.WithRetryOnRatelimit(false) so a 429 surfaces to the tool layer
// as *discordgo.RateLimitError instead of being absorbed by the session's
// default block-and-retry.
func (d *realDiscord) UserGuilds(limit int, before, after string, withCounts bool) ([]*discordgo.UserGuild, error) {
	return d.s.UserGuilds(limit, before, after, withCounts, discordgo.WithRetryOnRatelimit(false))
}

func (d *realDiscord) GuildChannels(id string) ([]*discordgo.Channel, error) {
	return d.s.GuildChannels(id, discordgo.WithRetryOnRatelimit(false))
}

func (d *realDiscord) ChannelMessages(id string, limit int, before, after, around string) ([]*discordgo.Message, error) {
	return d.s.ChannelMessages(id, limit, before, after, around, discordgo.WithRetryOnRatelimit(false))
}

func (d *realDiscord) ChannelMessageSend(channelID, content string) (*discordgo.Message, error) {
	return d.s.ChannelMessageSend(channelID, content, discordgo.WithRetryOnRatelimit(false))
}

func (d *realDiscord) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	return d.s.ChannelMessageSendComplex(channelID, data, discordgo.WithRetryOnRatelimit(false))
}

func (d *realDiscord) MessageReactionAdd(channelID, messageID, emoji string) error {
	return d.s.MessageReactionAdd(channelID, messageID, emoji, discordgo.WithRetryOnRatelimit(false))
}

// Server owns the SDK server object and (while running) the http.Server.
type Server struct {
	discord DiscordAPI
	port    int
	srv     *mcpSDK.Server
}

// NewServer constructs the Server (does NOT start, does NOT bind a port).
// All tools registered on the SDK server happen here (later bridge tasks add
// registrations).
func NewServer(d DiscordAPI, port int) *Server {
	srv := mcpSDK.NewServer(&mcpSDK.Implementation{
		Name:    "tugbot",
		Version: "1.0.0",
	}, nil)
	// The bridge tool groups: discovery first, then reading, then the write tools last.
	registerDiscoveryTools(srv, d)
	registerReadTools(srv, d)
	registerWriteTools(srv, d)
	return &Server{discord: d, port: port, srv: srv}
}

// Start runs the http.Server on 0.0.0.0:{port} (mux: "/mcp" → the
// stateless Streamable-HTTP handler; "/healthz" → 200 "ok"). It blocks
// until ctx cancels, then http.Server.Shutdown with a ≤10s grace. It returns
// the listen error (e.g. address-in-use) OR nil on clean shutdown — a
// clean-cancel shutdown never leaks context.Canceled or
// http.ErrServerClosed.
func (s *Server) Start(ctx context.Context) error {
	hs := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", s.port),
		Handler: s.handler(),
	}
	// Capacity 1: the single ListenAndServe result (the bind error or the
	// server-closed termination) is exactly once, and a buffered send never
	// blocks the goroutine on any path.
	errCh := make(chan error, 1)
	go func() { errCh <- hs.ListenAndServe() }()

	// Fail fast: a bind failure (e.g. the port is taken) is returned at boot
	// — no waiting for a signal — so cmd/tugbot/main.go's errgroup arm
	// (slog.Error + os.Exit(1)) fires at startup, not at the next SIGTERM.
	select {
	case <-ctx.Done():
		// Clean-cancel path: become the ≤10s shutdown grace.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)

		if shutdownCtx.Err() != nil {
			// Deadline: active connections may still be in flight. A previous
			// bind failure already delivered its error to the buffered channel,
			// so a non-blocking read is exact.
			select {
			case err := <-errCh:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				// Mapped clean listen-terminator (nil / ErrServerClosed): nil.
				return nil
			default:
				return nil
			}
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		// ListenAndServe finished without us cancelling: the bind failure
		// (address in use — returned immediately at boot) or a clean
		// listen-terminator (nil / ErrServerClosed → nil).
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// handler returns the mux Start serves and httptest-based tests mount.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSDK.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcpSDK.Server { return s.srv },
		&mcpSDK.StreamableHTTPOptions{Stateless: true},
	))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// toolErr is the IsError tool-result convention: "<bots>: <msg>".
func toolErr(bots, msg string) *mcpSDK.CallToolResult {
	return &mcpSDK.CallToolResult{
		IsError: true,
		Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: bots + ": " + msg}},
	}
}

// wrapDiscordErr maps a Discord-layer error to an IsError tool result —
// the ONE place the two real discordgo error types are understood (there is
// no *discordgo.Error in v0.29.0): *discordgo.RESTError →
// "discord <Response.Status>: <ResponseBody>"; *discordgo.RateLimitError →
// "discord rate_limited (retry after <RetryAfter>)"; anything else →
// "discord error: <err.Error()>". The tools just pass errors through it.
func wrapDiscordErr(err error) *mcpSDK.CallToolResult {
	switch e := err.(type) {
	case *discordgo.RESTError:
		return discordErrResult(fmt.Sprintf("%s: %s", e.Response.Status, e.ResponseBody))
	case *discordgo.RateLimitError:
		return discordErrResult(fmt.Sprintf("rate_limited (retry after %s)", e.RetryAfter))
	default:
		return discordErrResult("error: " + err.Error())
	}
}

// discordErrResult builds an IsError tool result with the fixed
// "discord " prefix (distinct from toolErr's "<bots>: " convention).
func discordErrResult(msg string) *mcpSDK.CallToolResult {
	return &mcpSDK.CallToolResult{
		IsError: true,
		Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "discord " + msg}},
	}
}
