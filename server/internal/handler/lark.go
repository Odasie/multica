package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// LarkInstallationResponse is the wire shape for an installation row.
// `app_secret_encrypted` is INTENTIONALLY absent — the encrypted blob
// is server-internal and there is no product reason to expose it (the
// only consumer that needs the plaintext is the WS hub, which calls
// InstallationService.DecryptAppSecret server-side). Likewise, the WS
// lease columns are omitted; they are runtime state, not API surface.
type LarkInstallationResponse struct {
	ID              string  `json:"id"`
	WorkspaceID     string  `json:"workspace_id"`
	AgentID         string  `json:"agent_id"`
	AppID           string  `json:"app_id"`
	TenantKey       *string `json:"tenant_key,omitempty"`
	BotOpenID       string  `json:"bot_open_id"`
	InstallerUserID string  `json:"installer_user_id"`
	Status          string  `json:"status"`
	InstalledAt     string  `json:"installed_at"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

func larkInstallationToResponse(row db.LarkInstallation) LarkInstallationResponse {
	resp := LarkInstallationResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		AgentID:         uuidToString(row.AgentID),
		AppID:           row.AppID,
		BotOpenID:       row.BotOpenID,
		InstallerUserID: uuidToString(row.InstallerUserID),
		Status:          row.Status,
		InstalledAt:     row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if row.TenantKey.Valid {
		tk := row.TenantKey.String
		resp.TenantKey = &tk
	}
	return resp
}

// CreateLarkInstallationRequest is the manual-install payload (admin
// pastes credentials from the Lark developer console). The OAuth
// callback path lands credentials via the same InstallationService
// pipeline, so adding it later does not change this contract.
type CreateLarkInstallationRequest struct {
	AgentID   string `json:"agent_id"`
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
	TenantKey string `json:"tenant_key,omitempty"`
	BotOpenID string `json:"bot_open_id"`
}

// CreateLarkInstallation (POST /api/workspaces/{id}/lark/installations)
// is admin-only at the router level. It performs the at-rest
// encryption of `app_secret` via InstallationService and refuses to
// fall back to plaintext storage when the master key is unset (503).
func (h *Handler) CreateLarkInstallation(w http.ResponseWriter, r *http.Request) {
	if h.LarkInstallations == nil {
		writeError(w, http.StatusServiceUnavailable, "lark integration not configured (MULTICA_LARK_SECRET_KEY)")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	var req CreateLarkInstallationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, req.AgentID, "agent_id")
	if !ok {
		return
	}
	// Validate the agent really belongs to this workspace before we
	// accept its credentials. Without this guard a workspace admin
	// could install Lark on an agent in a different workspace by
	// supplying that workspace's agent_id.
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	installerUUID, ok := parseUUIDOrBadRequest(w, userID, "installer user id")
	if !ok {
		return
	}

	inst, err := h.LarkInstallations.Upsert(r.Context(), lark.InstallationParams{
		WorkspaceID:     wsUUID,
		AgentID:         agentUUID,
		AppID:           req.AppID,
		AppSecret:       req.AppSecret,
		TenantKey:       req.TenantKey,
		BotOpenID:       req.BotOpenID,
		InstallerUserID: installerUUID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp := larkInstallationToResponse(inst)
	h.publish(protocol.EventLarkInstallationCreated, uuidToString(wsUUID), "user", userID, resp)
	writeJSON(w, http.StatusCreated, resp)
}

// ListLarkInstallations (GET /api/workspaces/{id}/lark/installations)
// is member-visible — the Integrations tab should not render blank
// for non-admins. Unlike the GitHub list, we do not strip any field
// here because no API surface column doubles as a management handle:
// revocation goes by the UUID id, which is meaningless without the
// admin route's authorization, so exposing it is harmless.
//
// Response fields:
//   - configured: at-rest encryption key is set (`LarkInstallations
//     != nil`). When false, no install flow can succeed at all; the
//     UI hides the tab.
//   - install_supported: a real Lark APIClient is wired (not the
//     stub). When false, the at-rest path works for already-installed
//     bots but new installs via OAuth would fail at the exchange
//     step; the UI hides install entry points (the Settings tab
//     surfaces a "coming soon" notice and the agent-detail "Bind to
//     Lark" button is hidden). This lets us land the install UI
//     incrementally without exposing a flow that is guaranteed to
//     fail end-to-end.
func (h *Handler) ListLarkInstallations(w http.ResponseWriter, r *http.Request) {
	if h.LarkInstallations == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"installations":     []LarkInstallationResponse{},
			"configured":        false,
			"install_supported": false,
		})
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	rows, err := h.LarkInstallations.ListByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list lark installations")
		return
	}
	out := make([]LarkInstallationResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, larkInstallationToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations":     out,
		"configured":        true,
		"install_supported": h.LarkAPIClient != nil && h.LarkAPIClient.IsConfigured(),
	})
}

