package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// notifyParentOfChildDone posts a top-level system comment on the parent
// issue when a child issue transitions from non-done into done. This replaces
// the agent-prompt rule that previously made child agents post the
// notification themselves (PR #2918 user feedback — the agent rule caused
// self-mention loops, planner ping-pong, and accidental `MUL-` prefix
// hardcoding because the agent did not always know the workspace prefix).
//
// Guards on whether the comment fires at all:
//   - prev.Status must not already be "done" (idempotent — repeat saves of
//     done do not re-fire; only the transition fires)
//   - issue.Status must be "done"
//   - issue.ParentIssueID must be set
//   - parent must not be "done" or "cancelled" — the parent is already
//     closed and a notification has no follow-up to drive
//
// The comment is inserted directly via db.Queries (not through the
// CreateComment HTTP handler) so it bypasses the generic on_comment trigger
// path. When the parent has an assignee, the comment body embeds a single
// `mention://{member,agent,squad}/<id>` link that targets the parent
// assignee — Bohan's product call on MUL-2538 ("system child-done comment
// 无脑 mention parent assignee，member/squad/agent 都覆盖"). To keep the
// platform in control of side effects, the cmd/server notification + subscriber
// listeners still skip system comments wholesale, so smuggled mentions from
// the child title cannot light up unrelated members. The parent assignee's
// own trigger / inbox row is fired explicitly by dispatchParentAssigneeTrigger
// below, with the loop and idempotency guards documented there.
//
// Errors are logged at warn level and swallowed: this is a best-effort
// notification on the side of a successful status update; failing it must
// not roll back the user's status change.
func (h *Handler) notifyParentOfChildDone(ctx context.Context, prev, issue db.Issue) {
	if !issue.ParentIssueID.Valid {
		return
	}
	if prev.Status == "done" || issue.Status != "done" {
		return
	}
	parent, err := h.Queries.GetIssue(ctx, issue.ParentIssueID)
	if err != nil {
		slog.Warn("child done: failed to load parent",
			"error", err,
			"child_id", uuidToString(issue.ID),
			"parent_id", uuidToString(issue.ParentIssueID))
		return
	}
	if parent.Status == "done" || parent.Status == "cancelled" {
		return
	}

	prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
	identifier := prefix + "-" + strconv.Itoa(int(issue.Number))
	childID := uuidToString(issue.ID)
	title := sanitizeChildTitleForSystemComment(issue.Title)

	// Build the parent-assignee mention prefix. Empty when the parent has no
	// assignee or the assignee row is missing (deleted member, archived
	// agent the workspace lost track of, etc.).
	mentionPrefix := h.buildParentAssigneeMention(ctx, parent)

	content := fmt.Sprintf(
		"%sSub-issue [%s](mention://issue/%s) — \"%s\" — is done. Confirm whether to advance the next step on this parent (and promote any waiting `backlog` sub-issues).",
		mentionPrefix, identifier, childID, title,
	)

	// author_type='system', author_id=zero UUID. The zero UUID is a valid 16
	// byte value and the column is NOT NULL; frontend code should branch on
	// author_type === 'system' rather than on the UUID value.
	comment, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     parent.ID,
		WorkspaceID: parent.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     content,
		Type:        "system",
		ParentID:    pgtype.UUID{Valid: false},
	})
	if err != nil {
		slog.Warn("child done: create system comment failed",
			"error", err,
			"child_id", childID,
			"parent_id", uuidToString(parent.ID))
		return
	}

	h.publish(protocol.EventCommentCreated, uuidToString(parent.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         parent.Title,
		"issue_assignee_type": textToPtr(parent.AssigneeType),
		"issue_assignee_id":   uuidToPtr(parent.AssigneeID),
		"issue_status":        parent.Status,
	})

	// Dispatch the explicit trigger / inbox row for the parent assignee.
	// Listener-level mention parsing is intentionally NOT involved (the
	// notification + subscriber listeners both short-circuit on
	// author_type='system'); this keeps smuggled mentions from the child
	// title inert and gives the platform a single place to apply the loop
	// and idempotency guards.
	h.dispatchParentAssigneeTrigger(ctx, parent, issue, comment)
}

