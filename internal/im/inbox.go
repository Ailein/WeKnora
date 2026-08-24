package im

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The operator inbox is the console workbench listing every IM conversation of
// a tenant: human-held conversations pinned first, unread counters, the latest
// message preview, and a realtime event stream so new messages appear without
// polling. It reads only the denormalized columns on im_channel_sessions that
// noteInboxActivity maintains — never the messages table.

// Inbox last-message roles. "operator" marks console manual replies so the
// preview can distinguish them from bot answers.
const (
	InboxRoleUser      = "user"
	InboxRoleAssistant = "assistant"
	InboxRoleOperator  = "operator"
)

const (
	// inboxPreviewMaxRunes bounds the denormalized preview; the column is
	// VARCHAR(500), so stay well under it even after UTF-8 expansion.
	inboxPreviewMaxRunes = 120
	// inboxPeerNameMaxRunes bounds captured display names (column is 255).
	inboxPeerNameMaxRunes = 120
	// inboxDefaultPageSize/inboxMaxPageSize bound the list endpoint.
	inboxDefaultPageSize = 100
	inboxMaxPageSize     = 200
	// inboxEventBuffer is each subscriber's channel depth. Events are dropped
	// (not blocked on) when a slow consumer falls behind; the list self-heals
	// on the next event for the same conversation.
	inboxEventBuffer = 32
	// RedisChannelInbox relays inbox events across replicas: inbound messages
	// arrive on the adapter leader while the operator's SSE stream may be
	// served by another instance. Self-published events are delivered back
	// through the subscription on purpose.
	RedisChannelInbox = "im:inbox:event"
)

// Quick-reply bounds: a handful of canned phrases, not a knowledge base.
const (
	maxQuickReplies    = 50
	maxQuickReplyRunes = 500
)

var (
	ErrQuickRepliesTooMany = fmt.Errorf("quick replies are limited to %d items", maxQuickReplies)
	ErrQuickReplyTooLong   = fmt.Errorf("a quick reply is limited to %d characters", maxQuickReplyRunes)
)

// InboxItem is the list/event shape of one IM conversation.
type InboxItem struct {
	SessionID   string `json:"session_id"`
	Platform    string `json:"platform"`
	IMChannelID string `json:"im_channel_id"`
	ChannelName string `json:"channel_name"`
	AgentID     string `json:"agent_id"`
	UserID      string `json:"user_id"`
	ChatID      string `json:"chat_id"`
	PeerName    string `json:"peer_name"`
	// Title is the bound session's auto-generated topic title.
	Title string `json:"title"`
	// HandlingMode is the effective mode: an expired human takeover is
	// reported as bot even before the next inbound message flips the row.
	HandlingMode       string     `json:"handling_mode"`
	HandlingExpiresAt  *time.Time `json:"handling_expires_at,omitempty"`
	UnreadCount        int        `json:"unread_count"`
	LastMessagePreview string     `json:"last_message_preview"`
	LastMessageRole    string     `json:"last_message_role"`
	LastMessageAt      *time.Time `json:"last_message_at,omitempty"`
	// ManualReplySupported tells the composer whether operators can answer
	// this conversation from the console (currently WhatsApp only).
	ManualReplySupported bool `json:"manual_reply_supported"`
}

// InboxList is the list endpoint response payload.
type InboxList struct {
	Items       []*InboxItem `json:"items"`
	Total       int64        `json:"total"`
	UnreadTotal int64        `json:"unread_total"`
}

// InboxListOptions filters the inbox list. Filter is "" (all), "human"
// (unexpired takeovers) or "unread".
type InboxListOptions struct {
	Filter      string
	IMChannelID string
	Page        int
	PageSize    int
}

// InboxEvent is one SSE payload. Type "ready" opens a stream (unread total
// only); type "session" carries the updated conversation.
type InboxEvent struct {
	Type        string     `json:"type"`
	Item        *InboxItem `json:"item,omitempty"`
	UnreadTotal int64      `json:"unread_total"`
}

// inboxWireEvent is the Redis relay envelope for cross-instance delivery.
type inboxWireEvent struct {
	TenantID uint64     `json:"tenant_id"`
	Event    InboxEvent `json:"event"`
}

// ── Subscriber hub ──

type inboxHub struct {
	mu   sync.Mutex
	subs map[uint64]map[chan InboxEvent]struct{}
}

