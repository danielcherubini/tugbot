-- Rust: src/db/mod.rs send_to_gulag — `diesel::insert_into(gulag_users::table).values(&new_user).get_result`
-- (release_at = created_at + gulag_length * 1s, computed in Go before the call)
-- NOTE: the `remod` column exists in Rust's hand-maintained schema.rs but is
-- never created by any migration (schema drift). Rust writes remod=false; the
-- column default is false, so this INSERT omits the column and works against
-- both the production shape and a fresh baseline shape.
-- name: insert_gulag_user :one
INSERT INTO gulag_users (user_id, guild_id, gulag_role_id, channel_id, in_gulag, gulag_length, created_at, release_at, message_id)
VALUES (@user_id, @guild_id, @gulag_role_id, @channel_id, true, @gulag_length, @created_at, @release_at, @message_id)
RETURNING id;
