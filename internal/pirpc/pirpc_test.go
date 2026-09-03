package pirpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielcherubini/tugbot/internal/app"
)

// Compile-time assertion: *PiRpc satisfies Task 1's app.PiBackend contract.
var _ app.PiBackend = (*PiRpc)(nil)

// --- helpers ---------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeFake is the test StartConfig for the fake_pi.sh harness. skillsDir is a
// temp dir with a tugbot-system-prompt.md so the arg vector is well-formed.
func makeFakeConfig(t *testing.T) StartConfig {
	t.Helper()
	skillsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillsDir, "tugbot-system-prompt.md"), []byte("test prompt"), 0o644); err != nil {
		t.Fatalf("writing fake skills prompt: %v", err)
	}
	return StartConfig{
		PiPath:    "testdata/fake_pi.sh",
		SkillsDir: skillsDir,
		Logger:    discardLogger(),
	}
}

func startFake(t *testing.T, vars map[string]string) *PiRpc {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
	p, err := Start(context.Background(), makeFakeConfig(t))
	if err != nil {
		t.Fatalf("Start(fake_pi): %v", err)
	}
	return p
}

// pollForFile waits up to d for a file to exist and returns its trimmed
// contents.
func pollForFile(t *testing.T, path string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

// waitProcessGone fails unless pid stops existing within d.
func waitProcessGone(t *testing.T, pid string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		cmd := exec.Command("kill", "-0", pid)
		if err := cmd.Run(); err != nil {
			return // no such process — it is gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %s still alive after Stop", pid)
}

func mustNonEmptyError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

// --- scenario tests --------------------------------------------------------

// (a) Start + Ask returns the fake's assistant text (trimmed).
func TestAskReturnsAssistantText(t *testing.T) {
	p := startFake(t, nil)
	defer p.Stop()

	got, err := p.Ask(context.Background(), "Say hi")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !strings.Contains(got, "hello from fake pi") {
		t.Errorf("Ask = %q, want it to contain %q", got, "hello from fake pi")
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("Ask returned untrimmed text: %q", got)
	}
}

// AskWithImages passes (mime, base64) pairs through to the request JSON.
func TestAskWithImages(t *testing.T) {
	p := startFake(t, nil)
	defer p.Stop()

	got, err := p.AskWithImages(context.Background(), "what is this?", []app.PiImage{
		{MimeType: "image/png", Data: "aGVsbG8="},
	})
	if err != nil {
		t.Fatalf("AskWithImages: %v", err)
	}
	if !strings.Contains(got, "saw-image") {
		t.Errorf("AskWithImages = %q, want it to contain the fake's image marker %q", got, "saw-image")
	}
}

// (b) Child dies (FAKE_PI_DIE=1, first run only) → first Ask fails, the
// supervisor restarts the subprocess, the second Ask succeeds.
func TestAutoRestartAfterChildDeath(t *testing.T) {
	state := filepath.Join(t.TempDir(), "ran")
	p := startFake(t, map[string]string{
		"FAKE_PI_DIE":        "1",
		"FAKE_PI_STATE_FILE": state,
	})
	defer p.Stop()

	// First ask hits the dying child: EOF on stdout.
	_, err := p.Ask(context.Background(), "first")
	mustNonEmptyError(t, err)
	if !errors.Is(err, ErrStdinEOF) {
		t.Errorf("first Ask error = %v, want errors.Is(ErrStdinEOF)", err)
	}

	// Second ask rides the restart and succeeds.
	got, err := p.Ask(context.Background(), "second")
	if err != nil {
		t.Fatalf("second Ask after restart: %v", err)
	}
	if !strings.Contains(got, "hello from fake pi") {
		t.Errorf("second Ask = %q, want the fake's assistant text", got)
	}
}

// (c) Stop kills the child — no lingering process.
func TestStopKillsChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := startFake(t, map[string]string{"FAKE_PI_PID_FILE": pidFile})

	pid := pollForFile(t, pidFile, 5*time.Second)
	if pid == "" {
		t.Fatal("empty pid file")
	}

	p.Stop()
	waitProcessGone(t, pid, 10*time.Second)

	// An Ask after Stop must fail cleanly (supervisor is gone).
	_, err := p.Ask(context.Background(), "after stop")
	mustNonEmptyError(t, err)
}

// (d) FAKE_PI_DELAY beyond the (injected 1s) ask timeout → timeout error.
func TestAskTimesOut(t *testing.T) {
	p := startFake(t, map[string]string{"FAKE_PI_DELAY": "3"})
	defer p.Stop()

	// Production default is 300s; tests inject a short timeout via the field.
	if p.askTimeout != 300*time.Second {
		t.Fatalf("askTimeout = %v, want the 300s production default before injection", p.askTimeout)
	}
	p.askTimeout = time.Second

	start := time.Now()
	_, err := p.Ask(context.Background(), "slow one")
	mustNonEmptyError(t, err)
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("Ask error = %v, want it to contain %q", err, "timed out")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Ask took %v; the short timeout should come back well before 5s", elapsed)
	}
}

