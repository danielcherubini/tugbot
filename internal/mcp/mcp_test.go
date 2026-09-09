package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// userGuildsCall records one captured UserGuilds call — the five-arg form is
// the one asserted (the state-empty fallback must call it on a single page
// without counts).
type userGuildsCall struct {
	limit      int
	before     string
	after      string
	withCounts bool
}

// fakeDiscord is the test implementation of the DiscordAPI seam (no live
// Discord connection needed): configurable fixtures + captured calls.
type fakeDiscord struct {
	mu sync.Mutex

	// Fixtures:
	stateGuilds           []*discordgo.Guild
	userGuilds            []*discordgo.UserGuild
	userGuildsErr         error
	guildChannels         map[string][]*discordgo.Channel
	guildChannelsErr      error
	channelMessages       []*discordgo.Message
	channelMessagesErr    error
	sentMessage           *discordgo.Message // returned by the send methods
	messageSendErr        error
	messageSendComplexErr error
	messageReactionAddErr error

	// Captured calls:
	stateGuildsCalls       int
	userGuildsCalls        []*userGuildsCall
	channelCalls           []string
	channelMessagesCalls   []messagesCall
	messageSendCalls       []messageSendCall
	messageSendComplexCall *messageSendComplexCall
	reactionAddCalls       []reactionAddCall
}

// messagesCall records one captured ChannelMessages call.
type messagesCall struct {
	id     string
	limit  int
	before string
	after  string
	around string
}

// messageSendCall records one captured ChannelMessageSend call.
type messageSendCall struct {
	channelID string
	content   string
}

// messageSendComplexCall records one captured ChannelMessageSendComplex call.
type messageSendComplexCall struct {
	channelID string
	data      *discordgo.MessageSend
}

// reactionAddCall records one captured MessageReactionAdd call.
type reactionAddCall struct {
	channelID string
	messageID string
	emoji     string
}

func (f *fakeDiscord) StateGuilds() []*discordgo.Guild {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateGuildsCalls++
	return f.stateGuilds
}

func (f *fakeDiscord) UserGuilds(limit int, before, after string, withCounts bool) ([]*discordgo.UserGuild, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userGuildsCalls = append(f.userGuildsCalls, &userGuildsCall{limit: limit, before: before, after: after, withCounts: withCounts})
	return f.userGuilds, f.userGuildsErr
}

func (f *fakeDiscord) GuildChannels(id string) ([]*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channelCalls = append(f.channelCalls, id)
	if f.guildChannelsErr != nil {
		return nil, f.guildChannelsErr
	}
	return f.guildChannels[id], nil
}
func (f *fakeDiscord) ChannelMessages(id string, limit int, before, after, around string) ([]*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channelMessagesCalls = append(f.channelMessagesCalls, messagesCall{id: id, limit: limit, before: before, after: after, around: around})
	return f.channelMessages, f.channelMessagesErr
}
func (f *fakeDiscord) ChannelMessageSend(channelID, content string) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messageSendCalls = append(f.messageSendCalls, messageSendCall{channelID: channelID, content: content})
	return f.sentMessage, f.messageSendErr
}
func (f *fakeDiscord) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messageSendComplexCall = &messageSendComplexCall{channelID: channelID, data: data}
	return f.sentMessage, f.messageSendComplexErr
}
func (f *fakeDiscord) MessageReactionAdd(channelID, messageID, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionAddCalls = append(f.reactionAddCalls, reactionAddCall{channelID: channelID, messageID: messageID, emoji: emoji})
	return f.messageReactionAddErr
}

// connectInProcess runs the bridge Server over the SDK's own in-memory
// transport pair and returns the connected client session (the SDK's own
// test pattern — there is no NewInMemoryClient constructor).
func connectInProcess(t *testing.T, srv *Server) *mcpSDK.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ct, st := mcpSDK.NewInMemoryTransports()
	ss, err := srv.srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	c := mcpSDK.NewClient(&mcpSDK.Implementation{Name: "fake-client", Version: "1.0.0"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool drives one tools/call; a protocol-level error fails the test (the
// IsError convention lives inside the result, not in this error).
func callTool(t *testing.T, cs *mcpSDK.ClientSession, name string, args any) *mcpSDK.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpSDK.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", name, err)
	}
	return res
}

// structuredMap normalizes the result's structured payload to a name→ID map.
func structuredMap(t *testing.T, res *mcpSDK.CallToolResult) map[string]string {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatalf("no structured content in result %+v", res)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal structured content %s: %v", b, err)
	}
	return m
}

// structuredRows enforces the SPEC contract on read_messages' payload (a
// top-level JSON array is NOT a valid MCP structuredContent — strict
// clients reject the whole call — so it must be a record) and returns the
// "messages" array.
func structuredRows(t *testing.T, res *mcpSDK.CallToolResult) []map[string]any {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatalf("no structured content in result %+v", res)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("structured content %s is not a record (an array?) — MCP structuredContent requires a JSON object: %v", b, err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(mustMarshal(t, rec["messages"]), &rows); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	return rows
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	return b
}

// freePort grabs a currently-free TCP port (best effort — the listener is
// closed before Start binds it).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestNewServerNonNil(t *testing.T) {
	srv := NewServer(&fakeDiscord{}, 0)
	if srv == nil {
		t.Fatal("NewServer() = nil, want non-nil")
	}
	// NewServer must NOT start or bind.
	if srv.handler() == nil {
		t.Error("handler() = nil after NewServer, want non-nil")
	}
}

func TestNewRealDiscordNonNil(t *testing.T) {
	d := NewRealDiscord(nil)
	if d == nil {
		t.Fatal("NewRealDiscord(nil) = nil, want non-nil (pointer form)")
	}
}

// TestRealDiscordStateGuildsIsolation is the review B1 regression: discordgo
// v0.29.0's State embeds a sync.RWMutex, and its gateway-event writers
// (GuildAdd, and the update path's in-place `*g = *guild` rewrite of an
// already-referenced *Guild) write UNDER that lock — so handing over the
// live slice (even a slice-header read under RLock) still leaves the
// caller's unlocked g.Name/g.ID field reads racing. StateGuilds must return
// a locked, per-entry struct-copy snapshot instead. A deterministic
// gateway-driven race repro is not feasible in a unit test (no network
// session here) — the -race build of this package (concurrent GuildAdd +
// StateGuilds below) is the race proof, and this test asserts the
// identity/isolation properties the copy gives callers.
func TestRealDiscordStateGuildsIsolation(t *testing.T) {
	st := discordgo.NewState()
	if err := st.GuildAdd(&discordgo.Guild{ID: "1", Name: "alpha"}); err != nil {
		t.Fatalf("GuildAdd(1): %v", err)
	}
	if err := st.GuildAdd(&discordgo.Guild{ID: "2", Name: "beta"}); err != nil {
		t.Fatalf("GuildAdd(2): %v", err)
	}
	// The session's State field is exported; a raw session (no gateway
	// connect) is just a holder for the State struct.
	s := &discordgo.Session{State: st}
	d := NewRealDiscord(s)

	got := d.StateGuilds()
	if len(got) != 2 {
		t.Fatalf("StateGuilds() = %d entries, want 2", len(got))
	}
	for i := range got {
		if got[i] == st.Guilds[i] {
			t.Fatalf("StateGuilds() entries[%d] is the state's own *Guild pointer; want a fresh copy (lock-and-copy snapshot)", i)
		}
	}
	if got[0].Name != "alpha" || got[1].Name != "beta" || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("snapshot content wrong: name=%q id=%q, name=%q id=%q", got[0].Name, got[0].ID, got[1].Name, got[1].ID)
	}

	// Simulate a gateway writer: under the state lock, mutate the ORIGINAL
	// guild in place (the way discordgo's update path does: *g = *guild).
	st.Lock()
	st.Guilds[0].Name = "alpha-renamed"
	st.Unlock()

	if got[0].Name != "alpha" {
		t.Fatalf("writer's in-place mutation leaked into the snapshot: name=%q, want %q", got[0].Name, "alpha")
	}
}

