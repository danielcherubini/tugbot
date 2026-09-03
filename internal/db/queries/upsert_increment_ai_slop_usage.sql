-- Rust: src/db/mod.rs atomic_increment_ai_slop —
-- `insert ... on_conflict((user_id, guild_id)).do_update().set(usage_count = usage_count + 1, last_slop_at = now())`
-- name: upsert_increment_ai_slop_usage :one
INSERT INTO ai_slop_usage (user_id, guild_id, usage_count, last_slop_at, created_at)
VALUES (@user_id, @guild_id, 1, now(), now())
ON CONFLICT (user_id, guild_id) DO UPDATE
SET usage_count = ai_slop_usage.usage_count + 1, last_slop_at = now()
RETURNING usage_count;
