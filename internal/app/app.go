// Package app is the central dependency-injection struct for the Go bot:
// every handler constructor receives an *App (mirroring Rust's Serenity
// TypeMap + get_pool keys).
package app

import (
	"context"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/config"
)

// PiImage mirrors Task 2's pirpc.Image (mime type + base64 data).
type PiImage struct {
	MimeType string
	Data     string
}

// PiBackend is the contract Task 2's *pirpc.PiRpc will satisfy. It is declared
// here (rather than importing the not-yet-existing internal/pirpc package) so
// Task 1 compiles standalone; Task 2 slots in its concrete type by assigning
// the *PiRpc value to App.Pi — it satisfies this interface with no changes.
type PiBackend interface {
	Ask(ctx context.Context, prompt string) (string, error)
	AskWithImages(ctx context.Context, prompt string, images []PiImage) (string, error)
	Stop()
}

// App is injected into every handler constructor (Rust: TypeMap keys).
//
// NOTE: the plan text's `*discordgo.Discordgo` is bwmarrin/discordgo's
// client type, which is named
// "Session" in every current release — the struct below uses that name.
type App struct {
	D *discordgo.Session

	Pool *pgxpool.Pool

	// Pi may be nil: pi startup is non-fatal (Task 7 mirrors Rust's
	// ready() — the mention feature degrades and logs `pi RPC not available`).
	Pi PiBackend

	Cfg *config.Config
}

// NewApp builds the App. Any startup failure before gateway start (config,
// pool, discordgo) is the caller's (cmd/tugbot main, Task 7) responsibility to
// log with slog.Error and exit non-zero, parity with Rust's expect().
func NewApp(cfg *config.Config, pool *pgxpool.Pool, d *discordgo.Session) *App {
	return &App{D: d, Pool: pool, Cfg: cfg}
}
