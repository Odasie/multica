package lark

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// InboundMessage is the normalized shape the WebSocket adapter hands
// to the Dispatcher. The adapter (Phase 2 PR) translates the raw Lark
// event payload into this struct; the Dispatcher does NOT know what a
// Lark event JSON object looks like. This keeps event-schema changes
// from rippling into business logic.
//
// AddressedToBot is the adapter's verdict on whether a group-chat
// message is an interaction with the Bot (@-mention or reply to a
// Bot card). For p2p messages this field is ignored.
type InboundMessage struct {
	EventType      string
	EventID        string
	AppID          string
	ChatID         ChatID
	ChatType       ChatType
	MessageID      string
	SenderOpenID   OpenID
	Body           string
	AddressedToBot bool
}

// Outcome categorizes what the Dispatcher decided to do with an
// inbound message. The WS adapter inspects this and chooses what to
// reply with on the Lark side.
type Outcome string

const (
	// OutcomeDropped — the message was not ingested (identity failed,
	// dedup hit, group filter, etc.). DispatchResult.DropReason holds
	// the audit category.
	OutcomeDropped Outcome = "dropped"

	// OutcomeNeedsBinding — the open_id is unbound; the WS adapter
	// should mint a binding token via BindingTokenService and send
	// the "click here to bind" card. DispatchResult.SenderOpenID and
	// .InstallationID are populated so the adapter can target the
	// reply.
	OutcomeNeedsBinding Outcome = "needs_binding"

	// OutcomeIngested — the message landed in chat_session and an
	// agent task was enqueued. Empty IssueCommand means a plain chat
	// message; non-empty means /issue ran (see IssueID for the new
	// issue's UUID).
	OutcomeIngested Outcome = "ingested"

	// OutcomeAgentOffline — the message landed in chat_session, but
	// the agent has no runtime bound at all (agent.runtime_id IS
	// NULL). The adapter should reply with "agent offline, will run
	// on next online." The chat_message row remains so the agent
	// picks it up on resume.
	//
	// IMPORTANT: this is NOT triggered when a daemon is merely
	// disconnected. If agent.runtime_id IS set, the chat task is
	// enqueued and waits for the daemon to claim it on next online;
	// that path returns OutcomeIngested with a TaskID.
	OutcomeAgentOffline Outcome = "agent_offline"

	// OutcomeAgentArchived — the message landed in chat_session, but
	// the agent has been archived. The adapter should reply with a
	// distinct copy ("this agent has been archived; ask an admin to
	// unarchive or rebind"). Kept separate from OutcomeAgentOffline
	// because the user-facing remediation differs.
	OutcomeAgentArchived Outcome = "agent_archived"
)

// DispatchResult is the typed return from Dispatcher.Handle. Callers
// (the WS adapter) consume this to drive their outbound side; nothing
// here implies the adapter MUST reply, only that it CAN.
type DispatchResult struct {
	Outcome        Outcome
	DropReason     DropReason
	InstallationID pgtype.UUID
	ChatSessionID  pgtype.UUID
	SenderOpenID   OpenID
	TaskID         pgtype.UUID
	IssueID        pgtype.UUID
	IssueNumber    int32
}

// IssueCreator is the narrow subset of service.IssueService the
// Dispatcher needs. Declared here as an interface so this package can
// be unit-tested without bringing the full service graph along.
type IssueCreator interface {
	Create(ctx context.Context, p service.IssueCreateParams, opts service.IssueCreateOpts) (service.IssueCreateResult, error)
}

// ChatTaskEnqueuer is the narrow subset of service.TaskService the
// Dispatcher needs. It exists for the same reason as IssueCreator:
// the Dispatcher is small enough that depending on the whole
// TaskService struct is gratuitous.
type ChatTaskEnqueuer interface {
	EnqueueChatTask(ctx context.Context, session db.ChatSession) (db.AgentTaskQueue, error)
}

// DispatcherQueries is the narrow subset of *db.Queries the Dispatcher
// needs for installation routing, identity lookup, dedup, and session
// reload. *db.Queries satisfies it directly; tests substitute a fake.
type DispatcherQueries interface {
	GetLarkInstallationByAppID(ctx context.Context, appID string) (db.LarkInstallation, error)
	GetLarkUserBindingByOpenID(ctx context.Context, arg db.GetLarkUserBindingByOpenIDParams) (db.LarkUserBinding, error)
	GetChatSession(ctx context.Context, id pgtype.UUID) (db.ChatSession, error)
	TryInsertLarkInboundDedup(ctx context.Context, messageID string) (string, error)
}

// Dispatcher is the single per-message entry point on the inbound
// path. It owns the order in which identity check, group filter,
// dedup, ingest, /issue, and task enqueue happen — the WS adapter
// MUST NOT bypass it. That ordering is the invariant that keeps the
// design's §4.3 safety property ("unbound users never reach
// chat_session") true at runtime.
type Dispatcher struct {
	Queries      DispatcherQueries
	Chat         ChatSessionService
	Audit        AuditLogger
	IssueService IssueCreator
	TaskService  ChatTaskEnqueuer
}