// TestRealDiscordStateGuildsNilState: with StateEnabled=false,
// Session.State is never allocated — StateGuilds must return nil cleanly
// rather than (in some future refactor) index into a nil *State.
func TestRealDiscordStateGuildsNilState(t *testing.T) {
	d := NewRealDiscord(&discordgo.Session{}) // raw session: State nil
	if got := d.StateGuilds(); got != nil {
		t.Fatalf("StateGuilds() with nil State = %v, want nil", got)
	}
}

// TestRealDiscordStateGuildsConcurrent: the -race proof. Concurrent writers
// (public State.GuildAdd — the same lock + append pattern the gateway event
// handlers use) racing unlocked readers (StateGuilds) must not trip the race
// detector now that StateGuilds takes the state's RLock and copies.
func TestRealDiscordStateGuildsConcurrent(t *testing.T) {
	st := discordgo.NewState()
	s := &discordgo.Session{State: st}
	d := NewRealDiscord(s)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := st.GuildAdd(&discordgo.Guild{ID: "x", Name: "g"}); err != nil {
					t.Errorf("GuildAdd: %v", err)
					return
				}
				_ = d.StateGuilds()
			}
		}()
	}
	wg.Wait()
}

func TestHandlerServesHealthzAndMCP(t *testing.T) {
	srv := NewServer(&fakeDiscord{}, 0)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", resp.StatusCode)
	}

	// /mcp is reachable: any 4xx from the SDK is OK (GET is not the
	// post-session MCP request shape); 200 is also fine.
	resp2, err := http.Get(ts.URL + "/mcp")
	if err != nil {
		t.Fatalf("GET /mcp: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusNotFound {
		t.Errorf("GET /mcp status = 404, want the SDK handler (any 4xx or 200)")
	}
}

func TestStartBoundPortErrors(t *testing.T) {
	// Occupy the port for the duration of the test.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	srv := NewServer(&fakeDiscord{}, port)
	// Ordinary context: a port conflict must fail startup on its own, not
	// via a timeout — Start returns the bind error without any cancel.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()

	select {
	case err := <-startErr:
		if err == nil {
			t.Fatal("Start() on a bound port = nil, want an error (not a panic)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not fail fast on a bound port (blocked without any ctx cancel)")
	}
}

// TestStartShutdownGraceWithInFlightRequest pins the ≤10s shutdown-grace
// contract with an in-flight request: Shutdown's 10s deadline hits with
// the request still open, and Start must return at the deadline — not when
// the request eventually finishes (unbounded). The in-flight request is a
// POST /mcp whose 4MB body never finishes sending before the deadline: the
// handler stays in its body read while Shutdown's wait group holds it for
// the full grace. Skips under -short (takes ~10s). (review I1)
func TestStartShutdownGraceWithInFlightRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("holds an in-flight request through the 10s shutdown grace (testing.Short)")
	}

	port := freePort(t)
	srv := NewServer(&fakeDiscord{}, port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()

	// Wait for the server to come up (/healthz must answer first).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Open the in-flight request: an upload that never finishes before the
	// 10s shutdown-grace deadline. The body is a JSON prefix ("{\"items\":[")
	// + whitespace that never closes the array, sent SLOWER than the handler can
	// drain (throttled to 32KB/100ms) — so the handler sits in its body read
	// for the whole 10s shutdown grace, whatever way the SDK consumes the
	// body (ReadAll or streaming JSON decode).
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial /mcp: %v", err)
	}
	defer conn.Close()
	// (the SDK caps request bodies at 4MB, so 4MB of readable data is the
	// hold: written at 32KB/100ms = ~12.8s, i.e. past the 10s grace)
	const total = 4194304
	uploadFinished := make(chan struct{}) // closed once the upload goroutine exits (whole body sent, or the server closed the conn)
	go func() {
		defer close(uploadFinished)
		header := "POST /mcp HTTP/1.1\r\nHost: 127.0.0.1:" + fmt.Sprint(port) +
			"\r\nMCP-Protocol-Version: 2025-06-18\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nContent-Length: " +
			fmt.Sprint(total+10) + "\r\n\r\n"
		if _, err := conn.Write([]byte(header)); err != nil {
			return
		}
		if _, err := conn.Write([]byte("{\"items\":[")); err != nil {
			return
		}
		buf := bytes.Repeat([]byte("\n"), 32<<10)
		sent := 0
		for sent < total {
			n, err := conn.Write(buf)
			sent += n
			if err != nil {
				return // conn closed: upload done, in-flight request ended
			}
			time.Sleep(100 * time.Millisecond) // throttle: 4MB takes ~12.8s, the 10s grace never sees EOF
		}
	}()
	// Let the upload get into flight so the request is genuinely in the
	// handler's body read before we cancel.
	time.Sleep(300 * time.Millisecond)

	// Cancel while the request is still in flight; Start must return nil at
	// the shutdown deadline (≤10s) — never after the request finishes.
	t0 := time.Now()
	cancel()
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("Start() after cancel with an in-flight request = %v (%T), want nil", err, err)
		}
		if elapsed := time.Since(t0); elapsed >= 12*time.Second {
			t.Fatalf("Start() took %v after cancel, want the ~10s shutdown deadline", elapsed)
		}
		select {
		case <-uploadFinished:
			t.Fatal("the in-flight upload had finished before Start returned — in-flight hold not exercised")
		default: // still in flight: the deadline path was actually exercised
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Start() did not return within 12s of cancel (the shutdown deadline is 10s)")
	}
}

// --- Task 3: discovery tools (list_guilds, list_channels) --------

// Acceptance: both tools appear in an in-process client's ListTools with the
// exact names.
func TestListToolsIncludesDiscoveryTools(t *testing.T) {
	srv := NewServer(&fakeDiscord{}, 0)
	cs := connectInProcess(t, srv)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"list_guilds", "list_channels"} {
		if !sliceContains(names, want) {
			t.Errorf("ListTools tools = %v, want it to include %q", names, want)
		}
	}
}

