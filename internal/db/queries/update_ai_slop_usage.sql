-- Rust: src/db/mod.rs increment_ai_slop_usage —
-- `diesel::update(ai_slop_usage.find(id)).set(usage_count, last_slop_at)`
-- name: update_ai_slop_usage :exec
UPDATE ai_slop_usage SET usage_count = @usage_count, last_slop_at = now() WHERE id = @id;
