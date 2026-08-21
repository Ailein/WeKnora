package whatsapp

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Tencent/WeKnora/internal/im"
)

// Test identities. The linked account carries both a phone-number JID and a
// LID identity, like a real companion device.
const (
	testSelfPhone = "8613800138000"
	testSelfLID   = "99887766554433"
	testPeerPhone = "15551234567"
)

// emptyLIDStore is a LID→PN store with no mappings, standing in for a fresh
// session that has not synced any from history yet.
type emptyLIDStore struct{}

func (emptyLIDStore) PutManyLIDMappings(context.Context, []store.LIDMapping) error { return nil }
func (emptyLIDStore) PutLIDMapping(context.Context, types.JID, types.JID) error    { return nil }
func (emptyLIDStore) GetPNForLID(context.Context, types.JID) (types.JID, error) {
	return types.JID{}, nil
}
func (emptyLIDStore) GetLIDForPN(context.Context, types.JID) (types.JID, error) {
	return types.JID{}, nil
}
func (emptyLIDStore) GetManyLIDsForPNs(context.Context, []types.JID) (map[types.JID]types.JID, error) {
	return nil, nil
}

// newOfflineClient builds a whatsmeow client that never connects; the message
// pipeline and the pairing watcher only read Store.ID / Store.LID / Store.LIDs.
func newOfflineClient(t *testing.T) *whatsmeow.Client {
	t.Helper()
	self := types.NewJID(testSelfPhone, types.DefaultUserServer)
	self.Device = 12
	device := &store.Device{
		ID:   &self,
		LID:  types.NewJID(testSelfLID, types.HiddenUserServer),
		LIDs: emptyLIDStore{},
	}
	return whatsmeow.NewClient(device, nil)
}

func newOfflineAdapter(t *testing.T, allowFrom string) *Adapter {
	t.Helper()
	return newAdapter("ch-test", newOfflineClient(t), parseAllowFrom(allowFrom))
}

func msgEvent(chat, sender types.JID, msg *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender},
			ID:            "MSG-1",
			PushName:      "Alice",
			Timestamp:     time.Now(),
		},
		Message: msg,
	}
}

func textMsg(s string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(s)}
}

// parseMessage is the security gate of the channel: everything that must not
// reach QA (and spend tokens) has to be dropped here.
func TestParseMessageDrops(t *testing.T) {
	a := newOfflineAdapter(t, "*")
	ctx := context.Background()
	peer := types.NewJID(testPeerPhone, types.DefaultUserServer)

	fromMe := msgEvent(peer, peer, textMsg("hi"))
	fromMe.Info.IsFromMe = true

	stale := msgEvent(peer, peer, textMsg("old"))
	stale.Info.Timestamp = time.Now().Add(-historyGrace - time.Minute)

	cases := []struct {
		name string
		evt  *events.Message
	}{
		{"own message", fromMe},
		{"nil payload", msgEvent(peer, peer, nil)},
		{"status broadcast", msgEvent(types.NewJID("status", types.BroadcastServer), peer, textMsg("story"))},
		{"newsletter", msgEvent(types.NewJID("120363000000000009", types.NewsletterServer), peer, textMsg("post"))},
		{"history replay", stale},
		{"no text or media", msgEvent(peer, peer, &waE2E.Message{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.parseMessage(ctx, tc.evt); got != nil {
				t.Errorf("parseMessage = %+v, want nil", got)
			}
		})
	}

	// Genuine offline catch-up inside the grace window must survive.
	recent := msgEvent(peer, peer, textMsg("catch-up"))
	recent.Info.Timestamp = time.Now().Add(-historyGrace + time.Minute)
	if a.parseMessage(ctx, recent) == nil {
		t.Error("message inside the history grace window was dropped")
	}
}

func TestParseMessageDMAllowlistAndFields(t *testing.T) {
	ctx := context.Background()
	peer := types.NewJID(testPeerPhone, types.DefaultUserServer)

	// allow_from entries are normalized, so a formatted number matches the
	// bare-number JID user.
	a := newOfflineAdapter(t, "+1 555-123-4567")
	in := a.parseMessage(ctx, msgEvent(peer, peer, textMsg("hello")))
	if in == nil {
		t.Fatal("allowlisted DM was dropped")
	}
	if in.Platform != im.PlatformWhatsApp || in.MessageType != im.MessageTypeText {
		t.Errorf("platform/type = %s/%s", in.Platform, in.MessageType)
	}
	if in.UserID != testPeerPhone || in.UserName != "Alice" || in.Content != "hello" || in.MessageID != "MSG-1" {
		t.Errorf("fields = %+v", in)
	}
	if in.ChatType != im.ChatTypeDirect || in.ChatID != "" {
		t.Errorf("chatType/chatID = %s/%q", in.ChatType, in.ChatID)
	}
	// SendReply resolves the reply target from Extra, not from UserID.
	if in.Extra[extraChatJID] != peer.String() || in.Extra[extraSenderJID] != peer.String() {
		t.Errorf("extra = %+v", in.Extra)
	}

	stranger := types.NewJID("12025550000", types.DefaultUserServer)
	if got := a.parseMessage(ctx, msgEvent(stranger, stranger, textMsg("hey"))); got != nil {
		t.Errorf("stranger DM passed the allowlist: %+v", got)
	}
}

// LID (hidden-user) senders resolve to a phone number via SenderAlt when
// present; without a mapping the DM must fail closed unless allow_from="*".
func TestParseMessageDMFromLIDSender(t *testing.T) {
	ctx := context.Background()
	lid := types.NewJID("777000111222", types.HiddenUserServer)

	a := newOfflineAdapter(t, testPeerPhone)

	withAlt := msgEvent(lid, lid, textMsg("via lid"))
	withAlt.Info.SenderAlt = types.NewJID(testPeerPhone, types.DefaultUserServer)
	in := a.parseMessage(ctx, withAlt)
	if in == nil || in.UserID != testPeerPhone {
		t.Fatalf("LID with SenderAlt: got %+v, want UserID %s", in, testPeerPhone)
	}

	if got := a.parseMessage(ctx, msgEvent(lid, lid, textMsg("via lid"))); got != nil {
		t.Errorf("unresolved LID must fail closed, got %+v", got)
	}

	open := newOfflineAdapter(t, "*")
	if got := open.parseMessage(ctx, msgEvent(lid, lid, textMsg("via lid"))); got == nil {
		t.Error(`allow_from="*" must admit unresolved LID senders`)
	}
}

func TestParseMessageGroupTrigger(t *testing.T) {
	ctx := context.Background()
	// The DM allowlist must not gate group messages: triggering is by mention.
	a := newOfflineAdapter(t, "")
	group := types.NewJID("120363000000000001", types.GroupServer)
	peer := types.NewJID(testPeerPhone, types.DefaultUserServer)

	mention := func(target, text string) *waE2E.Message {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{target}},
		}}
	}

	if got := a.parseMessage(ctx, msgEvent(group, peer, textMsg("just chatting"))); got != nil {
		t.Errorf("group message without mention passed: %+v", got)
	}
	if got := a.parseMessage(ctx, msgEvent(group, peer,
		mention("12025550000@s.whatsapp.net", "@12025550000 hi"))); got != nil {
		t.Errorf("mention of someone else passed: %+v", got)
	}

	in := a.parseMessage(ctx, msgEvent(group, peer,
		mention(testSelfPhone+"@s.whatsapp.net", "@"+testSelfPhone+" what is weknora?")))
	if in == nil {
		t.Fatal("mention of the bot was dropped")
	}
	if in.ChatType != im.ChatTypeGroup || in.ChatID != group.String() {
		t.Errorf("chatType/chatID = %s/%q", in.ChatType, in.ChatID)
	}
	if in.Content != "what is weknora?" {
		t.Errorf("self mention not stripped: %q", in.Content)
	}

	// Mentions may use the account's LID identity instead of the phone number.
	if got := a.parseMessage(ctx, msgEvent(group, peer,
		mention(testSelfLID+"@lid", "@"+testSelfLID+" ping"))); got == nil || got.Content != "ping" {
		t.Errorf("LID mention: got %+v", got)
	}

	reply := func(participant string) *waE2E.Message {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("and this?"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String("PREV-1"),
				Participant:   proto.String(participant),
				QuotedMessage: &waE2E.Message{Conversation: proto.String("earlier answer")},
			},
		}}
	}

	in = a.parseMessage(ctx, msgEvent(group, peer, reply(testSelfPhone+":12@s.whatsapp.net")))
	if in == nil {
		t.Fatal("reply to the bot's message was dropped")
	}
	if in.Quote == nil || !in.Quote.IsBotMessage || in.Quote.Content != "earlier answer" {
		t.Errorf("quote = %+v", in.Quote)
	}

	if got := a.parseMessage(ctx, msgEvent(group, peer, reply("12025550000@s.whatsapp.net"))); got != nil {
		t.Errorf("reply to another member passed: %+v", got)
	}
}

