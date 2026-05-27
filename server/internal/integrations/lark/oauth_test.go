package lark

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

// fakeInstallerBinder records the BindInstaller calls so tests can
// assert that the OAuth callback wired the installer auto-bind into
// the install journey. Returning bindErr lets a test simulate
// "installer's open_id is already claimed by someone else" or
// "installer is not a workspace member" to confirm the OAuth callback
// surfaces those failures correctly.
type fakeInstallerBinder struct {
	called  int
	lastReq InstallerBindParams
	bindErr error
}

func (f *fakeInstallerBinder) BindInstaller(_ context.Context, p InstallerBindParams) error {
	f.called++
	f.lastReq = p
	return f.bindErr
}

func newOAuthService(t *testing.T, now func() time.Time, client APIClient) *OAuthService {
	t.Helper()
	return newOAuthServiceWithBinder(t, now, client, &fakeInstallerBinder{})
}

func newOAuthServiceWithBinder(t *testing.T, now func() time.Time, client APIClient, binder InstallerBinder) *OAuthService {
	t.Helper()
	box := mustTestBox(t)
	inst, err := NewInstallationService(nil, box)
	if err != nil {
		t.Fatalf("InstallationService: %v", err)
	}
	cfg := OAuthConfig{
		AppID:              "cli_meta_app",
		AppSecret:          "shh",
		RedirectURI:        "https://multica.example/api/lark/install/callback",
		AuthorizeBaseURL:   "https://accounts.feishu.cn/open-apis/authen/v1/authorize",
		StateSigningSecret: "test-state-secret-32-bytes-of-rand!!",
		StateTTL:           10 * time.Minute,
		FrontendSuccessURL: "/settings?tab=lark",
		Now:                now,
	}
	svc, err := NewOAuthService(cfg, client, inst, binder)
	if err != nil {
		t.Fatalf("NewOAuthService: %v", err)
	}
	return svc
}

func TestOAuthStartInstallBuildsSignedURL(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	svc := newOAuthService(t, func() time.Time { return now }, NewStubAPIClient(newDiscardLogger()))

	res, err := svc.StartInstall(StartInstallParams{
		WorkspaceID: uuidFromString(t, "11111111-1111-1111-1111-111111111111"),
		AgentID:     uuidFromString(t, "22222222-2222-2222-2222-222222222222"),
		InitiatorID: uuidFromString(t, "33333333-3333-3333-3333-333333333333"),
	})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	u, err := url.Parse(res.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	if q.Get("app_id") != "cli_meta_app" {
		t.Fatalf("app_id not propagated: %s", q.Get("app_id"))
	}
	if q.Get("redirect_uri") != "https://multica.example/api/lark/install/callback" {
		t.Fatalf("redirect_uri not propagated: %s", q.Get("redirect_uri"))
	}
	if q.Get("state") == "" {
		t.Fatalf("state must be set")
	}
	if !strings.Contains(res.URL, "accounts.feishu.cn") {
		t.Fatalf("URL must point at Lark OAuth host: %s", res.URL)
	}
}

func TestOAuthDisabledWhenConfigMissing(t *testing.T) {
	box := mustTestBox(t)
	inst, err := NewInstallationService(nil, box)
	if err != nil {
		t.Fatalf("InstallationService: %v", err)
	}
	svc, err := NewOAuthService(OAuthConfig{}, NewStubAPIClient(newDiscardLogger()), inst, &fakeInstallerBinder{})
	if err != nil {
		t.Fatalf("NewOAuthService: %v", err)
	}
	_, err = svc.StartInstall(StartInstallParams{
		WorkspaceID: uuidFromString(t, "11111111-1111-1111-1111-111111111111"),
		AgentID:     uuidFromString(t, "22222222-2222-2222-2222-222222222222"),
		InitiatorID: uuidFromString(t, "33333333-3333-3333-3333-333333333333"),
	})
	if !errors.Is(err, ErrOAuthNotConfigured) {
		t.Fatalf("expected ErrOAuthNotConfigured, got %v", err)
	}
	_, err = svc.HandleCallback(context.Background(), CallbackParams{Code: "x", State: "y"})
	if !errors.Is(err, ErrOAuthNotConfigured) {
		t.Fatalf("expected ErrOAuthNotConfigured on callback, got %v", err)
	}
}

func TestOAuthCallbackRejectsInvalidState(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	svc := newOAuthService(t, func() time.Time { return now }, NewStubAPIClient(newDiscardLogger()))
	_, err := svc.HandleCallback(context.Background(), CallbackParams{Code: "code", State: "not-a-real-state"})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}
}

