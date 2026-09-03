-- Rust: src/db/mod.rs get_or_create_ai_slop_usage (the select half) —
-- `ai_slop_usage.filter(user_id.eq(...)).filter(guild_id.eq(...)).first()`
-- name: select_ai_slop_usage :one
SELECT id, user_id, guild_id, usage_count, last_slop_at, created_at
FROM ai_slop_usage WHERE user_id = @user_id AND guild_id = @guild_id;