// sanitizeChildTitleForSystemComment removes mention-style markdown from a
// child issue's title before it is embedded into the parent's system
// comment. Smuggled mentions are already harmless on the listener path
// (notification + subscriber listeners both skip system comments), but the
// timeline still renders the title verbatim — stripping the markdown keeps
// the rendered comment readable and stops a maliciously titled child issue
// from looking like a directive ("@all please look").
func sanitizeChildTitleForSystemComment(title string) string {
	// Replace any markdown link target so the regex no longer matches it,
	// while preserving the human-readable label text. `]` and `(` are the
	// minimum delimiters of the mention regex; replacing the `(` is enough
	// to break the match without mangling the label.
	cleaned := strings.ReplaceAll(title, "](mention://", "] (mention-stripped://")
	return cleaned
}

// buildParentAssigneeMention returns the markdown prefix that the system
// comment should lead with, including a trailing space, so the body reads
// like a normal mention-led comment. Returns the empty string when the
// parent has no assignee or the assignee row could not be loaded.
func (h *Handler) buildParentAssigneeMention(ctx context.Context, parent db.Issue) string {
	if !parent.AssigneeType.Valid || !parent.AssigneeID.Valid {
		return ""
	}
	label, ok := h.resolveAssigneeMentionLabel(ctx, parent.WorkspaceID, parent.AssigneeType.String, parent.AssigneeID)
	if !ok {
		return ""
	}
	return fmt.Sprintf("[@%s](mention://%s/%s) ", label, parent.AssigneeType.String, uuidToString(parent.AssigneeID))
}

// resolveAssigneeMentionLabel returns the label text to render inside the
// mention link. The label is for human display only — the mention regex
// keys off the URL path, not the label — but a sensible fallback keeps the
// rendered comment legible if the frontend has not pre-loaded the assignee.
// Returns ok=false when the assignee row cannot be loaded; the caller
// should then omit the mention entirely rather than emit a broken link.
func (h *Handler) resolveAssigneeMentionLabel(ctx context.Context, workspaceID pgtype.UUID, assigneeType string, assigneeID pgtype.UUID) (string, bool) {
	switch assigneeType {
	case "agent":
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
			ID:          assigneeID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return "", false
		}
		return sanitizeMentionLabel(agent.Name), true
	case "squad":
		squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
			ID:          assigneeID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return "", false
		}
		return sanitizeMentionLabel(squad.Name), true
	case "member":
		member, err := h.Queries.GetMember(ctx, assigneeID)
		if err != nil {
			return "", false
		}
		user, err := h.Queries.GetUser(ctx, member.UserID)
		if err != nil {
			// Member row exists but the user record is missing — fall back
			// to a generic label rather than dropping the mention entirely.
			return "member", true
		}
		return sanitizeMentionLabel(user.Name), true
	}
	return "", false
}

// sanitizeMentionLabel strips characters that would break the mention
// markdown if a name contained them. The mention regex is non-greedy on the
// label, so a stray `]` would short-circuit it. Names with `]` are
// vanishingly rare but cheap to defend against.
func sanitizeMentionLabel(name string) string {
	cleaned := strings.ReplaceAll(name, "]", "")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return "assignee"
	}
	return cleaned
}