func TestOAuthCallbackRejectsTamperedState(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	svc := newOAuthService(t, func() time.Time { return now }, NewStubAPIClient(newDiscardLogger()))
	res, err := svc.StartInstall(StartInstallParams{
		WorkspaceID: uuidFromString(t, "11111111-1111-1111-1111-111111111111"),
		AgentID:     uuidFromString(t, "22222222-2222-2222-2222-222222222222"),
		InitiatorID: uuidFromString(t, "33333333-3333-3333-3333-333333333333"),
	})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	// Flip a single character of the signature — should fail HMAC.
	last := res.State[len(res.State)-1]
	tampered := res.State[:len(res.State)-1]
	if last == 'a' {
		tampered += "b"
	} else {
		tampered += "a"
	}
	_, err = svc.HandleCallback(context.Background(), CallbackParams{Code: "code", State: tampered})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("tampered state must be rejected as invalid, got %v", err)
	}
}

func TestOAuthCallbackRejectsExpiredState(t *testing.T) {
	mintAt := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	clock := mintAt
	svc := newOAuthService(t, func() time.Time { return clock }, NewStubAPIClient(newDiscardLogger()))

	res, err := svc.StartInstall(StartInstallParams{
		WorkspaceID: uuidFromString(t, "11111111-1111-1111-1111-111111111111"),
		AgentID:     uuidFromString(t, "22222222-2222-2222-2222-222222222222"),
		InitiatorID: uuidFromString(t, "33333333-3333-3333-3333-333333333333"),
	})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}

	// Advance the clock past the 10-minute TTL.
	clock = mintAt.Add(11 * time.Minute)
	_, err = svc.HandleCallback(context.Background(), CallbackParams{Code: "code", State: res.State})
	if !errors.Is(err, ErrStateExpired) {
		t.Fatalf("expected ErrStateExpired, got %v", err)
	}
}

// fakeOAuthAPIClient is a minimal APIClient that returns a canned
// OAuthExchangeResult and refuses every other transport call. Used to
// drive HandleCallback through to the installer-bind step without
// dragging the real Lark HTTP wire protocol in.
type fakeOAuthAPIClient struct {
	exch OAuthExchangeResult
	err  error
}

func (f *fakeOAuthAPIClient) IsConfigured() bool { return true }

func (f *fakeOAuthAPIClient) SendInteractiveCard(_ context.Context, _ SendCardParams) (string, error) {
	return "", ErrAPIClientNotConfigured
}
func (f *fakeOAuthAPIClient) PatchInteractiveCard(_ context.Context, _ PatchCardParams) error {
	return ErrAPIClientNotConfigured
}
func (f *fakeOAuthAPIClient) SendBindingPromptCard(_ context.Context, _ BindingPromptParams) error {
	return ErrAPIClientNotConfigured
}
func (f *fakeOAuthAPIClient) ExchangeOAuthCode(_ context.Context, _ string, _ string) (OAuthExchangeResult, error) {
	return f.exch, f.err
}

// TestOAuthCallbackInstallerMissingOpenID pins the safety net for
// when Lark's exchange response omits the installer's open_id (rare
// but possible on some tenant configurations). The callback must
// surface this as a typed error rather than silently install without
// the auto-bind.
func TestOAuthCallbackInstallerMissingOpenID(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	stub := &fakeOAuthAPIClient{
		exch: OAuthExchangeResult{
			AppID:           "cli_meta_app",
			AppSecret:       "secret",
			BotOpenID:       "ou_bot",
			InstallerOpenID: "", // missing — must surface as a distinct error
		},
	}
	binder := &fakeInstallerBinder{}
	svc := newOAuthServiceWithBinder(t, func() time.Time { return now }, stub, binder)

	res, err := svc.StartInstall(StartInstallParams{
		WorkspaceID: uuidFromString(t, "11111111-1111-1111-1111-111111111111"),
		AgentID:     uuidFromString(t, "22222222-2222-2222-2222-222222222222"),
		InitiatorID: uuidFromString(t, "33333333-3333-3333-3333-333333333333"),
	})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	_, err = svc.HandleCallback(context.Background(), CallbackParams{Code: "ok", State: res.State})
	if !errors.Is(err, ErrExchangeMissingInstallerOpenID) {
		t.Fatalf("expected ErrExchangeMissingInstallerOpenID, got %v", err)
	}
	if binder.called != 0 {
		t.Fatalf("binder must not run when installer open_id is missing")
	}
}

