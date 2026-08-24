package im

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Handling modes for a ChannelSession. In bot mode the QA pipeline answers
// inbound messages as usual; in human mode an operator has taken the
// conversation over: inbound messages are recorded into the session history
// (Channel = types.ChannelIMTakeover) but the bot stays silent.
const (
	HandlingModeBot   = "bot"
	HandlingModeHuman = "human"
)

const (
	// defaultTakeoverTimeoutMinutes is applied when the operator does not pick
	// a window. Bounds keep a typo like 999999 from silencing the bot for weeks.
	defaultTakeoverTimeoutMinutes = 60
	minTakeoverTimeoutMinutes     = 5
	maxTakeoverTimeoutMinutes     = 1440 // 24h
)

// Sentinel errors let the HTTP handler map takeover failures to precise codes.
var (
	ErrHandlingNotIMSession = errors.New("session is not bound to an IM conversation")
	ErrHandlingUnsupported  = errors.New("human takeover is not supported for this platform")
	// ErrHandlingConversationGone: the mapping for this session was cleared
	// (e.g. the user sent /clear) and the peer has not started a new
	// conversation yet, so there is no live conversation to take over.
	ErrHandlingConversationGone = errors.New("the IM conversation was reset and has no active successor")
	ErrHandlingInvalidMode      = errors.New("handling mode must be \"bot\" or \"human\"")
	ErrHandlingInvalidTimeout   = fmt.Errorf("timeout_minutes must be 0 (no expiry) or between %d and %d",
		minTakeoverTimeoutMinutes, maxTakeoverTimeoutMinutes)
)

