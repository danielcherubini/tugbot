-- Rust: src/db/mod.rs update_is_this_real_usage —
-- `diesel::update(is_this_real_usage.find(id)).set(last_used_at)`
-- name: update_is_this_real_usage_last_used :exec
UPDATE is_this_real_usage SET last_used_at = now() WHERE id = @id;
