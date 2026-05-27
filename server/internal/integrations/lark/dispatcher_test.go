package lark

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeQueries is the unit-test seam for DispatcherQueries. Each field
// is the canned response the fake returns from the corresponding
// method; ErrNoRows variants pin specific failure modes.
//
// Dedup state lives in `dedupSeen` (set of message_ids previously
// inserted) so tests can simulate a Lark reconnect that replays the
// same event by calling Handle twice with the same MessageID. The
// default behavior — empty set — accepts every insert (first delivery
// for every test that does not pre-seed it).
type fakeQueries struct {
	installationByApp  db.LarkInstallation
	installationErr    error
	userBinding        db.LarkUserBinding
	userBindingErr     error
	chatSession        db.ChatSession
	chatSessionErr     error
	dedupSeen          map[string]struct{}
	dedupErr           error
	calledUserBinding  int
	calledChatSession  int
	calledInstallation int
	calledDedup        int
}

func (f *fakeQueries) GetLarkInstallationByAppID(ctx context.Context, appID string) (db.LarkInstallation, error) {
	f.calledInstallation++
	return f.installationByApp, f.installationErr
}

func (f *fakeQueries) GetLarkUserBindingByOpenID(ctx context.Context, arg db.GetLarkUserBindingByOpenIDParams) (db.LarkUserBinding, error) {
	f.calledUserBinding++
	return f.userBinding, f.userBindingErr
}

func (f *fakeQueries) GetChatSession(ctx context.Context, id pgtype.UUID) (db.ChatSession, error) {
	f.calledChatSession++
	return f.chatSession, f.chatSessionErr
}

func (f *fakeQueries) TryInsertLarkInboundDedup(ctx context.Context, messageID string) (string, error) {
	f.calledDedup++
	if f.dedupErr != nil {
		return "", f.dedupErr
	}
	if f.dedupSeen == nil {
		f.dedupSeen = map[string]struct{}{}
	}
	if _, hit := f.dedupSeen[messageID]; hit {
		// Mirror the real query: ON CONFLICT DO NOTHING ... RETURNING
		// surfaces pgx.ErrNoRows when the row already exists.
		return "", pgx.ErrNoRows
	}
	f.dedupSeen[messageID] = struct{}{}
	return messageID, nil
}

// fakeChat is a stub ChatSessionService that records what the
// dispatcher asked of it and returns canned outcomes.
type fakeChat struct {
	ensureID         pgtype.UUID
	ensureErr        error
	appendResult     AppendResult
	appendErr        error
	calledEnsure     int
	calledAppend     int
	lastAppendParams AppendUserMessageParams
	lastEnsureParams EnsureChatSessionParams
}

func (f *fakeChat) EnsureChatSession(ctx context.Context, p EnsureChatSessionParams) (pgtype.UUID, error) {
	f.calledEnsure++
	f.lastEnsureParams = p
	return f.ensureID, f.ensureErr
}

func (f *fakeChat) AppendUserMessage(ctx context.Context, p AppendUserMessageParams) (AppendResult, error) {
	f.calledAppend++
	f.lastAppendParams = p
	return f.appendResult, f.appendErr
}

type fakeAudit struct {
	drops []AuditDropParams
}

func (f *fakeAudit) RecordDrop(ctx context.Context, p AuditDropParams) error {
	f.drops = append(f.drops, p)
	return nil
}

type fakeIssueCreator struct {
	called int
	params service.IssueCreateParams
	result service.IssueCreateResult
	err    error
}

func (f *fakeIssueCreator) Create(ctx context.Context, p service.IssueCreateParams, _ service.IssueCreateOpts) (service.IssueCreateResult, error) {
	f.called++
	f.params = p
	return f.result, f.err
}

type fakeEnqueuer struct {
	called int
	task   db.AgentTaskQueue
	err    error
}

func (f *fakeEnqueuer) EnqueueChatTask(ctx context.Context, _ db.ChatSession) (db.AgentTaskQueue, error) {
	f.called++
	return f.task, f.err
}

