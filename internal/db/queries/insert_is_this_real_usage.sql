-- Rust: src/db/mod.rs get_or_create_is_this_real_usage (the insert half, on NotFound)
-- name: insert_is_this_real_usage :one
INSERT INTO is_this_real_usage (user_id, guild_id, last_used_at, created_at)
VALUES (@user_id, @guild_id, now(), now())
RETURNING id;
