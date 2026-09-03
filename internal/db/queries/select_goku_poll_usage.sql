-- Rust: src/db/mod.rs get_or_create_goku_poll_usage (the select half)
-- name: select_goku_poll_usage :one
SELECT id, user_id, guild_id, usage_count, last_goku_at, created_at
FROM goku_poll_usage WHERE user_id = @user_id AND guild_id = @guild_id;
