package lark

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// larkMsgTypeMergeForward is the msg_type of a "merged & forwarded"
// message — a bundle of other messages a user forwarded as one unit.
// Its own body.content is a fixed sentinel string; the actual forwarded
// messages come back as the extra items[] of a GetMessage call.
const larkMsgTypeMergeForward = "merge_forward"

// defaultMaxForwardChildren caps how many child messages we inline from
// a single forward. Lark itself bounds a merge_forward at 100 messages;
// we mirror that as a safety valve so a pathological bundle can't blow
// up the agent's context. Anything beyond the cap is dropped with a
// visible "... (N more truncated)" marker.
const defaultMaxForwardChildren = 100

// DefaultRecentContextSize is the window the production wiring uses for
// the group-context prefetch: how many of the most-recent group messages
// to inline as a <recent_context> block when a user @-mentions the Bot.
// 10 keeps the agent's prompt meaningfully contextual without bloating it
// or risking the inbound ACK budget (a single list call, page_size 10).
const DefaultRecentContextSize = 10

// Enricher expands an inbound message's body with context the user
// EXPLICITLY attached — a quoted reply or a merged-and-forwarded bundle
// — by calling back into Lark's IM API. It runs after the (fast,
// HTTP-free) decoder and before the dispatcher, turning a bare
// "@bot 总结一下" into a body that already carries the referenced
// conversation inline.
//
// It is best-effort by contract: every fetch failure degrades to a
// visible placeholder block and Enrich NEVER returns an error or blocks
// ingestion. A message with nothing to expand (no parent_id, not a
// merge_forward) is returned untouched without any network call.
type Enricher interface {
	Enrich(ctx context.Context, msg InboundMessage, creds InstallationCredentials) InboundMessage
}

// InboundEnricherConfig tunes the enricher. All fields default.
type InboundEnricherConfig struct {
	// MaxForwardChildren caps inlined forward children. <=0 uses
	// defaultMaxForwardChildren.
	MaxForwardChildren int
	// RecentContextSize caps how many surrounding group messages the
	// enricher prefetches and inlines as a <recent_context> block when a
	// user @-mentions the Bot in a group. <=0 DISABLES the prefetch
	// entirely (only explicitly-attached quote/forward context is used);
	// the production wiring sets DefaultRecentContextSize. Values above
	// Lark's 50-per-page cap are clamped by the client.
	RecentContextSize int
	// Logger receives best-effort warnings about fetch failures. Nil
	// uses slog.Default().
	Logger *slog.Logger
}

type inboundEnricher struct {
	client             APIClient
	maxForwardChildren int
	recentContextSize  int
	logger             *slog.Logger
}

// NewInboundEnricher builds an Enricher backed by the given Lark API
// client. The client supplies GetMessage; everything else (flattening,
// block assembly, speaker labelling) is local.
func NewInboundEnricher(client APIClient, cfg InboundEnricherConfig) Enricher {
	if cfg.MaxForwardChildren <= 0 {
		cfg.MaxForwardChildren = defaultMaxForwardChildren
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &inboundEnricher{
		client:             client,
		maxForwardChildren: cfg.MaxForwardChildren,
		recentContextSize:  cfg.RecentContextSize,
		logger:             cfg.Logger,
	}
}

// Enrich rewrites msg.Body to inline surrounding group context and/or
// any quoted-reply parent and/or forwarded bundle. Composition order
// goes broadest-to-narrowest: the surrounding group history first, then
// the explicitly-quoted parent (a specific reference), then the message's
// own content (or, for a forward, the rendered transcript).
//
//	<recent_context …>…</recent_context>
//
//	<quoted_message …>…</quoted_message>
//
//	<the user's own message, or the forwarded transcript>
//
// The <recent_context> block is only produced for a group message
// addressed to the Bot, and only when RecentContextSize > 0 — it answers
// MUL-3084 (the Bot saw only the single @-ed line, never the surrounding
// conversation). It is the one fetch here NOT triggered by something the
// user explicitly attached.
func (e *inboundEnricher) Enrich(ctx context.Context, msg InboundMessage, creds InstallationCredentials) InboundMessage {
	isForward := msg.MessageType == larkMsgTypeMergeForward
	wantRecent := e.recentContextSize > 0 && msg.ChatType == ChatTypeGroup && msg.AddressedToBot
	if msg.ParentID == "" && !isForward && !wantRecent {
		// Nothing to expand and no group prefetch wanted — no network call.
		return msg
	}
	// If the transport isn't wired (stub client on a deployment without
	// a Lark app), skip rather than stamp every reply with a fetch
	// error. Body stays whatever the decoder produced.
	if e.client == nil || !e.client.IsConfigured() {
		return msg
	}

	var b strings.Builder
	if wantRecent {
		if blk := e.renderRecentContext(ctx, creds, msg); blk != "" {
			b.WriteString(blk)
		}
	}
	if msg.ParentID != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(e.renderQuoted(ctx, creds, msg.ParentID))
	}

	var core string
	if isForward {
		core = e.renderForwarded(ctx, creds, msg.MessageID)
	} else {
		core = msg.Body
	}
	if b.Len() > 0 && core != "" {
		b.WriteString("\n\n")
	}
	b.WriteString(core)

	msg.Body = b.String()
	return msg
}

