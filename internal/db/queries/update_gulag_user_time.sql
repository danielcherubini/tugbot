-- Rust: src/db/mod.rs add_time_to_gulag — `diesel::update(gulag_users.find(id)).set(gulag_length, release_at)`
-- name: update_gulag_user_time :exec
UPDATE gulag_users SET gulag_length = @gulag_length, release_at = @release_at WHERE id = @id;