// validUUID builds a deterministic Valid pgtype.UUID from the supplied
// byte. Useful for distinguishing IDs in assertions.
func validUUID(b byte) pgtype.UUID {
	var u pgtype.UUID
	for i := range u.Bytes {
		u.Bytes[i] = b
	}
	u.Valid = true
	return u
}

func activeInstallation() db.LarkInstallation {
	return db.LarkInstallation{
		ID:              validUUID(0x11),
		WorkspaceID:     validUUID(0x22),
		AgentID:         validUUID(0x33),
		InstallerUserID: validUUID(0x99),
		Status:          string(InstallationActive),
	}
}

func boundUser() db.LarkUserBinding {
	return db.LarkUserBinding{
		ID:             validUUID(0x44),
		WorkspaceID:    validUUID(0x22),
		MulticaUserID:  validUUID(0x55),
		InstallationID: validUUID(0x11),
		LarkOpenID:     "ou_user_a",
	}
}

func TestDispatcher_UnknownAppDropped(t *testing.T) {
	queries := &fakeQueries{installationErr: pgx.ErrNoRows}
	audit := &fakeAudit{}
	d := &Dispatcher{Queries: queries, Audit: audit}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:     "missing",
		EventType: "im.message.receive_v1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != OutcomeDropped || res.DropReason != DropReasonInvalidEvent {
		t.Fatalf("unexpected outcome: %+v", res)
	}
	if len(audit.drops) != 1 || audit.drops[0].Reason != DropReasonInvalidEvent {
		t.Fatalf("expected one invalid_event audit row, got %+v", audit.drops)
	}
	if audit.drops[0].InstallationID.Valid {
		t.Fatalf("audit row should omit installation_id for unknown app: %+v", audit.drops[0])
	}
}

func TestDispatcher_RevokedInstallationDropped(t *testing.T) {
	inst := activeInstallation()
	inst.Status = string(InstallationRevoked)
	queries := &fakeQueries{installationByApp: inst}
	audit := &fakeAudit{}
	d := &Dispatcher{Queries: queries, Audit: audit}

	res, err := d.Handle(context.Background(), InboundMessage{AppID: "ok"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.DropReason != DropReasonRevokedInstallation {
		t.Fatalf("got drop reason %q", res.DropReason)
	}
	if len(audit.drops) != 1 || audit.drops[0].Reason != DropReasonRevokedInstallation {
		t.Fatalf("audit drops: %+v", audit.drops)
	}
}

func TestDispatcher_GroupWithoutMentionDropped(t *testing.T) {
	queries := &fakeQueries{installationByApp: activeInstallation()}
	audit := &fakeAudit{}
	d := &Dispatcher{Queries: queries, Audit: audit}

	res, _ := d.Handle(context.Background(), InboundMessage{
		AppID:          "ok",
		ChatType:       ChatTypeGroup,
		AddressedToBot: false,
	})
	if res.DropReason != DropReasonNotAddressedInGroup {
		t.Fatalf("got drop reason %q", res.DropReason)
	}
	if queries.calledUserBinding != 0 {
		t.Fatalf("identity check should be skipped before group filter, got %d calls", queries.calledUserBinding)
	}
}

func TestDispatcher_UnboundUserAsksForBinding(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBindingErr:    pgx.ErrNoRows,
	}
	audit := &fakeAudit{}
	d := &Dispatcher{Queries: queries, Audit: audit}

	res, _ := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
	})
	if res.Outcome != OutcomeNeedsBinding {
		t.Fatalf("expected OutcomeNeedsBinding, got %q", res.Outcome)
	}
	if res.DropReason != DropReasonUnboundUser {
		t.Fatalf("expected unbound_user drop reason, got %q", res.DropReason)
	}
	if res.SenderOpenID != "ou_user_a" {
		t.Fatalf("sender propagation broken: %q", res.SenderOpenID)
	}
	if len(audit.drops) != 1 || audit.drops[0].Reason != DropReasonUnboundUser {
		t.Fatalf("expected one unbound_user audit row, got %+v", audit.drops)
	}
}

