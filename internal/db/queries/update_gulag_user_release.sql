-- Rust: src/handlers/gulag/mod.rs run_gulag_check release —
-- `diesel::update(gulag_users.filter(id.eq(...))).set(in_gulag.eq(false))`
-- name: update_gulag_user_release :exec
UPDATE gulag_users SET in_gulag = false WHERE id = @id;
