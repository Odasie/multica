package lark

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// OAuthConfig captures the deployment-level OAuth knobs for the
// Multica-owned Lark app. Self-host operators set these via env vars
// when they want the Lark integration enabled. When AppID is empty
// the OAuth surface returns 503 — the manual-paste InstallationService
// path keeps working for operators that prefer that flow.
type OAuthConfig struct {
	// AppID is the Multica Lark app's app_id (the parent app users
	// install PersonalAgent bots from). Empty disables OAuth.
	AppID string

	// AppSecret authenticates Multica when exchanging the OAuth code
	// for installation credentials.
	AppSecret string

	// RedirectURI is the absolute URL Lark calls back after the user
	// authorizes the install. Must be registered in the Lark app
	// console. We do NOT derive it from request headers because a
	// reverse-proxy misconfiguration would let Lark land on the wrong
	// host.
	RedirectURI string

	// AuthorizeBaseURL is the Lark OAuth authorization endpoint
	// (https://accounts.feishu.cn/open-apis/authen/v1/authorize in
	// production). Configurable so dev / staging can point at a Lark
	// Beta endpoint.
	AuthorizeBaseURL string

	// StateSigningSecret is the HMAC key used to sign the OAuth state
	// token (binds workspace + agent into the callback). MUST be at
	// least 32 bytes. The token is opaque from the user's perspective.
	StateSigningSecret string

	// StateTTL caps how long an issued state token is valid. Default
	// 10 minutes; long enough for the user to walk through the Lark
	// authorize UI, short enough that an intercepted state cannot be
	// replayed days later.
	StateTTL time.Duration

	// FrontendSuccessURL is the post-install destination on the
	// Multica frontend. The callback redirects users here with
	// `?lark_installed=1&workspace=<slug>&installation=<id>` so the
	// settings/agent page can show confirmation copy without polling.
	// Empty defaults to "/settings?tab=lark".
	FrontendSuccessURL string

	// FrontendErrorURL is the post-failure destination. Empty defaults
	// to the same path as FrontendSuccessURL.
	FrontendErrorURL string

	// Now / Clock for tests.
	Now func() time.Time
}

