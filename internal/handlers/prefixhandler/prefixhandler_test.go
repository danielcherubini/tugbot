package prefixhandler

import (
	"testing"
)

// TestFeatureKey pins the DB-key mapping: the interaction's command name IS
// the features-table key (Rust prefix_handler.rs:50-72 — `command.data.name`
// is passed verbatim to Features::check_enabled). The live keys are
// "horny" and "phony" — NOT "is_this_real" (that AGENTS.md note is about
// the COMMAND prefix, not the flag key).
func TestFeatureKey(t *testing.T) {
	if got := FeatureKey("horny"); got != "horny" {
		t.Errorf("FeatureKey(horny) = %q, want %q", got, "horny")
	}
	if got := FeatureKey("phony"); got != "phony" {
		t.Errorf("FeatureKey(phony) = %q, want %q", got, "phony")
	}
}

// TestCleanUsername mirrors clean_username (prefix_handler.rs:9-11): strips
// the "phony | " and "horny | " prefixes.
func TestCleanUsername(t *testing.T) {
	if got := cleanUsername("horny | foo"); got != "foo" {
		t.Errorf("cleanUsername(horny | foo) = %q, want foo", got)
	}
	if got := cleanUsername("phony | foo"); got != "foo" {
		t.Errorf("cleanUsername(phony | foo) = %q, want foo", got)
	}
	if got := cleanUsername("phony | horny | foo"); got != "foo" {
		t.Errorf("cleanUsername(phony | horny | foo) = %q, want foo", got)
	}
	if got := cleanUsername("unprefixed"); got != "unprefixed" {
		t.Errorf("cleanUsername(unprefixed) = %q, want unchanged", got)
	}
}

// TestFixNickname ports the nine Rust unit tests (prefix_handler.rs:118-168)
// 1:1, plus a multi-pipe case.
func TestFixNickname(t *testing.T) {
	tests := []struct {
		name     string
		nick     string
		prefix   string
		want     string
		wantAdds bool // whether the prefix result starts with the prefix
	}{
		{"horny adds", "foo", "horny", "horny | foo", true},
		{"phony adds", "foo", "phony", "phony | foo", true},
		{"swap", "horny | foo", "phony", "phony | foo", true},
		{"clean one", "horny | foo", "horny", "foo", false},
		{"clean all", "phony | horny | foo", "phony", "foo", false},
		{"empty nickname", "", "horny", "horny | ", true},
		{"multiple pipes", "other | prefix | username", "horny", "horny | other | prefix | username", true},
		{"already prefixed", "phony | username", "phony", "username", false},
		{"has other pipe, prefix has none", "foo | bar", "horny", "horny | foo | bar", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FixNickname(tt.nick, tt.prefix); got != tt.want {
				t.Errorf("FixNickname(%q, %q) = %q, want %q", tt.nick, tt.prefix, got, tt.want)
			}
		})
	}
}

// TestActionWord pins the Added/Removed branch (prefix_handler.rs:80-84:
// was_already_prefixed = current_nick.contains("prefix | ")).
func TestActionWordPrefix(t *testing.T) {
	if got := hasPrefix("horny | foo", "horny"); !got {
		t.Error(`hasPrefix("horny | foo", "horny") = false, want true (NickRemoved)`)
	}
	if got := hasPrefix("foo", "horny"); got {
		t.Error(`hasPrefix("foo", "horny") = true, want false (NickAdded)`)
	}
}