func TestDispatcher_PlainMessageEnqueuesTask(t *testing.T) {
	sessionID := validUUID(0x66)
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
		chatSession:       db.ChatSession{ID: sessionID, AgentID: validUUID(0x33)},
	}
	chat := &fakeChat{
		ensureID:     sessionID,
		appendResult: AppendResult{},
	}
	enq := &fakeEnqueuer{task: db.AgentTaskQueue{ID: validUUID(0x77)}}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        chat,
		Audit:       &fakeAudit{},
		TaskService: enq,
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "hi bot",
		MessageID:    "msg-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != OutcomeIngested {
		t.Fatalf("expected ingested, got %q", res.Outcome)
	}
	if !res.TaskID.Valid || res.TaskID != enq.task.ID {
		t.Fatalf("task id propagation broken: %+v", res.TaskID)
	}
	// For p2p the session creator should be the bound user, not the
	// installer — verifies the chat-type branch in Handle.
	if chat.lastEnsureParams.Sender != queries.userBinding.MulticaUserID {
		t.Fatalf("p2p session creator should be sender; got %+v", chat.lastEnsureParams.Sender)
	}
}

func TestDispatcher_GroupMessageUsesInstallerAsCreator(t *testing.T) {
	inst := activeInstallation()
	sessionID := validUUID(0x66)
	queries := &fakeQueries{
		installationByApp: inst,
		userBinding:       boundUser(),
		chatSession:       db.ChatSession{ID: sessionID, AgentID: inst.AgentID},
	}
	chat := &fakeChat{ensureID: sessionID, appendResult: AppendResult{}}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        chat,
		Audit:       &fakeAudit{},
		TaskService: &fakeEnqueuer{task: db.AgentTaskQueue{ID: validUUID(0x77)}},
	}

	_, _ = d.Handle(context.Background(), InboundMessage{
		AppID:          "ok",
		ChatType:       ChatTypeGroup,
		AddressedToBot: true,
		SenderOpenID:   "ou_user_a",
		Body:           "hey",
		MessageID:      "msg-g",
	})
	if chat.lastEnsureParams.Sender != inst.InstallerUserID {
		t.Fatalf("group session creator should be installer; got %+v want %+v",
			chat.lastEnsureParams.Sender, inst.InstallerUserID)
	}
}

func TestDispatcher_DedupHitDoesNotEnqueue(t *testing.T) {
	// Pre-seed the dedup table so the top-level dedup gate trips on
	// the first Handle call — simulates a Lark reconnect replaying an
	// event we already processed in a previous run.
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
		dedupSeen:         map[string]struct{}{"msg-dup": {}},
	}
	chat := &fakeChat{
		ensureID: validUUID(0x66),
	}
	enq := &fakeEnqueuer{}
	audit := &fakeAudit{}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        chat,
		Audit:       audit,
		TaskService: enq,
	}

	res, _ := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "replay",
		MessageID:    "msg-dup",
	})
	if res.Outcome != OutcomeDropped || res.DropReason != DropReasonDuplicate {
		t.Fatalf("expected duplicate drop, got %+v", res)
	}
	if enq.called != 0 {
		t.Fatalf("dedup hit must not enqueue task, called=%d", enq.called)
	}
	if chat.calledEnsure != 0 || chat.calledAppend != 0 {
		t.Fatalf("dedup hit must short-circuit before chat lookup; ensure=%d append=%d",
			chat.calledEnsure, chat.calledAppend)
	}
	if queries.calledUserBinding != 0 {
		t.Fatalf("dedup hit must short-circuit before identity check, got %d binding calls",
			queries.calledUserBinding)
	}
	if len(audit.drops) != 1 || audit.drops[0].Reason != DropReasonDuplicate {
		t.Fatalf("expected duplicate audit row, got %+v", audit.drops)
	}
}

