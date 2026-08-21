package im

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// manualReplyTestAdapter records SendReply calls and can be told to fail, so
// tests can assert both the delivered payload and the no-persist-on-failure
// contract.
type manualReplyTestAdapter struct {
	lifecycleTestAdapter
	mu   sync.Mutex
	sent []struct {
		incoming *IncomingMessage
		reply    *ReplyMessage
	}
	err error
}

func (a *manualReplyTestAdapter) SendReply(_ context.Context, incoming *IncomingMessage, reply *ReplyMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.sent = append(a.sent, struct {
		incoming *IncomingMessage
		reply    *ReplyMessage
	}{incoming, reply})
	return nil
}

// manualReplyMessageService fakes just the CreateMessage path SendManualReply
// uses; every other MessageService method panics via the embedded nil.
type manualReplyMessageService struct {
	interfaces.MessageService
	mu      sync.Mutex
	created []*types.Message
	err     error
}

func (f *manualReplyMessageService) CreateMessage(_ context.Context, m *types.Message) (*types.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	cp := *m
	cp.ID = fmt.Sprintf("msg-%d", len(f.created)+1)
	f.created = append(f.created, &cp)
	return &cp, nil
}

func createManualReplyTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	// Production schemas use PostgreSQL defaults SQLite cannot parse; keep
	// equivalent minimal tables (same approach as newLifecycleTestDB).
	if err := db.Exec(`CREATE TABLE im_channel_sessions (
		id TEXT PRIMARY KEY,
		platform TEXT NOT NULL,
		user_id TEXT NOT NULL,
		chat_id TEXT NOT NULL DEFAULT '',
		thread_id TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL,
		tenant_id INTEGER NOT NULL,
		agent_id TEXT DEFAULT '',
		im_channel_id TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		metadata TEXT DEFAULT '{}',
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create im_channel_sessions: %v", err)
	}
	if err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		tenant_id INTEGER NOT NULL,
		user_id TEXT DEFAULT '',
		title TEXT DEFAULT '',
		description TEXT DEFAULT '',
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create sessions: %v", err)
	}
}

// newManualReplyFixture builds a service with one running whatsapp channel and
// an IM-bound session, returning the pieces tests poke at.
func newManualReplyFixture(t *testing.T) (*Service, *manualReplyTestAdapter, *manualReplyMessageService, *IMChannel) {
	t.Helper()
	db := newLifecycleTestDB(t)
	createManualReplyTables(t, db)

	adapter := &manualReplyTestAdapter{}
	msgSvc := &manualReplyMessageService{}
	svc := newLifecycleTestService(db, nil, "instance-one")
	svc.messageService = msgSvc
	svc.RegisterAdapterFactory("whatsapp", func(context.Context, *IMChannel, func(context.Context, *IncomingMessage) error) (Adapter, context.CancelFunc, error) {
		return adapter, nil, nil
	})
	t.Cleanup(svc.Stop)

	channel := &IMChannel{
		ID:          "wa-channel",
		TenantID:    1,
		AgentID:     "agent-1",
		Platform:    "whatsapp",
		Enabled:     true,
		Mode:        "webhook", // avoids leader election in tests; irrelevant to SendManualReply
		OutputMode:  "full",
		SessionMode: string(SessionModeUser),
		Credentials: types.JSON(`{"device_jid":"8613800138000:12@s.whatsapp.net"}`),
	}
	if err := db.Create(channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := svc.StartChannel(channel); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	return svc, adapter, msgSvc, channel
}

func createManualReplySession(t *testing.T, db *gorm.DB, cs *ChannelSession) {
	t.Helper()
	// Raw insert: Session.BeforeCreate unconditionally replaces the ID with a
	// fresh UUID, which would break the fixture's session_id references.
	stale := time.Now().Add(-time.Hour)
	if err := db.Exec(
		`INSERT INTO sessions (id, tenant_id, user_id, title, created_at, updated_at) VALUES (?, ?, '', 'wa chat', ?, ?)`,
		cs.SessionID, cs.TenantID, stale, stale,
	).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := db.Create(cs).Error; err != nil {
		t.Fatalf("create channel session: %v", err)
	}
}

func TestManualReplyIncomingAddressing(t *testing.T) {
	dm := manualReplyIncoming(&ChannelSession{Platform: "whatsapp", UserID: "8613800138000"})
	if dm.ChatType != ChatTypeDirect || dm.UserID != "8613800138000" || dm.ChatID != "" {
		t.Fatalf("DM envelope = %+v, want direct chat addressed by bare phone", dm)
	}
	if dm.MessageID != "" {
		t.Fatalf("synthesized envelope must not carry a MessageID (quote-reply would target it), got %q", dm.MessageID)
	}

	group := manualReplyIncoming(&ChannelSession{Platform: "whatsapp", UserID: "8613800138000", ChatID: "1234-5678@g.us"})
	if group.ChatType != ChatTypeGroup || group.ChatID != "1234-5678@g.us" {
		t.Fatalf("group envelope = %+v, want group chat addressed by stored JID", group)
	}
}

func TestManualReplySupported(t *testing.T) {
	if !ManualReplySupported("whatsapp") || !ManualReplySupported("WhatsApp") {
		t.Fatal("whatsapp must support manual replies (case-insensitive)")
	}
	for _, p := range []string{"telegram", "feishu", "slack", ""} {
		if ManualReplySupported(p) {
			t.Fatalf("platform %q unexpectedly reports manual-reply support", p)
		}
	}
}

func TestSendManualReplyDeliversAndPersists(t *testing.T) {
	svc, adapter, msgSvc, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform:    "whatsapp",
		UserID:      "8613800138000",
		SessionID:   "session-dm",
		TenantID:    1,
		AgentID:     channel.AgentID,
		IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)

	msg, err := svc.SendManualReply(context.Background(), 1, "session-dm", "  你好，这里是人工客服  ", nil)
	if err != nil {
		t.Fatalf("SendManualReply: %v", err)
	}

	if len(adapter.sent) != 1 {
		t.Fatalf("adapter deliveries = %d, want 1", len(adapter.sent))
	}
	delivered := adapter.sent[0]
	if delivered.reply.Content != "你好，这里是人工客服" || !delivered.reply.IsFinal {
		t.Fatalf("delivered reply = %+v, want trimmed final content", delivered.reply)
	}
	if delivered.incoming.UserID != cs.UserID || delivered.incoming.ChatType != ChatTypeDirect {
		t.Fatalf("delivered addressing = %+v, want DM to %s", delivered.incoming, cs.UserID)
	}

	if len(msgSvc.created) != 1 {
		t.Fatalf("persisted messages = %d, want 1", len(msgSvc.created))
	}
	persisted := msgSvc.created[0]
	if persisted.Role != "assistant" || persisted.Channel != ManualReplyChannel || !persisted.IsCompleted {
		t.Fatalf("persisted message = %+v, want completed assistant message on channel %q", persisted, ManualReplyChannel)
	}
	if persisted.Content != "你好，这里是人工客服" || persisted.SessionID != "session-dm" || persisted.RequestID == "" {
		t.Fatalf("persisted message fields wrong: %+v", persisted)
	}
	if msg.ID != persisted.ID {
		t.Fatalf("returned message %q does not match persisted %q", msg.ID, persisted.ID)
	}

	// The conversation must resurface at the top of the session list.
	var session types.Session
	if err := svc.db.Where("id = ?", "session-dm").First(&session).Error; err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if time.Since(session.UpdatedAt) > time.Minute {
		t.Fatalf("session updated_at was not bumped: %v", session.UpdatedAt)
	}
}

func TestSendManualReplyUsesSoftDeletedMapping(t *testing.T) {
	// /clear soft-deletes the mapping but the old session still identifies the
	// peer; replying inside that historical session must keep working.
	svc, adapter, _, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform:    "whatsapp",
		UserID:      "8613800138000",
		ChatID:      "1234-5678@g.us",
		SessionID:   "session-cleared",
		TenantID:    1,
		IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)
	if err := svc.db.Delete(&ChannelSession{}, "id = ?", cs.ID).Error; err != nil {
		t.Fatalf("soft-delete mapping: %v", err)
	}

	if _, err := svc.SendManualReply(context.Background(), 1, "session-cleared", "还在吗", nil); err != nil {
		t.Fatalf("SendManualReply after /clear: %v", err)
	}
	if len(adapter.sent) != 1 || adapter.sent[0].incoming.ChatType != ChatTypeGroup {
		t.Fatalf("expected one group delivery, got %+v", adapter.sent)
	}
}

func TestSendManualReplyErrorPaths(t *testing.T) {
	svc, adapter, msgSvc, channel := newManualReplyFixture(t)

	// Session without an IM mapping (plain web chat).
	if _, err := svc.SendManualReply(context.Background(), 1, "web-session", "hi", nil); !errors.Is(err, ErrManualReplyNotIMSession) {
		t.Fatalf("no-mapping error = %v, want ErrManualReplyNotIMSession", err)
	}

	// Tenant mismatch must behave exactly like a missing mapping.
	cs := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-dm", TenantID: 1, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)
	if _, err := svc.SendManualReply(context.Background(), 2, "session-dm", "hi", nil); !errors.Is(err, ErrManualReplyNotIMSession) {
		t.Fatalf("cross-tenant error = %v, want ErrManualReplyNotIMSession", err)
	}

	// Blank content never reaches the adapter.
	if _, err := svc.SendManualReply(context.Background(), 1, "session-dm", "   ", nil); !errors.Is(err, ErrManualReplyEmptyContent) {
		t.Fatalf("empty-content error = %v, want ErrManualReplyEmptyContent", err)
	}
	if _, err := svc.SendManualReply(context.Background(), 1, "session-dm", strings.Repeat("字", manualReplyMaxRunes+1), nil); !errors.Is(err, ErrManualReplyTooLong) {
		t.Fatalf("over-length error = %v, want ErrManualReplyTooLong", err)
	}

	// Unsupported platform mapping.
	tg := &ChannelSession{
		Platform: "telegram", UserID: "42",
		SessionID: "session-tg", TenantID: 1, IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, tg)
	if _, err := svc.SendManualReply(context.Background(), 1, "session-tg", "hi", nil); !errors.Is(err, ErrManualReplyUnsupported) {
		t.Fatalf("unsupported-platform error = %v, want ErrManualReplyUnsupported", err)
	}

	// Runtime not on this instance.
	orphan := &ChannelSession{
		Platform: "whatsapp", UserID: "8613800138000",
		SessionID: "session-orphan", TenantID: 1, IMChannelID: "missing-channel",
	}
	createManualReplySession(t, svc.db, orphan)
	if _, err := svc.SendManualReply(context.Background(), 1, "session-orphan", "hi", nil); !errors.Is(err, ErrManualReplyNotRunning) {
		t.Fatalf("no-runtime error = %v, want ErrManualReplyNotRunning", err)
	}

	// Delivery failure must not persist a phantom message.
	adapter.err = errors.New("socket closed")
	if _, err := svc.SendManualReply(context.Background(), 1, "session-dm", "hi", nil); !errors.Is(err, ErrManualReplyDelivery) {
		t.Fatalf("delivery error = %v, want ErrManualReplyDelivery", err)
	}
	if len(msgSvc.created) != 0 {
		t.Fatalf("failed delivery persisted %d messages, want 0", len(msgSvc.created))
	}
}

func TestValidateManualReplyAttachmentSizes(t *testing.T) {
	small := func(kind MessageType) *ReplyAttachment {
		return &ReplyAttachment{Kind: kind, FileName: "f", MimeType: "application/pdf", Data: []byte("x")}
	}
	if err := validateManualReplyAttachmentSizes(nil); err != nil {
		t.Fatalf("nil attachments: %v", err)
	}

	six := make([]*ReplyAttachment, ManualReplyMaxAttachments+1)
	for i := range six {
		six[i] = small(MessageTypeFile)
	}
	if err := validateManualReplyAttachmentSizes(six); !errors.Is(err, ErrManualReplyTooManyAttachments) {
		t.Fatalf("count error = %v, want ErrManualReplyTooManyAttachments", err)
	}

	if err := validateManualReplyAttachmentSizes([]*ReplyAttachment{{Kind: MessageTypeImage, FileName: "empty.png"}}); !errors.Is(err, ErrManualReplyBadAttachment) {
		t.Fatalf("empty payload error = %v, want ErrManualReplyBadAttachment", err)
	}

	bigImage := &ReplyAttachment{Kind: MessageTypeImage, FileName: "big.png", Data: make([]byte, manualReplyMaxImageBytes+1)}
	if err := validateManualReplyAttachmentSizes([]*ReplyAttachment{bigImage}); !errors.Is(err, ErrManualReplyAttachmentTooLarge) {
		t.Fatalf("oversize image error = %v, want ErrManualReplyAttachmentTooLarge", err)
	}
	// The same payload is fine as a document (higher cap).
	bigDoc := &ReplyAttachment{Kind: MessageTypeFile, FileName: "big.bin", Data: bigImage.Data}
	if err := validateManualReplyAttachmentSizes([]*ReplyAttachment{bigDoc}); err != nil {
		t.Fatalf("document under file cap rejected: %v", err)
	}

	hugeDoc := &ReplyAttachment{Kind: MessageTypeFile, FileName: "huge.bin", Data: make([]byte, manualReplyMaxFileBytes+1)}
	if err := validateManualReplyAttachmentSizes([]*ReplyAttachment{hugeDoc}); !errors.Is(err, ErrManualReplyAttachmentTooLarge) {
		t.Fatalf("oversize document error = %v, want ErrManualReplyAttachmentTooLarge", err)
	}
}

func TestManualReplyMessageMedia(t *testing.T) {
	atts := []*ReplyAttachment{
		{Kind: MessageTypeImage, FileName: "../sneaky/photo.png", MimeType: "image/png", Data: []byte("tiny-image")},
		{Kind: MessageTypeImage, FileName: "large.jpg", MimeType: "image/jpeg", Data: make([]byte, manualReplyInlineImageBytes+1)},
		{Kind: MessageTypeFile, FileName: "/tmp/Report Final.PDF", MimeType: "application/pdf", Data: []byte("12345")},
	}
	images, files := manualReplyMessageMedia(atts)

	if len(images) != 2 || len(files) != 1 {
		t.Fatalf("media split = %d images / %d files, want 2/1", len(images), len(files))
	}
	if !strings.HasPrefix(images[0].URL, "data:image/png;base64,") {
		t.Fatalf("small image URL = %q, want inline data URI", images[0].URL)
	}
	if images[1].URL != "" || !strings.Contains(images[1].Caption, "large.jpg") {
		t.Fatalf("oversize image = %+v, want empty URL with filename placeholder caption", images[1])
	}
	if files[0].FileName != "Report Final.PDF" || files[0].FileType != ".pdf" || files[0].FileSize != 5 {
		t.Fatalf("document metadata = %+v, want base name, lowercase ext, size 5", files[0])
	}
}

func TestSendManualReplyWithAttachments(t *testing.T) {
	svc, adapter, msgSvc, channel := newManualReplyFixture(t)
	cs := &ChannelSession{
		Platform:    "whatsapp",
		UserID:      "8613800138000",
		SessionID:   "session-media",
		TenantID:    1,
		AgentID:     channel.AgentID,
		IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)

	atts := []*ReplyAttachment{
		{Kind: MessageTypeImage, FileName: "invoice.png", MimeType: "image/png", Data: []byte("png-bytes")},
		{Kind: MessageTypeFile, FileName: "contract.pdf", MimeType: "application/pdf", Data: []byte("pdf-bytes")},
	}
	// Media-only replies (no text) are valid.
	msg, err := svc.SendManualReply(context.Background(), 1, "session-media", "", atts)
	if err != nil {
		t.Fatalf("SendManualReply with attachments: %v", err)
	}

	if len(adapter.sent) != 1 {
		t.Fatalf("adapter deliveries = %d, want 1", len(adapter.sent))
	}
	delivered := adapter.sent[0].reply
	if delivered.Content != "" || len(delivered.Attachments) != 2 {
		t.Fatalf("delivered reply = %+v, want empty text with 2 attachments", delivered)
	}
	if string(delivered.Attachments[0].Data) != "png-bytes" || delivered.Attachments[1].FileName != "contract.pdf" {
		t.Fatalf("delivered attachments corrupted: %+v", delivered.Attachments)
	}

	if len(msgSvc.created) != 1 {
		t.Fatalf("persisted messages = %d, want 1", len(msgSvc.created))
	}
	persisted := msgSvc.created[0]
	if len(persisted.Images) != 1 || !strings.HasPrefix(persisted.Images[0].URL, "data:image/png;base64,") {
		t.Fatalf("persisted images = %+v, want one inline data URI", persisted.Images)
	}
	if len(persisted.Attachments) != 1 || persisted.Attachments[0].FileName != "contract.pdf" {
		t.Fatalf("persisted attachments = %+v, want contract.pdf metadata", persisted.Attachments)
	}
	if msg.ID != persisted.ID {
		t.Fatalf("returned message %q does not match persisted %q", msg.ID, persisted.ID)
	}
}

func TestSendManualReplyMediaUnsupportedPlatform(t *testing.T) {
	svc, _, msgSvc, channel := newManualReplyFixture(t)

	// Register a synthetic platform whose adapter cannot deliver media; clean
	// up so the package-level whitelist is untouched for other tests.
	manualReplyPlatforms["textonly"] = manualReplyCaps{Media: false}
	t.Cleanup(func() { delete(manualReplyPlatforms, "textonly") })

	cs := &ChannelSession{
		Platform:    "textonly",
		UserID:      "u-1",
		SessionID:   "session-textonly",
		TenantID:    1,
		IMChannelID: channel.ID,
	}
	createManualReplySession(t, svc.db, cs)

	atts := []*ReplyAttachment{{Kind: MessageTypeImage, FileName: "a.png", MimeType: "image/png", Data: []byte("x")}}
	if _, err := svc.SendManualReply(context.Background(), 1, "session-textonly", "hi", atts); !errors.Is(err, ErrManualReplyMediaUnsupported) {
		t.Fatalf("media on text-only platform = %v, want ErrManualReplyMediaUnsupported", err)
	}
	if len(msgSvc.created) != 0 {
		t.Fatalf("rejected media reply persisted %d messages, want 0", len(msgSvc.created))
	}
}
