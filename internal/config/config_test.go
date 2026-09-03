package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// setEnv applies the env vars for a LoadConfig test. Helpers for table tests.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

// clearEnv ensures the vars are unset for a LoadConfig test.
// Note: t.Setenv cannot unset; we point them at "".
func validEnv() map[string]string {
	return map[string]string{
		"DISCORD_TOKEN":            "tok",
		"APPLICATION_ID":           "12345",
		"DATABASE_URL":             "postgres://u:p@localhost:5432/db",
		"ADMIN_USER_ID":            "",
		"COOLDOWN_EXEMPT_USER_IDS": "",
		"SLOW_USER_IDS":            "",
		"TUGBOT_SKILLS_DIR":        "",
		"RUST_LOG":                 "",
	}
}

func TestLoadConfigValid(t *testing.T) {
	vars := validEnv()
	vars["TUGBOT_SKILLS_DIR"] = "/tmp/skills-checkout"
	setEnv(t, vars)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}
	if cfg.Token != "tok" {
		t.Errorf("Token = %q, want %q", cfg.Token, "tok")
	}
	if cfg.ApplicationID != "12345" {
		t.Errorf("ApplicationID = %q, want %q", cfg.ApplicationID, "12345")
	}
	if cfg.DatabaseURL != "postgres://u:p@localhost:5432/db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.AdminUserID != 0 {
		t.Errorf("AdminUserID = %d, want 0", cfg.AdminUserID)
	}
	if len(cfg.CooldownExemptUserIDs) != 0 {
		t.Errorf("CooldownExemptUserIDs = %v, want empty", cfg.CooldownExemptUserIDs)
	}
	if len(cfg.SlowUserIDs) != 0 {
		t.Errorf("SlowUserIDs = %v, want empty", cfg.SlowUserIDs)
	}
	// Rust's ends_with("skills") resolution: a repo root points at <dir>/skills.
	if cfg.SkillsDir != filepath.Join("/tmp/skills-checkout", "skills") {
		t.Errorf("SkillsDir = %q, want %q", cfg.SkillsDir, filepath.Join("/tmp/skills-checkout", "skills"))
	}
	if len(cfg.SkillsDir) > 0 && filepath.Base(cfg.SkillsDir) == "skills" {
		// OK: the resolved path ends in skills.
	} else {
		t.Errorf("SkillsDir %q should resolve to a path ending in 'skills'", cfg.SkillsDir)
	}
}

func TestLoadConfigSkillsDirBothForms(t *testing.T) {
	// The var may point at the repo root OR at the skills dir itself.
	for _, tc := range []struct {
		name, in, want string
	}{
		{"repo root", "/tmp/repo", filepath.Join("/tmp/repo", "skills")},
		{"skills dir itself", "/tmp/repo/skills", "/tmp/repo/skills"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := validEnv()
			vars["TUGBOT_SKILLS_DIR"] = tc.in
			setEnv(t, vars)
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if cfg.SkillsDir != tc.want {
				t.Errorf("SkillsDir = %q, want %q", cfg.SkillsDir, tc.want)
			}
		})
	}
}

func TestLoadConfigDefaultSkillsDir(t *testing.T) {
	vars := validEnv()
	setEnv(t, vars)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	execDir := ""
	if p, err := os.Executable(); err == nil {
		execDir = filepath.Dir(p)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	execHasSkills := false
	if _, err := os.Stat(filepath.Join(execDir, "skills")); err == nil {
		execHasSkills = true
	}

	want := filepath.Join(cwd, "skills")
	if execHasSkills {
		want = filepath.Join(execDir, "skills")
	}
	// Base resolution then ends_with resolution; a base that already ends in
	// 'skills' is used as-is (it has a skills/ subdir of itself only when it IS
	// that — resolution leaves it alone).
	if execHasSkills && filepath.Base(execDir) == "skills" {
		want = execDir
	}
	if cfg.SkillsDir != want {
		t.Errorf("SkillsDir default = %q, want %q", cfg.SkillsDir, want)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	for _, missing := range []string{"DISCORD_TOKEN", "APPLICATION_ID", "DATABASE_URL"} {
		t.Run(missing, func(t *testing.T) {
			vars := validEnv()
			vars[missing] = ""
			setEnv(t, vars)
			if _, err := LoadConfig(); err == nil {
				t.Errorf("LoadConfig() with %s empty: error = nil, want error", missing)
			}
		})
	}
}

func TestLoadConfigMalformedApplicationID(t *testing.T) {
	vars := validEnv()
	vars["APPLICATION_ID"] = "notA1d"
	setEnv(t, vars)
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() with malformed APPLICATION_ID: error = nil, want error (Rust panics here)")
	}
}

func TestLoadConfigMalformedUserIDLists(t *testing.T) {
	for _, tc := range []struct {
		name   string
		envKey string
		value  string
	}{
		{"cooldown exemptions", "COOLDOWN_EXEMPT_USER_IDS", "1,abc, 2 ,x"},
		{"slow user ids", "SLOW_USER_IDS", "not-a-number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := validEnv()
			vars[tc.envKey] = tc.value
			setEnv(t, vars)
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			// Rust: filter_map — malformed parts are skipped, not fatal.
			if tc.envKey == "COOLDOWN_EXEMPT_USER_IDS" {
				if len(cfg.CooldownExemptUserIDs) != 2 {
					t.Fatalf("CooldownExemptUserIDs = %v, want {1,2}", cfg.CooldownExemptUserIDs)
				}
				for _, id := range []int64{1, 2} {
					if _, ok := cfg.CooldownExemptUserIDs[id]; !ok {
						t.Errorf("CooldownExemptUserIDs missing %d", id)
					}
				}
			} else if len(cfg.SlowUserIDs) != 0 {
				t.Errorf("SlowUserIDs = %v, want empty", cfg.SlowUserIDs)
			}
		})
	}
}