// TestDispatcher_DedupBeforeGroupFilter pins the §4.3 ordering: a
// replayed group event that was NOT addressed to the Bot must NOT
// re-write a not_addressed_in_group audit row on every reconnect, and
// must NOT re-trigger any binding-prompt side effect. The top-level
// dedup gate is what guarantees this; before this fix the group
// filter ran first and unbounded replays produced unbounded audit
// noise + reply-card spam.
func TestDispatcher_DedupBeforeGroupFilter(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		dedupSeen:         map[string]struct{}{"msg-replay": {}},
	}
	audit := &fakeAudit{}
	d := &Dispatcher{Queries: queries, Audit: audit}

	res, _ := d.Handle(context.Background(), InboundMessage{
		AppID:          "ok",
		ChatType:       ChatTypeGroup,
		AddressedToBot: false,
		MessageID:      "msg-replay",
	})
	if res.DropReason != DropReasonDuplicate {
		t.Fatalf("dedup must beat group filter; got drop reason %q", res.DropReason)
	}
	if len(audit.drops) != 1 || audit.drops[0].Reason != DropReasonDuplicate {
		t.Fatalf("expected exactly one duplicate audit row, got %+v", audit.drops)
	}
}

// TestDispatcher_DedupBeforeIdentityCheck pins the same ordering for
// unbound users: a replayed event from an unbound open_id must not
// re-fire the OutcomeNeedsBinding path on every reconnect — that
// would spam the user with binding-prompt cards.
func TestDispatcher_DedupBeforeIdentityCheck(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBindingErr:    pgx.ErrNoRows, // unbound — would normally trigger OutcomeNeedsBinding
		dedupSeen:         map[string]struct{}{"msg-replay": {}},
	}
	audit := &fakeAudit{}
	d := &Dispatcher{Queries: queries, Audit: audit}

	res, _ := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		MessageID:    "msg-replay",
	})
	if res.Outcome != OutcomeDropped || res.DropReason != DropReasonDuplicate {
		t.Fatalf("dedup must beat identity check; got %+v", res)
	}
	if queries.calledUserBinding != 0 {
		t.Fatalf("identity check must not run for a deduped replay, got %d calls",
			queries.calledUserBinding)
	}
}

func TestDispatcher_IssueCommandCreatesIssue(t *testing.T) {
	sessionID := validUUID(0x66)
	inst := activeInstallation()
	queries := &fakeQueries{
		installationByApp: inst,
		userBinding:       boundUser(),
		chatSession:       db.ChatSession{ID: sessionID, AgentID: inst.AgentID},
	}
	chat := &fakeChat{
		ensureID: sessionID,
		appendResult: AppendResult{
			IssueCommand: &IssueCommand{Title: "ship it", Description: "ship the thing"},
		},
	}
	issueSvc := &fakeIssueCreator{result: service.IssueCreateResult{Issue: db.Issue{ID: validUUID(0x88), Number: 42}}}
	d := &Dispatcher{
		Queries:      queries,
		Chat:         chat,
		Audit:        &fakeAudit{},
		IssueService: issueSvc,
		TaskService:  &fakeEnqueuer{task: db.AgentTaskQueue{ID: validUUID(0x77)}},
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "/issue ship it\nship the thing",
		MessageID:    "msg-ic",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issueSvc.called != 1 {
		t.Fatalf("expected IssueService.Create called once, got %d", issueSvc.called)
	}
	if issueSvc.params.Title != "ship it" || issueSvc.params.Description.String != "ship the thing" {
		t.Fatalf("wrong issue params: %+v", issueSvc.params)
	}
	if issueSvc.params.OriginType.String != originLarkChat {
		t.Fatalf("origin_type should be lark_chat, got %q", issueSvc.params.OriginType.String)
	}
	if !issueSvc.params.AssigneeType.Valid || issueSvc.params.AssigneeType.String != "agent" ||
		issueSvc.params.AssigneeID != inst.AgentID {
		t.Fatalf("assignee should default to the installation's agent: %+v", issueSvc.params)
	}
	if !res.IssueID.Valid || res.IssueNumber != 42 {
		t.Fatalf("issue id/number not propagated: %+v", res)
	}
}

func TestDispatcher_EmptyTitleSurfacesError(t *testing.T) {
	sessionID := validUUID(0x66)
	inst := activeInstallation()
	queries := &fakeQueries{
		installationByApp: inst,
		userBinding:       boundUser(),
		chatSession:       db.ChatSession{ID: sessionID, AgentID: inst.AgentID},
	}
	chat := &fakeChat{
		ensureID: sessionID,
		appendResult: AppendResult{
			IssueCommand: &IssueCommand{Title: ""},
		},
	}
	issueSvc := &fakeIssueCreator{}
	d := &Dispatcher{
		Queries:      queries,
		Chat:         chat,
		Audit:        &fakeAudit{},
		IssueService: issueSvc,
		TaskService:  &fakeEnqueuer{},
	}

	_, err := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "/issue",
		MessageID:    "msg-empty",
	})
	if !errors.Is(err, ErrEmptyIssueTitle) {
		t.Fatalf("expected ErrEmptyIssueTitle wrapped, got %v", err)
	}
	if issueSvc.called != 0 {
		t.Fatalf("IssueService.Create must not run when title is empty")
	}
}