// Handle processes one inbound Lark message end-to-end. It never
// returns an error for "this message was dropped" — those are
// reported via Outcome + DropReason and a non-nil err is reserved for
// real infrastructure failures (DB down, etc.) that the WS adapter
// should retry.
func (d *Dispatcher) Handle(ctx context.Context, msg InboundMessage) (DispatchResult, error) {
	// 1. Route to installation. The app_id is the only identifier
	//    that ties an event to its installation row.
	inst, err := d.Queries.GetLarkInstallationByAppID(ctx, msg.AppID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No installation for this app_id. Audit without an
			// installation FK (nullable on lark_inbound_audit).
			_ = d.Audit.RecordDrop(ctx, AuditDropParams{
				EventType:     msg.EventType,
				LarkEventID:   msg.EventID,
				LarkMessageID: msg.MessageID,
				ChatID:        msg.ChatID,
				Reason:        DropReasonInvalidEvent,
			})
			return DispatchResult{Outcome: OutcomeDropped, DropReason: DropReasonInvalidEvent}, nil
		}
		return DispatchResult{}, fmt.Errorf("load installation: %w", err)
	}
	if InstallationStatus(inst.Status) != InstallationActive {
		return d.drop(ctx, msg, inst.ID, DropReasonRevokedInstallation), nil
	}

	// 2. Top-level dedup. Spec §4.3 puts this before group filter and
	//    identity check so a WebSocket reconnect that replays an event
	//    cannot:
	//      a) re-trigger the binding prompt for an unbound user, or
	//      b) re-write the not_addressed_in_group / unbound_user audit
	//         rows, or
	//      c) re-touch the chat_session for a bound message.
	//    We keep an in-transaction dedup inside AppendUserMessage as a
	//    belt-and-suspenders safeguard, but THIS is the gate that
	//    keeps the audit / outbound side from getting noisy under
	//    reconnect storms.
	//
	//    Empty MessageID means the event has no Lark message_id at all
	//    (non-message events, malformed payloads) and skipping dedup
	//    is the safe default — we have no key to deduplicate by.
	if msg.MessageID != "" {
		if _, err := d.Queries.TryInsertLarkInboundDedup(ctx, msg.MessageID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return d.drop(ctx, msg, inst.ID, DropReasonDuplicate), nil
			}
			return DispatchResult{}, fmt.Errorf("dedup insert: %w", err)
		}
	}

	// 3. Group-mention filter (group chats only). We do this BEFORE
	//    identity check so that an unbound user's idle group chatter
	//    never produces an "you need to bind" reply card spam — the
	//    Bot is not addressed, so we say nothing.
	if msg.ChatType == ChatTypeGroup && !msg.AddressedToBot {
		return d.drop(ctx, msg, inst.ID, DropReasonNotAddressedInGroup), nil
	}

	// 4. Identity check. A row in lark_user_binding means the open_id
	//    maps to a current workspace member (the composite FK to
	//    member cascades the binding away on membership revocation).
	binding, err := d.Queries.GetLarkUserBindingByOpenID(ctx, db.GetLarkUserBindingByOpenIDParams{
		InstallationID: inst.ID,
		LarkOpenID:     string(msg.SenderOpenID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_ = d.Audit.RecordDrop(ctx, AuditDropParams{
				InstallationID: inst.ID,
				ChatID:         msg.ChatID,
				EventType:      msg.EventType,
				LarkEventID:    msg.EventID,
				LarkMessageID:  msg.MessageID,
				Reason:         DropReasonUnboundUser,
			})
			return DispatchResult{
				Outcome:        OutcomeNeedsBinding,
				DropReason:     DropReasonUnboundUser,
				InstallationID: inst.ID,
				SenderOpenID:   msg.SenderOpenID,
			}, nil
		}
		return DispatchResult{}, fmt.Errorf("load user binding: %w", err)
	}

	// 5. Resolve the chat_session. For group chats, the session
	//    creator is the INSTALLER (stable workspace identity that
	//    won't cascade-delete when individual group members churn);
	//    for p2p, the sender is the one and only human in the chat
	//    so we use them.
	sessionCreator := binding.MulticaUserID
	if msg.ChatType == ChatTypeGroup {
		sessionCreator = inst.InstallerUserID
	}
	sessionID, err := d.Chat.EnsureChatSession(ctx, EnsureChatSessionParams{
		WorkspaceID:    inst.WorkspaceID,
		InstallationID: inst.ID,
		AgentID:        inst.AgentID,
		ChatID:         msg.ChatID,
		ChatType:       msg.ChatType,
		Sender:         sessionCreator,
	})
	if err != nil {
		return DispatchResult{}, fmt.Errorf("ensure chat session: %w", err)
	}

	// 6. Append message. The top-level dedup gate above is the
	//    authoritative idempotency check; the UNIQUE (message_id)
	//    constraint on lark_inbound_message_dedup is the ultimate
	//    backstop if two Handle calls race past the gate, in which
	//    case AppendUserMessage will surface a DB-level error rather
	//    than silently corrupting chat_session.
	appendRes, err := d.Chat.AppendUserMessage(ctx, AppendUserMessageParams{
		ChatSessionID: sessionID,
		Sender:        binding.MulticaUserID,
		Body:          msg.Body,
		LarkMessageID: msg.MessageID,
	})
	if err != nil {
		return DispatchResult{}, fmt.Errorf("append user message: %w", err)
	}

	res := DispatchResult{
		Outcome:        OutcomeIngested,
		InstallationID: inst.ID,
		ChatSessionID:  sessionID,
		SenderOpenID:   msg.SenderOpenID,
	}

	// 7. /issue command, if present. The IssueCommand result from
	//    AppendUserMessage is non-nil only when the body parsed as a
	//    valid /issue invocation. We dispatch through IssueService so
	//    duplicate guard, counter, broadcast and analytics all run.
	if appendRes.IssueCommand != nil {
		issueRes, err := d.createIssueFromCommand(ctx, inst, binding.MulticaUserID, sessionID, *appendRes.IssueCommand)
		if err != nil {
			return DispatchResult{}, fmt.Errorf("create issue from command: %w", err)
		}
		res.IssueID = issueRes.Issue.ID
		res.IssueNumber = issueRes.Issue.Number
	}

	// 8. Enqueue the chat task that triggers the agent run.
	//
	//    Only the productizable rejections from EnqueueChatTask
	//    (agent archived, agent has no runtime configured) are mapped
	//    to a user-visible Outcome here — the chat_message is already
	//    written, so the user's input is not lost. Real infra
	//    failures (DB load / insert errors) are returned to the WS
	//    adapter as errors so it can retry or page; we don't disguise
	//    them as an "offline" card.
	//
	//    Note: a daemon that's merely disconnected is NOT an error
	//    here. As long as agent.runtime_id is set, the chat task is
	//    enqueued and waits for the daemon to claim it on next
	//    online; this path returns OutcomeIngested with a TaskID.
	session, err := d.Queries.GetChatSession(ctx, sessionID)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("reload chat session: %w", err)
	}
	task, err := d.TaskService.EnqueueChatTask(ctx, session)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrChatTaskAgentNoRuntime):
			res.Outcome = OutcomeAgentOffline
			return res, nil
		case errors.Is(err, service.ErrChatTaskAgentArchived):
			res.Outcome = OutcomeAgentArchived
			return res, nil
		default:
			return DispatchResult{}, fmt.Errorf("enqueue chat task: %w", err)
		}
	}
	res.TaskID = task.ID
	return res, nil
}

