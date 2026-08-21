package im

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ManualReplyChannel marks messages an operator typed in the Web console and
// delivered through an IM adapter. It is distinct from "im" (bot traffic) so
// the chat UI can label the bubble and the IM stream-recovery path ignores it,
// and distinct from "web" so the message reads as outbound-to-IM, not a web
// question. History assembly appends these replies to the preceding turn's
// answer (see LoadAgentHistory / loadAndProcessHistory), so the bot stays
// aware of what the operator told the user.
const ManualReplyChannel = types.ChannelIMManual

// manualReplyMaxRunes bounds a single manual reply. Platforms split long texts
// into segments themselves (WhatsApp does); the cap only guards against
// accidental huge payloads landing in the messages table.
const manualReplyMaxRunes = 8000

const (
	// ManualReplyMaxAttachments bounds files per manual reply (images and
	// documents combined). Matches the web composer's per-message cap.
	ManualReplyMaxAttachments = 5
	// manualReplyMaxImageBytes is WhatsApp's practical ceiling for an inline
	// image; larger files must travel as documents.
	manualReplyMaxImageBytes = 16 << 20
	// manualReplyMaxFileBytes mirrors the inbound IM attachment cap.
	manualReplyMaxFileBytes = MaxIMAttachmentBytes
	// manualReplyInlineImageBytes bounds images stored as data URIs on the
	// message row. Larger images are still delivered, but recorded as a
	// placeholder so history rows stay small enough for the per-turn history
	// fetch (GetRecentMessagesBySession loads whole rows every QA turn).
	manualReplyInlineImageBytes = 2 << 20
)

// manualReplyCaps describes what a platform's adapter supports for
// operator-initiated replies.
type manualReplyCaps struct {
	// Media marks that the adapter's SendReply understands
	// ReplyMessage.Attachments. Without it attachments would be silently
	// dropped, so the service rejects them up front.
	Media bool
}

// manualReplyPlatforms lists platforms whose SendReply is known to resolve a
// reply target from a stored ChannelSession alone, without a live
// IncomingMessage from a callback. Extend only after verifying the adapter:
//   - whatsapp: DMs store the bare phone in user_id (SendReply falls back to
//     phone@s.whatsapp.net) and groups store the full group JID in chat_id
//     (parsed directly); quote-building is skipped when MessageID is empty.
var manualReplyPlatforms = map[string]manualReplyCaps{
	string(PlatformWhatsApp): {Media: true},
}

// ManualReplySupported reports whether operators can send manual replies into
// conversations of the given platform.
func ManualReplySupported(platform string) bool {
	_, ok := manualReplyPlatforms[strings.ToLower(platform)]
	return ok
}

// Sentinel errors let the HTTP handler map failures to precise status codes.
var (
	ErrManualReplyEmptyContent       = errors.New("manual reply is empty")
	ErrManualReplyTooLong            = fmt.Errorf("manual reply exceeds %d characters", manualReplyMaxRunes)
	ErrManualReplyNotIMSession       = errors.New("session is not bound to an IM conversation")
	ErrManualReplyUnsupported        = errors.New("manual replies are not supported for this platform")
	ErrManualReplyNotRunning         = errors.New("IM channel runtime is not active on this instance")
	ErrManualReplyDelivery           = errors.New("failed to deliver the manual reply to the IM platform")
	ErrManualReplyTooManyAttachments = fmt.Errorf("manual reply exceeds %d attachments", ManualReplyMaxAttachments)
	ErrManualReplyAttachmentTooLarge = errors.New("manual reply attachment is too large")
	ErrManualReplyBadAttachment      = errors.New("manual reply attachment is empty or unreadable")
	ErrManualReplyMediaUnsupported   = errors.New("attachments are not supported for this platform")
)

// manualReplyIncoming rebuilds the addressing envelope an adapter's SendReply
// needs from the durable ChannelSession row. ChatID is only ever populated for
// group chats (adapters leave it empty for DMs), so its presence doubles as
// the chat-type signal.
func manualReplyIncoming(cs *ChannelSession) *IncomingMessage {
	incoming := &IncomingMessage{
		Platform: Platform(cs.Platform),
		UserID:   cs.UserID,
		ChatID:   cs.ChatID,
		ChatType: ChatTypeDirect,
	}
	if cs.ChatID != "" {
		incoming.ChatType = ChatTypeGroup
	}
	return incoming
}

// validateManualReplyAttachmentSizes enforces per-file and count limits that do
// not depend on the target platform, so obviously bad payloads fail before any
// DB lookup.
func validateManualReplyAttachmentSizes(attachments []*ReplyAttachment) error {
	if len(attachments) > ManualReplyMaxAttachments {
		return ErrManualReplyTooManyAttachments
	}
	for _, att := range attachments {
		if att == nil || len(att.Data) == 0 {
			return ErrManualReplyBadAttachment
		}
		limit := manualReplyMaxFileBytes
		if att.Kind == MessageTypeImage {
			limit = manualReplyMaxImageBytes
		}
		if len(att.Data) > limit {
			return fmt.Errorf("%w: %s is over %d MB", ErrManualReplyAttachmentTooLarge, att.FileName, limit>>20)
		}
	}
	return nil
}