func TestDispatcher_AgentOfflineFallsThroughCleanly(t *testing.T) {
	sessionID := validUUID(0x66)
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
		chatSession:       db.ChatSession{ID: sessionID, AgentID: validUUID(0x33)},
	}
	chat := &fakeChat{ensureID: sessionID, appendResult: AppendResult{}}
	enq := &fakeEnqueuer{err: service.ErrChatTaskAgentNoRuntime}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        chat,
		Audit:       &fakeAudit{},
		TaskService: enq,
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "hi",
		MessageID:    "msg-off",
	})
	if err != nil {
		t.Fatalf("offline path should not return error, got %v", err)
	}
	if res.Outcome != OutcomeAgentOffline {
		t.Fatalf("expected OutcomeAgentOffline, got %q", res.Outcome)
	}
	if res.ChatSessionID != sessionID {
		t.Fatalf("session id not propagated: %+v", res.ChatSessionID)
	}
}

func TestDispatcher_AgentArchivedSurfacesDistinctOutcome(t *testing.T) {
	sessionID := validUUID(0x66)
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
		chatSession:       db.ChatSession{ID: sessionID, AgentID: validUUID(0x33)},
	}
	chat := &fakeChat{ensureID: sessionID, appendResult: AppendResult{}}
	enq := &fakeEnqueuer{err: service.ErrChatTaskAgentArchived}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        chat,
		Audit:       &fakeAudit{},
		TaskService: enq,
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "hi",
		MessageID:    "msg-arch",
	})
	if err != nil {
		t.Fatalf("archived path should not return error, got %v", err)
	}
	if res.Outcome != OutcomeAgentArchived {
		t.Fatalf("expected OutcomeAgentArchived, got %q", res.Outcome)
	}
}

func TestDispatcher_InfraFailureSurfacesError(t *testing.T) {
	// A DB / load / create failure from TaskService.EnqueueChatTask is
	// NOT a productizable state — the WS adapter must see a real
	// error so it can retry or page, not an "offline" card that
	// silently hides the outage.
	sessionID := validUUID(0x66)
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
		chatSession:       db.ChatSession{ID: sessionID, AgentID: validUUID(0x33)},
	}
	chat := &fakeChat{ensureID: sessionID, appendResult: AppendResult{}}
	infraErr := errors.New("create chat task: connection refused")
	enq := &fakeEnqueuer{err: infraErr}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        chat,
		Audit:       &fakeAudit{},
		TaskService: enq,
	}

	_, err := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "hi",
		MessageID:    "msg-infra",
	})
	if err == nil {
		t.Fatalf("infra failure should surface as error, got nil")
	}
	if !errors.Is(err, infraErr) {
		t.Fatalf("infra error should propagate (errors.Is), got %v", err)
	}
}
