-- Rust: src/db/mod.rs query_all_tracked_user_ids_for_guild —
-- `user_activity.filter(guild_id.eq(...)).select(user_id)`
-- name: select_tracked_user_ids :many
SELECT user_id FROM user_activity WHERE guild_id = @guild_id;
