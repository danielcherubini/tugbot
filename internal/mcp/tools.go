package mcp

import "errors"

// Shared small helpers for the bridge tools (tasks 4–5).

// errGuildRequired and errChannelNotFound are the sentinel errors
// resolveChannelID wraps so handlers can distinguish domain errors (toolErr
// results) from Discord API failures (wrapDiscordErr results).
var (
	errChannelRequired = errors.New("channel_id required")
	errGuildRequired   = errors.New("guild required to resolve channel by name")
	errChannelNotFound = errors.New("channel not found")
)

// isSnowflake is true for a non-empty all-numeric string (the v1
// snowflake-validation rule — no length/validation beyond digit-ness).
func isSnowflake(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolveChannelID resolves a channel_id argument to a numeric snowflake ID:
// numeric → used as-is (no REST, no guild needed); non-numeric → resolved
// by exact name match against the guild's channel list (one
// d.GuildChannels REST call). Missing guild for a name form returns
// errGuildRequired; no name match returns errChannelNotFound; a REST failure
// passes through (handlers route it to wrapDiscordErr).
func resolveChannelID(d DiscordAPI, guildID, nameOrID string) (string, error) {
	if nameOrID == "" {
		return "", errChannelRequired
	}
	if isSnowflake(nameOrID) {
		return nameOrID, nil
	}
	if guildID == "" {
		return "", errGuildRequired
	}
	chans, err := d.GuildChannels(guildID)
	if err != nil {
		return "", err
	}
	for _, c := range chans {
		if c.Name == nameOrID {
			return c.ID, nil
		}
	}
	return "", errChannelNotFound
}
