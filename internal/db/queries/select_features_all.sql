-- Rust: src/features/mod.rs Features::all — `features.load(&mut conn)`
-- name: select_features_all :many
SELECT id, name, enabled FROM features;
