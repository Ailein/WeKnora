package im

import (
	"context"
	"io"
	"time"

	"github.com/gin-gonic/gin"
)

// Platform identifies an IM platform.
type Platform string

const (
	PlatformWeCom  Platform = "wecom"
	PlatformFeishu Platform = "feishu"
	// PlatformLark is Feishu's international edition (open.larksuite.com).
	// It shares the Feishu adapter; only the API host and tenant differ.
	PlatformLark       Platform = "lark"
	PlatformSlack      Platform = "slack"
	PlatformTelegram   Platform = "telegram"
	PlatformDingtalk   Platform = "dingtalk"
	PlatformMattermost Platform = "mattermost"
	PlatformWeChat     Platform = "wechat"
	PlatformQQBot      Platform = "qqbot"
	PlatformYunzhijia  Platform = "yunzhijia"
	// PlatformWhatsApp links a regular WhatsApp account as a companion
	// device via the WhatsApp Web multidevice protocol (whatsmeow), not the
	// official Cloud API.
	PlatformWhatsApp Platform = "whatsapp"
)

// SessionMode determines how IM sessions are resolved.
type SessionMode string

const (
	// SessionModeUser resolves sessions by (platform, user_id, chat_id, tenant_id).
	SessionModeUser SessionMode = "user"
	// SessionModeThread resolves sessions by (platform, thread_id, chat_id, tenant_id).
	SessionModeThread SessionMode = "thread"
)

// MessageType identifies the kind of IM message.
type MessageType string

const (
	MessageTypeText  MessageType = "text"
	MessageTypeFile  MessageType = "file"
	MessageTypeImage MessageType = "image"
)

// IncomingMessage is the unified message parsed from an IM callback.
type IncomingMessage struct {
	// Platform identifies which IM platform the message comes from.
	Platform Platform
	// MessageType is "text" (default) or "file".
	MessageType MessageType
	// UserID is the IM-platform user identifier.
	UserID string
	// UserName is the display name of the user (optional).
	UserName string
	// ChatID is the group/channel ID (empty for direct messages).
	ChatID string
	// ChatType distinguishes direct message from group chat.
	ChatType ChatType
	// Content is the text content of the message (empty for file messages).
	Content string
	// MessageID is the IM-platform message identifier (for dedup).
	MessageID string
	// FileKey is the platform file identifier (for file messages).
	FileKey string
	// FileName is the original file name (for file messages).
	FileName string
	// FileSize is the file size in bytes (for file messages, optional).
	FileSize int64
	// ThreadID is the platform-specific thread identifier.
	// - Slack: thread_ts (top-level message uses its own timestamp)
	// - Mattermost: root_id, or post_id if top-level
	// - Feishu/Lark: root_id, or message_id if top-level
	// - Telegram: message_thread_id (Forum Topics only)
	// Empty for platforms without thread support (WeCom, DingTalk).
	// In thread mode, top-level messages use their own ID as ThreadID,
	// effectively creating a new session per top-level message.
	ThreadID string
	// Quote is the quoted/replied message, if any.
	// Populated by adapters on platforms that support quote-reply.
	Quote *QuotedMessage
	// Extra holds platform-specific fields (e.g., WeCom stream ID).
	Extra map[string]string
}

// QuotedMessage holds the content and metadata of a quoted/replied message.
// Populated by platform adapters that support quote-reply (e.g. WeCom long-connection).
type QuotedMessage struct {
	// MessageID is the platform message ID of the quoted message.
	MessageID string
	// Content is the text content. Empty for non-text message types.
	Content string
	// SenderID is the platform user ID of the quoted message's author.
	SenderID string
	// IsBotMessage indicates whether the quoted message was from the bot.
	IsBotMessage bool
	// NonTextType records the original message type when the quoted message
	// has no extractable text (e.g. "image", "file", "video").
	// Empty when Content is populated. Used to generate LLM instructions
	// instead of content placeholders that cause hallucination.
	NonTextType string
}

// ChatType represents the IM chat type.
type ChatType string

const (
	ChatTypeDirect ChatType = "direct"
	ChatTypeGroup  ChatType = "group"
)

// ReplyMessage is what WeKnora sends back to the IM platform.
type ReplyMessage struct {
	// Content is the text content (Markdown).
	Content string
	// IsStreaming indicates whether this is a streaming chunk.
	IsStreaming bool
	// IsFinal marks the last chunk of a streaming reply.
	IsFinal bool
	// Attachments carries media files to deliver after the text. Only
	// operator manual replies populate it today; adapters that do not
	// support outbound media ignore it (the manual-reply platform caps
	// gate which platforms may receive attachments).
	Attachments []*ReplyAttachment
	// Extra holds platform-specific fields.
	Extra map[string]string
}