// RevokeLarkInstallation (DELETE /api/workspaces/{id}/lark/installations/{installationId})
// flips status to 'revoked' so the WS hub drops the connection on its
// next sweep. The row itself is preserved for audit; a re-install via
// CreateLarkInstallation flips status back to 'active' atomically.
func (h *Handler) RevokeLarkInstallation(w http.ResponseWriter, r *http.Request) {
	if h.LarkInstallations == nil {
		writeError(w, http.StatusServiceUnavailable, "lark integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	// Workspace-scoped lookup ensures one workspace cannot revoke
	// another's installation by guessing the UUID.
	if _, err := h.LarkInstallations.GetInWorkspace(r.Context(), instUUID, wsUUID); err != nil {
		if errors.Is(err, lark.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "lark installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	if err := h.LarkInstallations.Revoke(r.Context(), instUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return
	}
	h.publish(protocol.EventLarkInstallationRevoked, uuidToString(wsUUID), "user", userID, map[string]any{
		"id": uuidToString(instUUID),
	})
	w.WriteHeader(http.StatusNoContent)
}

// RedeemLarkBindingTokenRequest carries the raw token the user
// clicked through from the Bot's "you need to bind" reply card.
type RedeemLarkBindingTokenRequest struct {
	Token string `json:"token"`
}

// RedeemLarkBindingTokenResponse is the post-redemption shape. We
// echo the workspace/installation/open_id so the frontend can render
// "you are now bound to <workspace> via <agent>" without a second
// fetch.
type RedeemLarkBindingTokenResponse struct {
	WorkspaceID    string `json:"workspace_id"`
	InstallationID string `json:"installation_id"`
	LarkOpenID     string `json:"lark_open_id"`
}

// RedeemLarkBindingToken (POST /api/lark/binding/redeem) is the only
// path that writes a lark_user_binding row from user-driven action.
// The redeemer's identity is taken from the session, not the token,
// so a stolen token cannot bind a Lark open_id to an attacker's
// Multica account. The token only proves "this open_id requested
// binding" — combining it with the logged-in user is what creates
// the (open_id ↔ user) mapping.
//
// Consume + bind happen inside a single DB transaction (see
// lark.BindingTokenService.RedeemAndBind). The three failure modes
// each map to a distinct status code so the frontend can render the
// appropriate copy without a separate probe:
//   - 410 Gone:       token unknown / consumed / expired
//   - 409 Conflict:   open_id is already bound to a different user
//   - 403 Forbidden:  redeemer is not a workspace member
func (h *Handler) RedeemLarkBindingToken(w http.ResponseWriter, r *http.Request) {
	if h.LarkBindingTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "lark integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req RedeemLarkBindingTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}

	redeemed, err := h.LarkBindingTokens.RedeemAndBind(r.Context(), req.Token, userUUID)
	if err != nil {
		switch {
		case errors.Is(err, lark.ErrBindingTokenInvalid):
			writeError(w, http.StatusGone, "binding token invalid or expired")
		case errors.Is(err, lark.ErrBindingAlreadyAssigned):
			writeError(w, http.StatusConflict, "this Lark account is already bound to a different Multica user")
		case errors.Is(err, lark.ErrBindingNotWorkspaceMember):
			writeError(w, http.StatusForbidden, "binding refused (are you a workspace member?)")
		default:
			writeError(w, http.StatusInternalServerError, "failed to redeem token")
		}
		return
	}

	writeJSON(w, http.StatusOK, RedeemLarkBindingTokenResponse{
		WorkspaceID:    uuidToString(redeemed.WorkspaceID),
		InstallationID: uuidToString(redeemed.InstallationID),
		LarkOpenID:     string(redeemed.LarkOpenID),
	})
}

// StartLarkInstallResponse is the payload the agent detail "bind to
// Lark" button consumes. The frontend renders `url` as a QR (and as a
// clickable link fallback) so a phone user can scan it instead of
// typing.
type StartLarkInstallResponse struct {
	URL        string `json:"url"`
	Configured bool   `json:"configured"`
}

// StartLarkInstall (GET /api/workspaces/{id}/lark/install/start?agent_id=...)
// returns the Lark OAuth authorization URL the user must open (or
// scan as QR) to bind a Bot to the supplied agent. Admin-only at the
// router. The state token signed inside the URL binds workspace +
// agent + initiator, so the callback persists the installation
// against the correct rows without trusting query params.
func (h *Handler) StartLarkInstall(w http.ResponseWriter, r *http.Request) {
	if h.LarkInstallations == nil {
		writeError(w, http.StatusServiceUnavailable, "lark integration not configured (MULTICA_LARK_SECRET_KEY)")
		return
	}
	// install_supported gate. We check the APIClient before LarkOAuth
	// for two reasons:
	//   1. Even with OAuth env set, if the underlying APIClient is
	//      the stub (no real Lark HTTP transport wired yet), the
	//      callback will fail at the exchange step. Reporting
	//      `configured: false` here short-circuits the frontend
	//      before it sends the user into a flow that is guaranteed
	//      to break end-to-end.
	//   2. It makes the test seam tractable: stub APIClient +
	//      whatever LarkOAuth state produces a configured:false
	//      response without requiring the test to fully construct
	//      LarkOAuth (which needs a DB-backed InstallationService).
	if h.LarkAPIClient == nil || !h.LarkAPIClient.IsConfigured() {
		writeJSON(w, http.StatusOK, StartLarkInstallResponse{Configured: false})
		return
	}
	if h.LarkOAuth == nil {
		writeJSON(w, http.StatusOK, StartLarkInstallResponse{Configured: false})
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	agentIDStr := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentIDStr == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, agentIDStr, "agent_id")
	if !ok {
		return
	}
	// Reject install attempts against agents from other workspaces;
	// the OAuth state would otherwise let one workspace admin install
	// a Bot on another workspace's agent (the URL state is signed,
	// but pre-state validation is what blocks the attempt earliest).
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	installerUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	res, err := h.LarkOAuth.StartInstall(lark.StartInstallParams{
		WorkspaceID: wsUUID,
		AgentID:     agentUUID,
		InitiatorID: installerUUID,
	})
	if err != nil {
		if errors.Is(err, lark.ErrOAuthNotConfigured) {
			writeJSON(w, http.StatusOK, StartLarkInstallResponse{Configured: false})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to start install")
		return
	}
	writeJSON(w, http.StatusOK, StartLarkInstallResponse{URL: res.URL, Configured: true})
}

// LarkInstallCallback (GET /api/lark/install/callback) is the redirect
// destination Lark calls after a user authorizes the install. It is
// outside the workspace-scoped group on purpose — the state token IS
// the workspace credential here, the user may not currently have a
// session header attached to this request, and the callback always
// finishes with a frontend redirect (not a JSON body). Bouncing to
// the frontend Settings → Lark tab matches the GitHub callback's
// behavior and lets the existing settings page render success / error
// copy without polling.
func (h *Handler) LarkInstallCallback(w http.ResponseWriter, r *http.Request) {
	frontend := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	settingsURL := strings.TrimRight(frontend, "/") + "/settings?tab=lark"

	if h.LarkInstallations == nil || h.LarkOAuth == nil {
		http.Redirect(w, r, settingsURL+"&lark_error=not_configured", http.StatusFound)
		return
	}

	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	res, err := h.LarkOAuth.HandleCallback(r.Context(), lark.CallbackParams{
		Code:  code,
		State: state,
	})
	if err != nil {
		reason := larkOAuthErrorReason(err)
		slog.Warn("lark: oauth callback failed", "error", err, "reason", reason)
		http.Redirect(w, r, settingsURL+"&lark_error="+url.QueryEscape(reason), http.StatusFound)
		return
	}

	h.publish(protocol.EventLarkInstallationCreated, uuidToString(res.WorkspaceID), "system", "", map[string]any{
		"installation_id": uuidToString(res.InstallationID),
		"agent_id":        uuidToString(res.AgentID),
	})

	http.Redirect(w, r, settingsURL+"&lark_installed=1&installation="+url.QueryEscape(uuidToString(res.InstallationID)), http.StatusFound)
}

func larkOAuthErrorReason(err error) string {
	switch {
	case errors.Is(err, lark.ErrOAuthNotConfigured):
		return "not_configured"
	case errors.Is(err, lark.ErrMissingCode):
		return "missing_code"
	case errors.Is(err, lark.ErrInvalidState):
		return "invalid_state"
	case errors.Is(err, lark.ErrStateExpired):
		return "state_expired"
	case errors.Is(err, lark.ErrExchangeMissingAppID),
		errors.Is(err, lark.ErrExchangeMissingAppSecret),
		errors.Is(err, lark.ErrExchangeMissingBotOpenID),
		errors.Is(err, lark.ErrExchangeMissingInstallerOpenID):
		return "exchange_incomplete"
	case errors.Is(err, lark.ErrBindingAlreadyAssigned):
		// Installer auto-bind tripped the same-installation-different-user
		// guard — surface a dedicated code so the frontend can render
		// "this Lark account is bound to a different Multica user"
		// rather than a generic install failure.
		return "installer_already_bound_elsewhere"
	case errors.Is(err, lark.ErrBindingNotWorkspaceMember):
		// Installer is no longer (or never was) a member of the
		// target workspace. Distinct code so the UI can ask them to
		// re-verify their workspace membership before retrying.
		return "installer_not_workspace_member"
	default:
		return "internal_error"
	}
}
