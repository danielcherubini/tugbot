-- Rust: src/handlers/gulag/mod.rs run_gulag_check —
-- `gulag_users.filter(in_gulag.eq(true)).filter(release_at.le(now)).for_update().skip_locked().load()`
-- NOTE: `remod` (present in Rust schema.rs, absent from all migrations) is
-- omitted from the projection so this works against both DB shapes.
-- name: select_gulag_users_releasable :many
SELECT id, user_id, guild_id, gulag_role_id, channel_id, in_gulag,
       gulag_length, created_at, release_at, message_id
FROM gulag_users
WHERE in_gulag AND release_at <= @now
FOR UPDATE SKIP LOCKED;