func (c OAuthConfig) withDefaults() OAuthConfig {
	if c.StateTTL == 0 {
		c.StateTTL = 10 * time.Minute
	}
	if c.AuthorizeBaseURL == "" {
		c.AuthorizeBaseURL = "https://accounts.feishu.cn/open-apis/authen/v1/authorize"
	}
	if c.FrontendSuccessURL == "" {
		c.FrontendSuccessURL = "/settings?tab=lark"
	}
	if c.FrontendErrorURL == "" {
		c.FrontendErrorURL = c.FrontendSuccessURL
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Enabled reports whether OAuth installation is configured for this
// deployment. The HTTP layer uses this for the 503 short-circuit so
// manual-install operators are not forced to also configure OAuth.
func (c OAuthConfig) Enabled() bool {
	return c.AppID != "" && c.AppSecret != "" && c.RedirectURI != "" && c.StateSigningSecret != ""
}

// InstallerBinder is the narrow surface OAuthService needs to record
// the installer's lark_user_binding row in the same business step as
// the installation itself. The OAuth journey is "scan, authorize,
// you're done" — the installer should NOT have to redeem a binding
// token after the install completes. Without this step the first
// inbound message from the installer would be dropped as
// `unbound_user` (and the Bot would reply "you're not bound, click
// here…" to the person who just clicked "authorize" 30 seconds ago).
//
// Implementations MUST be idempotent on (installation_id, lark_open_id):
// a re-install by the same user should not error.
type InstallerBinder interface {
	BindInstaller(ctx context.Context, p InstallerBindParams) error
}

// InstallerBindParams carries the inputs InstallerBinder needs. Kept
// as a struct so adding union_id (Phase 2) does not break callers.
type InstallerBindParams struct {
	WorkspaceID    pgtype.UUID
	InstallationID pgtype.UUID
	MulticaUserID  pgtype.UUID // the installer's Multica account
	LarkOpenID     OpenID      // the installer's per-installation open_id
}

// OAuthService coordinates the install start / callback dance. It
// holds no DB state of its own — the InstallationService still owns
// every write to lark_installation; OAuthService is the
// channel-aware wrapper that obtains the credentials Lark hands us
// and forwards them to InstallationService.Upsert.
type OAuthService struct {
	cfg          OAuthConfig
	client       APIClient
	installation *InstallationService
	binder       InstallerBinder
}

// NewOAuthService constructs an OAuthService. cfg may be the zero
// value — Enabled() will simply return false and StartInstall/Callback
// will surface ErrOAuthNotConfigured. binder is required; the OAuth
// install journey REQUIRES the installer to be auto-bound (see
// InstallerBinder doc), so refusing to construct without one is the
// safer default than allowing a silent regression.
func NewOAuthService(cfg OAuthConfig, client APIClient, installation *InstallationService, binder InstallerBinder) (*OAuthService, error) {
	cfg = cfg.withDefaults()
	if client == nil {
		return nil, errors.New("lark oauth: APIClient is required")
	}
	if installation == nil {
		return nil, errors.New("lark oauth: InstallationService is required")
	}
	if binder == nil {
		return nil, errors.New("lark oauth: InstallerBinder is required")
	}
	return &OAuthService{cfg: cfg, client: client, installation: installation, binder: binder}, nil
}

// StartInstallParams carries the workspace + agent the install will
// bind to. The handler sources both from the URL path (workspace) and
// the query (agent) and has already validated workspace membership
// (admin-only at the router) and agent ↔ workspace ownership.
type StartInstallParams struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	InitiatorID pgtype.UUID
}

// StartInstallResult is what StartInstall returns to the handler.
// `URL` is the absolute Lark authorization URL the frontend should
// open in a new tab or display as a QR. `State` is exposed for tests
// and for handlers that want to log the binding (do NOT echo to the
// user).
type StartInstallResult struct {
	URL   string
	State string
}

// StartInstall builds a signed-state OAuth URL the user opens to
// authorize the install. The state token binds the workspace, agent,
// and initiating user so the callback can persist the installation
// against the correct rows without trusting query params.
func (s *OAuthService) StartInstall(p StartInstallParams) (StartInstallResult, error) {
	if !s.cfg.Enabled() {
		return StartInstallResult{}, ErrOAuthNotConfigured
	}
	if !p.WorkspaceID.Valid || !p.AgentID.Valid || !p.InitiatorID.Valid {
		return StartInstallResult{}, errors.New("workspace, agent, and initiator are required")
	}
	state, err := s.signState(uuidString(p.WorkspaceID), uuidString(p.AgentID), uuidString(p.InitiatorID))
	if err != nil {
		return StartInstallResult{}, fmt.Errorf("sign state: %w", err)
	}
	u := s.buildAuthorizeURL(state)
	return StartInstallResult{URL: u, State: state}, nil
}

// CallbackParams is what the handler hands to HandleCallback after
// pulling values out of the query string.
type CallbackParams struct {
	Code  string
	State string
}

// CallbackResult is what HandleCallback returns: the persisted
// installation row plus a redirect URL the handler should bounce the
// browser to.
type CallbackResult struct {
	WorkspaceID    pgtype.UUID
	AgentID        pgtype.UUID
	InstallationID pgtype.UUID
	InstallerOpenID OpenID
	RedirectURL    string
}

// HandleCallback finishes the install: verify state → exchange code
// → upsert installation. The handler is responsible for the
// HTTP-side redirect using the returned URL; this keeps the service
// HTTP-free for tests.
func (s *OAuthService) HandleCallback(ctx context.Context, p CallbackParams) (CallbackResult, error) {
	if !s.cfg.Enabled() {
		return CallbackResult{}, ErrOAuthNotConfigured
	}
	if strings.TrimSpace(p.Code) == "" {
		return CallbackResult{}, ErrMissingCode
	}
	if strings.TrimSpace(p.State) == "" {
		return CallbackResult{}, ErrInvalidState
	}
	binding, ok := s.verifyState(p.State)
	if !ok {
		return CallbackResult{}, ErrInvalidState
	}
	if s.cfg.Now().After(binding.ExpiresAt) {
		return CallbackResult{}, ErrStateExpired
	}

	exch, err := s.client.ExchangeOAuthCode(ctx, p.Code, s.cfg.RedirectURI)
	if err != nil {
		return CallbackResult{}, fmt.Errorf("exchange oauth code: %w", err)
	}
	if err := validateExchangeResult(exch); err != nil {
		return CallbackResult{}, err
	}

	inst, err := s.installation.Upsert(ctx, InstallationParams{
		WorkspaceID:     binding.WorkspaceID,
		AgentID:         binding.AgentID,
		AppID:           exch.AppID,
		AppSecret:       exch.AppSecret,
		TenantKey:       exch.TenantKey,
		BotOpenID:       exch.BotOpenID,
		InstallerUserID: binding.InitiatorID,
	})
	if err != nil {
		return CallbackResult{}, fmt.Errorf("upsert installation: %w", err)
	}

	// Auto-bind the installer. Without this the installer's first
	// inbound message would be dropped as `unbound_user` and they
	// would be asked to "click here to bind" 30 seconds after they
	// just clicked "authorize" — confusing, and breaks the §2.1
	// install journey. Bind FAILURE is fatal because the user-facing
	// promise of OAuth is "you're done"; a half-installed state is
	// worse than no install (Upsert is idempotent on re-install, so
	// the user can retry). validateExchangeResult above already
	// rejected an empty InstallerOpenID, so we know it is present
	// here.
	if err := s.binder.BindInstaller(ctx, InstallerBindParams{
		WorkspaceID:    binding.WorkspaceID,
		InstallationID: inst.ID,
		MulticaUserID:  binding.InitiatorID,
		LarkOpenID:     exch.InstallerOpenID,
	}); err != nil {
		return CallbackResult{}, fmt.Errorf("bind installer: %w", err)
	}

	return CallbackResult{
		WorkspaceID:     binding.WorkspaceID,
		AgentID:         binding.AgentID,
		InstallationID:  inst.ID,
		InstallerOpenID: exch.InstallerOpenID,
		RedirectURL:     s.cfg.FrontendSuccessURL,
	}, nil
}

// ErrorRedirect returns the URL the handler should bounce to when
// HandleCallback fails. Centralizing this lets us preserve the
// frontend-success path for the success case and a single error
// destination for every failure mode (with a code query param so the
// UI can pick the right copy).
func (s *OAuthService) ErrorRedirect(reason string) string {
	base := s.cfg.FrontendErrorURL
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "lark_error=" + url.QueryEscape(reason)
}

func (s *OAuthService) buildAuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("app_id", s.cfg.AppID)
	q.Set("redirect_uri", s.cfg.RedirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	q.Set("scope", "personal_agent:install")
	sep := "?"
	if strings.Contains(s.cfg.AuthorizeBaseURL, "?") {
		sep = "&"
	}
	return s.cfg.AuthorizeBaseURL + sep + q.Encode()
}

// stateBinding is the unpacked, verified state token. The handler
// trusts these fields once verifyState returns ok = true.
type stateBinding struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	InitiatorID pgtype.UUID
	ExpiresAt   time.Time
}

