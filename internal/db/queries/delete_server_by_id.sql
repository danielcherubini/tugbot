-- Rust: src/tugbot/servers.rs stale-row cleanup in get_servers —
-- `diesel::delete(servers.filter(id.eq(server_id)))`
-- name: delete_server_by_id :exec
DELETE FROM servers WHERE id = @id;