// manualReplyMessageMedia converts delivered attachments into the message-row
// form the Web console renders. Small images are inlined as data URIs (the row
// IS the storage — temporary-attachment uploads get garbage-collected, which
// would break permanent history). Oversized images and all documents keep
// metadata only.
func manualReplyMessageMedia(attachments []*ReplyAttachment) (types.MessageImages, types.MessageAttachments) {
	var images types.MessageImages
	var files types.MessageAttachments
	for _, att := range attachments {
		name := filepath.Base(att.FileName)
		if att.Kind == MessageTypeImage {
			img := types.MessageImage{}
			if len(att.Data) <= manualReplyInlineImageBytes {
				img.URL = "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
			} else {
				img.Caption = fmt.Sprintf("%s (%.1f MB)", name, float64(len(att.Data))/(1<<20))
			}
			images = append(images, img)
			continue
		}
		files = append(files, types.MessageAttachment{
			FileName: name,
			FileType: strings.ToLower(filepath.Ext(name)),
			FileSize: int64(len(att.Data)),
		})
	}
	return images, files
}

// SendManualReply delivers operator-typed text (and optional media) into the
// IM conversation bound to sessionID and records it as a completed assistant
// message so the Web console history matches what the IM user saw.
//
// The channel-session lookup is Unscoped on purpose: /clear soft-deletes the
// mapping (and rebinds the peer to a fresh session), but the old row still
// identifies the peer, and an operator replying inside a historical session
// means exactly that peer. The tenant filter stays, so no cross-tenant row can
// ever be used.
//
// Delivery requires the adapter runtime on THIS instance. In multi-instance
// deployments a websocket adapter (WhatsApp) lives only on the elected leader,
// so requests landing elsewhere fail with ErrManualReplyNotRunning rather than
// being forwarded.
func (s *Service) SendManualReply(
	ctx context.Context, tenantID uint64, sessionID, content string, attachments []*ReplyAttachment,
) (*types.Message, error) {
	content = strings.TrimSpace(content)
	if content == "" && len(attachments) == 0 {
		return nil, ErrManualReplyEmptyContent
	}
	if len([]rune(content)) > manualReplyMaxRunes {
		return nil, ErrManualReplyTooLong
	}
	if err := validateManualReplyAttachmentSizes(attachments); err != nil {
		return nil, err
	}

	var cs ChannelSession
	err := s.db.Unscoped().
		Where("session_id = ? AND tenant_id = ?", sessionID, tenantID).
		Order("created_at DESC").
		First(&cs).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrManualReplyNotIMSession
		}
		return nil, fmt.Errorf("load channel session: %w", err)
	}
	caps, ok := manualReplyPlatforms[strings.ToLower(cs.Platform)]
	if !ok {
		return nil, ErrManualReplyUnsupported
	}
	if len(attachments) > 0 && !caps.Media {
		return nil, ErrManualReplyMediaUnsupported
	}
	if cs.IMChannelID == "" {
		// Legacy mappings created before channel binding cannot resolve a runtime.
		return nil, ErrManualReplyNotRunning
	}

	adapter, channel, ok := s.GetChannelAdapter(cs.IMChannelID)
	if !ok || channel.TenantID != tenantID {
		return nil, ErrManualReplyNotRunning
	}

	reply := &ReplyMessage{Content: content, IsFinal: true, Attachments: attachments}
	if err := adapter.SendReply(ctx, manualReplyIncoming(&cs), reply); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManualReplyDelivery, err)
	}

	// Persist only after successful delivery: a failed send must not leave a
	// phantom reply in the console history. The caller's authenticated context
	// carries the tenant; IM sessions have an empty user_id, which every
	// caller's session scope matches.
	images, files := manualReplyMessageMedia(attachments)
	msg, err := s.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   sessionID,
		RequestID:   uuid.New().String(),
		Role:        "assistant",
		Content:     content,
		Images:      images,
		Attachments: files,
		IsCompleted: true,
		Channel:     ManualReplyChannel,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("manual reply delivered but failed to persist: %w", err)
	}

	// Surface the conversation at the top of the session list; delivery already
	// succeeded, so an ordering hiccup must not fail the request.
	if err := s.db.Model(&types.Session{}).Where("id = ?", sessionID).
		Update("updated_at", time.Now()).Error; err != nil {
		logger.Warnf(ctx, "[IM] Failed to bump session %s after manual reply: %v", sessionID, err)
	}

	logger.Infof(ctx, "[IM] Manual reply sent: platform=%s channel=%s session=%s chars=%d attachments=%d",
		cs.Platform, cs.IMChannelID, sessionID, len([]rune(content)), len(attachments))
	return msg, nil
}
