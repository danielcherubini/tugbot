-- Rust: src/db/mod.rs get_server_by_guild_id — `servers.filter(guild_id.eq(...)).first()`
-- name: select_server_by_guild_id :one
SELECT id, guild_id, gulag_id FROM servers WHERE guild_id = @guild_id;
