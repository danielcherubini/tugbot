package feat

import (
	"errors"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
)

// Port of `Feat::setup_command` (feat.rs:10-20): slash command "feature",
// description "Toggle Feature", one optional string option "name" described
// "The feature to enable".
func TestSetupCommandShape(t *testing.T) {
	cmd := New(&app.App{}).SetupCommand()
	if cmd.Type != discordgo.ChatApplicationCommand {
		t.Errorf("Type = %v, want ChatApplicationCommand", cmd.Type)
	}
	if cmd.Name != "feature" {
		t.Errorf("Name = %q, want \"feature\"", cmd.Name)
	}
	if cmd.Description != "Toggle Feature" {
		t.Errorf("Description = %q, want \"Toggle Feature\"", cmd.Description)
	}
	if len(cmd.Options) != 1 {
		t.Fatalf("Options = %d entries, want 1", len(cmd.Options))
	}
	opt := cmd.Options[0]
	if opt.Name != "name" {
		t.Errorf("option Name = %q, want \"name\"", opt.Name)
	}
	if opt.Type != discordgo.ApplicationCommandOptionString {
		t.Errorf("option Type = %v, want String", opt.Type)
	}
	if opt.Description != "The feature to enable" {
		t.Errorf("option Description = %q, want \"The feature to enable\"", opt.Description)
	}
	if opt.Required {
		t.Errorf("option Required = true, want false (feat.rs: required(false))")
	}
}

// Port of `handle_list_features` (feat.rs:65-78): "Here's all the features"
// followed by one "\nName: `{name}` Enabled: `{enabled}`" line per feature,
// in the order `Features::all` returns them.
func TestFormatFeatureList(t *testing.T) {
	if got := formatFeatureList(nil); got != "Here's all the features" {
		t.Errorf("formatFeatureList(nil) = %q, want \"Here's all the features\"", got)
	}

	fs := []features.Feature{
		{Name: "gulag", Enabled: false},
		{Name: "teh", Enabled: true},
	}
	got := formatFeatureList(fs)
	want := "Here's all the features\nName: `gulag` Enabled: `false`\nName: `teh` Enabled: `true`"
	if got != want {
		t.Errorf("formatFeatureList = %q, want %q", got, want)
	}
}

// Port of the name match in `handle_feature` (feat.rs:48): a missing feature
// yields the exact "Couldn't match feature" response (feat.rs:58-62).
func TestFeatureNotFoundError(t *testing.T) {
	resp := featureNotMatched()
	if resp.Content != "Couldn't match feature" {
		t.Errorf("featureNotMatched().Content = %q, want \"Couldn't match feature\"", resp.Content)
	}
	if !resp.Ephemeral {
		t.Errorf("featureNotMatched().Ephemeral = false, want true (HandlerResponse.default)")
	}
}

// Every Feat response is ephemeral (feat.rs HandlerResponse: ephemeral: true
// in every constructor branch tested here).
func TestListResponseIsEphemeral(t *testing.T) {
	if !listResponse(nil).Ephemeral {
		t.Error("listResponse().Ephemeral = false, want true")
	}
	if !errorResponse("boom").Ephemeral {
		t.Error("errorResponse().Ephemeral = false, want true")
	}
}

// Port of the error string in `handle_feature` (feat.rs:52): "Failed to
// update feature: <err>".
func TestUpdateErrorText(t *testing.T) {
	if got := updateErrorText(errors.New("boom")); got != "Failed to update feature: boom" {
		t.Errorf("updateErrorText = %q, want \"Failed to update feature: boom\"", got)
	}
}

// Port of setup_interaction's non-string-option branch (feat.rs:36).
func TestInvalidOptionText(t *testing.T) {
	if got := invalidOptionText(); got != "Please provide a valid feature name" {
		t.Errorf("invalidOptionText() = %q, want \"Please provide a valid feature name\"", got)
	}
}