func newInboxHub() *inboxHub {
	return &inboxHub{subs: make(map[uint64]map[chan InboxEvent]struct{})}
}

func (h *inboxHub) subscribe(tenantID uint64) (chan InboxEvent, func()) {
	ch := make(chan InboxEvent, inboxEventBuffer)
	h.mu.Lock()
	if h.subs[tenantID] == nil {
		h.subs[tenantID] = make(map[chan InboxEvent]struct{})
	}
	h.subs[tenantID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			if set := h.subs[tenantID]; set != nil {
				delete(set, ch)
				if len(set) == 0 {
					delete(h.subs, tenantID)
				}
			}
			h.mu.Unlock()
		})
	}
	return ch, cancel
}

// publish delivers without blocking: a full subscriber just misses the event.
func (h *inboxHub) publish(tenantID uint64, evt InboxEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[tenantID] {
		select {
		case ch <- evt:
		default:
		}
	}
}

// inbox returns the lazily created hub so hand-built test services need no
// extra wiring.
func (s *Service) inbox() *inboxHub {
	s.inboxHubOnce.Do(func() { s.inboxHub = newInboxHub() })
	return s.inboxHub
}

// SubscribeInbox registers a realtime listener for one tenant's inbox events.
// The returned cancel func must be called when the stream closes.
func (s *Service) SubscribeInbox(tenantID uint64) (<-chan InboxEvent, func()) {
	return s.inbox().subscribe(tenantID)
}

// publishInboxEvent fans an event out to this tenant's subscribers. With Redis
// the event travels through pub/sub so subscribers on other replicas see it
// too (the publishing instance receives its own copy back); without Redis it
// goes straight to the local hub.
func (s *Service) publishInboxEvent(tenantID uint64, evt InboxEvent) {
	if s.stopped.Load() || tenantID == 0 {
		return
	}
	if s.redis != nil {
		payload, err := json.Marshal(inboxWireEvent{TenantID: tenantID, Event: evt})
		if err == nil {
			if err := s.redis.Publish(context.Background(), RedisChannelInbox, payload).Err(); err == nil {
				return
			}
			// Fall through: local delivery beats losing the event entirely.
		}
	}
	s.inbox().publish(tenantID, evt)
}

// startInboxEventSubscriber begins relaying Redis inbox events into the local
// hub. No-op without Redis (publishInboxEvent already delivers locally).
func (s *Service) startInboxEventSubscriber() {
	if s.redis == nil {
		return
	}
	s.inboxSubOnce.Do(func() { go s.inboxEventSubscriberLoop() })
}

func (s *Service) inboxEventSubscriberLoop() {
	pubsub := s.redis.Subscribe(context.Background(), RedisChannelInbox)
	defer pubsub.Close()

	messages := pubsub.Channel()
	for {
		select {
		case <-s.stopCh:
			return
		case msg, ok := <-messages:
			if !ok {
				return
			}
			var wire inboxWireEvent
			if err := json.Unmarshal([]byte(msg.Payload), &wire); err != nil {
				logger.Warnf(context.Background(), "[IM] Ignore invalid inbox event: %v", err)
				continue
			}
			if wire.TenantID == 0 {
				continue
			}
			s.inbox().publish(wire.TenantID, wire.Event)
		}
	}
}

// ── Activity tracking ──

// inboxNote describes one recorded message for the denormalized columns.
type inboxNote struct {
	// Role is InboxRoleUser / InboxRoleAssistant / InboxRoleOperator.
	Role    string
	Preview string
	// ResetUnread clears the counter (operator activity implies the operator
	// has seen the conversation). User-role notes increment it instead.
	ResetUnread bool
}

// inboxPreview collapses whitespace and truncates content for the list column.
func inboxPreview(content string) string {
	content = strings.Join(strings.Fields(content), " ")
	runes := []rune(content)
	if len(runes) > inboxPreviewMaxRunes {
		return string(runes[:inboxPreviewMaxRunes])
	}
	return content
}

