package aislop

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestSetupCommandShape pins the Rust-identical registration
// (ai_slop.rs:22-26): kind = Message, name "AI Slop", EMPTY description,
// no options. (The "Add Gulag Vote" registration is independent work, not
// part of this module.)
func TestSetupCommandShape(t *testing.T) {
	h := &AiSlop{}
	cmd := h.SetupCommand()
	if cmd.Type != discordgo.MessageApplicationCommand {
		t.Errorf("Type = %v, want MessageApplicationCommand (kind=Message)", cmd.Type)
	}
	if cmd.Name != "AI Slop" {
		t.Errorf("Name = %q, want %q", cmd.Name, "AI Slop")
	}
	if cmd.Description != "" {
		t.Errorf("Description = %q, want empty string (Rust: .description(\"\"))", cmd.Description)
	}
	if len(cmd.Options) != 0 {
		t.Errorf("Options = %v, want none", cmd.Options)
	}
}

// TestFeatureKey pins the feature flag key (ai_slop.rs:31).
func TestFeatureKey(t *testing.T) {
	if FeatureKey != "ai_slop" {
		t.Errorf("FeatureKey = %q, want %q", FeatureKey, "ai_slop")
	}
}

// TestOffenseDuration pins the offense conversion (ai_slop.rs:98-110):
// new_count.saturating_sub(1) -> (negative is clamped to 0, in line with
// Rust's i32 -> u32 conversion which errors on negative values — the
// DB count is effectively >= 1) -> GulagDurationForOffense.
func TestOffenseDuration(t *testing.T) {
	tests := []struct {
		name    string
		occurs  int64
		want    int64
		wantErr bool
	}{
		{"first offense", 1, 1800, false},
		{"second offense", 2, 3600, false},
		{"twenty-second offense", 21, 1_887_436_800, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := OffenseDuration(tt.occurs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (duration %d)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("OffenseDuration(%d) = %d, want %d", tt.occurs, got, tt.want)
			}
		})
	}

	// The clamped-value clause: a zero count saturates to offense 0.
	if got, _ := OffenseDuration(0); got != 1800 {
		t.Errorf("OffenseDuration(0) = %d, want 1800 (saturating_sub clamp)", got)
	}
}

// TestNextOffenseFormatInput pins the final next-offense branch
// (ai_slop.rs:179-185): format(GulagDurationForOffense(new_count)) where the
// int32 -> u32 conversion only fails on out-of-range counts (Rust's u32
// clamp, fallback 2_592_000).
func TestNextOffenseFormatInput(t *testing.T) {
	if got := NextOffenseSeconds(0); got != 1800 {
		t.Errorf("NextOffenseSeconds(0) = %d, want 1800", got)
	}
	// count 21 fits in u32 -> raw exponential value, no clamp.
	if got := NextOffenseSeconds(21); got != 3_774_873_600 {
		t.Errorf("NextOffenseSeconds(21) = %d, want 3_774_873_600", got)
	}
	// above u32 max -> the 30-day fallback (Rust Err branch).
	if got := NextOffenseSeconds(4_294_967_296); got != 2_592_000 {
		t.Errorf("NextOffenseSeconds(above u32 max) = %d, want 2_592_000", got)
	}
}

// TestResponseTexts pins the byte-for-byte response strings from
// ai_slop.rs (the AI-slop heuristics are these gate texts + the
// duration/next-offense messaging).
func TestResponseTexts(t *testing.T) {
	golden := map[string]string{
		"disabled":       "This feature is currently disabled.",
		"guildOnly":      "Error: This command can only be used in a guild",
		"permVerify":     "Error: Could not verify your permissions",
		"roleRequired":   "Error: You need Highly Regarded or admin role to use this command",
		"noTarget":       "Error: Could not find target message",
		"selfSlop":       "Error: You cannot AI Slop yourself!",
		"botTarget":      "Error: You cannot AI Slop the bot!",
		"botVerify":      "Error: Could not verify bot status",
		"unconfigured":   "Error: This server is not configured. Please ensure a gulag role exists.",
		"recordFail":     "Error: Failed to record AI slop usage",
		"countTooHigh":   "Error: Usage count too high for gulag calculation",
		"sendFailPrefix": "Error: Failed to send to gulag: ",
	}
	got := responseTexts()
	for k, want := range golden {
		if g, ok := got[k]; !ok || g != want {
			t.Errorf("responseTexts()[%q] = %q, want %q", k, g, want)
		}
	}
}

// TestRoleAllowlist pins the role allowlist (ai_slop.rs:51).
func TestRoleAllowlist(t *testing.T) {
	roles := AllowedRoles()
	if len(roles) != 2 || roles[0] != "Highly Regarded" || roles[1] != "admin" {
		t.Errorf("AllowedRoles() = %v, want [Highly Regarded admin]", roles)
	}
}
