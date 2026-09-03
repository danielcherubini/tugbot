-- Rust: src/db/mod.rs get_or_create_ai_slop_usage (the insert half, on NotFound)
-- name: insert_ai_slop_usage :one
INSERT INTO ai_slop_usage (user_id, guild_id, usage_count, last_slop_at, created_at)
VALUES (@user_id, @guild_id, 0, now(), now())
RETURNING id;