// ReplyAttachment is one media file carried by a ReplyMessage.
type ReplyAttachment struct {
	// Kind is MessageTypeImage for inline images, MessageTypeFile otherwise.
	Kind MessageType
	// FileName is the display name (base name only, no path).
	FileName string
	// MimeType is the content type used by the platform upload.
	MimeType string
	// Data is the raw file payload.
	Data []byte
}

// Adapter is the interface every IM platform must implement.
type Adapter interface {
	// Platform returns the platform identifier.
	Platform() Platform

	// VerifyCallback verifies the signature/token of an incoming callback request.
	// Returns nil if verification passes.
	VerifyCallback(c *gin.Context) error

	// ParseCallback parses the raw IM callback request into a unified IncomingMessage.
	// Returns nil message for non-message events (e.g., URL verification).
	ParseCallback(c *gin.Context) (*IncomingMessage, error)

	// SendReply sends a reply back to the IM platform.
	SendReply(ctx context.Context, incoming *IncomingMessage, reply *ReplyMessage) error

	// HandleURLVerification handles the initial URL verification challenge from the IM platform.
	// Returns true if this request is a verification request and has been handled.
	HandleURLVerification(c *gin.Context) bool
}

// StreamSender is an optional interface that adapters can implement to support streaming replies.
// When an adapter implements StreamSender, the IM service will push answer chunks in real-time
// instead of waiting for the full answer.
type StreamSender interface {
	// StartStream initializes a streaming reply session (e.g., creates a streaming card).
	// Returns a platform-specific stream ID for subsequent chunk/end calls.
	StartStream(ctx context.Context, incoming *IncomingMessage) (string, error)

	// UpdateStreamContent replaces the user-visible stream text with fullContent so far.
	// Platforms with replace semantics (WeCom, Telegram edit, etc.) show this as the entire message.
	UpdateStreamContent(ctx context.Context, incoming *IncomingMessage, streamID string, fullContent string) error

	// FinalizeStream performs the final replace with answer-only content (thinking/tools stripped).
	FinalizeStream(ctx context.Context, incoming *IncomingMessage, streamID string, finalContent string) error

	// EndStream finalizes a streaming reply.
	EndStream(ctx context.Context, incoming *IncomingMessage, streamID string) error
}

// FileDownloader is an optional interface that adapters can implement to support
// downloading file attachments from the IM platform. It allows file/image
// messages to be supplied to QA as attachments; when a knowledge_base_id is
// configured, the same capability also enables asynchronous knowledge-base save.
type FileDownloader interface {
	// DownloadFile downloads a file resource from the IM platform.
	// Returns the file content reader, the resolved file name, and any error.
	DownloadFile(ctx context.Context, msg *IncomingMessage) (io.ReadCloser, string, error)
}

// ── Channel runtime status ──

// Channel runtime states. The first group is reported live by StatusReporter
// adapters; the second is synthesized by the service/handler layer when no
// instance holds a live runtime for the channel.
const (
	// ChannelStateConnected: the long connection is up and authenticated.
	ChannelStateConnected = "connected"
	// ChannelStateConnecting: connecting or auto-reconnecting (transient).
	ChannelStateConnecting = "connecting"
	// ChannelStateLoggedOut: the platform revoked the session (e.g. the
	// WhatsApp device was unlinked from the phone); re-pairing is required.
	ChannelStateLoggedOut = "logged_out"
	// ChannelStateStreamReplaced: another client connected with the same
	// session and took over the connection.
	ChannelStateStreamReplaced = "stream_replaced"
	// ChannelStateError: terminal platform error (temporary ban, outdated
	// client, …) described by Detail.
	ChannelStateError = "error"

	// ChannelStateRunning: the adapter is registered but has no live
	// connection to report on (webhook platforms).
	ChannelStateRunning = "running"
	// ChannelStateNotRunning: the channel is enabled but no instance holds a
	// runtime for it (start failure or a leadership gap).
	ChannelStateNotRunning = "not_running"
	// ChannelStateNeedsPairing: no usable pairing credentials exist; an admin
	// must (re-)scan the QR code.
	ChannelStateNeedsPairing = "needs_pairing"
	// ChannelStateDisabled: the channel row is disabled.
	ChannelStateDisabled = "disabled"
)

// ChannelStatus is a point-in-time health snapshot of a channel runtime.
type ChannelStatus struct {
	State  string    `json:"state"`
	Detail string    `json:"detail,omitempty"`
	Since  time.Time `json:"since,omitzero"`
}

// StatusReporter is an optional interface for long-connection adapters that
// can report live connection health (e.g. WhatsApp). Webhook adapters have no
// meaningful live state and simply don't implement it.
type StatusReporter interface {
	// ChannelStatus returns the current connection-state snapshot.
	ChannelStatus() ChannelStatus
}
