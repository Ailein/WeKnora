package whatsapp

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"

	"github.com/Tencent/WeKnora/internal/im"
)

func TestReplyTarget(t *testing.T) {
	a := &Adapter{}

	// The full JID in Extra wins over everything else.
	in := &im.IncomingMessage{
		ChatType: im.ChatTypeGroup,
		ChatID:   "999@g.us",
		UserID:   "111",
		Extra:    map[string]string{extraChatJID: "123-456@g.us"},
	}
	jid, err := a.replyTarget(in)
	if err != nil || jid.String() != "123-456@g.us" {
		t.Errorf("extra target = %s, err %v", jid, err)
	}

	// Group fallback: ChatID.
	jid, err = a.replyTarget(&im.IncomingMessage{ChatType: im.ChatTypeGroup, ChatID: "999@g.us"})
	if err != nil || jid.String() != "999@g.us" {
		t.Errorf("group target = %s, err %v", jid, err)
	}

	// DM fallback: bare phone number on the default server.
	jid, err = a.replyTarget(&im.IncomingMessage{ChatType: im.ChatTypeDirect, UserID: testPeerPhone})
	if err != nil || jid != types.NewJID(testPeerPhone, types.DefaultUserServer) {
		t.Errorf("dm target = %s, err %v", jid, err)
	}

	if _, err := a.replyTarget(&im.IncomingMessage{}); err == nil {
		t.Error("empty message should not resolve a target")
	}
}

func TestBuildQuotedReply(t *testing.T) {
	a := &Adapter{}

	in := &im.IncomingMessage{
		MessageID: "M-1",
		Content:   strings.Repeat("问", quotePreviewLimit+30),
		Extra:     map[string]string{extraSenderJID: testPeerPhone + "@s.whatsapp.net"},
	}
	msg := a.buildQuotedReply("the answer", in)
	ext := msg.GetExtendedTextMessage()
	if ext == nil {
		t.Fatal("expected an extended message carrying the quote")
	}
	if ext.GetText() != "the answer" {
		t.Errorf("text = %q", ext.GetText())
	}
	ci := ext.GetContextInfo()
	if ci.GetStanzaID() != "M-1" || ci.GetParticipant() != testPeerPhone+"@s.whatsapp.net" {
		t.Errorf("contextInfo = %+v", ci)
	}
	preview := ci.GetQuotedMessage().GetConversation()
	if got := len([]rune(preview)); got != quotePreviewLimit+1 { // limit runes + "…"
		t.Errorf("preview length = %d runes, want %d", got, quotePreviewLimit+1)
	}
	if !strings.HasSuffix(preview, "…") {
		t.Errorf("preview not truncated with ellipsis: %q", preview)
	}

	// Without the fields needed for a quote, fall back to a plain message.
	plain := a.buildQuotedReply("seg", &im.IncomingMessage{Content: "q"})
	if plain.GetExtendedTextMessage() != nil || plain.GetConversation() != "seg" {
		t.Errorf("fallback = %+v", plain)
	}
}
