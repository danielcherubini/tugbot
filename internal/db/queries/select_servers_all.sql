-- Rust: src/tugbot/servers.rs Servers::get_servers — `servers.load::<Server>`
-- name: select_servers_all :many
SELECT id, guild_id, gulag_id FROM servers;
