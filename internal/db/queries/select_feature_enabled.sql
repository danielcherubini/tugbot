-- Rust: src/features/mod.rs Features::check_enabled / Features::is_enabled —
-- `features.filter(name.eq(...)).select(enabled).first::<bool>(...).optional()`
-- name: select_feature_enabled :one
SELECT enabled FROM features WHERE name = @name;
