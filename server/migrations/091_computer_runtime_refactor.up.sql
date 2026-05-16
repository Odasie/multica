-- §6.4 / D4: long-lived daemon_token semantics.
-- Additive nullable column; existing schema, indexes, and the `expires_at > now()`
-- filter in GetDaemonTokenByHash stay intact. The query is updated to AND-stack
-- `revoked_at IS NULL` so revoke takes effect immediately, while the 100-year
-- expiry on mdt_ tokens means cleanup never collects a live credential.
ALTER TABLE daemon_token
    ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;

-- §6.2 / D3: install-time provenance carried on the daemon credential.
-- `created_by_user_id` is the install token's minter (the user who clicked
-- "Add Computer" in the UI) — DaemonRegister copies it into the new
-- agent_runtime row's owner_id so the script-installed Computer is owned by
-- the person who staged the install, not the daemon auth context (which has
-- no notion of a user). `install_source` is `script` for tokens minted via
-- POST /api/install-tokens (the one-liner install path); PAT-fallback
-- registers leave it NULL, and the handler maps NULL → `manual` /
-- `desktop_auto` based on the cli_version + launched_by metadata. Both
-- columns are nullable so existing rows are unaffected.
ALTER TABLE daemon_token
    ADD COLUMN IF NOT EXISTS created_by_user_id UUID REFERENCES "user"(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS install_source TEXT;

-- §6.4 / D5: short-lived install token (`mit_` prefix).
-- Single-use, 15-minute window, workspace-scoped at mint time. The exchange
-- endpoint flips `used_at` on success; subsequent calls return 401.
CREATE TABLE IF NOT EXISTS install_token (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash TEXT NOT NULL,
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    created_by_user_id UUID NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_install_token_hash ON install_token(token_hash);
CREATE INDEX IF NOT EXISTS idx_install_token_workspace ON install_token(workspace_id);
