-- Rust: src/handlers/gulag/gulag_list_handler.rs —
-- `gulag_users.filter(in_gulag.eq(true)).filter(guild_id.eq(...)).load()`
-- NOTE: `remod` (present in Rust schema.rs, absent from all migrations) is
-- omitted from the projection so this works against both DB shapes.
-- name: select_gulag_users_in_gulag_for_guild :many
SELECT id, user_id, guild_id, gulag_role_id, channel_id, in_gulag,
       gulag_length, created_at, release_at, message_id
FROM gulag_users WHERE in_gulag AND guild_id = @guild_id;