// renderRecentContext fetches the most recent messages in the group and
// renders a <recent_context> block of the surrounding conversation,
// excluding the triggering message itself and the directly-quoted parent
// (which gets its own <quoted_message> block, so it isn't duplicated).
// Messages render oldest-first as "[<speaker>]: <text>" using the same
// positional speaker labels the forwarded-transcript renderer uses. Any
// fetch failure degrades to a visible placeholder; like the rest of the
// enricher it never blocks ingestion. An empty/whitespace window yields
// "" so Enrich emits no block at all.
func (e *inboundEnricher) renderRecentContext(ctx context.Context, creds InstallationCredentials, msg InboundMessage) string {
	items, err := e.client.ListChatMessages(ctx, creds, ListMessagesParams{
		ChatID:   msg.ChatID,
		PageSize: e.recentContextSize,
		// Anchor the window to the trigger message's time (Lark sends it
		// as epoch millis; end_time wants seconds) so we pull the
		// conversation up to the @-mention, not whatever is newest by the
		// time this prefetch runs. A missing/unparseable time yields 0,
		// which the client treats as "no end_time" (newest N).
		EndTime: parseLarkMillis(msg.CreateTime) / 1000,
	})
	if err != nil {
		e.logger.Warn("lark enricher: recent context fetch failed",
			"chat_id", string(msg.ChatID), "err", err)
		return recentContextErrorBlock()
	}

	// Drop the trigger and the quoted parent so neither is duplicated
	// alongside the user's core message / its own <quoted_message> block.
	exclude := map[string]bool{msg.MessageID: true}
	if msg.ParentID != "" {
		exclude[msg.ParentID] = true
	}
	kept := make([]LarkMessage, 0, len(items))
	for _, it := range items {
		if exclude[it.MessageID] {
			continue
		}
		kept = append(kept, it)
	}
	if len(kept) == 0 {
		return ""
	}

	// The list endpoint returns newest-first; render oldest-first so the
	// transcript reads top-to-bottom like the chat does.
	sort.SliceStable(kept, func(i, j int) bool {
		return parseLarkMillis(kept[i].CreateTime) < parseLarkMillis(kept[j].CreateTime)
	})

	labeler := newSpeakerLabeler()
	lines := make([]string, 0, len(kept))
	for _, m := range kept {
		label := labeler.label(m)
		var text string
		switch {
		case m.MessageType == larkMsgTypeMergeForward:
			text = "[merge_forward, expand manually]"
		default:
			text = e.flattenMessage(m)
			if text == "" {
				text = "[empty message]"
			}
		}
		lines = append(lines, fmt.Sprintf("[%s]: %s", label, text))
	}
	return fmt.Sprintf("<recent_context count=\"%d\">\n%s\n</recent_context>",
		len(kept), strings.Join(lines, "\n"))
}

func recentContextErrorBlock() string {
	return "<recent_context type=\"error\">[unable to fetch recent context]</recent_context>"
}

// renderQuoted fetches the directly-quoted parent and renders a
// <quoted_message> block. A parent that is itself a merge_forward nests
// a <forwarded_messages> transcript inside the quoted block (the
// GetMessage response already carries both the forward sentinel and its
// children, so no extra round-trip is needed). Any failure degrades to
// the documented error block.
func (e *inboundEnricher) renderQuoted(ctx context.Context, creds InstallationCredentials, parentID string) string {
	items, err := e.client.GetMessage(ctx, creds, parentID)
	if err != nil || len(items) == 0 {
		e.logger.Warn("lark enricher: quoted parent fetch failed",
			"parent_id", parentID, "items", len(items), "err", err)
		return quotedErrorBlock(parentID)
	}
	parent := items[0]
	if parent.Deleted {
		return quotedErrorBlock(parentID)
	}

	labeler := newSpeakerLabeler()
	sender := labeler.label(parent)

	if parent.MessageType == larkMsgTypeMergeForward {
		inner := e.renderForwardedItems(items, parentID)
		return wrapQuoted(parentID, sender, larkMsgTypeMergeForward, inner)
	}
	text := e.flattenMessage(parent)
	if text == "" {
		text = "[empty message]"
	}
	return wrapQuoted(parentID, sender, parent.MessageType, text)
}

// renderForwarded fetches a merge_forward by id and renders its bundled
// children. The GetMessage response is [sentinel, child…]; we filter the
// forward's own record out by id (robust to whether Lark returns the
// sentinel first or not) and render the rest.
func (e *inboundEnricher) renderForwarded(ctx context.Context, creds InstallationCredentials, forwardID string) string {
	items, err := e.client.GetMessage(ctx, creds, forwardID)
	if err != nil {
		e.logger.Warn("lark enricher: forward fetch failed", "message_id", forwardID, "err", err)
		return forwardedErrorBlock()
	}
	return e.renderForwardedItems(items, forwardID)
}

