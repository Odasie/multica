package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// §6.4 / D5: short-lived install token. 15-minute window, single use.
// Anything longer widens the leak window (screenshots, clipboard, chat
// paste) without giving real ergonomic benefit — the user is staring at
// the modal while the script runs.
const installTokenTTL = 15 * time.Minute

// §6.4 / D4: mdt_ tokens behave "long-lived until revoke". The existing
// daemon_token schema requires expires_at NOT NULL and GetDaemonTokenByHash
// keeps the `expires_at > now()` filter, so we encode "never expires" as
// a 100-year future timestamp. Revoke is the real kill switch.
const daemonTokenLifetime = 100 * 365 * 24 * time.Hour

// MintInstallTokenRequest is the body of POST /api/install-tokens.
// Workspace is taken from the request context (X-Workspace-ID header), not
// the body, so a member of workspace A can never mint a token bound to
// workspace B.
type MintInstallTokenRequest struct{}

type MintInstallTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// MintInstallToken issues a single-use `mit_` install token bound to the
// caller's current workspace. The token leaves the server exactly once;
// only its hash is persisted.
func (h *Handler) MintInstallToken(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	token, err := generateInstallToken()
	if err != nil {
		slog.Error("mint install token: generate", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to mint install token")
		return
	}

	expiresAt := time.Now().Add(installTokenTTL)
	row, err := h.Queries.CreateInstallToken(r.Context(), db.CreateInstallTokenParams{
		TokenHash:       auth.HashToken(token),
		WorkspaceID:     parseUUID(workspaceID),
		CreatedByUserID: member.UserID,
		ExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		slog.Error("mint install token: insert", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to mint install token")
		return
	}

	writeJSON(w, http.StatusCreated, MintInstallTokenResponse{
		Token:     token,
		ExpiresAt: timestampToString(row.ExpiresAt),
	})
}

// ExchangeInstallTokenRequest is the body of POST /api/install-tokens/exchange.
// daemon_id is generated client-side (uuid) before the exchange call so the
// daemon's identity is stable across the very first heartbeat.
type ExchangeInstallTokenRequest struct {
	Token    string `json:"token"`
	DaemonID string `json:"daemon_id"`
}

type ExchangeInstallTokenResponse struct {
	DaemonToken string `json:"daemon_token"`
	WorkspaceID string `json:"workspace_id"`
	DaemonID    string `json:"daemon_id"`
	ExpiresAt   string `json:"expires_at"`
}

// ExchangeInstallToken consumes a `mit_` and returns a long-lived `mdt_`
// bound to (workspace_id from the mit_, daemon_id supplied by the caller).
// Endpoint is intentionally unauthenticated apart from the install token
// itself — the install.sh script doesn't have a PAT at this point.
//
// D5 contract: strict single use. We rely on ConsumeInstallToken atomically
// flipping `used_at`. A second call against the same mit_ matches zero rows
// and returns 401 install_token_already_used. There is no token-layer
// idempotency for network blips; the daemon's keychain decides whether to
// retry (no mdt_ written) or skip (mdt_ already written).
func (h *Handler) ExchangeInstallToken(w http.ResponseWriter, r *http.Request) {
	var body ExchangeInstallTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	body.DaemonID = strings.TrimSpace(body.DaemonID)
	if body.Token == "" || !strings.HasPrefix(body.Token, "mit_") {
		writeError(w, http.StatusBadRequest, "invalid install token")
		return
	}
	if body.DaemonID == "" {
		writeError(w, http.StatusBadRequest, "daemon_id is required")
		return
	}

	consumed, err := h.Queries.ConsumeInstallToken(r.Context(), auth.HashToken(body.Token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either never existed, already used, or expired. Distinguish
			// "already used" from "expired/unknown" with a follow-up read
			// so the install.sh can surface the precise error.
			existing, lookupErr := h.Queries.GetInstallToken(r.Context(), auth.HashToken(body.Token))
			if lookupErr == nil && existing.UsedAt.Valid {
				writeError(w, http.StatusUnauthorized, "install_token_already_used")
				return
			}
			writeError(w, http.StatusUnauthorized, "install_token_invalid_or_expired")
			return
		}
		slog.Error("exchange install token: consume", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to exchange install token")
		return
	}

	daemonToken, err := auth.GenerateDaemonToken()
	if err != nil {
		slog.Error("exchange install token: generate daemon token", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to issue daemon token")
		return
	}

	expiresAt := time.Now().Add(daemonTokenLifetime)
	row, err := h.Queries.CreateDaemonToken(r.Context(), db.CreateDaemonTokenParams{
		TokenHash:   auth.HashToken(daemonToken),
		WorkspaceID: consumed.WorkspaceID,
		DaemonID:    body.DaemonID,
		ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
		// D3 / §6.2: persist install-time provenance so DaemonRegister can
		// hydrate the new agent_runtime row's owner_id (= the user who
		// minted the install token) and metadata.install_source. Without
		// this, every script-installed Computer is ownerless and shows
		// "Unknown" install source in the UI — and a non-admin installer
		// is not recognised as owner by canRemove / canEditRuntime.
		CreatedByUserID: consumed.CreatedByUserID,
		InstallSource:   pgtype.Text{String: "script", Valid: true},
	})
	if err != nil {
		slog.Error("exchange install token: insert daemon token", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to issue daemon token")
		return
	}

	slog.Info(
		"install token exchanged",
		"workspace_id", uuidToString(consumed.WorkspaceID),
		"daemon_id", body.DaemonID,
	)

	writeJSON(w, http.StatusOK, ExchangeInstallTokenResponse{
		DaemonToken: daemonToken,
		WorkspaceID: uuidToString(consumed.WorkspaceID),
		DaemonID:    body.DaemonID,
		ExpiresAt:   timestampToString(row.ExpiresAt),
	})
}

// generateInstallToken: 20 random bytes hex-encoded with a `mit_` prefix,
// matching the existing `mul_` / `mdt_` token shapes so install.sh and
// internal regex-based scrubbers can detect token strings uniformly.
func generateInstallToken() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "mit_" + hex.EncodeToString(b), nil
}
