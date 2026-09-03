-- Rust: src/features/mod.rs Features::update — `diesel::update(...).set(enabled.eq(...))`
-- name: update_feature_enabled :exec
UPDATE features SET enabled = @enabled WHERE name = @name;