// SessionHandling is the API shape of a conversation's takeover state.
// SessionID is the session the state actually lives on: when the operator
// acted from a historical (cleared) session it is the live successor's session,
// which is where the peer's new messages land.
type SessionHandling struct {
	SessionID      string     `json:"session_id"`
	Platform       string     `json:"platform"`
	Mode           string     `json:"mode"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	TimeoutMinutes int        `json:"timeout_minutes,omitempty"`
}

func handlingFromChannelSession(cs *ChannelSession) *SessionHandling {
	h := &SessionHandling{
		SessionID: cs.SessionID,
		Platform:  cs.Platform,
		Mode:      cs.HandlingMode,
	}
	if h.Mode == "" {
		h.Mode = HandlingModeBot
	}
	if h.Mode == HandlingModeHuman {
		h.ExpiresAt = cs.HandlingExpiresAt
		h.TimeoutMinutes = cs.HandlingTimeoutMinutes
	}
	return h
}

// resolveLiveChannelSession finds the ChannelSession that governs the LIVE
// conversation for the peer bound to sessionID. The session_id lookup is
// Unscoped like SendManualReply's: an operator acting inside a historical
// session (its mapping soft-deleted by /clear) still means that peer. But
// takeover state is only ever read from the live mapping by HandleMessage, so
// when the direct row is soft-deleted the state must be applied to the peer's
// current live row instead — silencing a dead row would silence nothing.
func (s *Service) resolveLiveChannelSession(ctx context.Context, tenantID uint64, sessionID string) (*ChannelSession, error) {
	var cs ChannelSession
	err := s.db.Unscoped().
		Where("session_id = ? AND tenant_id = ?", sessionID, tenantID).
		Order("created_at DESC").
		First(&cs).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrHandlingNotIMSession
		}
		return nil, fmt.Errorf("load channel session: %w", err)
	}
	if !cs.DeletedAt.Valid {
		return &cs, nil
	}

	// Same peer keys resolveSession uses (thread_id included so thread-mode
	// conversations map to their own live row, user-mode rows have '').
	var live ChannelSession
	err = s.db.
		Where("platform = ? AND user_id = ? AND chat_id = ? AND thread_id = ? AND tenant_id = ? AND agent_id = ? AND deleted_at IS NULL",
			cs.Platform, cs.UserID, cs.ChatID, cs.ThreadID, cs.TenantID, cs.AgentID).
		First(&live).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrHandlingConversationGone
		}
		return nil, fmt.Errorf("load live channel session: %w", err)
	}
	return &live, nil
}

// GetSessionHandling reports the takeover state of the live conversation bound
// to sessionID.
func (s *Service) GetSessionHandling(ctx context.Context, tenantID uint64, sessionID string) (*SessionHandling, error) {
	cs, err := s.resolveLiveChannelSession(ctx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	if !ManualReplySupported(cs.Platform) {
		return nil, ErrHandlingUnsupported
	}
	return handlingFromChannelSession(cs), nil
}

// SetSessionHandling switches the live conversation bound to sessionID between
// bot and human handling. In human mode timeoutMinutes bounds the takeover:
// the bot resumes automatically once the window elapses with no operator
// activity (each manual reply refreshes it); 0 means no expiry — the takeover
// lasts until an operator switches back. Passing a negative timeout selects
// the default window.
//
// Gated on ManualReplySupported: taking a conversation over only makes sense
// where the operator can actually answer from the console.
func (s *Service) SetSessionHandling(
	ctx context.Context, tenantID uint64, sessionID, mode string, timeoutMinutes int,
) (*SessionHandling, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != HandlingModeBot && mode != HandlingModeHuman {
		return nil, ErrHandlingInvalidMode
	}
	if timeoutMinutes < 0 {
		timeoutMinutes = defaultTakeoverTimeoutMinutes
	}
	if timeoutMinutes != 0 &&
		(timeoutMinutes < minTakeoverTimeoutMinutes || timeoutMinutes > maxTakeoverTimeoutMinutes) {
		return nil, ErrHandlingInvalidTimeout
	}

	cs, err := s.resolveLiveChannelSession(ctx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	if !ManualReplySupported(cs.Platform) {
		return nil, ErrHandlingUnsupported
	}

	updates := map[string]interface{}{
		"handling_mode":            mode,
		"handling_expires_at":      nil,
		"handling_timeout_minutes": 0,
		"updated_at":               time.Now(),
	}
	cs.HandlingMode = mode
	cs.HandlingExpiresAt = nil
	cs.HandlingTimeoutMinutes = 0
	if mode == HandlingModeHuman && timeoutMinutes > 0 {
		expires := time.Now().Add(time.Duration(timeoutMinutes) * time.Minute)
		updates["handling_expires_at"] = expires
		updates["handling_timeout_minutes"] = timeoutMinutes
		cs.HandlingExpiresAt = &expires
		cs.HandlingTimeoutMinutes = timeoutMinutes
	}
	if err := s.db.Model(&ChannelSession{}).Where("id = ?", cs.ID).Updates(updates).Error; err != nil {
		return nil, fmt.Errorf("update handling mode: %w", err)
	}

	logger.Infof(ctx, "[IM] Session handling set: platform=%s session=%s mode=%s timeout=%dmin",
		cs.Platform, cs.SessionID, mode, timeoutMinutes)
	return handlingFromChannelSession(cs), nil
}

// takeoverGate decides whether the bot must stay silent for an inbound
// message. In human mode (and not expired) it records the message into the
// session history so the operator console shows it, bumps the session so the
// conversation resurfaces in the list, and reports true — the caller skips the
// QA pipeline entirely, sending nothing to the IM user. An expired takeover is
// flipped back to bot mode in place and the message proceeds normally.
//
// Slash-commands are dispatched before this gate, so the IM user keeps /clear
// and friends during a takeover. Note /clear rebinds the peer to a fresh
// mapping, which starts in bot mode — i.e. it also ends the takeover.
func (s *Service) takeoverGate(ctx context.Context, cs *ChannelSession, session *types.Session, msg *IncomingMessage) bool {
	if cs == nil || cs.HandlingMode != HandlingModeHuman {
		return false
	}
	if cs.HandlingExpiresAt != nil && time.Now().After(*cs.HandlingExpiresAt) {
		if err := s.db.Model(&ChannelSession{}).Where("id = ?", cs.ID).Updates(map[string]interface{}{
			"handling_mode":            HandlingModeBot,
			"handling_expires_at":      nil,
			"handling_timeout_minutes": 0,
		}).Error; err != nil {
			// Fail open to bot mode: answering after an expired takeover is the
			// documented behavior, a stale row only delays the next flip attempt.
			logger.Warnf(ctx, "[IM] Failed to expire takeover for session %s: %v", cs.SessionID, err)
		} else {
			logger.Infof(ctx, "[IM] Takeover expired, bot resumed: platform=%s session=%s", cs.Platform, cs.SessionID)
		}
		cs.HandlingMode = HandlingModeBot
		cs.HandlingExpiresAt = nil
		cs.HandlingTimeoutMinutes = 0
		return false
	}

	if content := strings.TrimSpace(msg.Content); content != "" {
		if _, err := s.messageService.CreateMessage(ctx, &types.Message{
			SessionID:   session.ID,
			RequestID:   uuid.New().String(),
			Role:        "user",
			Content:     msg.Content,
			IsCompleted: true,
			Channel:     types.ChannelIMTakeover,
			CreatedAt:   time.Now(),
		}); err != nil {
			logger.Errorf(ctx, "[IM] Failed to record message during takeover for session %s: %v", session.ID, err)
		}
	}
	// Surface the conversation for the operator; best-effort like the manual
	// reply path.
	if err := s.db.Model(&types.Session{}).Where("id = ?", session.ID).
		Update("updated_at", time.Now()).Error; err != nil {
		logger.Warnf(ctx, "[IM] Failed to bump session %s during takeover: %v", session.ID, err)
	}
	logger.Infof(ctx, "[IM] Bot silenced by takeover: platform=%s session=%s user=%s", cs.Platform, session.ID, msg.UserID)
	return true
}

// extendTakeoverAfterManualReply refreshes the takeover window after a
// successful operator reply, so an ongoing human conversation never has the
// bot resume mid-dialogue. No-op unless the live conversation is in human mode
// with a bounded window. Best-effort: the reply already succeeded.
func (s *Service) extendTakeoverAfterManualReply(ctx context.Context, tenantID uint64, sessionID string) {
	cs, err := s.resolveLiveChannelSession(ctx, tenantID, sessionID)
	if err != nil {
		if !errors.Is(err, ErrHandlingNotIMSession) && !errors.Is(err, ErrHandlingConversationGone) {
			logger.Warnf(ctx, "[IM] Failed to resolve live conversation to extend takeover for session %s: %v", sessionID, err)
		}
		return
	}
	if cs.HandlingMode != HandlingModeHuman || cs.HandlingExpiresAt == nil || cs.HandlingTimeoutMinutes <= 0 {
		return
	}
	expires := time.Now().Add(time.Duration(cs.HandlingTimeoutMinutes) * time.Minute)
	if err := s.db.Model(&ChannelSession{}).Where("id = ?", cs.ID).
		Update("handling_expires_at", expires).Error; err != nil {
		logger.Warnf(ctx, "[IM] Failed to extend takeover for session %s: %v", cs.SessionID, err)
	}
}