// noteInboxActivity refreshes the inbox columns of the live conversation bound
// to sessionID after a message was recorded, then pushes the updated item to
// realtime subscribers. Strictly best-effort: the message itself is already
// persisted, so inbox bookkeeping must never fail the caller — errors are
// logged and swallowed. Callers on cancellable QA paths still get their note
// written because the DB work runs on a detached context.
func (s *Service) noteInboxActivity(ctx context.Context, sessionID string, note inboxNote) {
	if sessionID == "" || note.Role == "" {
		return
	}
	dbCtx := context.WithoutCancel(ctx)
	now := time.Now()
	updates := map[string]interface{}{
		"last_message_preview": inboxPreview(note.Preview),
		"last_message_role":    note.Role,
		"last_message_at":      now,
		"updated_at":           now,
	}
	switch {
	case note.ResetUnread:
		updates["operator_unread_count"] = 0
	case note.Role == InboxRoleUser:
		updates["operator_unread_count"] = gorm.Expr("operator_unread_count + 1")
	}

	res := s.db.WithContext(dbCtx).Model(&ChannelSession{}).
		Where("session_id = ? AND deleted_at IS NULL", sessionID).
		Updates(updates)
	if res.Error != nil {
		logger.Warnf(ctx, "[IM] Failed to note inbox activity for session %s: %v", sessionID, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		// Not an IM-bound live conversation (e.g. a reply into a /clear-ed
		// historical session) — nothing to surface.
		return
	}

	s.publishInboxItemUpdate(dbCtx, sessionID)
}

// publishInboxItemUpdate loads the live conversation bound to sessionID and
// pushes it to the tenant's realtime subscribers. Best-effort.
func (s *Service) publishInboxItemUpdate(ctx context.Context, sessionID string) {
	item, err := s.loadInboxItem(ctx, sessionID)
	if err != nil || item == nil {
		if err != nil {
			logger.Warnf(ctx, "[IM] Failed to load inbox item for session %s: %v", sessionID, err)
		}
		return
	}
	total, err := s.inboxUnreadTotalByTenant(ctx, item.tenantID)
	if err != nil {
		logger.Warnf(ctx, "[IM] Failed to sum inbox unread for tenant %d: %v", item.tenantID, err)
	}
	s.publishInboxEvent(item.tenantID, InboxEvent{Type: "session", Item: &item.InboxItem, UnreadTotal: total})
}

// rememberPeerName captures the peer's display name from an inbound message
// the first time it appears (or when it changes). Best-effort.
func (s *Service) rememberPeerName(ctx context.Context, cs *ChannelSession, name string) {
	if cs == nil {
		return
	}
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > inboxPeerNameMaxRunes {
		name = string(runes[:inboxPeerNameMaxRunes])
	}
	if name == "" || name == cs.PeerName {
		return
	}
	if err := s.db.WithContext(ctx).Model(&ChannelSession{}).
		Where("id = ?", cs.ID).Update("peer_name", name).Error; err != nil {
		logger.Warnf(ctx, "[IM] Failed to remember peer name for session %s: %v", cs.SessionID, err)
		return
	}
	cs.PeerName = name
}

// ── List / read ──

// inboxRow is the scan target joining the channel name and session title onto
// the denormalized conversation columns.
type inboxRow struct {
	SessionID          string
	Platform           string
	IMChannelID        string
	ChannelName        string
	AgentID            string
	UserID             string
	ChatID             string
	PeerName           string
	Title              string
	HandlingMode       string
	HandlingExpiresAt  *time.Time
	OperatorUnread     int
	LastMessagePreview string
	LastMessageRole    string
	LastMessageAt      *time.Time
	TenantID           uint64
}

const inboxRowSelect = `cs.session_id AS session_id, cs.platform AS platform,
	cs.im_channel_id AS im_channel_id, COALESCE(ch.name, '') AS channel_name,
	cs.agent_id AS agent_id, cs.user_id AS user_id, cs.chat_id AS chat_id,
	cs.peer_name AS peer_name, COALESCE(se.title, '') AS title,
	cs.handling_mode AS handling_mode, cs.handling_expires_at AS handling_expires_at,
	cs.operator_unread_count AS operator_unread, cs.last_message_preview AS last_message_preview,
	cs.last_message_role AS last_message_role, cs.last_message_at AS last_message_at,
	cs.tenant_id AS tenant_id`

// scopedInboxItem pairs the API shape with the tenant it belongs to (events
// need the tenant; the HTTP list already knows it).
type scopedInboxItem struct {
	InboxItem
	tenantID uint64
}

func (row *inboxRow) toItem(now time.Time) *scopedInboxItem {
	item := &scopedInboxItem{
		InboxItem: InboxItem{
			SessionID:            row.SessionID,
			Platform:             row.Platform,
			IMChannelID:          row.IMChannelID,
			ChannelName:          row.ChannelName,
			AgentID:              row.AgentID,
			UserID:               row.UserID,
			ChatID:               row.ChatID,
			PeerName:             row.PeerName,
			Title:                row.Title,
			HandlingMode:         row.HandlingMode,
			HandlingExpiresAt:    row.HandlingExpiresAt,
			UnreadCount:          row.OperatorUnread,
			LastMessagePreview:   row.LastMessagePreview,
			LastMessageRole:      row.LastMessageRole,
			LastMessageAt:        row.LastMessageAt,
			ManualReplySupported: ManualReplySupported(row.Platform),
		},
		tenantID: row.TenantID,
	}
	if item.HandlingMode == "" {
		item.HandlingMode = HandlingModeBot
	}
	// Present an expired takeover as bot handling: HandleMessage flips the row
	// lazily on the next inbound message, but the operator should not see a
	// conversation as "human-held" once the window has passed.
	if item.HandlingMode == HandlingModeHuman &&
		item.HandlingExpiresAt != nil && now.After(*item.HandlingExpiresAt) {
		item.HandlingMode = HandlingModeBot
		item.HandlingExpiresAt = nil
	}
	return item
}

func (s *Service) inboxBaseQuery(ctx context.Context, tenantID uint64) *gorm.DB {
	return s.db.WithContext(ctx).Table("im_channel_sessions AS cs").
		Joins("LEFT JOIN im_channels ch ON ch.id = cs.im_channel_id").
		Joins("LEFT JOIN sessions se ON se.id = cs.session_id AND se.deleted_at IS NULL").
		Where("cs.tenant_id = ? AND cs.deleted_at IS NULL", tenantID)
}

// humanHandlingCond matches conversations under an active (unexpired) human
// takeover; the parameter is "now".
const humanHandlingCond = "(cs.handling_mode = 'human' AND (cs.handling_expires_at IS NULL OR cs.handling_expires_at > ?))"

// ListInbox returns one page of the tenant's IM conversations, active human
// takeovers pinned first, then most recent activity.
func (s *Service) ListInbox(ctx context.Context, tenantID uint64, opts InboxListOptions) (*InboxList, error) {
	if opts.Page < 1 {
		opts.Page = 1
	}
	if opts.PageSize < 1 {
		opts.PageSize = inboxDefaultPageSize
	}
	if opts.PageSize > inboxMaxPageSize {
		opts.PageSize = inboxMaxPageSize
	}
	now := time.Now()

	// Rebuild the filtered query per use: gorm finishers execute the shared
	// statement, so reusing one chain for Count and Scan corrupts conditions.
	buildQuery := func() *gorm.DB {
		q := s.inboxBaseQuery(ctx, tenantID)
		if opts.IMChannelID != "" {
			q = q.Where("cs.im_channel_id = ?", opts.IMChannelID)
		}
		switch opts.Filter {
		case "human":
			q = q.Where(humanHandlingCond, now)
		case "unread":
			q = q.Where("cs.operator_unread_count > 0")
		}
		return q
	}

	var total int64
	if err := buildQuery().Count(&total).Error; err != nil {
		return nil, fmt.Errorf("count inbox: %w", err)
	}

	// Pinning is a selected rank column because gorm's Order() cannot carry a
	// parameterized expression.
	var rows []inboxRow
	err := buildQuery().
		Select(inboxRowSelect+", CASE WHEN "+humanHandlingCond+" THEN 0 ELSE 1 END AS pinned_rank", now).
		Order("pinned_rank").
		Order("cs.last_message_at DESC NULLS LAST").
		Order("cs.created_at DESC").
		Offset((opts.Page - 1) * opts.PageSize).
		Limit(opts.PageSize).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list inbox: %w", err)
	}

	items := make([]*InboxItem, 0, len(rows))
	for i := range rows {
		items = append(items, &rows[i].toItem(now).InboxItem)
	}

	unread, err := s.inboxUnreadTotalByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return &InboxList{Items: items, Total: total, UnreadTotal: unread}, nil
}

