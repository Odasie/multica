package lark

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakePatcherQueries struct {
	mu              sync.Mutex
	binding         db.LarkChatSessionBinding
	bindingErr      error
	installation    db.LarkInstallation
	installationErr error
	agent           db.Agent
	agentErr        error
	card            db.LarkOutboundCardMessage
	cardErr         error
	created         []db.CreateLarkOutboundCardMessageParams
	createReturn    db.LarkOutboundCardMessage
	statusUpdates   []db.UpdateLarkOutboundCardStatusParams
}

func (f *fakePatcherQueries) GetAgentTask(ctx context.Context, id pgtype.UUID) (db.AgentTaskQueue, error) {
	return db.AgentTaskQueue{}, nil
}
func (f *fakePatcherQueries) GetChatSession(ctx context.Context, id pgtype.UUID) (db.ChatSession, error) {
	return db.ChatSession{}, nil
}
func (f *fakePatcherQueries) GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error) {
	return f.agent, f.agentErr
}
func (f *fakePatcherQueries) GetLarkInstallation(ctx context.Context, id pgtype.UUID) (db.LarkInstallation, error) {
	return f.installation, f.installationErr
}
func (f *fakePatcherQueries) GetLarkChatSessionBindingBySession(ctx context.Context, sessID pgtype.UUID) (db.LarkChatSessionBinding, error) {
	return f.binding, f.bindingErr
}
func (f *fakePatcherQueries) GetLarkOutboundCardByTask(ctx context.Context, taskID pgtype.UUID) (db.LarkOutboundCardMessage, error) {
	return f.card, f.cardErr
}
func (f *fakePatcherQueries) CreateLarkOutboundCardMessage(ctx context.Context, arg db.CreateLarkOutboundCardMessageParams) (db.LarkOutboundCardMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, arg)
	return f.createReturn, nil
}
func (f *fakePatcherQueries) UpdateLarkOutboundCardStatus(ctx context.Context, arg db.UpdateLarkOutboundCardStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusUpdates = append(f.statusUpdates, arg)
	return nil
}

type fakeCredentials struct{ secret string }

func (f fakeCredentials) DecryptAppSecret(inst db.LarkInstallation) (string, error) {
	return f.secret, nil
}

type fakeAPIClient struct {
	mu          sync.Mutex
	sent        []SendCardParams
	patched     []PatchCardParams
	sendReturn  string
	sendErr     error
	patchErr    error
	bindingSent []BindingPromptParams
}

func (f *fakeAPIClient) IsConfigured() bool { return true }

func (f *fakeAPIClient) SendInteractiveCard(ctx context.Context, p SendCardParams) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p)
	return f.sendReturn, f.sendErr
}
func (f *fakeAPIClient) PatchInteractiveCard(ctx context.Context, p PatchCardParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patched = append(f.patched, p)
	return f.patchErr
}
func (f *fakeAPIClient) SendBindingPromptCard(ctx context.Context, p BindingPromptParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindingSent = append(f.bindingSent, p)
	return nil
}
func (f *fakeAPIClient) ExchangeOAuthCode(ctx context.Context, code, redirectURI string) (OAuthExchangeResult, error) {
	return OAuthExchangeResult{}, nil
}

