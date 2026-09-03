-- Rust: src/db/mod.rs bulk_upsert_activity —
-- `insert ... on_conflict((user_id, guild_id)).do_update().set(last_message_at = GREATEST(last_message_at, excluded.last_message_at))`
-- (one row per call; the Go caller loops per (user, guild) pair)
-- name: upsert_user_activity :exec
INSERT INTO user_activity (user_id, guild_id, last_message_at, created_at)
VALUES (@user_id, @guild_id, now(), now())
ON CONFLICT (user_id, guild_id) DO UPDATE
SET last_message_at = GREATEST(user_activity.last_message_at, now());