func TestLoadConfigMalformedAdminUserID(t *testing.T) {
	// Rust: .ok().and_then(|s| s.parse().ok()).unwrap_or(0) — malformed → 0, no merge.
	vars := validEnv()
	vars["ADMIN_USER_ID"] = "not-a-number"
	vars["COOLDOWN_EXEMPT_USER_IDS"] = "7"
	setEnv(t, vars)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.AdminUserID != 0 {
		t.Errorf("AdminUserID = %d, want 0", cfg.AdminUserID)
	}
	if len(cfg.CooldownExemptUserIDs) != 1 {
		t.Errorf("CooldownExemptUserIDs = %v, want {7} only (no admin merge)", cfg.CooldownExemptUserIDs)
	}
}

func TestLoadConfigAdminUserIDLegacyMerge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		admin   string
		exempt  string
		wantIDs []int64
	}{
		{"admin not in list", "9", "1,2", []int64{1, 2, 9}},
		{"admin already in list", "9", "9,2", []int64{9, 2}},
		{"admin zero does not merge", "0", "1", []int64{1}},
		{"admin alone", "5", "", []int64{5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := validEnv()
			vars["ADMIN_USER_ID"] = tc.admin
			vars["COOLDOWN_EXEMPT_USER_IDS"] = tc.exempt
			setEnv(t, vars)
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if len(cfg.CooldownExemptUserIDs) != len(tc.wantIDs) {
				t.Fatalf("CooldownExemptUserIDs = %v, want %v", cfg.CooldownExemptUserIDs, tc.wantIDs)
			}
			for _, id := range tc.wantIDs {
				if _, ok := cfg.CooldownExemptUserIDs[id]; !ok {
					t.Errorf("CooldownExemptUserIDs missing %d: %v", id, cfg.CooldownExemptUserIDs)
				}
			}
		})
	}
}

func TestLoadConfigSlowUserIDs(t *testing.T) {
	vars := validEnv()
	vars["SLOW_USER_IDS"] = "101, 202 ,bad"
	setEnv(t, vars)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	for _, id := range []int64{101, 202} {
		if _, ok := cfg.SlowUserIDs[id]; !ok {
			t.Errorf("SlowUserIDs missing %d: %v", id, cfg.SlowUserIDs)
		}
	}
	if len(cfg.SlowUserIDs) != 2 {
		t.Errorf("SlowUserIDs = %v, want 2 entries", cfg.SlowUserIDs)
	}
}

func TestLoadConfigLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rustLog   string
		wantLevel slog.Level
	}{
		{"empty defaults to info", "", slog.LevelInfo},
		{"error", "error", slog.LevelError},
		{"warn", "warn", slog.LevelWarn},
		{"info", "info", slog.LevelInfo},
		{"debug", "debug", slog.LevelDebug},
		{"trace maps to debug", "trace", slog.LevelDebug},
		{"module-scoped form takes first selector", "info,tugbot=debug", slog.LevelInfo},
		{"off defaults to info (Rust default verbosity)", "off", slog.LevelInfo},
		{"directive with scope prefix", "tugbot=debug", slog.LevelDebug},
		// env_logger numeric scale (0=error, 2=warn, 3=info, 4=debug, 5=trace).
		{"numeric 0 maps to error", "0", slog.LevelError},
		{"numeric 1 maps to error", "1", slog.LevelError},
		{"numeric 2 maps to warn", "2", slog.LevelWarn},
		{"numeric 3 maps to info", "3", slog.LevelInfo},
		{"numeric 4 maps to debug", "4", slog.LevelDebug},
		{"numeric 5 maps to debug (no trace in slog)", "5", slog.LevelDebug},
		{"numeric scoped form takes first selector", "1,tugbot=3", slog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := validEnv()
			vars["RUST_LOG"] = tc.rustLog
			setEnv(t, vars)
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if cfg.LogLevel != tc.wantLevel {
				t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, tc.wantLevel)
			}
		})
	}
}

func TestLoadConfigDotenvDoesNotOverwriteExistingEnv(t *testing.T) {
	// dotenv (Rust and godotenv alike) does not overwrite existing env vars.
	t.Setenv("DISCORD_TOKEN", "existing-wins")
	// t.Setenv restored after; the .env file is created in CWD and cleaned up.
	envFile := ".env"
	if err := os.WriteFile(envFile, []byte("DISCORD_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", envFile, err)
	}
	t.Cleanup(func() { _ = os.Remove(envFile) })

	for k, v := range validEnv() {
		if k == "DISCORD_TOKEN" {
			continue
		}
		t.Setenv(k, v)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Token != "existing-wins" {
		t.Errorf("Token = %q, want existing-wins (dotenv must not override)", cfg.Token)
	}
}

func TestLoadConfigDotenvOverridesWhenUnset(t *testing.T) {
	envFile := ".env"
	if err := os.WriteFile(envFile, []byte("DISCORD_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", envFile, err)
	}
	t.Cleanup(func() { _ = os.Remove(envFile) })
	_ = os.Unsetenv("DISCORD_TOKEN")
	t.Cleanup(func() { _ = os.Unsetenv("DISCORD_TOKEN") })

	for k, v := range validEnv() {
		if k == "DISCORD_TOKEN" {
			continue
		}
		t.Setenv(k, v)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v (if .env had godotenv access it would have supplied the token)", err)
	}
	if cfg.Token != "from-file" {
		t.Errorf("Token = %q, want from-file (dotenv value when var unset)", cfg.Token)
	}
}