func (d *Dispatcher) drop(ctx context.Context, msg InboundMessage, instID pgtype.UUID, reason DropReason) DispatchResult {
	_ = d.Audit.RecordDrop(ctx, AuditDropParams{
		InstallationID: instID,
		ChatID:         msg.ChatID,
		EventType:      msg.EventType,
		LarkEventID:    msg.EventID,
		LarkMessageID:  msg.MessageID,
		Reason:         reason,
	})
	return DispatchResult{
		Outcome:        OutcomeDropped,
		DropReason:     reason,
		InstallationID: instID,
	}
}

func (d *Dispatcher) createIssueFromCommand(
	ctx context.Context,
	inst db.LarkInstallation,
	creatorUserID pgtype.UUID,
	sessionID pgtype.UUID,
	cmd IssueCommand,
) (service.IssueCreateResult, error) {
	// Empty title at this point means the /issue alone fallback found
	// no previous user message either. The product copy ("请填标题")
	// belongs in the WS adapter's reply card; we surface this to the
	// caller as ErrEmptyIssueTitle so the dispatcher can short-circuit
	// without paying the IssueService cost.
	if cmd.Title == "" {
		return service.IssueCreateResult{}, ErrEmptyIssueTitle
	}
	params := service.IssueCreateParams{
		WorkspaceID:  inst.WorkspaceID,
		Title:        cmd.Title,
		Description:  pgtype.Text{String: cmd.Description, Valid: cmd.Description != ""},
		Status:       "todo",
		Priority:     "none",
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
		AssigneeID:   inst.AgentID,
		CreatorType:  "member",
		CreatorID:    creatorUserID,
		OriginType:   pgtype.Text{String: originLarkChat, Valid: true},
		OriginID:     sessionID,
	}
	return d.IssueService.Create(ctx, params, service.IssueCreateOpts{})
}

// originLarkChat is the issue.origin_type label written for issues
// created via the Lark `/issue` command. The analytics classifier in
// service.classifyOrigin currently maps unknown origin_type values to
// SourceManual with a warning — that is acceptable for MVP. A
// dedicated analytics source label can be added when product asks for
// it.
const originLarkChat = "lark_chat"

// ErrEmptyIssueTitle is returned by createIssueFromCommand when the
// caller invoked /issue with no title AND the previous-user-message
// fallback found nothing usable. The WS adapter translates this into
// the "please supply a title" reply card per §2.3.
var ErrEmptyIssueTitle = errors.New("issue title is empty")
