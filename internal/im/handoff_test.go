package im

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestParseHandoffConfigClamps(t *testing.T) {
	cfg := ParseHandoffConfig(types.JSON(`{
		"enabled": true,
		"keywords": ["  转人工  ", "", "human agent"],
		"fallback_threshold": 99,
		"timeout_minutes": 100000,
		"webhook_format": "WeCom"
	}`))
	if !cfg.Enabled {
		t.Fatal("enabled flag lost in parse")
	}
	if len(cfg.Keywords) != 2 || cfg.Keywords[0] != "转人工" || cfg.Keywords[1] != "human agent" {
		t.Fatalf("keywords = %v, want trimmed non-empty list", cfg.Keywords)
	}
	if cfg.FallbackThreshold != maxHandoffFallbacks {
		t.Fatalf("threshold = %d, want clamped to %d", cfg.FallbackThreshold, maxHandoffFallbacks)
	}
	if cfg.TimeoutMinutes != maxTakeoverTimeoutMinutes {
		t.Fatalf("timeout = %d, want clamped to %d", cfg.TimeoutMinutes, maxTakeoverTimeoutMinutes)
	}
	if cfg.WebhookFormat != HandoffWebhookWeCom {
		t.Fatalf("format = %q, want case-insensitive wecom", cfg.WebhookFormat)
	}

	// Defaults: empty config parses to a disabled feature with sane bounds.
	cfg = ParseHandoffConfig(types.JSON(`{}`))
	if cfg.Enabled || len(cfg.Keywords) != 0 || cfg.FallbackThreshold != 0 {
		t.Fatalf("empty config = %+v, want disabled zero config", cfg)
	}
	if cfg.TimeoutMinutes != defaultTakeoverTimeoutMinutes {
		t.Fatalf("default timeout = %d, want %d", cfg.TimeoutMinutes, defaultTakeoverTimeoutMinutes)
	}
	if cfg.WebhookFormat != HandoffWebhookGeneric {
		t.Fatalf("default format = %q, want generic", cfg.WebhookFormat)
	}

	// Malformed JSON must not panic and yields the disabled zero config.
	cfg = ParseHandoffConfig(types.JSON(`not-json`))
	if cfg.Enabled {
		t.Fatal("malformed config must parse as disabled")
	}
}

