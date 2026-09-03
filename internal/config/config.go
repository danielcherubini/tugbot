// Package config mirrors src/tugbot/config.rs of the Rust bot, including its
// dotenv().ok() call: when a ./.env file exists it is loaded FIRST (without
// overriding existing env vars), then the environment is read. Env var names
// are byte-identical to the Rust names.
package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config is the bot's runtime configuration (Rust `Config` + the Go-only
// SkillsDir/LogLevel fields needed by the Go port).
type Config struct {
	Token string

	// ApplicationID is the Discord application (bot) ID, as a string.
	ApplicationID string

	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string

	// AdminUserID is the Discord user ID that bypasses mention-feature
	// cooldowns. Kept for backward compatibility; prefer CooldownExemptUserIDs.
	// Default: 0 (disabled).
	AdminUserID int64

	// CooldownExemptUserIDs are the Discord user IDs that bypass the mention
	// cooldown entirely. Default: empty.
	CooldownExemptUserIDs map[int64]struct{}

	// SlowUserIDs are the Discord user IDs that get the slower cooldown (2h
	// instead of 5m) and trigger the auto-gulag on bot mention. Default: empty.
	SlowUserIDs map[int64]struct{}

	// SkillsDir is the resolved skills directory (see parseSkillsDir). The
	// TUGBOT_SKILLS_DIR var may point at the repo root or at the skills dir
	// itself. Default: the directory of os.Executable(); if that directory
	// contains no skills/ subdir (e.g. `go run` from a temp dir), the working
	// directory. Production: the built unit runs from /opt/tugbot where
	// skills/ lives; `go run` dev setups must set TUGBOT_SKILLS_DIR.
	SkillsDir string

	// LogLevel maps the RUST_LOG env var (name kept — existing
	// `journalctl -u tugbot` workflows rely on it) to a slog level.
	LogLevel slog.Level
}

// LoadConfig mirrors Rust's `Config::get_config()`: if a ./.env file exists it
// is loaded first (Rust's `dotenv().ok()` — errors are ignored), then the
// environment is read. It errors when DISCORD_TOKEN, APPLICATION_ID, or
// DATABASE_URL is empty, or when APPLICATION_ID is not a valid ID (Rust
// panics on the latter; Go returns an error instead).
func LoadConfig() (*Config, error) {
	// Rust: dotenv().ok()
	if _, err := os.Stat(".env"); err == nil {
		_ = godotenv.Load()
	}

	token := os.Getenv("DISCORD_TOKEN")
	dbURL := os.Getenv("DATABASE_URL")
	appIDStr := os.Getenv("APPLICATION_ID")

	var errs []string
	if token == "" {
		errs = append(errs, "expected DISCORD_TOKEN in the environment")
	}
	if appIDStr == "" {
		errs = append(errs, "expected APPLICATION_ID in the environment")
	}
	if dbURL == "" {
		errs = append(errs, "expected DATABASE_URL in the environment")
	}
	if len(errs) > 0 {
		return nil, &LoadError{problems: errs}
	}
	if _, err := strconv.ParseUint(appIDStr, 10, 64); err != nil {
		return nil, &LoadError{problems: []string{"APPLICATION_ID is not a valid id: " + appIDStr}}
	}

	// ADMIN_USER_ID — bypasses mention cooldowns. Default: 0 (disabled).
	// Rust: .ok().and_then(|s| s.parse().ok()).unwrap_or(0)
	adminUserID := parseID(os.Getenv("ADMIN_USER_ID"))

	// COOLDOWN_EXEMPT_USER_IDS — comma-separated Discord user IDs. Malformed
	// parts are skipped (Rust: filter_map).
	exempt := parseIDList(os.Getenv("COOLDOWN_EXEMPT_USER_IDS"))

	// ADMIN_USER_ID kept for backward compatibility — include it in the
	// exemption list if it's set to a non-zero value.
	if adminUserID != 0 {
		if _, ok := exempt[adminUserID]; !ok {
			exempt[adminUserID] = struct{}{}
		}
	}

	// SLOW_USER_IDS — same parsing as the exemption list.
	slow := parseIDList(os.Getenv("SLOW_USER_IDS"))

	return &Config{
		Token:                 token,
		ApplicationID:         appIDStr,
		DatabaseURL:           dbURL,
		AdminUserID:           adminUserID,
		CooldownExemptUserIDs: exempt,
		SlowUserIDs:           slow,
		SkillsDir:             parseSkillsDir(os.Getenv("TUGBOT_SKILLS_DIR")),
		LogLevel:              parseLogLevel(os.Getenv("RUST_LOG")),
	}, nil
}

