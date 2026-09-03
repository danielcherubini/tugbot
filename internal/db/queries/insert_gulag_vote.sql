-- Rust: src/db/mod.rs new_gulag_vote — `diesel::insert_into(gulag_votes::table).values(&new_gulag_vote).get_result`
-- name: insert_gulag_vote :one
INSERT INTO gulag_votes (requester_id, sender_id, guild_id, channel_id, gulag_role_id, processed, message_id, created_at)
VALUES (@requester_id, @sender_id, @guild_id, @channel_id, @gulag_role_id, false, @message_id, now())
RETURNING id;