func TestOAuthCallbackPropagatesExchangeError(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	stub := NewStubAPIClient(newDiscardLogger()) // returns ErrAPIClientNotConfigured
	svc := newOAuthService(t, func() time.Time { return now }, stub)

	res, err := svc.StartInstall(StartInstallParams{
		WorkspaceID: uuidFromString(t, "11111111-1111-1111-1111-111111111111"),
		AgentID:     uuidFromString(t, "22222222-2222-2222-2222-222222222222"),
		InitiatorID: uuidFromString(t, "33333333-3333-3333-3333-333333333333"),
	})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	_, err = svc.HandleCallback(context.Background(), CallbackParams{Code: "code", State: res.State})
	if !errors.Is(err, ErrAPIClientNotConfigured) {
		t.Fatalf("expected stub-client error to propagate, got %v", err)
	}
}

// TestOAuthRequiresInstallerBinder pins the constructor-misuse rule:
// the OAuth install journey REQUIRES installer auto-binding (see the
// InstallerBinder doc) so a nil binder must fail at construction time
// rather than slip past and produce installs whose installer is left
// in an unbound state.
func TestOAuthRequiresInstallerBinder(t *testing.T) {
	box := mustTestBox(t)
	inst, err := NewInstallationService(nil, box)
	if err != nil {
		t.Fatalf("InstallationService: %v", err)
	}
	_, err = NewOAuthService(OAuthConfig{}, NewStubAPIClient(newDiscardLogger()), inst, nil)
	if err == nil {
		t.Fatal("expected nil InstallerBinder to be rejected at construction")
	}
	if !strings.Contains(err.Error(), "InstallerBinder") {
		t.Fatalf("expected InstallerBinder error, got %v", err)
	}
}

func TestValidateExchangeResult(t *testing.T) {
	good := OAuthExchangeResult{
		AppID:           "cli_app",
		AppSecret:       "secret",
		BotOpenID:       "bot_open_id",
		InstallerOpenID: "ou_installer",
	}
	if err := validateExchangeResult(good); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	if err := validateExchangeResult(OAuthExchangeResult{AppSecret: "x", BotOpenID: "y", InstallerOpenID: "z"}); !errors.Is(err, ErrExchangeMissingAppID) {
		t.Fatalf("missing app_id: %v", err)
	}
	if err := validateExchangeResult(OAuthExchangeResult{AppID: "x", BotOpenID: "y", InstallerOpenID: "z"}); !errors.Is(err, ErrExchangeMissingAppSecret) {
		t.Fatalf("missing app_secret: %v", err)
	}
	if err := validateExchangeResult(OAuthExchangeResult{AppID: "x", AppSecret: "y", InstallerOpenID: "z"}); !errors.Is(err, ErrExchangeMissingBotOpenID) {
		t.Fatalf("missing bot_open_id: %v", err)
	}
	if err := validateExchangeResult(OAuthExchangeResult{AppID: "x", AppSecret: "y", BotOpenID: "z"}); !errors.Is(err, ErrExchangeMissingInstallerOpenID) {
		t.Fatalf("missing installer_open_id: %v", err)
	}
}

// uuidsAndPgtype is a sanity check that pgtype.UUID round-trips
// through the state token; if Scan fails on the parsed substring
// the verifyState branch silently rejects valid tokens.
func TestVerifyStateRoundTripsAllFields(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	svc := newOAuthService(t, func() time.Time { return now }, NewStubAPIClient(newDiscardLogger()))

	wsID := uuidFromString(t, "11111111-1111-1111-1111-111111111111")
	agentID := uuidFromString(t, "22222222-2222-2222-2222-222222222222")
	initiatorID := uuidFromString(t, "33333333-3333-3333-3333-333333333333")
	res, err := svc.StartInstall(StartInstallParams{
		WorkspaceID: wsID,
		AgentID:     agentID,
		InitiatorID: initiatorID,
	})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	binding, ok := svc.verifyState(res.State)
	if !ok {
		t.Fatalf("verifyState rejected freshly-signed token")
	}
	if !uuidEqual(binding.WorkspaceID, wsID) ||
		!uuidEqual(binding.AgentID, agentID) ||
		!uuidEqual(binding.InitiatorID, initiatorID) {
		t.Fatalf("round-trip mismatch: %+v", binding)
	}
}

func uuidEqual(a, b pgtype.UUID) bool {
	return a.Valid == b.Valid && a.Bytes == b.Bytes
}

// mustTestBox is a tiny helper that constructs a secretbox.Box from a
// stable 32-byte key — used so InstallationService validation passes
// in the OAuth tests above. The key contents are not security-meaningful
// in this test (we never actually encrypt anything reachable to a third
// party).
func mustTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}
