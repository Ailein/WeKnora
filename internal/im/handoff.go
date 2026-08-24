package im

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Automatic human handoff: a channel can be configured to hand a conversation
// to operators when the user asks for a human (keyword trigger) or when the
// bot fails to answer several messages in a row (fallback trigger). Firing a
// trigger silences the bot (human takeover, WhatsApp only — the one platform
// operators can reply into from the console), tells the user, and notifies
// operators through a configurable webhook.

const (
	// Bounds keep a channel config from turning the trigger into noise (or a
	// permanently silenced bot). Timeout bounds reuse the manual-takeover ones.
	maxHandoffKeywords     = 20
	maxHandoffKeywordRunes = 50
	maxHandoffFallbacks    = 10
	maxHandoffReplyRunes   = 500
	// handoffNotifyCooldown suppresses repeat notifications for the same
	// conversation: a user spamming "转人工" must not spam the operators.
	handoffNotifyCooldown = 10 * time.Minute
	// handoffMessageExcerptRunes caps the user-message excerpt embedded in
	// notifications.
	handoffMessageExcerptRunes = 200

	defaultHandoffAutoReply = "已为您转接人工客服，请稍候，人工客服看到后会尽快回复您。"

	HandoffReasonKeyword  = "keyword"
	HandoffReasonFallback = "fallback"

	HandoffWebhookGeneric  = "generic"
	HandoffWebhookWeCom    = "wecom"
	HandoffWebhookDingTalk = "dingtalk"
	HandoffWebhookFeishu   = "feishu"
	HandoffWebhookSlack    = "slack"
)

// handoffWebhookClient posts trigger notifications; short timeout so a dead
// webhook endpoint never backs up message handling (sends run in goroutines).
var handoffWebhookClient = &http.Client{Timeout: 8 * time.Second}

// HandoffConfig is the per-channel trigger configuration stored in
// im_channels.handoff_config. Zero value = feature off.
type HandoffConfig struct {
	Enabled bool `json:"enabled"`
	// Keywords hand the conversation off when a user message contains one
	// (case-insensitive). Keep them specific ("转人工", "人工客服") — "人工"
	// alone would also match "人工智能".
	Keywords []string `json:"keywords,omitempty"`
	// FallbackThreshold hands off after this many consecutive bot replies that
	// failed to answer (error or empty result). 0 disables the trigger.
	FallbackThreshold int `json:"fallback_threshold,omitempty"`
	// AutoReply is sent to the user when a trigger fires; empty selects the
	// built-in default.
	AutoReply string `json:"auto_reply,omitempty"`
	// TimeoutMinutes bounds the automatic takeover window (bot resumes after
	// it, like a manual takeover). 0 selects the default window — an
	// auto-trigger is never unbounded, nobody chose to hold the conversation.
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
	// WebhookURL, when set, receives a notification per trigger. WebhookFormat
	// picks the payload shape: generic JSON or a chat-bot format
	// (wecom/dingtalk/feishu/slack custom robots).
	WebhookURL    string `json:"webhook_url,omitempty"`
	WebhookFormat string `json:"webhook_format,omitempty"`
}

// ParseHandoffConfig decodes and normalizes a stored config. Unknown or
// out-of-range values are clamped rather than rejected: message handling must
// never fail on a config row written by an older or newer build.
func ParseHandoffConfig(raw types.JSON) HandoffConfig {
	var cfg HandoffConfig
	if len(raw) > 0 {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	keywords := make([]string, 0, len(cfg.Keywords))
	for _, kw := range cfg.Keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" || len([]rune(kw)) > maxHandoffKeywordRunes {
			continue
		}
		keywords = append(keywords, kw)
		if len(keywords) == maxHandoffKeywords {
			break
		}
	}
	cfg.Keywords = keywords
	if cfg.FallbackThreshold < 0 {
		cfg.FallbackThreshold = 0
	}
	if cfg.FallbackThreshold > maxHandoffFallbacks {
		cfg.FallbackThreshold = maxHandoffFallbacks
	}
	if runes := []rune(strings.TrimSpace(cfg.AutoReply)); len(runes) > maxHandoffReplyRunes {
		cfg.AutoReply = string(runes[:maxHandoffReplyRunes])
	} else {
		cfg.AutoReply = string(runes)
	}
	if cfg.TimeoutMinutes <= 0 {
		cfg.TimeoutMinutes = defaultTakeoverTimeoutMinutes
	}
	if cfg.TimeoutMinutes < minTakeoverTimeoutMinutes {
		cfg.TimeoutMinutes = minTakeoverTimeoutMinutes
	}
	if cfg.TimeoutMinutes > maxTakeoverTimeoutMinutes {
		cfg.TimeoutMinutes = maxTakeoverTimeoutMinutes
	}
	cfg.WebhookURL = strings.TrimSpace(cfg.WebhookURL)
	cfg.WebhookFormat = normalizeHandoffWebhookFormat(cfg.WebhookFormat)
	return cfg
}

