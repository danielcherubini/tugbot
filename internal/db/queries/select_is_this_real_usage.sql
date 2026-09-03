-- Rust: src/db/mod.rs get_is_this_real_usage / get_or_create_is_this_real_usage (the select half)
-- name: select_is_this_real_usage :one
SELECT id, user_id, guild_id, last_used_at, created_at
FROM is_this_real_usage WHERE user_id = @user_id AND guild_id = @guild_id;