func sliceContains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func TestListGuildsFromState(t *testing.T) {
	f := &fakeDiscord{stateGuilds: []*discordgo.Guild{
		{ID: "1", Name: "alpha"},
		{ID: "2", Name: "beta"},
		{ID: "3", Name: "gamma"},
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "list_guilds", nil)
	if res.IsError {
		t.Fatalf("list_guilds from state: IsError, text %q", textOf(t, res))
	}
	m := structuredMap(t, res)
	want := map[string]string{"alpha": "1", "beta": "2", "gamma": "3"}
	if !mapsEqual(m, want) {
		t.Errorf("state map = %v, want %v", m, want)
	}
	text := textOf(t, res)
	if !strings.Contains(text, "3 guilds:") || !strings.Contains(text, "alpha=1") || !strings.Contains(text, "beta=2") || !strings.Contains(text, "gamma=3") {
		t.Errorf("state text = %q, want \"3 guilds: …\" with name=id pairs", text)
	}
	// The state path is zero-REST: UserGuilds must not be called.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.userGuildsCalls) != 0 {
		t.Errorf("UserGuilds called %d times from the state path, want 0", len(f.userGuildsCalls))
	}
}

// state-empty → the REST fallback: a single five-param page, no counts.
func TestListGuildsFallsBackToUserGuilds(t *testing.T) {
	f := &fakeDiscord{userGuilds: []*discordgo.UserGuild{{ID: "10", Name: "alpha"}}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "list_guilds", nil)
	if res.IsError {
		t.Fatalf("list_guilds fallback: IsError, text %q", textOf(t, res))
	}
	m := structuredMap(t, res)
	if !mapsEqual(m, map[string]string{"alpha": "10"}) {
		t.Errorf("fallback map = %v, want {alpha:10}", m)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.userGuildsCalls) != 1 {
		t.Fatalf("UserGuilds called %d times, want exactly 1 (single page)", len(f.userGuildsCalls))
	}
	c := f.userGuildsCalls[0]
	if c.limit != 100 || c.before != "" || c.after != "" || c.withCounts {
		t.Errorf("UserGuilds call = %+v, want {limit:100, before:\"\", after:\"\", withCounts:false}", c)
	}
}

// 100-guild page → the capped note is appended.
func TestListGuildsFallbackPageCapNote(t *testing.T) {
	s := make([]*discordgo.UserGuild, 100)
	for i := range s {
		s[i] = &discordgo.UserGuild{ID: fmt.Sprintf("%d", i+1), Name: fmt.Sprintf("g%d", i+1)}
	}
	f := &fakeDiscord{userGuilds: s}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "list_guilds", nil)
	if res.IsError {
		t.Fatalf("IsError: %q", textOf(t, res))
	}
	text := textOf(t, res)
	if !strings.Contains(text, "(more than 100 guilds; first page only)") {
		t.Errorf("text = %q, want the 100-cap note", text)
	}
}

func TestListChannelsTextLikeFiltering(t *testing.T) {
	f := &fakeDiscord{guildChannels: map[string][]*discordgo.Channel{
		"1": {
			{Name: "general", ID: "c1", Type: discordgo.ChannelTypeGuildText},
			{Name: "announcements", ID: "c2", Type: discordgo.ChannelTypeGuildNews},
			{Name: "general-voice", ID: "c3", Type: discordgo.ChannelTypeGuildVoice},
			{Name: "workflow", ID: "c4", Type: discordgo.ChannelTypeGuildCategory},
		},
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "list_channels", map[string]any{"guild_id": "1"})
	if res.IsError {
		t.Fatalf("list_channels: IsError, text %q", textOf(t, res))
	}
	m := structuredMap(t, res)
	want := map[string]string{"general": "c1", "announcements": "c2"}
	if !mapsEqual(m, want) {
		t.Errorf("map = %v, want exactly the 2 text-like channels %v", m, want)
	}
	text := textOf(t, res)
	if !strings.Contains(text, "2 channels:") {
		t.Errorf("text = %q, want the \"N channels: …\" summary", text)
	}
}

func TestListChannelsEmptyGuildID(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "list_channels", map[string]any{})
	if !res.IsError {
		t.Fatalf("empty guild_id: not IsError")
	}
	want := "tugbot: no default guild is configured; pass guild_id"
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.channelCalls) != 0 {
		t.Errorf("GuildChannels called %d times on empty guild_id, want 0", len(f.channelCalls))
	}
}