// ValidateHandoffConfigJSON checks a client-submitted config document. It
// accepts anything ParseHandoffConfig would clamp, and rejects only what must
// not be stored at all: non-JSON bodies and non-HTTP webhook URLs.
func ValidateHandoffConfigJSON(raw types.JSON) error {
	if len(raw) == 0 {
		return nil
	}
	var cfg HandoffConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return fmt.Errorf("handoff_config is not a valid config object: %w", err)
	}
	if webhook := strings.TrimSpace(cfg.WebhookURL); webhook != "" {
		u, err := url.Parse(webhook)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("handoff_config.webhook_url must be an http(s) URL")
		}
	}
	return nil
}

func normalizeHandoffWebhookFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case HandoffWebhookWeCom:
		return HandoffWebhookWeCom
	case HandoffWebhookDingTalk:
		return HandoffWebhookDingTalk
	case HandoffWebhookFeishu:
		return HandoffWebhookFeishu
	case HandoffWebhookSlack:
		return HandoffWebhookSlack
	default:
		return HandoffWebhookGeneric
	}
}

// matchHandoffKeyword reports the first configured keyword contained in the
// message (case-insensitive). Containment, not equality, so "帮我转人工" still
// triggers; the config hint warns against over-generic keywords.
func matchHandoffKeyword(content string, keywords []string) string {
	content = strings.ToLower(strings.TrimSpace(content))
	if content == "" {
		return ""
	}
	for _, kw := range keywords {
		if strings.Contains(content, strings.ToLower(kw)) {
			return kw
		}
	}
	return ""
}

// handoffAutoReply resolves the user-facing reply for a fired trigger.
func handoffAutoReply(cfg HandoffConfig) string {
	if cfg.AutoReply != "" {
		return cfg.AutoReply
	}
	return defaultHandoffAutoReply
}

// handoffExcerpt bounds the user-message text embedded in notifications.
func handoffExcerpt(content string) string {
	content = strings.TrimSpace(content)
	runes := []rune(content)
	if len(runes) <= handoffMessageExcerptRunes {
		return content
	}
	return string(runes[:handoffMessageExcerptRunes]) + "…"
}

// handoffGate is the keyword trigger. Called for inbound messages after the
// takeover gate (an already-taken-over conversation records messages there):
// when the message contains a configured keyword it records the message,
// switches the conversation to human handling where operators can answer from
// the console, replies with the configured notice, and notifies operators.
// Returns true when the message was consumed (the QA pipeline must not run).
func (s *Service) handoffGate(
	ctx context.Context, channel *IMChannel, cs *ChannelSession, session *types.Session,
	msg *IncomingMessage, adapter Adapter,
) bool {
	if channel == nil || cs == nil {
		return false
	}
	cfg := ParseHandoffConfig(channel.HandoffConfig)
	if !cfg.Enabled || len(cfg.Keywords) == 0 {
		return false
	}
	keyword := matchHandoffKeyword(msg.Content, cfg.Keywords)
	if keyword == "" {
		return false
	}

	// The QA pipeline is skipped, so record the triggering message ourselves;
	// im_takeover marks it as received outside bot handling, like takeoverGate.
	if _, err := s.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		RequestID:   uuid.New().String(),
		Role:        "user",
		Content:     strings.TrimSpace(msg.Content),
		IsCompleted: true,
		Channel:     types.ChannelIMTakeover,
		CreatedAt:   time.Now(),
	}); err != nil {
		logger.Errorf(ctx, "[IM] Failed to record handoff trigger message for session %s: %v", session.ID, err)
	}

	logger.Infof(ctx, "[IM] Handoff keyword trigger: platform=%s session=%s user=%s keyword=%q",
		cs.Platform, session.ID, msg.UserID, keyword)
	s.triggerHandoff(ctx, channel, cs, session, msg, adapter, cfg, handoffTrigger{
		Reason:  HandoffReasonKeyword,
		Keyword: keyword,
		Message: handoffExcerpt(msg.Content),
	})
	return true
}