// renderForwardedItems renders the children of a forward whose own
// record id is forwardID. Children are time-ordered, capped, and each
// rendered as "[<speaker>]: <text>"; a child that is itself a forward is
// not recursed into (it gets a manual-expand placeholder) so the HTTP
// fan-out on the ACK-latency-sensitive inbound path stays bounded.
func (e *inboundEnricher) renderForwardedItems(items []LarkMessage, forwardID string) string {
	// The verified contract is that GetMessage(forward_id) returns one
	// level of bundling: [sentinel, direct-children…]. We therefore
	// treat every non-sentinel item as a direct child. We filter by id
	// (not by upper_message_id == forwardID) on purpose: a strict
	// upper_message_id match would silently DROP a real child if Lark
	// ever returned one with that field unpopulated. A child that is
	// itself a forward is rendered as a manual-expand placeholder below
	// rather than recursed into, so grandchildren are never inlined.
	children := make([]LarkMessage, 0, len(items))
	for _, it := range items {
		if it.MessageID == forwardID {
			continue // the forward sentinel itself
		}
		children = append(children, it)
	}
	total := len(children)
	if total == 0 {
		return "<forwarded_messages count=\"0\">\n[no forwarded content available]\n</forwarded_messages>"
	}

	sort.SliceStable(children, func(i, j int) bool {
		return parseLarkMillis(children[i].CreateTime) < parseLarkMillis(children[j].CreateTime)
	})

	truncated := 0
	if total > e.maxForwardChildren {
		truncated = total - e.maxForwardChildren
		children = children[:e.maxForwardChildren]
	}

	labeler := newSpeakerLabeler()
	lines := make([]string, 0, len(children))
	for _, c := range children {
		label := labeler.label(c)
		var text string
		switch {
		case c.MessageType == larkMsgTypeMergeForward:
			text = "[nested merge_forward, expand manually]"
		default:
			text = e.flattenMessage(c)
			if text == "" {
				text = "[empty message]"
			}
		}
		lines = append(lines, fmt.Sprintf("[%s]: %s", label, text))
	}
	body := strings.Join(lines, "\n")
	if truncated > 0 {
		body += fmt.Sprintf("\n... (%d more truncated)", truncated)
	}
	return fmt.Sprintf("<forwarded_messages count=\"%d\">\n%s\n</forwarded_messages>", total, body)
}

// flattenMessage turns one fetched message into plain text: structural
// flatten by msg_type, then @_user_N placeholder resolution against the
// message's own mentions. The bot mention is NOT stripped here (unlike
// the inbound decoder) — a quoted / forwarded message is historical
// context, not a fresh trigger, so passing empty bot identifiers leaves
// every @-mention rendered as a readable @name.
func (e *inboundEnricher) flattenMessage(m LarkMessage) string {
	if m.Deleted {
		return "[deleted message]"
	}
	raw := flattenContent(m.MessageType, m.Content)
	if raw == "" {
		return ""
	}
	return resolveMentions(raw, restMentionsToEvent(m.Mentions), "", "")
}

// restMentionsToEvent adapts the IM REST mention shape (flat string id)
// to the WS-event larkMention shape resolveMentions consumes, so a
// single mention-resolution implementation serves both ingress paths.
func restMentionsToEvent(ms []LarkMessageMention) []larkMention {
	if len(ms) == 0 {
		return nil
	}
	out := make([]larkMention, 0, len(ms))
	for _, m := range ms {
		lm := larkMention{Key: m.Key, Name: m.Name}
		lm.ID.OpenID = m.ID
		out = append(out, lm)
	}
	return out
}

func wrapQuoted(messageID, sender, msgType, inner string) string {
	return fmt.Sprintf("<quoted_message message_id=%q sender=%q type=%q>\n%s\n</quoted_message>",
		messageID, sender, msgType, inner)
}

func quotedErrorBlock(messageID string) string {
	return fmt.Sprintf("<quoted_message message_id=%q type=\"error\">[unable to fetch]</quoted_message>", messageID)
}

func forwardedErrorBlock() string {
	return "<forwarded_messages type=\"error\">[unable to fetch]</forwarded_messages>"
}

func parseLarkMillis(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// speakerLabeler assigns stable, human-readable labels to the senders
// within one rendered block. Lark message items carry only a sender id
// (no display name — resolving real names needs a separate Contact API
// lookup, tracked as a follow-up), so we map each distinct user id to
// "User 1", "User 2", … in first-appearance order, and app senders to
// "Bot". This keeps the conversation's turn-taking structure legible
// without a per-sender network round-trip.
type speakerLabeler struct {
	seen map[string]string
	n    int
}

func newSpeakerLabeler() *speakerLabeler {
	return &speakerLabeler{seen: make(map[string]string)}
}

func (l *speakerLabeler) label(m LarkMessage) string {
	if m.SenderType == "app" {
		return "Bot"
	}
	key := m.SenderID
	if key == "" {
		key = "unknown"
	}
	if lbl, ok := l.seen[key]; ok {
		return lbl
	}
	l.n++
	lbl := fmt.Sprintf("User %d", l.n)
	l.seen[key] = lbl
	return lbl
}
