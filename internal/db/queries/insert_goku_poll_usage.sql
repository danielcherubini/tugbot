-- Rust: src/db/mod.rs get_or_create_goku_poll_usage (the insert half, on NotFound)
-- name: insert_goku_poll_usage :one
INSERT INTO goku_poll_usage (user_id, guild_id, usage_count, last_goku_at, created_at)
VALUES (@user_id, @guild_id, 0, now(), now())
RETURNING id;