func TestParseMessageMedia(t *testing.T) {
	ctx := context.Background()
	a := newOfflineAdapter(t, "*")
	peer := types.NewJID(testPeerPhone, types.DefaultUserServer)

	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:    proto.String("look at this"),
		FileLength: proto.Uint64(2048),
	}}
	in := a.parseMessage(ctx, msgEvent(peer, peer, img))
	if in == nil {
		t.Fatal("image message dropped")
	}
	if in.MessageType != im.MessageTypeImage || in.Content != "look at this" ||
		in.FileName != "photo.jpg" || in.FileSize != 2048 {
		t.Errorf("image fields = %+v", in)
	}
	cached, ok := a.media.get(in.FileKey)
	if !ok || cached.msg != img {
		t.Error("image message not cached for DownloadFile")
	}

	docEvt := msgEvent(peer, peer, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		FileName:   proto.String("report.pdf"),
		FileLength: proto.Uint64(4096),
	}})
	docEvt.Info.ID = "MSG-2"
	in = a.parseMessage(ctx, docEvt)
	if in == nil {
		t.Fatal("document message dropped")
	}
	if in.MessageType != im.MessageTypeFile || in.FileName != "report.pdf" || in.FileSize != 4096 {
		t.Errorf("document fields = %+v", in)
	}
	if _, ok := a.media.get(in.FileKey); !ok {
		t.Error("document message not cached for DownloadFile")
	}
}

func TestParseQuoteNonText(t *testing.T) {
	ctx := context.Background()
	a := newOfflineAdapter(t, "*")
	peer := types.NewJID(testPeerPhone, types.DefaultUserServer)

	evt := msgEvent(peer, peer, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("what is in this picture?"),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID:      proto.String("IMG-1"),
			Participant:   proto.String(testPeerPhone + "@s.whatsapp.net"),
			QuotedMessage: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}},
		},
	}})
	in := a.parseMessage(ctx, evt)
	if in == nil {
		t.Fatal("message with quote dropped")
	}
	if in.Quote == nil {
		t.Fatal("quote missing")
	}
	if in.Quote.MessageID != "IMG-1" || in.Quote.NonTextType != "image" || in.Quote.IsBotMessage {
		t.Errorf("quote = %+v", in.Quote)
	}
}
