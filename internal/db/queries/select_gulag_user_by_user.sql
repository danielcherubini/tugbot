-- Rust: src/handlers/gulag/mod.rs Gulag::is_user_in_gulag —
-- `gulag_users.filter(user_id.eq(...)).filter(in_gulag.eq(true)).first().optional()`
-- NOTE: `remod` (present in Rust schema.rs, absent from all migrations) is
-- omitted from the projection so this works against both DB shapes.
-- name: select_gulag_user_by_user :one
SELECT id, user_id, guild_id, gulag_role_id, channel_id, in_gulag,
       gulag_length, created_at, release_at, message_id
FROM gulag_users WHERE user_id = @user_id AND in_gulag;