// loadInboxItem fetches the single live conversation bound to sessionID.
// Returns (nil, nil) when the session has no live IM binding.
func (s *Service) loadInboxItem(ctx context.Context, sessionID string) (*scopedInboxItem, error) {
	var rows []inboxRow
	err := s.db.WithContext(ctx).Table("im_channel_sessions AS cs").
		Joins("LEFT JOIN im_channels ch ON ch.id = cs.im_channel_id").
		Joins("LEFT JOIN sessions se ON se.id = cs.session_id AND se.deleted_at IS NULL").
		Where("cs.session_id = ? AND cs.deleted_at IS NULL", sessionID).
		Select(inboxRowSelect).Limit(1).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].toItem(time.Now()), nil
}

func (s *Service) inboxUnreadTotalByTenant(ctx context.Context, tenantID uint64) (int64, error) {
	var total int64
	err := s.db.WithContext(ctx).Model(&ChannelSession{}).
		Where("tenant_id = ? AND deleted_at IS NULL", tenantID).
		Select("COALESCE(SUM(operator_unread_count), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("sum inbox unread: %w", err)
	}
	return total, nil
}

// InboxUnreadTotal exposes the tenant-wide unread counter (stream handshake).
func (s *Service) InboxUnreadTotal(ctx context.Context, tenantID uint64) (int64, error) {
	return s.inboxUnreadTotalByTenant(ctx, tenantID)
}

