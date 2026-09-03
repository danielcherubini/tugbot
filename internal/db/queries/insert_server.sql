-- Rust: src/db/mod.rs create_server — `diesel::insert_into(servers::table).values(&new_server).get_result`
-- name: insert_server :one
INSERT INTO servers (guild_id, gulag_id) VALUES (@guild_id, @gulag_id)
RETURNING id;