// noteBotAnswerOutcome is the fallback trigger's counter. Called after every
// bot answer with whether it failed (pipeline error or nothing to say — the
// user got a canned apology). Success resets the conversation's streak;
// failures accumulate and hand the conversation off at the threshold.
func (s *Service) noteBotAnswerOutcome(
	ctx context.Context, channel *IMChannel, session *types.Session,
	msg *IncomingMessage, adapter Adapter, failed bool,
) {
	if channel == nil || session == nil {
		return
	}
	cfg := ParseHandoffConfig(channel.HandoffConfig)
	if !cfg.Enabled || cfg.FallbackThreshold <= 0 {
		return
	}

	if !failed {
		// Blind conditional reset: one UPDATE that matches nothing on the
		// common (streak already 0) path.
		if err := s.db.Model(&ChannelSession{}).
			Where("session_id = ? AND deleted_at IS NULL AND consecutive_failures <> 0", session.ID).
			Update("consecutive_failures", 0).Error; err != nil {
			logger.Warnf(ctx, "[IM] Failed to reset handoff failure streak for session %s: %v", session.ID, err)
		}
		return
	}

	if err := s.db.Model(&ChannelSession{}).
		Where("session_id = ? AND deleted_at IS NULL", session.ID).
		UpdateColumn("consecutive_failures", gorm.Expr("consecutive_failures + 1")).Error; err != nil {
		logger.Warnf(ctx, "[IM] Failed to bump handoff failure streak for session %s: %v", session.ID, err)
		return
	}

	var cs ChannelSession
	if err := s.db.Where("session_id = ? AND deleted_at IS NULL", session.ID).First(&cs).Error; err != nil {
		logger.Warnf(ctx, "[IM] Failed to reload channel session %s after failure bump: %v", session.ID, err)
		return
	}
	if cs.ConsecutiveFailures < cfg.FallbackThreshold {
		return
	}

	logger.Infof(ctx, "[IM] Handoff fallback trigger: platform=%s session=%s user=%s failures=%d",
		cs.Platform, session.ID, msg.UserID, cs.ConsecutiveFailures)
	s.triggerHandoff(ctx, channel, &cs, session, msg, adapter, cfg, handoffTrigger{
		Reason:        HandoffReasonFallback,
		FallbackCount: cs.ConsecutiveFailures,
		Message:       handoffExcerpt(msg.Content),
	})
}

// handoffTrigger describes one fired trigger for the reply/notification.
type handoffTrigger struct {
	Reason        string
	Keyword       string
	FallbackCount int
	Message       string
}

// triggerHandoff performs the handoff: silence the bot where operators can
// take over from the console, reply to the user, surface the conversation, and
// notify operators. Every step past the state switch is best-effort — a broken
// webhook must not undo the takeover.
func (s *Service) triggerHandoff(
	ctx context.Context, channel *IMChannel, cs *ChannelSession, session *types.Session,
	msg *IncomingMessage, adapter Adapter, cfg HandoffConfig, trigger handoffTrigger,
) {
	now := time.Now()
	notified := cs.HandoffNotifiedAt != nil && now.Sub(*cs.HandoffNotifiedAt) < handoffNotifyCooldown

	updates := map[string]interface{}{
		"consecutive_failures": 0,
		"updated_at":           now,
	}
	if !notified {
		// Only a delivered notification stamps the cooldown; a suppressed
		// re-trigger must not keep pushing the next one further out.
		updates["handoff_notified_at"] = now
	}
	// Human handling only where operators can actually answer from the console
	// (WhatsApp). Elsewhere the trigger still notifies — the operator answers
	// out-of-band — but silencing the bot would strand the user.
	if ManualReplySupported(cs.Platform) && cs.HandlingMode != HandlingModeHuman {
		expires := now.Add(time.Duration(cfg.TimeoutMinutes) * time.Minute)
		updates["handling_mode"] = HandlingModeHuman
		updates["handling_expires_at"] = expires
		updates["handling_timeout_minutes"] = cfg.TimeoutMinutes
	}
	if err := s.db.Model(&ChannelSession{}).Where("id = ?", cs.ID).Updates(updates).Error; err != nil {
		logger.Errorf(ctx, "[IM] Failed to switch session %s to human handling on trigger: %v", cs.SessionID, err)
		return
	}

	if notified {
		// Same conversation triggered within the cooldown: the state switch
		// above still applies (a fresh window), but stay quiet on both sides.
		logger.Infof(ctx, "[IM] Handoff re-trigger within cooldown, notification suppressed: session=%s", cs.SessionID)
		return
	}

	reply := handoffAutoReply(cfg)
	if err := adapter.SendReply(ctx, msg, &ReplyMessage{Content: reply, IsFinal: true}); err != nil {
		logger.Warnf(ctx, "[IM] Failed to send handoff auto-reply for session %s: %v", session.ID, err)
	}
	// Persist the notice so the operator console shows what the user was told.
	if _, err := s.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		RequestID:   uuid.New().String(),
		Role:        "assistant",
		Content:     reply,
		IsCompleted: true,
		Channel:     types.ChannelIM,
		CreatedAt:   time.Now(),
	}); err != nil {
		logger.Errorf(ctx, "[IM] Failed to record handoff auto-reply for session %s: %v", session.ID, err)
	}
	// Resurface the conversation in the operator's session list.
	if err := s.db.Model(&types.Session{}).Where("id = ?", session.ID).
		Update("updated_at", time.Now()).Error; err != nil {
		logger.Warnf(ctx, "[IM] Failed to bump session %s on handoff: %v", session.ID, err)
	}

	if cfg.WebhookURL != "" {
		notification := HandoffNotification{
			Event:         "im.handoff_requested",
			Reason:        trigger.Reason,
			Keyword:       trigger.Keyword,
			FallbackCount: trigger.FallbackCount,
			Platform:      cs.Platform,
			ChannelID:     channel.ID,
			ChannelName:   channel.Name,
			SessionID:     session.ID,
			UserID:        msg.UserID,
			ChatID:        msg.ChatID,
			Message:       trigger.Message,
			TriggeredAt:   now,
		}
		notifyCtx := logger.CloneContext(context.WithoutCancel(ctx))
		go s.sendHandoffWebhook(notifyCtx, cfg.WebhookURL, cfg.WebhookFormat, notification)
	}
}

