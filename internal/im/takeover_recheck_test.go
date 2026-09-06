package im

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// A WhatsApp number that any channel ever carried — live or deleted — has an
// owner; a bare device_jid pointing at it must not be claimable elsewhere.
func TestWhatsAppNumberOwner(t *testing.T) {
	svc, _, _, channel := newManualReplyFixture(t)

	owner, found, err := svc.WhatsAppNumberOwner(context.Background(), "8613800138000:99@s.whatsapp.net")
	if err != nil || !found || owner != channel.TenantID {
		t.Fatalf("live channel: owner=%d found=%v err=%v, want tenant %d", owner, found, err, channel.TenantID)
	}
	if _, found, err := svc.WhatsAppNumberOwner(context.Background(), "8600000000000:1@s.whatsapp.net"); err != nil || found {
		t.Fatalf("unknown number: found=%v err=%v, want not found", found, err)
	}
	if _, found, _ := svc.WhatsAppNumberOwner(context.Background(), ""); found {
		t.Fatal("empty jid must not resolve to an owner")
	}

	// Soft-deleting the channel keeps the number attributed.
	if err := svc.db.Delete(&IMChannel{}, "id = ?", channel.ID).Error; err != nil {
		t.Fatalf("soft delete channel: %v", err)
	}
	owner, found, err = svc.WhatsAppNumberOwner(context.Background(), "8613800138000:12@s.whatsapp.net")
	if err != nil || !found || owner != channel.TenantID {
		t.Fatalf("deleted channel: owner=%d found=%v err=%v, want tenant %d", owner, found, err, channel.TenantID)
	}
}

func TestHumanHoldsSessionReadsTheRow(t *testing.T) {
	svc, _, _, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-hold", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)

	if svc.humanHoldsSession(context.Background(), "session-hold") {
		t.Fatal("bot mode must not count as held")
	}
	if _, err := svc.SetSessionHandling(context.Background(), 1, "session-hold", "human", 0); err != nil {
		t.Fatal(err)
	}
	if !svc.humanHoldsSession(context.Background(), "session-hold") {
		t.Fatal("indefinite takeover must count as held")
	}
	expired := time.Now().Add(-time.Minute)
	if err := svc.db.Model(&ChannelSession{}).Where("id = ?", cs.ID).
		Update("handling_expires_at", expired).Error; err != nil {
		t.Fatal(err)
	}
	if svc.humanHoldsSession(context.Background(), "session-hold") {
		t.Fatal("expired takeover must not count as held")
	}
	if svc.humanHoldsSession(context.Background(), "") {
		t.Fatal("empty session id must not count as held")
	}
}

// Taking a conversation over must stop the turn already running for that
// peer, and a turn that reaches the worker afterwards must be silenced.
func TestTakeoverCancelsInFlightAndRechecksAtWorker(t *testing.T) {
	svc, _, msgSvc, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-inflight", TenantID: 1, AgentID: channel.AgentID, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)

	key := makeUserKey(channel.ID, cs.UserID, cs.ChatID, "")
	cancelled := false
	svc.inflight.Store(key, &inflightEntry{cancel: func() { cancelled = true }})

	if _, err := svc.SetSessionHandling(context.Background(), 1, "session-inflight", "human", 0); err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("takeover must cancel the peer's in-flight QA")
	}
	if _, still := svc.inflight.Load(key); still {
		t.Fatal("cancelled in-flight entry must be removed")
	}

	// A queued turn that starts after the takeover: the worker re-reads the
	// row, records the message for the console, and skips QA.
	req := &qaRequest{
		ctx:            context.Background(),
		msg:            &IncomingMessage{Platform: PlatformWhatsApp, UserID: cs.UserID, Content: "还在吗"},
		session:        &types.Session{ID: cs.SessionID},
		channel:        channel,
		channelSession: &ChannelSession{ID: cs.ID, HandlingMode: HandlingModeBot}, // stale enqueue-time copy
	}
	if !svc.recheckTakeover(context.Background(), req) {
		t.Fatal("worker recheck must silence the bot after a takeover")
	}
	msgSvc.mu.Lock()
	recorded := len(msgSvc.created)
	var last *types.Message
	if recorded > 0 {
		last = msgSvc.created[recorded-1]
	}
	msgSvc.mu.Unlock()
	if last == nil || last.Role != "user" || last.Channel != types.ChannelIMTakeover || last.Content != "还在吗" {
		t.Fatalf("recheck must record the user message as a takeover note, got %+v", last)
	}

	// Released back to the bot: the recheck lets the turn through.
	if _, err := svc.SetSessionHandling(context.Background(), 1, "session-inflight", "bot", 0); err != nil {
		t.Fatal(err)
	}
	if svc.recheckTakeover(context.Background(), req) {
		t.Fatal("bot mode must let the turn run")
	}
}
