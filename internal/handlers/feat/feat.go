// Package feat is the Go port of the Rust bot's src/handlers/feat.rs
// (100 lines).
//
// Wiring parity: in src/handlers/mod.rs, Feat is NOT a message handler
// (OnMessageCreate does not call it). It is a SLASH COMMAND only:
//   - `ready()` registers `Feat::setup_command()` — the 9 guild commands
//     include "feature" — for every configured server
//     (src/handlers/mod.rs, ready());
//   - `interaction_create` dispatches command name "feature" to
//     `Feat::setup_interaction` (src/handlers/mod.rs).
//
// This port mirrors that exact shape: SetupCommand returns the registration
// and HandleInteraction is the interaction entry point.
package feat

import (
	"context"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
)

// Feat handles the "feature" slash command (toggle a feature / list them
// all).
type Feat struct {
	app *app.App
}

// New builds the handler.
func New(app *app.App) *Feat {
	return &Feat{app: app}
}

// Response is this module's share of Rust's HandlerResponse shape: Feat never
// defers and never sets components, so the fields it uses are Content and
// Ephemeral (always true); DeferResponse is present for parity with the Rust
// struct default `Option::None`.
type Response struct {
	Content       string
	Ephemeral     bool
	DeferResponse *bool
}

// SetupCommand mirrors `Feat::setup_command` (feat.rs:9-19): slash command
// "feature", description "Toggle Feature", one optional string option
// "name" described "The feature to enable".
func (h *Feat) SetupCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "feature",
		Description: "Toggle Feature",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "name",
				Type:        discordgo.ApplicationCommandOptionString,
				Description: "The feature to enable",
				// Required left false — Rust: .required(false)
			},
		},
	}
}

// HandleInteraction mirrors `Feat::setup_interaction` (feat.rs:21-41). Rust's
// entry point never returns an error — DB failures become the error
// response — so this one doesn't either (the 404/timeout reply semantics are
// Task 7's orchestration job, not Feat's).
func (h *Feat) HandleInteraction(i *discordgo.Interaction) Response {
	ctx := context.Background()
	var firstOpt *discordgo.ApplicationCommandInteractionDataOption
	if data, ok := i.Data.(*discordgo.ApplicationCommandInteractionData); ok && len(data.Options) > 0 {
		firstOpt = data.Options[0]
	}

	// Rust: match command.data.options.first()
	if firstOpt != nil {
		// Rust: if CommandDataOptionValue::String(name) => ...
		name, ok := firstOpt.Value.(string)
		if !ok {
			return errorResponse(invalidOptionText())
		}
		// Rust: match Features::all(&pool) { Ok(f) => handle_feature, Err(e) => handle_error }
		featuresList, err := features.All(ctx, h.app.Pool)
		if err != nil {
			return errorResponse(err.Error())
		}
		return h.handleFeature(ctx, featuresList, name)
	}

	// Rust's None branch: all → handle_list_features / handle_error.
	featuresList, err := features.All(ctx, h.app.Pool)
	if err != nil {
		return errorResponse(err.Error())
	}
	return listResponse(featuresList)
}

// handleFeature mirrors `Feat::handle_feature` (feat.rs:43-63): find the
// feature by name; on match, toggle it, then re-list and send the list;
// on miss, "Couldn't match feature".
func (h *Feat) handleFeature(ctx context.Context, featuresList []features.Feature, name string) Response {
	for _, f := range featuresList {
		if f.Name != name {
			continue
		}
		if err := features.Update(ctx, h.app.Pool, f.Name, !f.Enabled); err != nil {
			return errorResponse(updateErrorText(err))
		}
		// Rust: after a successful toggle, fetches the list again.
		upd, err := features.All(ctx, h.app.Pool)
		if err != nil {
			return errorResponse(err.Error())
		}
		return listResponse(upd)
	}
	return featureNotMatched()
}

// formatFeatureList mirrors `handle_list_features` (feat.rs:65-78): the
// header plus one "\nName: `<name>` Enabled: `<enabled>`" line per feature,
// in order.
func formatFeatureList(flist []features.Feature) string {
	content := "Here's all the features"
	for _, f := range flist {
		content = content + "\nName: `" + f.Name + "` Enabled: `" + enabledString(f.Enabled) + "`"
	}
	return content
}

func enabledString(on bool) string {
	if on {
		return "true"
	}
	return "false"
}

// Response constructors (Rust: the HandlerResponse literals at
// feat.rs:58-88; either branch is ephemeral: true).
func featureNotMatched() Response {
	return Response{Content: "Couldn't match feature", Ephemeral: true}
}

func listResponse(flist []features.Feature) Response {
	return Response{Content: formatFeatureList(flist), Ephemeral: true}
}

func errorResponse(content string) Response {
	return Response{Content: content, Ephemeral: true}
}

func updateErrorText(err error) string {
	return "Failed to update feature: " + err.Error()
}

func invalidOptionText() string {
	return "Please provide a valid feature name"
}