func TestValidateHandoffConfigJSON(t *testing.T) {
	for _, ok := range []string{
		``, `{}`, `{"enabled":true,"webhook_url":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x"}`,
	} {
		if err := ValidateHandoffConfigJSON(types.JSON(ok)); err != nil {
			t.Fatalf("config %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		`not-json`, `{"webhook_url":"ftp://example.com/x"}`, `{"webhook_url":"just-a-path"}`,
	} {
		if err := ValidateHandoffConfigJSON(types.JSON(bad)); err == nil {
			t.Fatalf("config %q unexpectedly accepted", bad)
		}
	}
}

func TestMatchHandoffKeyword(t *testing.T) {
	keywords := []string{"转人工", "Human Agent"}
	if kw := matchHandoffKeyword("请帮我转人工，谢谢", keywords); kw != "转人工" {
		t.Fatalf("containment match = %q, want 转人工", kw)
	}
	if kw := matchHandoffKeyword("I need a HUMAN AGENT now", keywords); kw != "Human Agent" {
		t.Fatalf("case-insensitive match = %q, want Human Agent", kw)
	}
	for _, miss := range []string{"", "   ", "人工智能是什么", "help"} {
		if kw := matchHandoffKeyword(miss, keywords); kw != "" {
			t.Fatalf("message %q matched %q, want no match", miss, kw)
		}
	}
}

// handoffWebhookRecorder captures webhook deliveries for assertions.
type handoffWebhookRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
	hit    chan struct{}
}

func newHandoffWebhookRecorder() (*handoffWebhookRecorder, *httptest.Server) {
	rec := &handoffWebhookRecorder{hit: make(chan struct{}, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, body)
		rec.mu.Unlock()
		rec.hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	return rec, srv
}

func (r *handoffWebhookRecorder) waitOne(t *testing.T) []byte {
	t.Helper()
	select {
	case <-r.hit:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook was not delivered within 3s")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodies[len(r.bodies)-1]
}

func (r *handoffWebhookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func setHandoffConfig(t *testing.T, svc *Service, channel *IMChannel, cfg string) {
	t.Helper()
	channel.HandoffConfig = types.JSON(cfg)
	if err := svc.db.Model(&IMChannel{}).Where("id = ?", channel.ID).
		Update("handoff_config", cfg).Error; err != nil {
		t.Fatalf("store handoff config: %v", err)
	}
}

func TestHandoffGateKeywordTrigger(t *testing.T) {
	svc, adapter, msgSvc, channel := newManualReplyFixture(t)
	rec, srv := newHandoffWebhookRecorder()
	defer srv.Close()
	setHandoffConfig(t, svc, channel,
		`{"enabled":true,"keywords":["转人工"],"timeout_minutes":30,"auto_reply":"稍等，人工马上来","webhook_url":"`+srv.URL+`","webhook_format":"generic"}`)

	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)
	session := &types.Session{ID: cs.SessionID, TenantID: 1}
	msg := &IncomingMessage{Platform: "whatsapp", UserID: cs.UserID, Content: "请帮我转人工", ChatType: ChatTypeDirect}

	if !svc.handoffGate(context.Background(), channel, cs, session, msg, adapter) {
		t.Fatal("keyword message must be consumed by the handoff gate")
	}

	// Conversation switched to a bounded human takeover.
	row := reloadChannelSession(t, svc, cs.ID)
	if row.HandlingMode != HandlingModeHuman || row.HandlingTimeoutMinutes != 30 || row.HandlingExpiresAt == nil {
		t.Fatalf("row after trigger = mode=%s timeout=%d expires=%v, want bounded human mode",
			row.HandlingMode, row.HandlingTimeoutMinutes, row.HandlingExpiresAt)
	}
	if row.HandoffNotifiedAt == nil {
		t.Fatal("handoff_notified_at must be stamped on a delivered notification")
	}

	// User got the configured notice.
	if len(adapter.sent) != 1 || adapter.sent[0].reply.Content != "稍等，人工马上来" {
		t.Fatalf("adapter deliveries = %+v, want the configured auto-reply", adapter.sent)
	}

	// Console history: the trigger message and the notice were both recorded.
	if len(msgSvc.created) != 2 {
		t.Fatalf("persisted messages = %d, want trigger + notice", len(msgSvc.created))
	}
	if msgSvc.created[0].Role != "user" || msgSvc.created[0].Channel != types.ChannelIMTakeover {
		t.Fatalf("trigger message = %+v, want user message on im_takeover", msgSvc.created[0])
	}
	if msgSvc.created[1].Role != "assistant" || msgSvc.created[1].Channel != types.ChannelIM {
		t.Fatalf("notice message = %+v, want assistant message on im", msgSvc.created[1])
	}

	// Generic webhook payload carries the trigger details.
	var n HandoffNotification
	if err := json.Unmarshal(rec.waitOne(t), &n); err != nil {
		t.Fatalf("decode webhook payload: %v", err)
	}
	if n.Event != "im.handoff_requested" || n.Reason != HandoffReasonKeyword || n.Keyword != "转人工" ||
		n.Platform != "whatsapp" || n.ChannelID != channel.ID || n.SessionID != "session-dm" ||
		n.UserID != cs.UserID || n.Message != "请帮我转人工" {
		t.Fatalf("webhook payload = %+v, want full keyword trigger details", n)
	}
}

func TestHandoffGateNoTriggerPaths(t *testing.T) {
	svc, adapter, msgSvc, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)
	session := &types.Session{ID: cs.SessionID, TenantID: 1}

	// Feature off (default '{}' config).
	msg := &IncomingMessage{Platform: "whatsapp", UserID: cs.UserID, Content: "转人工"}
	if svc.handoffGate(context.Background(), channel, cs, session, msg, adapter) {
		t.Fatal("disabled config must not consume messages")
	}

	// Enabled but the message does not contain a keyword.
	setHandoffConfig(t, svc, channel, `{"enabled":true,"keywords":["转人工"]}`)
	miss := &IncomingMessage{Platform: "whatsapp", UserID: cs.UserID, Content: "今天天气怎么样"}
	if svc.handoffGate(context.Background(), channel, cs, session, miss, adapter) {
		t.Fatal("non-matching message must not consume messages")
	}

	if len(adapter.sent) != 0 || len(msgSvc.created) != 0 {
		t.Fatalf("no-trigger paths must stay silent, got sent=%d created=%d", len(adapter.sent), len(msgSvc.created))
	}
	row := reloadChannelSession(t, svc, cs.ID)
	if row.HandlingMode != HandlingModeBot {
		t.Fatalf("handling mode = %s, want untouched bot mode", row.HandlingMode)
	}
}

func TestNoteBotAnswerOutcomeFallbackStreak(t *testing.T) {
	svc, adapter, msgSvc, channel := newManualReplyFixture(t)
	rec, srv := newHandoffWebhookRecorder()
	defer srv.Close()
	setHandoffConfig(t, svc, channel,
		`{"enabled":true,"fallback_threshold":2,"webhook_url":"`+srv.URL+`","webhook_format":"wecom"}`)

	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)
	session := &types.Session{ID: cs.SessionID, TenantID: 1}
	msg := &IncomingMessage{Platform: "whatsapp", UserID: cs.UserID, Content: "订单怎么退款"}

	// First failure: streak advances, no trigger yet.
	svc.noteBotAnswerOutcome(context.Background(), channel, session, msg, adapter, true)
	row := reloadChannelSession(t, svc, cs.ID)
	if row.ConsecutiveFailures != 1 || row.HandlingMode != HandlingModeBot {
		t.Fatalf("after one failure: failures=%d mode=%s, want streak 1 in bot mode", row.ConsecutiveFailures, row.HandlingMode)
	}
	if len(adapter.sent) != 0 {
		t.Fatal("no auto-reply may be sent below the threshold")
	}

	// A successful answer resets the streak.
	svc.noteBotAnswerOutcome(context.Background(), channel, session, msg, adapter, false)
	if row = reloadChannelSession(t, svc, cs.ID); row.ConsecutiveFailures != 0 {
		t.Fatalf("streak after success = %d, want 0", row.ConsecutiveFailures)
	}

	// Two straight failures hit the threshold and hand the conversation off.
	svc.noteBotAnswerOutcome(context.Background(), channel, session, msg, adapter, true)
	svc.noteBotAnswerOutcome(context.Background(), channel, session, msg, adapter, true)
	row = reloadChannelSession(t, svc, cs.ID)
	if row.HandlingMode != HandlingModeHuman || row.ConsecutiveFailures != 0 || row.HandoffNotifiedAt == nil {
		t.Fatalf("after threshold: mode=%s failures=%d notified=%v, want human mode with reset streak",
			row.HandlingMode, row.ConsecutiveFailures, row.HandoffNotifiedAt)
	}
	if len(adapter.sent) != 1 || !strings.Contains(adapter.sent[0].reply.Content, "人工") {
		t.Fatalf("auto-reply = %+v, want the default handoff notice", adapter.sent)
	}
	if len(msgSvc.created) != 1 || msgSvc.created[0].Role != "assistant" {
		t.Fatalf("persisted = %+v, want just the assistant notice (QA already recorded the exchange)", msgSvc.created)
	}

	// WeCom-format webhook wraps the operator text.
	var wecom struct {
		MsgType string `json:"msgtype"`
		Text    struct {
			Content string `json:"content"`
		} `json:"text"`
	}
	if err := json.Unmarshal(rec.waitOne(t), &wecom); err != nil {
		t.Fatalf("decode wecom payload: %v", err)
	}
	if wecom.MsgType != "text" || !strings.Contains(wecom.Text.Content, "连续 2 条消息未能回答") {
		t.Fatalf("wecom payload = %+v, want text with the failure count", wecom)
	}
}

func TestTriggerHandoffCooldownAndUnsupportedPlatform(t *testing.T) {
	svc, adapter, msgSvc, channel := newManualReplyFixture(t)
	rec, srv := newHandoffWebhookRecorder()
	defer srv.Close()
	setHandoffConfig(t, svc, channel,
		`{"enabled":true,"keywords":["转人工"],"webhook_url":"`+srv.URL+`"}`)

	// Recently-notified conversation: the trigger still switches state but
	// stays quiet on both the IM side and the webhook.
	recent := time.Now().Add(-time.Minute)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
		HandoffNotifiedAt: &recent,
	}
	createManualReplySession(t, svc.db, cs)
	session := &types.Session{ID: cs.SessionID, TenantID: 1}
	msg := &IncomingMessage{Platform: "whatsapp", UserID: cs.UserID, Content: "转人工"}
	if !svc.handoffGate(context.Background(), channel, cs, session, msg, adapter) {
		t.Fatal("keyword message must still be consumed during the cooldown")
	}
	row := reloadChannelSession(t, svc, cs.ID)
	if row.HandlingMode != HandlingModeHuman {
		t.Fatal("cooldown must not prevent the takeover switch")
	}
	if len(adapter.sent) != 0 || rec.count() != 0 {
		t.Fatalf("cooldown must suppress reply+webhook, got sent=%d webhooks=%d", len(adapter.sent), rec.count())
	}
	// The trigger message itself is still recorded for the console.
	if len(msgSvc.created) != 1 || msgSvc.created[0].Channel != types.ChannelIMTakeover {
		t.Fatalf("persisted during cooldown = %+v, want just the trigger message", msgSvc.created)
	}

	// Platform without console replies (telegram): notify + auto-reply, but
	// never silence a bot the operator cannot stand in for.
	tg := &ChannelSession{
		Platform: "telegram", UserID: "42",
		SessionID: "session-tg", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, tg)
	tgSession := &types.Session{ID: tg.SessionID, TenantID: 1}
	tgMsg := &IncomingMessage{Platform: "telegram", UserID: tg.UserID, Content: "转人工"}
	if !svc.handoffGate(context.Background(), channel, tg, tgSession, tgMsg, adapter) {
		t.Fatal("keyword message must be consumed on unsupported platforms too")
	}
	tgRow := reloadChannelSession(t, svc, tg.ID)
	if tgRow.HandlingMode != HandlingModeBot || tgRow.HandoffNotifiedAt == nil {
		t.Fatalf("telegram row = mode=%s notified=%v, want bot mode with stamped notification",
			tgRow.HandlingMode, tgRow.HandoffNotifiedAt)
	}
	if len(adapter.sent) != 1 {
		t.Fatalf("telegram trigger deliveries = %d, want the auto-reply", len(adapter.sent))
	}
	rec.waitOne(t)
}

func TestBuildHandoffWebhookBodyFormats(t *testing.T) {
	n := HandoffNotification{
		Event: "im.handoff_requested", Reason: HandoffReasonKeyword, Keyword: "转人工",
		Platform: "whatsapp", ChannelName: "售后", UserID: "861380000", Message: "转人工",
	}
	cases := map[string]string{
		HandoffWebhookWeCom:    `"msgtype":"text"`,
		HandoffWebhookDingTalk: `"msgtype":"text"`,
		HandoffWebhookFeishu:   `"msg_type":"text"`,
		HandoffWebhookSlack:    `"text":"`,
		HandoffWebhookGeneric:  `"event":"im.handoff_requested"`,
	}
	for format, marker := range cases {
		body, err := buildHandoffWebhookBody(format, n)
		if err != nil {
			t.Fatalf("build %s payload: %v", format, err)
		}
		if !strings.Contains(string(body), marker) {
			t.Fatalf("%s payload %s lacks marker %s", format, body, marker)
		}
		if format != HandoffWebhookGeneric && !strings.Contains(string(body), "转人工") {
			t.Fatalf("%s payload must embed the keyword, got %s", format, body)
		}
	}
}
