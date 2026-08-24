package im

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func reloadChannelSession(t *testing.T, svc *Service, id string) *ChannelSession {
	t.Helper()
	var cs ChannelSession
	if err := svc.db.Unscoped().Where("id = ?", id).First(&cs).Error; err != nil {
		t.Fatalf("reload channel session %s: %v", id, err)
	}
	return &cs
}

func TestSetSessionHandlingLifecycle(t *testing.T) {
	svc, _, _, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)

	// Omitted timeout (negative sentinel) selects the default window.
	h, err := svc.SetSessionHandling(context.Background(), 1, "session-dm", "human", -1)
	if err != nil {
		t.Fatalf("takeover with default window: %v", err)
	}
	if h.Mode != HandlingModeHuman || h.TimeoutMinutes != defaultTakeoverTimeoutMinutes || h.ExpiresAt == nil {
		t.Fatalf("handling = %+v, want human mode with default %d-minute window", h, defaultTakeoverTimeoutMinutes)
	}
	wantExpiry := time.Now().Add(defaultTakeoverTimeoutMinutes * time.Minute)
	if h.ExpiresAt.Before(wantExpiry.Add(-time.Minute)) || h.ExpiresAt.After(wantExpiry.Add(time.Minute)) {
		t.Fatalf("expiry = %v, want ≈ %v", h.ExpiresAt, wantExpiry)
	}

	// Explicit window.
	h, err = svc.SetSessionHandling(context.Background(), 1, "session-dm", "human", 30)
	if err != nil {
		t.Fatalf("takeover with 30-minute window: %v", err)
	}
	if h.TimeoutMinutes != 30 || h.ExpiresAt == nil {
		t.Fatalf("handling = %+v, want 30-minute window", h)
	}

	// Zero = takeover with no expiry.
	h, err = svc.SetSessionHandling(context.Background(), 1, "session-dm", "human", 0)
	if err != nil {
		t.Fatalf("indefinite takeover: %v", err)
	}
	if h.Mode != HandlingModeHuman || h.ExpiresAt != nil || h.TimeoutMinutes != 0 {
		t.Fatalf("handling = %+v, want indefinite human mode", h)
	}

	// Release back to the bot clears every takeover field.
	h, err = svc.SetSessionHandling(context.Background(), 1, "session-dm", "bot", -1)
	if err != nil {
		t.Fatalf("release takeover: %v", err)
	}
	if h.Mode != HandlingModeBot || h.ExpiresAt != nil || h.TimeoutMinutes != 0 {
		t.Fatalf("handling after release = %+v, want clean bot mode", h)
	}
	row := reloadChannelSession(t, svc, cs.ID)
	if row.HandlingMode != HandlingModeBot || row.HandlingExpiresAt != nil || row.HandlingTimeoutMinutes != 0 {
		t.Fatalf("stored row after release = %+v, want clean bot mode", row)
	}

	// GetSessionHandling mirrors the stored state.
	got, err := svc.GetSessionHandling(context.Background(), 1, "session-dm")
	if err != nil {
		t.Fatalf("GetSessionHandling: %v", err)
	}
	if got.Mode != HandlingModeBot || got.Platform != "whatsapp" || got.SessionID != "session-dm" {
		t.Fatalf("GetSessionHandling = %+v, want bot mode on session-dm", got)
	}
}

func TestSetSessionHandlingValidation(t *testing.T) {
	svc, _, _, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)

	if _, err := svc.SetSessionHandling(context.Background(), 1, "session-dm", "robot", -1); !errors.Is(err, ErrHandlingInvalidMode) {
		t.Fatalf("bad mode error = %v, want ErrHandlingInvalidMode", err)
	}
	for _, bad := range []int{minTakeoverTimeoutMinutes - 1, maxTakeoverTimeoutMinutes + 1} {
		if _, err := svc.SetSessionHandling(context.Background(), 1, "session-dm", "human", bad); !errors.Is(err, ErrHandlingInvalidTimeout) {
			t.Fatalf("timeout %d error = %v, want ErrHandlingInvalidTimeout", bad, err)
		}
	}
	if _, err := svc.SetSessionHandling(context.Background(), 1, "web-session", "human", -1); !errors.Is(err, ErrHandlingNotIMSession) {
		t.Fatalf("no-mapping error = %v, want ErrHandlingNotIMSession", err)
	}
	if _, err := svc.SetSessionHandling(context.Background(), 2, "session-dm", "human", -1); !errors.Is(err, ErrHandlingNotIMSession) {
		t.Fatalf("cross-tenant error = %v, want ErrHandlingNotIMSession", err)
	}

	tg := &ChannelSession{
		Platform: "telegram", UserID: "42",
		SessionID: "session-tg", TenantID: 1, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, tg)
	if _, err := svc.SetSessionHandling(context.Background(), 1, "session-tg", "human", -1); !errors.Is(err, ErrHandlingUnsupported) {
		t.Fatalf("unsupported-platform error = %v, want ErrHandlingUnsupported", err)
	}
	if _, err := svc.GetSessionHandling(context.Background(), 1, "session-tg"); !errors.Is(err, ErrHandlingUnsupported) {
		t.Fatalf("unsupported-platform get error = %v, want ErrHandlingUnsupported", err)
	}
}