// MarkInboxRead zeroes the unread counter of one conversation (the operator
// opened it) and notifies subscribers so other tabs drop their badges too.
// Idempotent: marking an already-read or non-IM session is a no-op.
func (s *Service) MarkInboxRead(ctx context.Context, tenantID uint64, sessionID string) (int64, error) {
	res := s.db.WithContext(ctx).Model(&ChannelSession{}).
		Where("session_id = ? AND tenant_id = ? AND deleted_at IS NULL AND operator_unread_count <> 0", sessionID, tenantID).
		Update("operator_unread_count", 0)
	if res.Error != nil {
		return 0, fmt.Errorf("mark inbox read: %w", res.Error)
	}
	total, err := s.inboxUnreadTotalByTenant(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	if res.RowsAffected > 0 {
		if item, err := s.loadInboxItem(ctx, sessionID); err == nil && item != nil {
			s.publishInboxEvent(tenantID, InboxEvent{Type: "session", Item: &item.InboxItem, UnreadTotal: total})
		}
	}
	return total, nil
}

// ── Quick replies ──

// IMQuickReplies stores a tenant's canned inbox phrases as one JSON row.
type IMQuickReplies struct {
	TenantID  uint64     `gorm:"primaryKey"`
	Items     types.JSON `gorm:"type:jsonb;not null;default:'[]'"`
	UpdatedAt time.Time
}

func (IMQuickReplies) TableName() string { return "im_quick_replies" }

// GetQuickReplies returns the tenant's canned phrases (empty when unset).
func (s *Service) GetQuickReplies(ctx context.Context, tenantID uint64) ([]string, error) {
	var row IMQuickReplies
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load quick replies: %w", err)
	}
	var items []string
	if err := json.Unmarshal([]byte(row.Items), &items); err != nil {
		// A corrupt row must not brick the inbox; treat as unset.
		logger.Warnf(ctx, "[IM] Invalid quick replies for tenant %d: %v", tenantID, err)
		return []string{}, nil
	}
	if items == nil {
		items = []string{}
	}
	return items, nil
}

// SetQuickReplies replaces the tenant's canned phrases. Items are trimmed and
// empties dropped; the normalized list is returned.
func (s *Service) SetQuickReplies(ctx context.Context, tenantID uint64, items []string) ([]string, error) {
	normalized := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if len([]rune(item)) > maxQuickReplyRunes {
			return nil, ErrQuickReplyTooLong
		}
		normalized = append(normalized, item)
	}
	if len(normalized) > maxQuickReplies {
		return nil, ErrQuickRepliesTooMany
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal quick replies: %w", err)
	}
	row := IMQuickReplies{TenantID: tenantID, Items: types.JSON(payload), UpdatedAt: time.Now()}
	err = s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"items", "updated_at"}),
	}).Create(&row).Error
	if err != nil {
		return nil, fmt.Errorf("save quick replies: %w", err)
	}
	return normalized, nil
}