// signState produces a token of the form:
//
//	workspaceID.agentID.initiatorID.expiresAtUnix.nonceHex.sigHex
//
// signed with HMAC-SHA256(StateSigningSecret). The HMAC covers the
// concatenated payload (no length-prefix needed because every field is
// fixed-width except nonceHex, which is consumed last before the sig).
func (s *OAuthService) signState(workspaceID, agentID, initiatorID string) (string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	expires := s.cfg.Now().Add(s.cfg.StateTTL).Unix()
	payload := fmt.Sprintf("%s.%s.%s.%d.%s",
		workspaceID, agentID, initiatorID, expires, hex.EncodeToString(nonce))
	mac := hmac.New(sha256.New, []byte(s.cfg.StateSigningSecret))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

func (s *OAuthService) verifyState(token string) (stateBinding, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 6 {
		return stateBinding{}, false
	}
	workspaceIDStr, agentIDStr, initiatorIDStr, expiresStr, nonceHex, sigHex :=
		parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]
	payload := strings.Join(parts[:5], ".")
	mac := hmac.New(sha256.New, []byte(s.cfg.StateSigningSecret))
	mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sigHex)) {
		return stateBinding{}, false
	}
	_ = nonceHex // included in the signature, no further use
	var workspaceID, agentID, initiatorID pgtype.UUID
	if err := workspaceID.Scan(workspaceIDStr); err != nil {
		return stateBinding{}, false
	}
	if err := agentID.Scan(agentIDStr); err != nil {
		return stateBinding{}, false
	}
	if err := initiatorID.Scan(initiatorIDStr); err != nil {
		return stateBinding{}, false
	}
	var expiresUnix int64
	if _, err := fmt.Sscanf(expiresStr, "%d", &expiresUnix); err != nil {
		return stateBinding{}, false
	}
	return stateBinding{
		WorkspaceID: workspaceID,
		AgentID:     agentID,
		InitiatorID: initiatorID,
		ExpiresAt:   time.Unix(expiresUnix, 0),
	}, true
}