func TestSetSessionHandlingFollowsLiveMapping(t *testing.T) {
	svc, _, _, channel := newManualReplyFixture(t)

	// Historical mapping soft-deleted by /clear...
	old := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-old", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, old)
	if err := svc.db.Delete(&ChannelSession{}, "id = ?", old.ID).Error; err != nil {
		t.Fatalf("soft-delete mapping: %v", err)
	}

	// ...with no live successor yet: nothing to take over.
	if _, err := svc.SetSessionHandling(context.Background(), 1, "session-old", "human", -1); !errors.Is(err, ErrHandlingConversationGone) {
		t.Fatalf("no-successor error = %v, want ErrHandlingConversationGone", err)
	}

	// Once the peer re-messages, a live successor exists; acting from the
	// historical session must land the takeover on the live conversation.
	live := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-live", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, live)

	h, err := svc.SetSessionHandling(context.Background(), 1, "session-old", "human", 30)
	if err != nil {
		t.Fatalf("takeover via historical session: %v", err)
	}
	if h.SessionID != "session-live" {
		t.Fatalf("handling applied to session %q, want live successor session-live", h.SessionID)
	}
	if row := reloadChannelSession(t, svc, live.ID); row.HandlingMode != HandlingModeHuman {
		t.Fatalf("live row mode = %q, want human", row.HandlingMode)
	}
	if row := reloadChannelSession(t, svc, old.ID); row.HandlingMode == HandlingModeHuman {
		t.Fatal("historical row must not carry the takeover state")
	}
}

func TestTakeoverGateSilencesAndRecords(t *testing.T) {
	svc, _, msgSvc, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, IMChannelID: channel.ID,
		HandlingMode: HandlingModeHuman,
	}
	createManualReplySession(t, svc.db, cs)

	msg := &IncomingMessage{Platform: PlatformWhatsApp, UserID: cs.UserID, Content: "人呢？"}
	if !svc.takeoverGate(context.Background(), cs, &types.Session{ID: "session-dm"}, msg) {
		t.Fatal("takeoverGate = false in human mode, want silenced")
	}

	if len(msgSvc.created) != 1 {
		t.Fatalf("recorded messages = %d, want 1", len(msgSvc.created))
	}
	rec := msgSvc.created[0]
	if rec.Role != "user" || rec.Channel != types.ChannelIMTakeover || !rec.IsCompleted ||
		rec.Content != "人呢？" || rec.SessionID != "session-dm" || rec.RequestID == "" {
		t.Fatalf("recorded message = %+v, want completed user message on channel %q", rec, types.ChannelIMTakeover)
	}

	// The conversation must resurface for the operator.
	var session types.Session
	if err := svc.db.Where("id = ?", "session-dm").First(&session).Error; err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if time.Since(session.UpdatedAt) > time.Minute {
		t.Fatalf("session updated_at was not bumped: %v", session.UpdatedAt)
	}

	// Whitespace-only content is silenced without recording an empty row.
	blank := &IncomingMessage{Platform: PlatformWhatsApp, UserID: cs.UserID, Content: "   "}
	if !svc.takeoverGate(context.Background(), cs, &types.Session{ID: "session-dm"}, blank) {
		t.Fatal("takeoverGate = false for blank content, want silenced")
	}
	if len(msgSvc.created) != 1 {
		t.Fatalf("blank content recorded a message: %d rows", len(msgSvc.created))
	}

	// Voice messages carry no text during a takeover (transcription is
	// skipped); a placeholder row keeps the console aware the user spoke.
	voice := &IncomingMessage{Platform: PlatformWhatsApp, UserID: cs.UserID, MessageType: MessageTypeVoice}
	if !svc.takeoverGate(context.Background(), cs, &types.Session{ID: "session-dm"}, voice) {
		t.Fatal("takeoverGate = false for voice message, want silenced")
	}
	if len(msgSvc.created) != 2 || msgSvc.created[1].Content != "[语音消息]" {
		t.Fatalf("voice placeholder rows = %+v, want a [语音消息] row", msgSvc.created)
	}
}

