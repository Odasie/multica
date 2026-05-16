package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// InstallTokenExchangeRequest mirrors handler.ExchangeInstallTokenRequest.
type InstallTokenExchangeRequest struct {
	Token    string `json:"token"`
	DaemonID string `json:"daemon_id"`
}

// InstallTokenExchangeResponse mirrors handler.ExchangeInstallTokenResponse.
type InstallTokenExchangeResponse struct {
	DaemonToken string `json:"daemon_token"`
	WorkspaceID string `json:"workspace_id"`
	DaemonID    string `json:"daemon_id"`
	ExpiresAt   string `json:"expires_at"`
}

// ErrInstallTokenAlreadyUsed is returned when the server responds with
// install_token_already_used — a strict single-use indicator. install.sh
// surfaces a "this token has already been redeemed; mint a new one" message
// when it sees this.
var ErrInstallTokenAlreadyUsed = errors.New("install token already redeemed")

// ErrInstallTokenInvalid covers expired, malformed, or unknown tokens — any
// state where the server cannot consume the mit_ but the token is not
// known to have been used.
var ErrInstallTokenInvalid = errors.New("install token invalid or expired")

// ExchangeInstallToken POSTs to /api/install-tokens/exchange. The endpoint
// is unauthenticated apart from the mit_ in the body, so the request omits
// Authorization and X-Workspace-ID headers.
//
// The mit_ is single-use: a successful response means the token has been
// burned server-side. The caller is responsible for persisting the returned
// mdt_ before doing anything else; if the credential is lost between the
// exchange and the save, the user must mint a new mit_ from the workspace UI.
func ExchangeInstallToken(ctx context.Context, serverURL, token, daemonID string) (InstallTokenExchangeResponse, error) {
	baseURL := strings.TrimRight(serverURL, "/")
	if baseURL == "" {
		return InstallTokenExchangeResponse{}, fmt.Errorf("server URL is required")
	}
	body, err := json.Marshal(InstallTokenExchangeRequest{Token: token, DaemonID: daemonID})
	if err != nil {
		return InstallTokenExchangeResponse{}, fmt.Errorf("encode exchange request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/install-tokens/exchange", bytes.NewReader(body))
	if err != nil {
		return InstallTokenExchangeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Identity headers so the server can attribute the call in its logs;
	// matches what every other CLI request sends.
	if ClientPlatform != "" {
		req.Header.Set("X-Client-Platform", ClientPlatform)
	}
	if ClientVersion != "" {
		req.Header.Set("X-Client-Version", ClientVersion)
	}
	if ClientOS != "" {
		req.Header.Set("X-Client-OS", ClientOS)
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return InstallTokenExchangeResponse{}, fmt.Errorf("exchange install token: %w", err)
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusUnauthorized {
		// Server distinguishes "already used" vs "invalid/expired" via the
		// error message body. See handler/install_token.go.
		msg := strings.ToLower(strings.TrimSpace(string(respData)))
		if strings.Contains(msg, "install_token_already_used") {
			return InstallTokenExchangeResponse{}, ErrInstallTokenAlreadyUsed
		}
		return InstallTokenExchangeResponse{}, ErrInstallTokenInvalid
	}
	if resp.StatusCode >= 400 {
		return InstallTokenExchangeResponse{}, fmt.Errorf("exchange install token: server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respData)))
	}

	var out InstallTokenExchangeResponse
	if err := json.Unmarshal(respData, &out); err != nil {
		return InstallTokenExchangeResponse{}, fmt.Errorf("decode exchange response: %w", err)
	}
	if out.DaemonToken == "" || out.WorkspaceID == "" {
		return InstallTokenExchangeResponse{}, fmt.Errorf("exchange install token: server returned empty credential")
	}
	return out, nil
}