// dispatchParentAssigneeTrigger fires the explicit side effect that pairs
// with the @mention link in the system comment body — an agent task for
// agent / squad-leader assignees, an inbox row for member assignees. The
// generic comment listener is intentionally bypassed (it short-circuits on
// author_type='system'), so this is the single place where the platform
// applies loop and idempotency guards for the child-done notification.
//
// Guards applied here:
//   - No-op when the parent has no assignee row.
//   - Loop guard: skip when the parent assignee is the same entity that
//     "owns" the child (same agent id for agent/squad-leader assignees, or
//     same member id for member assignees). Without this an agent that
//     drives both child and parent immediately re-runs on the parent and
//     can post another child, looping; a member who flips their own
//     child's status receives a self-notification they did not ask for.
//   - Idempotency: HasPendingTaskForIssueAndAgent dedupes rapid-fire enqueues
//     for the same parent (e.g. two children finishing back-to-back).
//   - Readiness: archived agents / missing runtimes are silently skipped
//     so a closed-out agent does not surface as a phantom assignee.
func (h *Handler) dispatchParentAssigneeTrigger(ctx context.Context, parent, child db.Issue, systemComment db.Comment) {
	if !parent.AssigneeType.Valid || !parent.AssigneeID.Valid {
		return
	}

	switch parent.AssigneeType.String {
	case "agent":
		h.triggerChildDoneAgent(ctx, parent, child, systemComment.ID)
	case "squad":
		h.triggerChildDoneSquad(ctx, parent, child, systemComment.ID)
	case "member":
		h.triggerChildDoneMember(ctx, parent, child, systemComment)
	}
}

// triggerChildDoneAgent enqueues a mention-style task for the parent's
// agent assignee, applying the self-trigger guard documented on
// dispatchParentAssigneeTrigger.
func (h *Handler) triggerChildDoneAgent(ctx context.Context, parent, child db.Issue, triggerCommentID pgtype.UUID) {
	if childAssigneeIsAgent(child, parent.AssigneeID) {
		return
	}

	agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
		ID:          parent.AssigneeID,
		WorkspaceID: parent.WorkspaceID,
	})
	if err != nil || !agent.RuntimeID.Valid || agent.ArchivedAt.Valid {
		return
	}

	hasPending, err := h.Queries.HasPendingTaskForIssueAndAgent(ctx, db.HasPendingTaskForIssueAndAgentParams{
		IssueID: parent.ID,
		AgentID: parent.AssigneeID,
	})
	if err != nil || hasPending {
		return
	}

	if _, err := h.TaskService.EnqueueTaskForMention(ctx, parent, parent.AssigneeID, triggerCommentID); err != nil {
		slog.Warn("child done: enqueue parent agent task failed",
			"error", err,
			"parent_id", uuidToString(parent.ID),
			"agent_id", uuidToString(parent.AssigneeID))
	}
}

// triggerChildDoneSquad enqueues a leader-role task for the parent's squad
// assignee, applying the self-trigger guard against both the squad leader
// (same-agent loop) and the case where the child was driven by the same
// squad (the leader would just observe its own work).
func (h *Handler) triggerChildDoneSquad(ctx context.Context, parent, child db.Issue, triggerCommentID pgtype.UUID) {
	squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
		ID:          parent.AssigneeID,
		WorkspaceID: parent.WorkspaceID,
	})
	if err != nil {
		return
	}

	// Same-squad child → the leader has already observed the work via its
	// own coordination cycle on the child; firing again on the parent would
	// just re-trigger the same leader run with no new signal.
	if childAssigneeIsSquad(child, parent.AssigneeID) {
		return
	}
	// Same-agent loop: child driven by an agent who is also the leader.
	if childAssigneeIsAgent(child, squad.LeaderID) {
		return
	}

	agent, err := h.Queries.GetAgent(ctx, squad.LeaderID)
	if err != nil || !agent.RuntimeID.Valid || agent.ArchivedAt.Valid {
		return
	}

	hasPending, err := h.Queries.HasPendingTaskForIssueAndAgent(ctx, db.HasPendingTaskForIssueAndAgentParams{
		IssueID: parent.ID,
		AgentID: squad.LeaderID,
	})
	if err != nil || hasPending {
		return
	}

	if _, err := h.TaskService.EnqueueTaskForSquadLeader(ctx, parent, squad.LeaderID, triggerCommentID); err != nil {
		slog.Warn("child done: enqueue parent squad leader task failed",
			"error", err,
			"parent_id", uuidToString(parent.ID),
			"squad_id", uuidToString(squad.ID),
			"leader_id", uuidToString(squad.LeaderID))
	}
}