func validateExchangeResult(r OAuthExchangeResult) error {
	switch {
	case r.AppID == "":
		return ErrExchangeMissingAppID
	case r.AppSecret == "":
		return ErrExchangeMissingAppSecret
	case r.BotOpenID == "":
		return ErrExchangeMissingBotOpenID
	case r.InstallerOpenID == "":
		// Installer auto-binding (see HandleCallback) requires the
		// installer's per-installation open_id; without it we
		// cannot honor the §2.1 "scan to bind, you're done"
		// promise, so we fail fast BEFORE upserting the
		// installation row.
		return ErrExchangeMissingInstallerOpenID
	}
	return nil
}

// Public sentinels so handlers can map service errors to HTTP status
// codes without parsing strings.

// ErrOAuthNotConfigured is returned when StartInstall / HandleCallback
// is called against a deployment that has not set up the Multica Lark
// app credentials (AppID / AppSecret / RedirectURI / StateSigningSecret).
// Handlers translate this to 503.
var ErrOAuthNotConfigured = errors.New("lark oauth: not configured")

// ErrMissingCode means the callback fired without a `code` param —
// either the user denied the install or Lark malformed the redirect.
var ErrMissingCode = errors.New("lark oauth: missing code")

// ErrInvalidState means the state token was missing, malformed, or
// failed HMAC verification. Could be a replay against a different
// signing secret (key rotation) or an attempt to forge a callback;
// both surface the same opaque error.
var ErrInvalidState = errors.New("lark oauth: invalid state")

// ErrStateExpired means the state token is well-formed but its TTL
// has elapsed. The user should restart the install from the agent
// detail page.
var ErrStateExpired = errors.New("lark oauth: state expired")

// ErrExchange* surfaces the (rare) case where Lark's OAuth exchange
// returned a response missing fields we need to persist. The
// stubAPIClient returns ErrAPIClientNotConfigured before any of these
// can fire; the real client should validate up-stream.
var (
	ErrExchangeMissingAppID            = errors.New("lark oauth: exchange response missing app_id")
	ErrExchangeMissingAppSecret        = errors.New("lark oauth: exchange response missing app_secret")
	ErrExchangeMissingBotOpenID        = errors.New("lark oauth: exchange response missing bot_open_id")
	ErrExchangeMissingInstallerOpenID  = errors.New("lark oauth: exchange response missing installer open_id")
)
