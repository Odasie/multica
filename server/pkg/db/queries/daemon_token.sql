-- name: CreateDaemonToken :one
INSERT INTO daemon_token (
    token_hash,
    workspace_id,
    daemon_id,
    expires_at,
    created_by_user_id,
    install_source
)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetActiveDaemonTokenInstall :one
-- Returns the install-time provenance (creator + source) for the active
-- daemon_token bound to (workspace_id, daemon_id). DaemonRegister calls
-- this on the mdt_ auth branch to seed agent_runtime.owner_id and
-- metadata.install_source so script-installed Computers carry their
-- minter identity through the register path. Picks the most recent
-- non-revoked row if multiple mdt_ tokens exist for the same daemon.
SELECT created_by_user_id, install_source
FROM daemon_token
WHERE workspace_id = $1
  AND daemon_id = $2
  AND revoked_at IS NULL
  AND expires_at > now()
ORDER BY created_at DESC
LIMIT 1;

-- name: GetDaemonTokenByHash :one
-- §6.4 / D4: keep the original `expires_at > now()` filter and AND-stack
-- `revoked_at IS NULL`. mdt_ tokens are issued with expires_at = now() + 100y,
-- so the live path uses revoked_at as the kill switch while the cleanup query
-- below stays untouched and still collects genuinely expired rows.
SELECT * FROM daemon_token
WHERE token_hash = $1 AND expires_at > now() AND revoked_at IS NULL;

-- name: RevokeDaemonToken :exec
-- §6.4 / D4 explicit revoke path. Sets revoked_at = now(); GetDaemonTokenByHash
-- starts returning no rows immediately. The row itself stays until the 100-year
-- expires_at + cleanup catches it (purely a tidy-up — auth is already dead).
UPDATE daemon_token
SET revoked_at = now()
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: RevokeDaemonTokensByWorkspaceAndDaemon :many
-- §6.3 DELETE /api/computers/<daemon_id>: revoke every daemon_token for this
-- (workspace, daemon) pair without touching tokens that belong to the same
-- daemon in other workspaces. Returns token_hash so the caller can invalidate
-- DaemonTokenCache before the 10-minute TTL expires.
UPDATE daemon_token
SET revoked_at = now()
WHERE workspace_id = $1
  AND daemon_id = $2
  AND revoked_at IS NULL
RETURNING token_hash;

-- name: DeleteDaemonTokensByWorkspaceAndDaemons :many
-- Deletes every daemon_token row matching the (workspace_id, daemon_id)
-- pairs implied by `daemon_ids`. Used by the member-revocation flow to
-- nuke tokens for all runtimes a leaving member owned in one shot.
-- Returns token_hash so the caller can invalidate auth.DaemonTokenCache
-- before the 10-minute TTL expires — without that invalidate, a daemon
-- can keep using its stale token until cache eviction even though the
-- DB row is gone.
DELETE FROM daemon_token
WHERE workspace_id = @workspace_id
  AND daemon_id = ANY(@daemon_ids::text[])
RETURNING token_hash;

-- name: DeleteExpiredDaemonTokens :exec
DELETE FROM daemon_token
WHERE expires_at <= now();