// triggerChildDoneMember creates a "mentioned"-type inbox row for the
// parent's member assignee so it surfaces in their inbox the same way an
// explicit @mention from a human comment would. The generic comment-mention
// listener does not run on system comments (see notification_listeners.go),
// so this is where the personal feed actually gets populated.
func (h *Handler) triggerChildDoneMember(ctx context.Context, parent, child db.Issue, systemComment db.Comment) {
	if childAssigneeIsMember(child, parent.AssigneeID) {
		return
	}

	member, err := h.Queries.GetMember(ctx, parent.AssigneeID)
	if err != nil {
		return
	}
	recipientUserID := member.UserID
	if !recipientUserID.Valid {
		return
	}

	details, _ := json.Marshal(map[string]string{
		"comment_id": uuidToString(systemComment.ID),
	})

	item, err := h.Queries.CreateInboxItem(ctx, db.CreateInboxItemParams{
		WorkspaceID:   parent.WorkspaceID,
		RecipientType: "member",
		RecipientID:   recipientUserID,
		Type:          "mentioned",
		Severity:      "info",
		IssueID:       parent.ID,
		Title:         parent.Title,
		Body:          pgtype.Text{String: systemComment.Content, Valid: true},
		ActorType:     pgtype.Text{String: "system", Valid: true},
		ActorID:       pgtype.UUID{},
		Details:       details,
	})
	if err != nil {
		slog.Warn("child done: create parent member inbox failed",
			"error", err,
			"parent_id", uuidToString(parent.ID),
			"member_id", uuidToString(parent.AssigneeID))
		return
	}

	h.Bus.Publish(events.Event{
		Type:        protocol.EventInboxNew,
		WorkspaceID: uuidToString(parent.WorkspaceID),
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"item": childDoneInboxItemPayload(item, parent.Status),
		},
	})
}

// childDoneInboxItemPayload mirrors the shape of inboxItemToResponse in
// cmd/server/notification_listeners.go so frontend clients can read the
// platform-generated row through the same path they read mention rows. We
// avoid importing the cmd/server package (handler → cmd would be a cycle)
// and instead inline the minimal set of fields the WS consumers care about.
func childDoneInboxItemPayload(item db.InboxItem, issueStatus string) map[string]any {
	resp := map[string]any{
		"id":             uuidToString(item.ID),
		"workspace_id":   uuidToString(item.WorkspaceID),
		"recipient_type": item.RecipientType,
		"recipient_id":   uuidToString(item.RecipientID),
		"type":           item.Type,
		"severity":       item.Severity,
		"issue_id":       uuidToPtr(item.IssueID),
		"title":          item.Title,
		"body":           textToPtr(item.Body),
		"read":           item.Read,
		"archived":       item.Archived,
		"created_at":     timestampToString(item.CreatedAt),
		"actor_type":     textToPtr(item.ActorType),
		"actor_id":       uuidToPtr(item.ActorID),
		"details":        json.RawMessage(item.Details),
		"issue_status":   issueStatus,
	}
	return resp
}

func childAssigneeIsAgent(child db.Issue, agentID pgtype.UUID) bool {
	if !child.AssigneeType.Valid || child.AssigneeType.String != "agent" || !child.AssigneeID.Valid {
		return false
	}
	return uuidToString(child.AssigneeID) == uuidToString(agentID)
}

func childAssigneeIsSquad(child db.Issue, squadID pgtype.UUID) bool {
	if !child.AssigneeType.Valid || child.AssigneeType.String != "squad" || !child.AssigneeID.Valid {
		return false
	}
	return uuidToString(child.AssigneeID) == uuidToString(squadID)
}

func childAssigneeIsMember(child db.Issue, memberID pgtype.UUID) bool {
	if !child.AssigneeType.Valid || child.AssigneeType.String != "member" || !child.AssigneeID.Valid {
		return false
	}
	return uuidToString(child.AssigneeID) == uuidToString(memberID)
}