// LoadError aggregates the problems found while loading the configuration.
type LoadError struct{ problems []string }

func (e *LoadError) Error() string { return strings.Join(e.problems, "; ") }

// parseID mirrors Rust's `.ok().and_then(|s| s.parse().ok()).unwrap_or(0)`:
// an unset or malformed value is treated as 0.
func parseID(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseIDList mirrors Rust's `s.split(',').filter_map(|part| part.trim().parse().ok())`:
// entries are trimmed, malformed entries are skipped (never an error).
func parseIDList(s string) map[int64]struct{} {
	out := map[int64]struct{}{}
	if s == "" {
		return out
	}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			continue
		}
		out[v] = struct{}{}
	}
	return out
}

// ParseIDList is the exported form of the comma-separated ID list parser
// (malformed parts are skipped), reused by later tasks that read the same
// env vars outside LoadConfig.
func ParseIDList(s string) map[int64]struct{} { return parseIDList(s) }

// parseSkillsDir keeps Rust's ends_with("skills") resolution (the var may point
// at the repo root or at the skills dir itself) with the Go port's default:
// when TUGBOT_SKILLS_DIR is unset, use the directory of os.Executable(); if
// that directory contains no skills/ subdir (e.g. `go run` from a temp dir),
// fall back to the working directory.
func parseSkillsDir(v string) string {
	base := v
	if base == "" {
		execDir := ""
		if p, err := os.Executable(); err == nil {
			execDir = filepath.Dir(p)
		}
		if execDir != "" {
			if st, err := os.Stat(filepath.Join(execDir, "skills")); err == nil && st.IsDir() {
				base = execDir
			}
		}
		if base == "" || !hasSkillsSubdir(base) {
			if wd, err := os.Getwd(); err == nil {
				base = wd
			}
		}
	}
	return resolveSkillsDir(base)
}

// resolveSkillsDir implements Rust's ends_with("skills") rule: a base that
// points at the skills dir itself is used as-is; a base pointing at the repo
// root gets /skills appended.
func resolveSkillsDir(base string) string {
	if strings.HasSuffix(strings.TrimRight(base, "/"), "skills") {
		return base
	}
	return filepath.Join(base, "skills")
}

func hasSkillsSubdir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "skills"))
	return err == nil && st.IsDir()
}

// parseLogLevel maps the RUST_LOG env var to a slog level: the first selector
// (before any ',' or '=') decides; "trace" maps to debug (slog has no trace
// level). Empty / "off" map to info, Rust's default verbosity.
//
// RUST_LOG also accepts env_logger's numeric scale, which is pinned here
// (env_logger: 0=error, 1=error (reserved/unused), 2=warn, 3=info, 4=debug,
// 5=trace): 0/1 -> LevelError, 2 -> LevelWarn, 3 -> LevelInfo, 4/5 ->
// LevelDebug (slog has no trace level, so trace is folded into debug.
// Values above 5 don't exist in env_logger; they also map to LevelDebug).
func parseLogLevel(v string) slog.Level {
	selector := v
	if i := strings.IndexAny(selector, ","); i >= 0 {
		selector = selector[:i]
	}
	if i := strings.IndexByte(selector, '='); i >= 0 {
		selector = selector[i+1:]
	}
	selector = strings.ToLower(strings.TrimSpace(selector))
	switch selector {
	case "error":
		return slog.LevelError
	case "warn", "warning":
		return slog.LevelWarn
	case "info", "", "off":
		return slog.LevelInfo
	case "0", "1": // env_logger: 0=error, 1=error (reserved)
		return slog.LevelError
	case "2": // env_logger: 2=warn
		return slog.LevelWarn
	case "3": // env_logger: 3=info
		return slog.LevelInfo
	default:
		// debug, trace, 4=debug, 5=trace (folded into debug), anything else
		// at/above debug.
		return slog.LevelDebug
	}
}

// LogLevelFromEnv is the exported form for the process logger setup.
func LogLevelFromEnv() slog.Level { return parseLogLevel(os.Getenv("RUST_LOG")) }
