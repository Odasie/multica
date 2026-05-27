package lark

import (
	"context"
	"errors"
	"log/slog"
)

// APIClient is the narrow surface this package needs from the Lark Open
// Platform HTTP API. It is intentionally defined here (rather than
// taken from a vendor SDK) so the rest of the package can be built and
// unit-tested without dragging Lark's transport into every test, and
// so we can swap implementations (real SDK, stub, fake) without
// touching call sites.
//
// All methods are scoped to a single installation — the caller has
// already authenticated the installation row and decrypted its
// app_secret. The client never reads `lark_installation` itself.
type APIClient interface {
	// IsConfigured reports whether this APIClient can actually reach
	// Lark. The stub implementation returns false; the real Lark HTTP
	// client (Phase-2 follow-up) returns true. UI surfaces (the
	// Settings → Lark tab, the "Bind to Lark" agent-detail button)
	// hide themselves when this returns false so users do not enter
	// an OAuth flow that is guaranteed to fail at the exchange step.
	IsConfigured() bool

	// SendInteractiveCard posts an interactive card into a Lark chat
	// and returns Lark's message_id for the card. The patcher persists
	// this id in lark_outbound_card_message so subsequent patches can
	// target the same card.
	SendInteractiveCard(ctx context.Context, p SendCardParams) (string, error)

	// PatchInteractiveCard replaces the body of a previously-sent card.
	// The throttling decision belongs to the caller; this method just
	// performs the network call.
	PatchInteractiveCard(ctx context.Context, p PatchCardParams) error

	// SendBindingPromptCard is the dedicated "you need to bind"
	// outbound. Kept separate from SendInteractiveCard so the
	// abstraction stays stable when the production card template
	// changes — call sites in identity check don't have to know about
	// Lark's card schema.
	SendBindingPromptCard(ctx context.Context, p BindingPromptParams) error

	// ExchangeOAuthCode swaps a Lark OAuth `code` for the installation
	// metadata we persist (app credentials, bot identity, installer
	// open_id). The OAuth callback handler is the only caller.
	ExchangeOAuthCode(ctx context.Context, code, redirectURI string) (OAuthExchangeResult, error)
}

// SendCardParams is the input shape for posting a fresh card.
type SendCardParams struct {
	InstallationID InstallationCredentials
	ChatID         ChatID
	// CardJSON is the raw Lark interactive card JSON body. We pass it
	// through opaque so the card-template package can evolve without
	// dragging this transport interface along.
	CardJSON string
}

// PatchCardParams is the input shape for updating an existing card.
type PatchCardParams struct {
	InstallationID    InstallationCredentials
	LarkCardMessageID string
	CardJSON          string
}

// BindingPromptParams carries the data needed to render and send the
// member-binding prompt card (single CTA: open the binding URL).
type BindingPromptParams struct {
	InstallationID InstallationCredentials
	OpenID         OpenID
	// BindURL is the absolute URL the user clicks. The token is
	// embedded in the URL by the caller; the client never sees it.
	BindURL string
}

// OAuthExchangeResult is the credentials extracted from a successful
// PersonalAgent OAuth grant. We deliberately surface only the fields
// we persist into `lark_installation` so the call site cannot
// accidentally leak transient values (e.g. access tokens) into storage.
type OAuthExchangeResult struct {
	AppID            string
	AppSecret        string
	BotOpenID        string
	TenantKey        string
	InstallerOpenID  OpenID
	InstallerUnionID string
}

// InstallationCredentials is the per-installation transport context the
// client needs to authenticate against Lark on behalf of a workspace's
// bot. Passing these explicitly to each call (rather than constructing
// per-installation clients) keeps lifecycle simple: the hub decrypts
// app_secret once and reuses the struct for every outbound call.
//
// The plaintext app_secret lives inside this struct exactly while a
// call is in flight; callers MUST NOT log or persist it.
type InstallationCredentials struct {
	AppID     string
	AppSecret string
	TenantKey string
}

// ErrAPIClientNotConfigured is returned by the stub client to signal
// that a real Lark client has not been wired in yet. Call sites SHOULD
// treat this as an expected condition on self-host deployments without
// a Lark app — log a warning, fall back to "Lark integration not
// configured", and continue serving other workspace functionality.
var ErrAPIClientNotConfigured = errors.New("lark: API client not configured")

// stubAPIClient is the default APIClient used when no production client
// has been registered. It refuses every transport call with
// ErrAPIClientNotConfigured so a misconfigured deployment fails loudly
// instead of silently dropping cards / OAuth callbacks.
//
// We deliberately do NOT silently succeed: a stub that returned ""
// message IDs would let the inbound dispatcher record bogus
// lark_outbound_card_message rows pointing at nothing.
type stubAPIClient struct {
	log *slog.Logger
}

// NewStubAPIClient returns the default no-op APIClient. The hub
// constructs one of these when no real implementation has been
// supplied, so subsystems that depend on APIClient (outbound patcher,
// OAuth callback) can still wire up; their first call surfaces a clear
// error.
func NewStubAPIClient(log *slog.Logger) APIClient {
	if log == nil {
		log = slog.Default()
	}
	return &stubAPIClient{log: log}
}

func (s *stubAPIClient) IsConfigured() bool { return false }

func (s *stubAPIClient) SendInteractiveCard(ctx context.Context, p SendCardParams) (string, error) {
	s.log.Warn("lark stub client: SendInteractiveCard called", "chat_id", string(p.ChatID))
	return "", ErrAPIClientNotConfigured
}

func (s *stubAPIClient) PatchInteractiveCard(ctx context.Context, p PatchCardParams) error {
	s.log.Warn("lark stub client: PatchInteractiveCard called", "card_message_id", p.LarkCardMessageID)
	return ErrAPIClientNotConfigured
}

func (s *stubAPIClient) SendBindingPromptCard(ctx context.Context, p BindingPromptParams) error {
	s.log.Warn("lark stub client: SendBindingPromptCard called", "open_id", string(p.OpenID))
	return ErrAPIClientNotConfigured
}

func (s *stubAPIClient) ExchangeOAuthCode(ctx context.Context, code, redirectURI string) (OAuthExchangeResult, error) {
	s.log.Warn("lark stub client: ExchangeOAuthCode called")
	return OAuthExchangeResult{}, ErrAPIClientNotConfigured
}
