-- Rust: src/db/mod.rs atomic_increment_goku_poll —
-- `insert ... on_conflict((user_id, guild_id)).do_update().set(usage_count = usage_count + 1, last_goku_at = now())`
-- name: upsert_increment_goku_poll_usage :one
INSERT INTO goku_poll_usage (user_id, guild_id, usage_count, last_goku_at, created_at)
VALUES (@user_id, @guild_id, 1, now(), now())
ON CONFLICT (user_id, guild_id) DO UPDATE
SET usage_count = goku_poll_usage.usage_count + 1, last_goku_at = now()
RETURNING usage_count;