// (e) success:false ack → "rejected prompt" error, no restart (second Ask
// succeeds on the same child).
func TestRejectedPrompt(t *testing.T) {
	p := startFake(t, map[string]string{"FAKE_PI_REJECT": "1"})
	defer p.Stop()

	_, err := p.Ask(context.Background(), "rejected")
	mustNonEmptyError(t, err)
	if !strings.Contains(err.Error(), "pi RPC rejected prompt") {
		t.Errorf("Ask error = %v, want it to contain %q", err, "pi RPC rejected prompt")
	}
}

// (f) agent_end before the prompt was accepted → error, no silent text.
func TestAgentEndBeforeAcceptance(t *testing.T) {
	p := startFake(t, map[string]string{"FAKE_PI_AGENT_END_FIRST": "1"})
	defer p.Stop()

	got, err := p.Ask(context.Background(), "premature end")
	mustNonEmptyError(t, err)
	if got != "" {
		t.Errorf("Ask returned %q with an error; text must be empty", got)
	}
	if !strings.Contains(err.Error(), "agent_end before prompt was accepted") {
		t.Errorf("Ask error = %v, want it to contain %q", err, "agent_end before prompt was accepted")
	}
}

// (g) agent_end after acceptance carrying an error field → "agent_end error"
// error, no silent text return.
func TestAgentEndErrorField(t *testing.T) {
	p := startFake(t, map[string]string{"FAKE_PI_AGENT_END_ERROR": "1"})
	defer p.Stop()

	got, err := p.Ask(context.Background(), "agent broke")
	mustNonEmptyError(t, err)
	if got != "" {
		t.Errorf("Ask returned %q with an error; text must be empty", got)
	}
	if !strings.Contains(err.Error(), "pi RPC agent_end error") {
		t.Errorf("Ask error = %v, want it to contain %q", err, "pi RPC agent_end error")
	}
}

// Stop must be safe to call twice and after Start's error path.
func TestStopIdempotent(t *testing.T) {
	p := startFake(t, nil)
	p.Stop()
	p.Stop() // must not panic, hang, or double-kill

	// Start with a missing binary: the error surfaces, no orphans.
	_, err := Start(context.Background(), StartConfig{
		PiPath:    filepath.Join(t.TempDir(), "no-such-pi-binary"),
		SkillsDir: t.TempDir(),
		Logger:    discardLogger(),
	})
	mustNonEmptyError(t, err)
}

// --- extractAssistantText parity with src/pi_rpc.rs's #[test]s -------------

func testExtract(t *testing.T, raw, want string, wantErr bool) {
	t.Helper()
	got, err := extractAssistantText([]byte(raw))
	if wantErr {
		if err == nil {
			t.Errorf("extractAssistantText(%s) = %q, want error", raw, got)
		}
		return
	}
	if err != nil {
		t.Fatalf("extractAssistantText(%s): %v", raw, err)
	}
	if got != want {
		t.Errorf("extractAssistantText = %q, want %q", got, want)
	}
}

func TestExtractAssistantTextStringContent(t *testing.T) {
	testExtract(t, `{"messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":"Hi there!"}]}`, "Hi there!", false)
}

func TestExtractAssistantTextArrayContent(t *testing.T) {
	testExtract(t, `{"messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":[{"type":"text","text":"Hi "},{"type":"text","text":"there!"}] }]}`, "Hi there!", false)
}

func TestExtractAssistantTextMixedBlocks(t *testing.T) {
	testExtract(t, `{"messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":[{"type":"text","text":"Answer: "},{"type":"tool_use","name":"search"},{"type":"text","text":"42"}] }]}`, "Answer: 42", false)
}

func TestExtractAssistantTextLastAssistant(t *testing.T) {
	testExtract(t, `{"messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":"First response"},{"role":"user","content":"Again?"},{"role":"assistant","content":"Second response"}]}`, "Second response", false)
}

func TestExtractAssistantTextNoAssistant(t *testing.T) {
	testExtract(t, `{"messages":[{"role":"user","content":"Hello"}]}`, "", true)
}

func TestExtractAssistantTextMissingMessagesArray(t *testing.T) {
	testExtract(t, `{"foo":"bar"}`, "", true)
}

func TestExtractAssistantTextNonContentTypes(t *testing.T) {
	// content neither a string nor an array
	testExtract(t, `{"messages":[{"role":"assistant","content":{"unexpected":true}}]}`, "", true)
}