// HandoffNotification is the generic-format webhook payload; chat-bot formats
// render it as text.
type HandoffNotification struct {
	Event         string    `json:"event"`
	Reason        string    `json:"reason"`
	Keyword       string    `json:"keyword,omitempty"`
	FallbackCount int       `json:"fallback_count,omitempty"`
	Platform      string    `json:"platform"`
	ChannelID     string    `json:"channel_id"`
	ChannelName   string    `json:"channel_name"`
	SessionID     string    `json:"session_id"`
	UserID        string    `json:"user_id"`
	ChatID        string    `json:"chat_id,omitempty"`
	Message       string    `json:"message,omitempty"`
	TriggeredAt   time.Time `json:"triggered_at"`
}

// handoffNotificationText renders the operator-facing text used by the
// chat-bot webhook formats.
func handoffNotificationText(n HandoffNotification) string {
	var b strings.Builder
	b.WriteString("【WeKnora 转人工提醒】\n")
	fmt.Fprintf(&b, "渠道：%s（%s）\n", n.ChannelName, n.Platform)
	fmt.Fprintf(&b, "用户：%s\n", n.UserID)
	switch n.Reason {
	case HandoffReasonFallback:
		fmt.Fprintf(&b, "原因：机器人连续 %d 条消息未能回答\n", n.FallbackCount)
	default:
		fmt.Fprintf(&b, "原因：用户消息命中关键词「%s」\n", n.Keyword)
	}
	if n.Message != "" {
		fmt.Fprintf(&b, "最近消息：%s\n", n.Message)
	}
	b.WriteString("请尽快到 WeKnora 控制台会话页处理。")
	return b.String()
}

// buildHandoffWebhookBody renders the notification for the configured format.
func buildHandoffWebhookBody(format string, n HandoffNotification) ([]byte, error) {
	text := handoffNotificationText(n)
	switch format {
	case HandoffWebhookWeCom, HandoffWebhookDingTalk:
		return json.Marshal(map[string]interface{}{
			"msgtype": "text",
			"text":    map[string]string{"content": text},
		})
	case HandoffWebhookFeishu:
		return json.Marshal(map[string]interface{}{
			"msg_type": "text",
			"content":  map[string]string{"text": text},
		})
	case HandoffWebhookSlack:
		return json.Marshal(map[string]string{"text": text})
	default:
		return json.Marshal(n)
	}
}

// sendHandoffWebhook posts one notification; failures only log — operators
// still see the conversation flagged in the console.
func (s *Service) sendHandoffWebhook(ctx context.Context, webhookURL, format string, n HandoffNotification) {
	body, err := buildHandoffWebhookBody(format, n)
	if err != nil {
		logger.Errorf(ctx, "[IM] Failed to build handoff webhook payload: %v", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		logger.Errorf(ctx, "[IM] Invalid handoff webhook URL: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := handoffWebhookClient.Do(req)
	if err != nil {
		logger.Errorf(ctx, "[IM] Handoff webhook delivery failed: session=%s err=%v", n.SessionID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		logger.Errorf(ctx, "[IM] Handoff webhook rejected: session=%s status=%d", n.SessionID, resp.StatusCode)
		return
	}
	logger.Infof(ctx, "[IM] Handoff webhook delivered: session=%s reason=%s format=%s", n.SessionID, n.Reason, format)
}