// A RESTError from the fake must surface as wrapDiscordErr's IsError shape.
func TestListChannelsDiscordError(t *testing.T) {
	f := &fakeDiscord{guildChannelsErr: &discordgo.RESTError{
		Response:     &http.Response{Status: "500 Internal Server Error"},
		ResponseBody: []byte(`{"message": "internal error"}`),
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "list_channels", map[string]any{"guild_id": "1"})
	if !res.IsError {
		t.Fatalf("discord error: not IsError")
	}
	want := `discord 500 Internal Server Error: {"message": "internal error"}`
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

// review N1(e): the state path's 60-guild fixture is SHARPER than the
// 100-cap fallback test — 51+ is the cap threshold and the text line must
// cut at exactly 50 while the structured payload keeps every entry.
func TestListGuildsSummary50Cap(t *testing.T) {
	s := make([]*discordgo.Guild, 60)
	for i := range s {
		s[i] = &discordgo.Guild{ID: fmt.Sprintf("gid%d", i+1), Name: fmt.Sprintf("alpha-%d", i+1)}
	}
	f := &fakeDiscord{stateGuilds: s}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "list_guilds", nil)
	if res.IsError {
		t.Fatalf("list_guilds: IsError, text %q", textOf(t, res))
	}
	text := textOf(t, res)
	// (1) the heading still reports the real count.
	if !strings.Contains(text, "60 guilds:") {
		t.Errorf("text = %q, want the \"60 guilds:\" heading", text)
	}
	// (2) past the 50-entry line cap the text ends with the ellipsis.
	if !strings.HasSuffix(text, "…") {
		t.Errorf("text = %q, want it to end with \"…\"", text)
	}
	// (3) at most 50 alpha-N= entries: the highest name (alpha-60, which
	// sorts after alpha-5x) must be absent while the first is present.
	if strings.Contains(text, "alpha-60=") {
		t.Errorf("text = %q contains alpha-60=, want at most 50 entries on the text line", text)
	}
	if !strings.Contains(text, "alpha-1=") {
		t.Errorf("text = %q, want it to contain \"alpha-1=\"", text)
	}
	// (4) the cap is text-only: the structured payload carries all 60.
	m := structuredMap(t, res)
	if len(m) != 60 {
		t.Errorf("structured payload = %d entries, want all 60 (the cap applies to the text line only)", len(m))
	}
}

func TestListGuildsDiscordError(t *testing.T) {
	f := &fakeDiscord{userGuildsErr: &discordgo.RESTError{
		Response:     &http.Response{Status: "500 Internal Server Error"},
		ResponseBody: []byte(`{"message": "internal error"}`),
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "list_guilds", nil)
	if !res.IsError {
		t.Fatalf("discord error: not IsError")
	}
	want := `discord 500 Internal Server Error: {"message": "internal error"}`
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

// readMessageFixture builds one fixture message with all fields set.
func readMessageFixture(id, authorID, username, ts, text string, attachments int) *discordgo.Message {
	tm, _ := time.Parse(time.RFC3339, ts)
	m := &discordgo.Message{ID: id, Timestamp: tm, Content: text}
	if authorID != "" || username != "" {
		m.Author = &discordgo.User{ID: authorID, Username: username}
	}
	for i := 0; i < attachments; i++ {
		m.Attachments = append(m.Attachments, &discordgo.MessageAttachment{})
	}
	return m
}

// --- Task 4: read_messages ------------------------------

// Acceptance: the registration test — ListTools carries read_messages with
// the exact description string.
func TestListToolsIncludesReadMessages(t *testing.T) {
	srv := NewServer(&fakeDiscord{}, 0)
	cs := connectInProcess(t, srv)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "read_messages" {
			want := "Reads a channel's recent message history. channel_id accepts a numeric snowflake or a channel name (name form requires guild_id). Author filter matches exact username (author_name) or user ID (author_id). On Discord rate-limit (429) the tool returns a rate_limited error with a retry-after — no retry loop; re-call later."
			if tool.Description != want {
				t.Errorf("read_messages description =\n%q\nwant\n%q", tool.Description, want)
			}
			return
		}
	}
	t.Fatal("ListTools tools missing read_messages")
}

// 3-message page: happy path — default limit 50, text + full payload shape.
func TestReadMessagesHappyPath(t *testing.T) {
	ts1, ts2, ts3 := "2026-02-02T10:00:00Z", "2026-02-02T11:00:00Z", "2026-02-02T12:00:00Z"
	f := &fakeDiscord{channelMessages: []*discordgo.Message{
		readMessageFixture("m1", "u1", "alice", ts1, "hello", 1),
		readMessageFixture("m2", "u1", "alice", ts2, "world", 0),
		readMessageFixture("m3", "u2", "bob", ts3, "bye", 2),
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	text := textOf(t, res)
	if !strings.Contains(text, "3 messages") || !strings.Contains(text, ts1) || !strings.Contains(text, ts3) {
		t.Errorf("text = %q, want \"3 messages\" + first/last timestamps %s / %s", text, ts1, ts3)
	}
	rows := structuredRows(t, res)
	if len(rows) != 3 {
		t.Fatalf("payload length = %d, want 3", len(rows))
	}
	want0 := map[string]any{"id": "m1", "author": "alice", "timestamp": ts1, "text": "hello", "attachment_count": float64(1)}
	for k, v := range want0 {
		if rows[0][k] != v {
			t.Errorf("row0[%q] = %v, want %v (row %v)", k, rows[0][k], v, rows[0])
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.channelMessagesCalls) != 1 {
		t.Fatalf("ChannelMessages called %d times, want 1", len(f.channelMessagesCalls))
	}
	if c := f.channelMessagesCalls[0]; c.id != "42" || c.limit != 50 || c.before != "" || c.after != "" || c.around != "" {
		t.Errorf("ChannelMessages call = %+v, want {id:42, limit:50, no offsets}", c)
	}
}

// limit 250 → clamped to 100 in the REST call + the note in the text.
func TestReadMessagesLimitClamp(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42", "limit": 250})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	if text := textOf(t, res); !strings.Contains(text, "limit clamped to 100") {
		t.Errorf("text = %q, want the \"limit clamped to 100\" note", text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.channelMessagesCalls[0]; c.limit != 100 {
		t.Errorf("ChannelMessages limit = %d, want the clamped 100", c.limit)
	}
}

// Non-numeric before_id/after_id → immediate IsError, zero REST.
func TestReadMessagesOffsetValidation(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	for _, arg := range []string{"before_id", "after_id"} {
		res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42", arg: "not-a-number"})
		if !res.IsError {
			t.Fatalf("%s: not IsError, text %q", arg, textOf(t, res))
		}
		want := "tugbot: before_id/after_id must be a snowflake ID"
		if got := textOf(t, res); got != want {
			t.Errorf("%s: text = %q, want %q", arg, got, want)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.channelMessagesCalls) != 0 {
		t.Errorf("ChannelMessages called %d times on validation failure, want 0", len(f.channelMessagesCalls))
	}
}

// Valid offsets → the REST call carries them through unchanged.
func TestReadMessagesOffsetArgs(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42", "before_id": "123", "after_id": "456"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.channelMessagesCalls[0]; c.before != "123" || c.after != "456" {
		t.Errorf("ChannelMessages offsets = before %q / after %q, want 123 / 456", c.before, c.after)
	}
}

// author_id: exact ID match (2 of 3 here) with no name interference.
func TestReadMessagesAuthorIDFilter(t *testing.T) {
	f := &fakeDiscord{channelMessages: []*discordgo.Message{
		readMessageFixture("m1", "u1", "alice", "2026-02-02T10:00:00Z", "a", 0),
		readMessageFixture("m2", "u1", "alice", "2026-02-02T11:00:00Z", "b", 0),
		readMessageFixture("m3", "u2", "bob", "2026-02-02T12:00:00Z", "c", 0),
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42", "author_id": "u1"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	r := textOf(t, res)
	if !strings.Contains(r, "2 messages") {
		t.Errorf("text = %q, want 2 of 3 matching author_id u1", r)
	}
	rows := structuredRows(t, res)
	if len(rows) != 2 || rows[0]["id"] != "m1" || rows[1]["id"] != "m2" {
		t.Errorf("filtered rows = %v, want exactly m1 and m2", rows)
	}
}

// author_name: exact username match (case-sensitive), and both-set → OR
// semantics.
func TestReadMessagesAuthorNameFilter(t *testing.T) {
	f := &fakeDiscord{channelMessages: []*discordgo.Message{
		readMessageFixture("m1", "u1", "alice", "2026-02-02T10:00:00Z", "a", 0),
		readMessageFixture("m2", "u1", "alice", "2026-02-02T11:00:00Z", "b", 0),
		readMessageFixture("m3", "u2", "bob", "2026-02-02T12:00:00Z", "c", 0),
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)

	// Exact match: a case-mismatched username matches nothing.
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42", "author_name": "Alice"})
	if text := textOf(t, res); !strings.Contains(text, "0 messages") {
		t.Errorf("case-mismatch text = %q, want 0 messages (exact match)", text)
	}
	res = callTool(t, cs, "read_messages", map[string]any{"channel_id": "42", "author_name": "alice"})
	if text := textOf(t, res); !strings.Contains(text, "2 messages") {
		t.Errorf("text = %q, want 2 messages for username alice", text)
	}
	// Both set → OR: the u2/ID leg (m3) plus the alice leg (m1+m2).
	res = callTool(t, cs, "read_messages", map[string]any{"channel_id": "42", "author_id": "u2", "author_name": "alice"})
	if text := textOf(t, res); !strings.Contains(text, "3 messages") {
		t.Errorf("OR text = %q, want all 3 (u2 OR alice)", text)
	}
}

// review N1(d): a webhook message (Author == nil) must render as
// "author" == "" in its row without panicking, and the summary still
// counts it ("N messages").
func TestReadMessagesNilAuthor(t *testing.T) {
	tm, _ := time.Parse(time.RFC3339, "2026-02-02T13:00:00Z")
	webhook := &discordgo.Message{ID: "m2", Timestamp: tm, Content: "no author here"} // Author: nil
	f := &fakeDiscord{channelMessages: []*discordgo.Message{
		readMessageFixture("m1", "u1", "alice", "2026-02-02T10:00:00Z", "hi", 0),
		webhook,
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	// Text summary still renders the full count.
	if text := textOf(t, res); !strings.Contains(text, "2 messages") {
		t.Errorf("text = %q, want \"2 messages\" (the nil-author row counts)", text)
	}
	rows := structuredRows(t, res)
	if len(rows) != 2 {
		t.Fatalf("payload rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r["id"] == "m2" {
			if r["author"] != "" {
				t.Errorf("nil-author row's author = %v, want \"\" (no panic, empty string)", r["author"])
			}
		} else if r["author"] != "alice" {
			t.Errorf("row %v author = %v, want \"alice\"", r["id"], r["author"])
		}
	}
}

// 429: the fake returns *discordgo.RateLimitError; the tool must surface
// it as an IsError result with the canonical wording and NO structured
// payload — a failed fetch has no partial page to return.
func TestReadMessagesRateLimited(t *testing.T) {
	f := &fakeDiscord{channelMessagesErr: &discordgo.RateLimitError{
		RateLimit: &discordgo.RateLimit{
			TooManyRequests: &discordgo.TooManyRequests{RetryAfter: 7 * time.Second},
			URL:             "https://discord.com/api/v10/channels/42/messages",
		},
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42"})
	if !res.IsError {
		t.Fatalf("rate limit: not IsError")
	}
	want := "discord rate_limited (retry after 7s)"
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if res.StructuredContent != nil {
		t.Errorf("StructuredContent = %+v, want nil (a failed fetch returns no partial page)", res.StructuredContent)
	}
}

// Channel-by-name: the name is resolved through GuildChannels; a wrong
// name and a missing guild are IsError results.
func TestReadMessagesChannelByName(t *testing.T) {
	f := &fakeDiscord{
		guildChannels: map[string][]*discordgo.Channel{
			"1": {
				{Name: "general", ID: "c1", Type: discordgo.ChannelTypeGuildText},
				{Name: "random", ID: "c2", Type: discordgo.ChannelTypeGuildText},
			},
		},
		channelMessages: []*discordgo.Message{readMessageFixture("m1", "u1", "alice", "2026-02-02T10:00:00Z", "hi", 0)},
	}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)

	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "general", "guild_id": "1"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	f.mu.Lock()
	if len(f.channelCalls) != 1 || f.channelCalls[0] != "1" {
		t.Errorf("GuildChannels calls = %v, want exactly [\"1\"]", f.channelCalls)
	}
	if len(f.channelMessagesCalls) != 1 || f.channelMessagesCalls[0].id != "c1" {
		t.Errorf("ChannelMessages = %+v, want it called with the resolved channel ID c1", f.channelMessagesCalls)
	}
	f.mu.Unlock()

	res = callTool(t, cs, "read_messages", map[string]any{"channel_id": "nonsense", "guild_id": "1"})
	if !res.IsError {
		t.Fatalf("wrong channel name: not IsError")
	}
	if got := textOf(t, res); !strings.Contains(got, "channel not found") {
		t.Errorf("text = %q, want a \"channel not found\" error", got)
	}

	res = callTool(t, cs, "read_messages", map[string]any{"channel_id": "general"})
	if !res.IsError {
		t.Fatalf("name without guild: not IsError")
	}
	if got := textOf(t, res); got != "tugbot: guild required to resolve channel by name" {
		t.Errorf("text = %q, want \"tugbot: guild required to resolve channel by name\"", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.channelMessagesCalls) != 1 {
		t.Errorf("ChannelMessages called %d times overall, want 1 (only the happy path)", len(f.channelMessagesCalls))
	}
}

// review I3: a REST failure from resolveChannelID (the fake's GuildChannels
// error knob) must route to wrapDiscordErr's "discord <status>" shape —
// NEVER toolErr's "tugbot:" domain shape — through the switch's
// default arm. The fixture map has the guild's channels; the error knob
// wins (the fake errors before referencing it).
func TestReadMessagesResolveRESTError(t *testing.T) {
	f := &fakeDiscord{
		guildChannels: map[string][]*discordgo.Channel{
			"1": {{Name: "general", ID: "c1", Type: discordgo.ChannelTypeGuildText}},
		},
		guildChannelsErr: &discordgo.RESTError{
			Response:     &http.Response{Status: "502 Bad Gateway"},
			ResponseBody: []byte(`{"message": "bad gateway"}`),
		},
	}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "nonsense", "guild_id": "1"})
	if !res.IsError {
		t.Fatalf("resolve REST error: not IsError, text %q", textOf(t, res))
	}
	got := textOf(t, res)
	// The domain-error shape never appears for a REST failure through
	// resolution: the default arm is wrapDiscordErr, not toolErr.
	if !strings.Contains(got, "discord 502") {
		t.Errorf("text = %q, want it to contain \"discord 502\" (wrapDiscordErr shape)", got)
	}
	if strings.Contains(got, "tugbot:") {
		t.Errorf("text = %q, must NOT contain \"tugbot:\" (toolErr domain shape is wrong for a REST failure)", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.channelMessagesCalls) != 0 {
		t.Errorf("ChannelMessages called %d times after a resolve failure, want 0", len(f.channelMessagesCalls))
	}
}

// review N1(a): an empty (omitted) channel_id must fail validation with the
// exact "tugbot: channel_id required" BEFORE any REST.
func TestReadMessagesEmptyChannelID(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"guild_id": "1"}) // channel_id key omitted
	if !res.IsError {
		t.Fatalf("empty channel_id: not IsError")
	}
	want := "tugbot: channel_id required"
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.channelMessagesCalls) != 0 || len(f.channelCalls) != 0 {
		t.Errorf("REST called on empty channel_id (messages=%v channels=%v), want none", f.channelMessagesCalls, f.channelCalls)
	}
}

// Empty fixture → "0 messages" and the payload is [] (not nil).
func TestReadMessagesEmpty(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "read_messages", map[string]any{"channel_id": "42"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	if text := textOf(t, res); text != "0 messages" {
		t.Errorf("text = %q, want \"0 messages\"", text)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if string(b) != `{"messages":[]}` {
		t.Errorf("structured payload = %s, want %s (not nil, not an error result)", b, `{"messages":[]}`)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// --- Task 5: post_message + react ----------------------------

// Acceptance: exactly 5 tools total at end of Task 5.
func TestListToolsShowsExactlyFiveTools(t *testing.T) {
	srv := NewServer(&fakeDiscord{}, 0)
	cs := connectInProcess(t, srv)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if len(names) != 5 {
		t.Fatalf("ListTools = %d tools %v, want exactly 5", len(names), names)
	}
	for _, want := range []string{"list_guilds", "list_channels", "read_messages", "post_message", "react"} {
		if !sliceContains(names, want) {
			t.Errorf("ListTools tools = %v, want it to include %q", names, want)
		}
	}
}

// post happy: the fake captured channel + text; output + payload shape.
func TestPostMessageHappyPath(t *testing.T) {
	f := &fakeDiscord{sentMessage: &discordgo.Message{ID: "m100"}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "post_message", map[string]any{"channel_id": "42", "text": "hello"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	if text := textOf(t, res); text != "posted message m100" {
		t.Errorf("text = %q, want \"posted message m100\"", text)
	}
	m := structuredMap(t, res)
	if m["id"] != "m100" {
		t.Errorf("payload = %v, want {id:m100}", m)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messageSendCalls) != 1 {
		t.Fatalf("ChannelMessageSend called %d times, want 1", len(f.messageSendCalls))
	}
	if c := f.messageSendCalls[0]; c.channelID != "42" || c.content != "hello" {
		t.Errorf("ChannelMessageSend = %+v, want {channelID:42, content:hello}", c)
	}
	if f.messageSendComplexCall != nil {
		t.Errorf("ChannelMessageSendComplex called %+v, want it used only for replies", f.messageSendComplexCall)
	}
}

// post with reply: ChannelMessageSendComplex with Reference.MessageID set
// (the field is Reference, NOT MessageReference).
func TestPostMessageWithReply(t *testing.T) {
	f := &fakeDiscord{sentMessage: &discordgo.Message{ID: "m101"}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "post_message", map[string]any{"channel_id": "42", "text": "rep", "reply_to_id": "77"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.messageSendComplexCall == nil {
		t.Fatal("ChannelMessageSendComplex not called for a reply, want it (not ChannelMessageSend)")
	}
	c := f.messageSendComplexCall
	if c.channelID != "42" || c.data.Content != "rep" {
		t.Errorf("complex call = %+v / content %q, want channel 42 with content \"rep\"", c.channelID, c.data.Content)
	}
	if c.data.Reference == nil || c.data.Reference.MessageID != "77" {
		t.Errorf("MessageSend.Reference = %+v, want Reference.MessageID == \"77\"", c.data.Reference)
	}
	if len(f.messageSendCalls) != 0 {
		t.Errorf("ChannelMessageSend called %d times for a reply, want 0", len(f.messageSendCalls))
	}
}

// >1999 runes: truncated to 1999 + the truncation note.
func TestPostMessageTruncation(t *testing.T) {
	f := &fakeDiscord{sentMessage: &discordgo.Message{ID: "m102"}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "post_message", map[string]any{"channel_id": "42", "text": strings.Repeat("a", 2500)})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	text := textOf(t, res)
	if !strings.Contains(text, "text truncated to 1999 chars (Discord cap); for longer content post in parts") {
		t.Errorf("text = %q, want the truncation note", text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.messageSendCalls[0]; len([]rune(c.content)) != 1999 {
		t.Errorf("ChannelMessageSend content = %d runes, want the truncation to 1999", len([]rune(c.content)))
	}
}

// empty text → IsError before any REST call.
func TestPostMessageEmptyText(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "post_message", map[string]any{"channel_id": "42"})
	if !res.IsError {
		t.Fatalf("empty text: not IsError")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messageSendCalls) != 0 || f.messageSendComplexCall != nil {
		t.Errorf("send called (send=%v complex=%v) on empty text, want none", f.messageSendCalls, f.messageSendComplexCall)
	}
}

// non-numeric reply_to_id → IsError before any REST call.
func TestPostMessageNonNumericReplyTo(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "post_message", map[string]any{"channel_id": "42", "text": "hi", "reply_to_id": "not-a-number"})
	if !res.IsError {
		t.Fatalf("non-numeric reply_to_id: not IsError, text %q", textOf(t, res))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messageSendCalls) != 0 || f.messageSendComplexCall != nil || len(f.channelCalls) != 0 {
		t.Errorf("REST called on validation failure (send=%v complex=%v channels=%v), want none", f.messageSendCalls, f.messageSendComplexCall, f.channelCalls)
	}
}

// A RESTError from the fake → the wrapDiscordErr "discord <status>" shape.
func TestPostMessageDiscordError(t *testing.T) {
	f := &fakeDiscord{messageSendErr: &discordgo.RESTError{
		Response:     &http.Response{Status: "403 Forbidden"},
		ResponseBody: []byte(`{"message": "Missing Permissions"}`),
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "post_message", map[string]any{"channel_id": "42", "text": "hi"})
	if !res.IsError {
		t.Fatalf("discord error: not IsError")
	}
	want := `discord 403 Forbidden: {"message": "Missing Permissions"}`
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

// review I3 (post): a REST failure from resolveChannelID must surface via
// wrapDiscordErr, not the toolErr domain shape.
func TestPostMessageResolveRESTError(t *testing.T) {
	f := &fakeDiscord{
		guildChannels: map[string][]*discordgo.Channel{
			"1": {{Name: "general", ID: "c1", Type: discordgo.ChannelTypeGuildText}},
		},
		guildChannelsErr: &discordgo.RESTError{
			Response:     &http.Response{Status: "502 Bad Gateway"},
			ResponseBody: []byte(`{"message": "bad gateway"}`),
		},
	}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "post_message", map[string]any{"channel_id": "nonsense", "guild_id": "1", "text": "hello"})
	if !res.IsError {
		t.Fatalf("resolve REST error: not IsError, text %q", textOf(t, res))
	}
	got := textOf(t, res)
	if !strings.Contains(got, "discord 502") {
		t.Errorf("text = %q, want it to contain \"discord 502\" (wrapDiscordErr shape)", got)
	}
	if strings.Contains(got, "tugbot:") {
		t.Errorf("text = %q, must NOT contain \"tugbot:\" (toolErr domain shape is wrong for a REST failure)", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messageSendCalls) != 0 || f.messageSendComplexCall != nil {
		t.Errorf("send called after a resolve failure (send=%v complex=%v), want none", f.messageSendCalls, f.messageSendComplexCall)
	}
}

// review N1(a) (post): an omitted channel_id fails validation with the exact
// "tugbot: channel_id required" before any REST (a valid text "hello" is
// present so the handler reaches the resolve path).
func TestPostMessageEmptyChannelID(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "post_message", map[string]any{"guild_id": "1", "text": "hello"}) // channel_id key omitted
	if !res.IsError {
		t.Fatalf("empty channel_id: not IsError")
	}
	want := "tugbot: channel_id required"
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messageSendCalls) != 0 || f.messageSendComplexCall != nil || len(f.channelCalls) != 0 {
		t.Errorf("REST called on empty channel_id (send=%v complex=%v channels=%v), want none", f.messageSendCalls, f.messageSendComplexCall, f.channelCalls)
	}
}

// react happy: the fake captured the exact channel/message/emoji args.
func TestReactHappyPath(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "react", map[string]any{"channel_id": "42", "message_id": "77", "emoji": "🔥"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	if text := textOf(t, res); text != "reacted 🔥 on 77" {
		t.Errorf("text = %q, want \"reacted 🔥 on 77\"", text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reactionAddCalls) != 1 {
		t.Fatalf("MessageReactionAdd called %d times, want 1", len(f.reactionAddCalls))
	}
	if c := f.reactionAddCalls[0]; c.channelID != "42" || c.messageID != "77" || c.emoji != "🔥" {
		t.Errorf("MessageReactionAdd = %+v, want {channelID:42, messageID:77, emoji:🔥}", c)
	}
}

// custom-emoji token passes through as-is (the tool description documents
// the input as \"any emoji token Discord accepts; unicode or custom-emoji
// name\").
func TestReactCustomEmojiTokenPassthrough(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "react", map[string]any{"channel_id": "42", "message_id": "77", "emoji": "name:123456789"})
	if res.IsError {
		t.Fatalf("IsError, text %q", textOf(t, res))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reactionAddCalls[0].emoji != "name:123456789" {
		t.Errorf("emoji = %q, want the token passed through as-is", f.reactionAddCalls[0].emoji)
	}
}

// non-numeric message_id → IsError before any REST call.
func TestReactNonNumericMessageID(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "react", map[string]any{"channel_id": "42", "message_id": "not-a-number", "emoji": "🔥"})
	if !res.IsError {
		t.Fatalf("non-numeric message_id: not IsError, text %q", textOf(t, res))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reactionAddCalls) != 0 || len(f.channelCalls) != 0 {
		t.Errorf("REST called on validation failure (reaction=%v channels=%v), want none", f.reactionAddCalls, f.channelCalls)
	}
}

// empty emoji → IsError before any REST call.
func TestReactEmptyEmoji(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "react", map[string]any{"channel_id": "42", "message_id": "77"})
	if !res.IsError {
		t.Fatalf("empty emoji: not IsError")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reactionAddCalls) != 0 {
		t.Errorf("MessageReactionAdd called %d times on empty emoji, want 0", len(f.reactionAddCalls))
	}
}

// review N1(c): react with an omitted message_id fails validation with the
// EXACT "tugbot: message_id required" before any REST (a valid channel_id
// is present so this is the message_id leg, not the channel leg).
func TestReactEmptyMessageID(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "react", map[string]any{"channel_id": "42", "emoji": "x"}) // message_id key omitted
	if !res.IsError {
		t.Fatalf("empty message_id: not IsError")
	}
	want := "tugbot: message_id required"
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reactionAddCalls) != 0 || len(f.channelCalls) != 0 {
		t.Errorf("REST called on empty message_id (reaction=%v channels=%v), want none", f.reactionAddCalls, f.channelCalls)
	}
}

// A RESTError from the fake → the wrapDiscordErr "discord <status>" shape.
func TestReactDiscordError(t *testing.T) {
	f := &fakeDiscord{messageReactionAddErr: &discordgo.RESTError{
		Response:     &http.Response{Status: "404 Not Found"},
		ResponseBody: []byte(`{"message": "Unknown Message"}`),
	}}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "react", map[string]any{"channel_id": "42", "message_id": "77", "emoji": "🔥"})
	if !res.IsError {
		t.Fatalf("discord error: not IsError")
	}
	want := `discord 404 Not Found: {"message": "Unknown Message"}`
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

// review I3 (react): a REST failure from resolveChannelID must surface via
// wrapDiscordErr, not the toolErr domain shape.
func TestReactResolveRESTError(t *testing.T) {
	f := &fakeDiscord{
		guildChannels: map[string][]*discordgo.Channel{
			"1": {{Name: "general", ID: "c1", Type: discordgo.ChannelTypeGuildText}},
		},
		guildChannelsErr: &discordgo.RESTError{
			Response:     &http.Response{Status: "502 Bad Gateway"},
			ResponseBody: []byte(`{"message": "bad gateway"}`),
		},
	}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "react", map[string]any{"channel_id": "nonsense", "guild_id": "1", "message_id": "77", "emoji": "x"})
	if !res.IsError {
		t.Fatalf("resolve REST error: not IsError, text %q", textOf(t, res))
	}
	got := textOf(t, res)
	if !strings.Contains(got, "discord 502") {
		t.Errorf("text = %q, want it to contain \"discord 502\" (wrapDiscordErr shape)", got)
	}
	if strings.Contains(got, "tugbot:") {
		t.Errorf("text = %q, must NOT contain \"tugbot:\" (toolErr domain shape is wrong for a REST failure)", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reactionAddCalls) != 0 {
		t.Errorf("MessageReactionAdd called %d times after a resolve failure, want 0", len(f.reactionAddCalls))
	}
}

// review N1(a) (react): an omitted channel_id fails validation with the exact
// "tugbot: channel_id required" before any REST (valid message_id + emoji
// are present so the handler reaches the resolve path).
func TestReactEmptyChannelID(t *testing.T) {
	f := &fakeDiscord{}
	srv := NewServer(f, 0)
	cs := connectInProcess(t, srv)
	res := callTool(t, cs, "react", map[string]any{"guild_id": "1", "message_id": "77", "emoji": "x"}) // channel_id key omitted
	if !res.IsError {
		t.Fatalf("empty channel_id: not IsError")
	}
	want := "tugbot: channel_id required"
	if got := textOf(t, res); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reactionAddCalls) != 0 || len(f.channelCalls) != 0 {
		t.Errorf("REST called on empty channel_id (reaction=%v channels=%v), want none", f.reactionAddCalls, f.channelCalls)
	}
}

// TestStartNilOnCancel pins the invariant Task 6's os.Exit(1) branch
// depends on: a clean ctx-cancel shutdown returns nil — NOT
// context.Canceled, NOT http.ErrServerClosed.
func TestStartNilOnCancel(t *testing.T) {
	port := freePort(t)
	srv := NewServer(&fakeDiscord{}, port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Let the server come up (/healthz must answer before cancel).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start() after cancel = %v (%T), want nil", err, err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Start() leaked %v on clean cancel, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Start() did not return within 15s of cancel (shutdown grace must be ≤10s)")
	}
}

// wrapDiscordErr — the one place the two REAL discordgo error types are
// understood (there is no *discordgo.Error in v0.29.0).

func TestWrapDiscordErrRESTError(t *testing.T) {
	err := &discordgo.RESTError{
		Response:     &http.Response{Status: "403 Forbidden"},
		ResponseBody: []byte(`{"message": "Missing Permissions"}`),
	}
	res := wrapDiscordErr(err)
	if res == nil || !res.IsError {
		t.Fatalf("wrapDiscordErr(RESTError) = %+v, want IsError result", res)
	}
	text := textOf(t, res)
	want := `discord 403 Forbidden: {"message": "Missing Permissions"}`
	if text != want {
		t.Errorf("wrapDiscordErr text = %q, want %q", text, want)
	}
}

func TestWrapDiscordErrRateLimitError(t *testing.T) {
	err := &discordgo.RateLimitError{
		RateLimit: &discordgo.RateLimit{
			TooManyRequests: &discordgo.TooManyRequests{RetryAfter: 7 * time.Second},
			URL:             "https://discord.com/api/v10/channels/1/messages",
		},
	}
	res := wrapDiscordErr(err)
	if res == nil || !res.IsError {
		t.Fatalf("wrapDiscordErr(RateLimitError) = %+v, want IsError result", res)
	}
	text := textOf(t, res)
	want := "discord rate_limited (retry after 7s)"
	if text != want {
		t.Errorf("wrapDiscordErr text = %q, want %q", text, want)
	}
}

func TestWrapDiscordErrGeneric(t *testing.T) {
	err := errors.New("boom")
	res := wrapDiscordErr(err)
	if res == nil || !res.IsError {
		t.Fatalf("wrapDiscordErr(generic) = %+v, want IsError result", res)
	}
	text := textOf(t, res)
	want := "discord error: boom"
	if text != want {
		t.Errorf("wrapDiscordErr text = %q, want %q", text, want)
	}
}

func TestToolErr(t *testing.T) {
	res := toolErr("tugbot", "no such channel")
	if res == nil || !res.IsError {
		t.Fatalf("toolErr() = %+v, want IsError result", res)
	}
	text := textOf(t, res)
	want := "tugbot: no such channel"
	if text != want {
		t.Errorf("toolErr text = %q, want %q", text, want)
	}
}

// --- Task 6: wire test — the bridge over REAL HTTP --------------
//
// The in-memory tests exercise the tool logic; this test exercises the
// wire path: the very same mux Start's http.Server serves, behind
// httptest.NewServer, driven by the SDK's own streamable client
// (StreamableClientTransport — real HTTP round-trips; NOT a
// NewInMemoryTransports pair).

// connectOverHTTP serves Server's handler() over real HTTP and
// connects the SDK's streamable client to it. It returns the test
// server (so /healthz can be hit over real HTTP) and the client
// session. The SDK's streamable connection detaches from the Connect
// context, so a background context is fine here.
func connectOverHTTP(t *testing.T, srv *Server) (*httptest.Server, *mcpSDK.ClientSession) {
	t.Helper()
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	cs, err := mcpSDK.NewClient(&mcpSDK.Implementation{Name: "wire-client", Version: "1.0.0"}, nil).Connect(
		context.Background(),
		&mcpSDK.StreamableClientTransport{Endpoint: ts.URL + "/mcp"},
		nil,
	)
	if err != nil {
		t.Fatalf("streamable client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return ts, cs
}

// TestWireEndToEnd locks the wire path end-to-end: the fake behind
// the production NewServer, served by httptest (the same mux Start
// serves), driven by the SDK client over real HTTP — healthz 200,
// EXACTLY 5 tools visible, one happy-path call per tool (with the
// fake's post/reaction captures asserted), and one error shape (a bad
// snowflake must arrive as an IsError tool result, not a protocol
// error). Passes by construction; its job is to lock the wire path.
func TestWireEndToEnd(t *testing.T) {
	f := &fakeDiscord{
		stateGuilds: []*discordgo.Guild{{ID: "100", Name: "alpha"}},
		guildChannels: map[string][]*discordgo.Channel{
			"100": {
				{Name: "general", ID: "42", Type: discordgo.ChannelTypeGuildText},
				{Name: "general-voice", ID: "43", Type: discordgo.ChannelTypeGuildVoice},
			},
		},
		channelMessages: []*discordgo.Message{
			readMessageFixture("77", "u1", "alice", "2026-02-02T10:00:00Z", "hello", 0),
			readMessageFixture("78", "u2", "bob", "2026-02-02T11:00:00Z", "world", 0),
		},
		sentMessage: &discordgo.Message{ID: "m100"},
	}
	srv := NewServer(f, 0)
	ts, cs := connectOverHTTP(t, srv)

	// healthz over real HTTP.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}

	// Exactly 5 tools visible over the wire.
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if len(names) != 5 {
		t.Fatalf("ListTools = %d tools %v, want exactly 5", len(names), names)
	}
	for _, want := range []string{"list_guilds", "list_channels", "read_messages", "post_message", "react"} {
		if !sliceContains(names, want) {
			t.Fatalf("ListTools tools = %v, want it to include %q", names, want)
		}
	}

	// One happy-path call per tool, over the wire (the numeric snowflake
	// form — a by-name channel would require guild_id in its arguments).
	res := callTool(t, cs, "list_guilds", nil)
	if res.IsError {
		t.Fatalf("list_guilds: IsError, %q", textOf(t, res))
	}
	if m := structuredMap(t, res); !mapsEqual(m, map[string]string{"alpha": "100"}) {
		t.Errorf("list_guilds = %v, want {alpha:100}", m)
	}

	res = callTool(t, cs, "list_channels", map[string]any{"guild_id": "100"})
	if res.IsError {
		t.Fatalf("list_channels: IsError, %q", textOf(t, res))
	}
	if m := structuredMap(t, res); !mapsEqual(m, map[string]string{"general": "42"}) {
		t.Errorf("list_channels = %v, want {general:42} (text-like only)", m)
	}

	res = callTool(t, cs, "read_messages", map[string]any{"channel_id": "42"})
	if res.IsError {
		t.Fatalf("read_messages: IsError, %q", textOf(t, res))
	}
	if text := textOf(t, res); !strings.Contains(text, "2 messages") {
		t.Errorf("read_messages text = %q, want \"2 messages\"", text)
	}
	if rows := structuredRows(t, res); len(rows) != 2 || rows[0]["id"] != "77" || rows[1]["id"] != "78" {
		t.Errorf("read_messages rows = %v, want 77 and 78", rows)
	}

	res = callTool(t, cs, "post_message", map[string]any{"channel_id": "42", "text": "hello"})
	if res.IsError {
		t.Fatalf("post_message: IsError, %q", textOf(t, res))
	}
	if m := structuredMap(t, res); m["id"] != "m100" {
		t.Errorf("post_message = %v, want {id:m100}", m)
	}

	res = callTool(t, cs, "react", map[string]any{"channel_id": "42", "message_id": "77", "emoji": "🔥"})
	if res.IsError {
		t.Fatalf("react: IsError, %q", textOf(t, res))
	}
	if text := textOf(t, res); text != "reacted 🔥 on 77" {
		t.Errorf("react text = %q, want \"reacted 🔥 on 77\"", text)
	}

	// The capture side of the write path (post + reaction) over the wire.
	f.mu.Lock()
	if len(f.messageSendCalls) != 1 || f.messageSendCalls[0].channelID != "42" || f.messageSendCalls[0].content != "hello" {
		t.Errorf("ChannelMessageSend capture = %v, want one call {channelID:42, content:hello}", f.messageSendCalls)
	}
	if len(f.reactionAddCalls) != 1 || f.reactionAddCalls[0].channelID != "42" || f.reactionAddCalls[0].messageID != "77" || f.reactionAddCalls[0].emoji != "🔥" {
		t.Errorf("MessageReactionAdd capture = %v, want one call {channelID:42, messageID:77, emoji:🔥}", f.reactionAddCalls)
	}
	f.mu.Unlock()

	// One error shape over the wire: a bad snowflake arrives as an
	// IsError tool result (never a protocol-level error).
	res = callTool(t, cs, "react", map[string]any{"channel_id": "42", "message_id": "not-a-number", "emoji": "🔥"})
	if !res.IsError {
		t.Fatalf("bad snowflake: not IsError, %q", textOf(t, res))
	}
	if got := textOf(t, res); got != "tugbot: message_id must be a snowflake ID" {
		t.Errorf("bad snowflake text = %q, want %q", got, "tugbot: message_id must be a snowflake ID")
	}
}

// textOf extracts the first TextContent of a CallToolResult (success results
// carry the tool's text summary plus an auto-added JSON-content block, per
// the SDK's pre-SEP-2106 fallback — success items number two, error results
// carry only their single text item).
func textOf(t *testing.T, res *mcpSDK.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("result = %+v, want at least one content item", res)
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcpSDK.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("no TextContent among %d content items in result %+v", len(res.Content), res)
	return ""
}