func TestTakeoverGateExpiryResumesBot(t *testing.T) {
	svc, _, msgSvc, channel := newManualReplyFixture(t)
	expired := time.Now().Add(-time.Minute)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, IMChannelID: channel.ID,
		HandlingMode: HandlingModeHuman, HandlingExpiresAt: &expired, HandlingTimeoutMinutes: 30,
	}
	createManualReplySession(t, svc.db, cs)

	msg := &IncomingMessage{Platform: PlatformWhatsApp, UserID: cs.UserID, Content: "早上好"}
	if svc.takeoverGate(context.Background(), cs, &types.Session{ID: "session-dm"}, msg) {
		t.Fatal("takeoverGate = true after expiry, want bot resumed")
	}
	if len(msgSvc.created) != 0 {
		t.Fatalf("expired takeover recorded %d messages, want 0", len(msgSvc.created))
	}
	if cs.HandlingMode != HandlingModeBot {
		t.Fatalf("in-memory mode = %q, want bot", cs.HandlingMode)
	}
	row := reloadChannelSession(t, svc, cs.ID)
	if row.HandlingMode != HandlingModeBot || row.HandlingExpiresAt != nil || row.HandlingTimeoutMinutes != 0 {
		t.Fatalf("stored row after expiry = %+v, want clean bot mode", row)
	}
}

func TestTakeoverGateBotModePassesThrough(t *testing.T) {
	svc, _, msgSvc, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)

	msg := &IncomingMessage{Platform: PlatformWhatsApp, UserID: cs.UserID, Content: "你好"}
	if svc.takeoverGate(context.Background(), cs, &types.Session{ID: "session-dm"}, msg) {
		t.Fatal("takeoverGate = true in bot mode, want pass-through")
	}
	if len(msgSvc.created) != 0 {
		t.Fatalf("bot mode recorded %d messages, want 0", len(msgSvc.created))
	}
}

func TestManualReplyExtendsTakeoverWindow(t *testing.T) {
	svc, _, _, channel := newManualReplyFixture(t)
	soon := time.Now().Add(time.Minute)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
		HandlingMode: HandlingModeHuman, HandlingExpiresAt: &soon, HandlingTimeoutMinutes: 30,
	}
	createManualReplySession(t, svc.db, cs)

	if _, err := svc.SendManualReply(context.Background(), 1, "session-dm", "我在，请讲", nil); err != nil {
		t.Fatalf("SendManualReply: %v", err)
	}

	row := reloadChannelSession(t, svc, cs.ID)
	if row.HandlingExpiresAt == nil {
		t.Fatal("expiry cleared by manual reply, want refreshed window")
	}
	wantExpiry := time.Now().Add(30 * time.Minute)
	if row.HandlingExpiresAt.Before(wantExpiry.Add(-time.Minute)) || row.HandlingExpiresAt.After(wantExpiry.Add(time.Minute)) {
		t.Fatalf("expiry after reply = %v, want ≈ %v (refreshed 30-minute window)", row.HandlingExpiresAt, wantExpiry)
	}

	// An indefinite takeover has no window to refresh and must stay indefinite.
	if err := svc.db.Model(&ChannelSession{}).Where("id = ?", cs.ID).Updates(map[string]interface{}{
		"handling_expires_at": nil, "handling_timeout_minutes": 0,
	}).Error; err != nil {
		t.Fatalf("reset to indefinite: %v", err)
	}
	if _, err := svc.SendManualReply(context.Background(), 1, "session-dm", "还有别的问题吗", nil); err != nil {
		t.Fatalf("SendManualReply (indefinite): %v", err)
	}
	if row := reloadChannelSession(t, svc, cs.ID); row.HandlingExpiresAt != nil {
		t.Fatalf("indefinite takeover gained an expiry: %v", row.HandlingExpiresAt)
	}
}

// HandleMessage consults the takeover gate before the empty-message hint: while
// an operator holds the conversation the bot must stay silent even for messages
// the QA pipeline could never answer, or the customer sees a bot reply in the
// middle of a human conversation.
func TestTakeoverSilencesUnanswerableMessage(t *testing.T) {
	svc, _, msgSvc, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, IMChannelID: channel.ID,
		HandlingMode: HandlingModeHuman,
	}
	createManualReplySession(t, svc.db, cs)

	// A rich message the platform could not normalize into text; outside a
	// takeover this shape draws the "unsupported message" hint from the bot.
	msg := &IncomingMessage{
		Platform: PlatformWhatsApp, UserID: cs.UserID,
		Extra: map[string]string{"raw_msgtype": "video"},
	}
	if _, empty := emptyIncomingMessageReply(msg); !empty {
		t.Fatal("fixture no longer represents an unanswerable message")
	}
	if !svc.takeoverGate(context.Background(), cs, &types.Session{ID: "session-dm"}, msg) {
		t.Fatal("takeoverGate = false for an unanswerable message, want silenced")
	}
	if len(msgSvc.created) != 0 {
		t.Fatalf("contentless message recorded %d rows, want 0", len(msgSvc.created))
	}
}