func newTestPatcher(t *testing.T) (*Patcher, *fakePatcherQueries, *fakeAPIClient) {
	t.Helper()
	q := &fakePatcherQueries{
		binding: db.LarkChatSessionBinding{
			ChatSessionID:   uuidFromString(t, "cccccccc-cccc-cccc-cccc-cccccccccccc"),
			InstallationID:  uuidFromString(t, "1111aaaa-1111-1111-1111-111111111111"),
			LarkChatID:      "oc_test_chat",
			LarkChatType:    "p2p",
		},
		installation: db.LarkInstallation{
			ID:                 uuidFromString(t, "1111aaaa-1111-1111-1111-111111111111"),
			AppID:              "cli_test_app",
			AppSecretEncrypted: []byte("ciphertext"),
			Status:             string(InstallationActive),
			AgentID:            uuidFromString(t, "aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		},
		agent:   db.Agent{Name: "TestAgent"},
		cardErr: pgx.ErrNoRows,
	}
	api := &fakeAPIClient{sendReturn: "lark_card_msg_1"}
	p := NewPatcher(q, fakeCredentials{secret: "shh"}, api, PatcherConfig{
		MinPatchInterval: 50 * time.Millisecond,
		Logger:           newDiscardLogger(),
		Now:              time.Now,
	})
	return p, q, api
}

func TestPatcherSendsThinkingCardOnTaskQueued(t *testing.T) {
	p, q, api := newTestPatcher(t)
	taskID := uuidFromString(t, "ee111111-ee11-ee11-ee11-eeeeeeeeeeee")
	sessionID := q.binding.ChatSessionID

	p.handleEvent(events.Event{
		Type:          protocol.EventTaskQueued,
		TaskID:        uuidString(taskID),
		ChatSessionID: uuidString(sessionID),
		Payload: map[string]any{
			"task_id":         uuidString(taskID),
			"chat_session_id": uuidString(sessionID),
			"status":          "queued",
		},
	})

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 1 {
		t.Fatalf("expected one SendInteractiveCard call, got %d", len(api.sent))
	}
	if api.sent[0].InstallationID.AppID != "cli_test_app" {
		t.Fatalf("expected app_id propagated, got %q", api.sent[0].InstallationID.AppID)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.created) != 1 {
		t.Fatalf("expected one CreateLarkOutboundCardMessage call, got %d", len(q.created))
	}
	if q.created[0].Status != string(CardStatusPending) {
		t.Fatalf("initial card status should be 'pending', got %q", q.created[0].Status)
	}
	if q.created[0].LarkCardMessageID != "lark_card_msg_1" {
		t.Fatalf("expected card binding to use returned message id, got %q", q.created[0].LarkCardMessageID)
	}
}

func TestPatcherSkipsWhenNoChatSessionBinding(t *testing.T) {
	p, q, api := newTestPatcher(t)
	q.bindingErr = pgx.ErrNoRows

	p.handleEvent(events.Event{
		Type:          protocol.EventTaskQueued,
		TaskID:        uuidString(uuidFromString(t, "ee222222-ee22-ee22-ee22-eeeeeeeeeeee")),
		ChatSessionID: uuidString(uuidFromString(t, "cc222222-cc22-cc22-cc22-cccccccccccc")),
	})

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 0 {
		t.Fatalf("web-only chat sessions must not produce Lark cards; got %d sends", len(api.sent))
	}
}

func TestPatcherFinalCardBypassesThrottle(t *testing.T) {
	p, q, api := newTestPatcher(t)
	taskID := uuidFromString(t, "ee333333-ee33-ee33-ee33-eeeeeeeeeeee")
	cardID := uuidFromString(t, "dd111111-dd11-dd11-dd11-dddddddddddd")
	q.card = db.LarkOutboundCardMessage{
		ID:                cardID,
		LarkCardMessageID: "lark_card_msg_existing",
		ChatSessionID:     q.binding.ChatSessionID,
		LarkChatID:        q.binding.LarkChatID,
		Status:            string(CardStatusStreaming),
	}
	q.cardErr = nil
	// Pretend we just patched ms ago so the throttle would otherwise refuse.
	p.markPatched(taskID)

	p.handleEvent(events.Event{
		Type:          protocol.EventChatDone,
		TaskID:        uuidString(taskID),
		ChatSessionID: uuidString(q.binding.ChatSessionID),
		Payload: protocol.ChatDonePayload{
			TaskID:        uuidString(taskID),
			ChatSessionID: uuidString(q.binding.ChatSessionID),
			Content:       "agent reply text",
		},
	})

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.patched) != 1 {
		t.Fatalf("final transition must bypass throttle; got %d patches", len(api.patched))
	}
	if api.patched[0].LarkCardMessageID != "lark_card_msg_existing" {
		t.Fatalf("final patch must target existing card id, got %q", api.patched[0].LarkCardMessageID)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.statusUpdates) != 1 || q.statusUpdates[0].Status != string(CardStatusFinal) {
		t.Fatalf("expected single final status update, got %+v", q.statusUpdates)
	}
}

func TestPatcherFailEventTransitionsToError(t *testing.T) {
	p, q, api := newTestPatcher(t)
	taskID := uuidFromString(t, "ee444444-ee44-ee44-ee44-eeeeeeeeeeee")
	q.card = db.LarkOutboundCardMessage{
		ID:                uuidFromString(t, "dd222222-dd22-dd22-dd22-dddddddddddd"),
		LarkCardMessageID: "lark_card_msg_existing",
		Status:            string(CardStatusStreaming),
	}
	q.cardErr = nil

	p.handleEvent(events.Event{
		Type:          protocol.EventTaskFailed,
		TaskID:        uuidString(taskID),
		ChatSessionID: uuidString(q.binding.ChatSessionID),
		Payload: map[string]any{
			"task_id":         uuidString(taskID),
			"chat_session_id": uuidString(q.binding.ChatSessionID),
			"error":           "boom",
		},
	})

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.patched) != 1 {
		t.Fatalf("fail event must patch the card; got %d patches", len(api.patched))
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.statusUpdates) != 1 || q.statusUpdates[0].Status != string(CardStatusError) {
		t.Fatalf("expected single error status update, got %+v", q.statusUpdates)
	}
}

func TestPatcherSwallowsInstallationLoadErrors(t *testing.T) {
	p, q, api := newTestPatcher(t)
	q.installationErr = errors.New("db down")

	p.handleEvent(events.Event{
		Type:          protocol.EventTaskQueued,
		TaskID:        uuidString(uuidFromString(t, "ee555555-ee55-ee55-ee55-eeeeeeeeeeee")),
		ChatSessionID: uuidString(q.binding.ChatSessionID),
	})

	// The patcher logs but never panics; no card sent.
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 0 {
		t.Fatalf("DB failure must not result in a card send; got %d", len(api.sent))
	}
}
