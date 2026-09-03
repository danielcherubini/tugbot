-- Rust: src/handlers/gulag/mod.rs run_gulag_check (release + 404-cleanup) and
-- gulag_remove_handler.rs — `diesel::delete(gulag_users.filter(id.eq(...)))`
-- name: delete_gulag_user_by_id :exec
DELETE FROM gulag_users WHERE id = @id;
