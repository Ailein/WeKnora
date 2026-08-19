package whatsapp

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/logger"
)

// Compile-time checks.
var (
	_ im.Adapter        = (*Adapter)(nil)
	_ im.FileDownloader = (*Adapter)(nil)
	_ im.StatusReporter = (*Adapter)(nil)
)

const (
	// historyGrace tolerates offline catch-up: messages older than this
	// relative to adapter start are treated as history and ignored.
	historyGrace = 30 * time.Minute

	// mediaCacheTTL bounds how long incoming media stays downloadable.
	// Decryption needs the original message payload (media key), which only
	// exists in memory, so downloads must happen shortly after receipt.
	mediaCacheTTL = 30 * time.Minute

	// quotePreviewLimit truncates the quoted-message preview attached to
	// group replies.
	quotePreviewLimit = 120

	extraChatJID   = "chat_jid"
	extraSenderJID = "sender_jid"
)

// Adapter implements im.Adapter for WhatsApp via whatsmeow.
//
// It intentionally does NOT implement im.StreamSender: streaming would mean
// editing a WhatsApp message every few hundred milliseconds, which is very
// visible automation on an unofficial client. Channels run in "full" output
// mode and the reply is sent once, split into ≤4000-rune segments.
type Adapter struct {
	channelID string
	client    *whatsmeow.Client
	// allowFrom is the DM allowlist of normalized phone numbers. The special
	// key "*" opens DMs to everyone. An empty map is fail-closed: linking a
	// personal account must not let any stranger drive the bot (and spend
	// tokens) by default.
	allowFrom map[string]bool
	startedAt time.Time
	media     *mediaCache

	// Connection-state snapshot for the status API. whatsmeow dispatches
	// events sequentially, but ChannelStatus is read from HTTP handlers and
	// the Redis status mirror, hence the mutex.
	stateMu     sync.Mutex
	state       string
	stateDetail string
	stateSince  time.Time
}

func newAdapter(channelID string, client *whatsmeow.Client, allowFrom map[string]bool) *Adapter {
	return &Adapter{
		channelID:  channelID,
		client:     client,
		allowFrom:  allowFrom,
		startedAt:  time.Now(),
		media:      newMediaCache(),
		state:      im.ChannelStateConnecting,
		stateSince: time.Now(),
	}
}

// ChannelStatus implements im.StatusReporter.
func (a *Adapter) ChannelStatus() im.ChannelStatus {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return im.ChannelStatus{State: a.state, Detail: a.stateDetail, Since: a.stateSince}
}

// setState records a connection-state transition for the status API.
func (a *Adapter) setState(state, detail string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.state == state && a.stateDetail == detail {
		return
	}
	a.state = state
	a.stateDetail = detail
	a.stateSince = time.Now()
}

// trackConnState updates the status snapshot from a whatsmeow event. Kept
// separate from makeEventHandler so the transition table is unit-testable
// without a live client.
func (a *Adapter) trackConnState(evt any) {
	switch v := evt.(type) {
	case *events.Connected:
		a.setState(im.ChannelStateConnected, "")
	case *events.Disconnected:
		a.markReconnecting()
	case *events.LoggedOut:
		a.setState(im.ChannelStateLoggedOut,
			fmt.Sprintf("device was unlinked from the phone (reason=%v); re-scan the QR code", v.Reason))
	case *events.StreamReplaced:
		a.setState(im.ChannelStateStreamReplaced, "another client connected with the same session")
	case *events.TemporaryBan:
		a.setState(im.ChannelStateError,
			fmt.Sprintf("temporarily banned by WhatsApp (code=%d, expires in %s)", int(v.Code), v.Expire))
	case *events.ClientOutdated:
		a.setState(im.ChannelStateError, "client protocol version outdated; upgrade the whatsmeow dependency")
	}
}

// markReconnecting flags an unexpected disconnect. Terminal states must
// survive the trailing Disconnected event whatsmeow may emit while shutting
// the socket down, so they are never overwritten here.
func (a *Adapter) markReconnecting() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	switch a.state {
	case im.ChannelStateLoggedOut, im.ChannelStateStreamReplaced, im.ChannelStateError:
		return
	}
	if a.state == im.ChannelStateConnecting {
		return
	}
	a.state = im.ChannelStateConnecting
	a.stateDetail = "connection lost; reconnecting"
	a.stateSince = time.Now()
}

func (a *Adapter) Platform() im.Platform {
	return im.PlatformWhatsApp
}

// The WhatsApp channel is connection-based; there is no HTTP callback. Fail
// closed (like the WeChat stub) so unauthenticated probes of the callback
// endpoint get a 403 instead of a friendly ACK.
func (a *Adapter) HandleURLVerification(c *gin.Context) bool { return false }
func (a *Adapter) VerifyCallback(c *gin.Context) error {
	return fmt.Errorf("whatsapp adapter does not accept webhook callbacks")
}
func (a *Adapter) ParseCallback(c *gin.Context) (*im.IncomingMessage, error) {
	return nil, fmt.Errorf("whatsapp adapter does not accept webhook callbacks")
}

// run owns the connection lifecycle: initial connect with backoff, then
// whatsmeow's built-in auto-reconnect takes over until ctx is cancelled.
func (a *Adapter) run(ctx context.Context) {
	go a.media.reap(ctx)

	backoff := 5 * time.Second
	for {
		err := a.client.Connect()
		if err == nil {
			break
		}
		a.setState(im.ChannelStateConnecting, fmt.Sprintf("connect failed: %v", err))
		logger.Errorf(ctx, "[WhatsApp] Connect failed for channel %s (retry in %s): %v", a.channelID, backoff, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}

	<-ctx.Done()
	a.client.Disconnect()
}

// makeEventHandler adapts whatsmeow events into the unified message handler.
func (a *Adapter) makeEventHandler(msgHandler func(context.Context, *im.IncomingMessage) error) func(evt any) {
	return func(evt any) {
		ctx := context.Background()
		a.trackConnState(evt)
		switch v := evt.(type) {
		case *events.Message:
			incoming := a.parseMessage(ctx, v)
			if incoming == nil {
				return
			}
			// whatsmeow dispatches all events from one sequential loop; handle
			// the message in its own goroutine (like the WeChat long-poll path)
			// so a slow QA turn cannot stall receipts, app-state sync or
			// LoggedOut/StreamReplaced handling for this account.
			go func() {
				if err := msgHandler(ctx, incoming); err != nil {
					logger.Errorf(ctx, "[WhatsApp] Handle message failed for channel %s: %v", a.channelID, err)
				}
			}()
		case *events.Connected:
			logger.Infof(ctx, "[WhatsApp] Connected: channel=%s jid=%s", a.channelID, a.selfJIDString())
		case *events.LoggedOut:
			// The account was unlinked from the phone. Reconnecting is
			// pointless until an admin re-pairs the device via QR code.
			logger.Errorf(ctx, "[WhatsApp] Logged out (reason=%v) for channel %s: re-scan the QR code in channel settings", v.Reason, a.channelID)
			go a.client.Disconnect()
		case *events.StreamReplaced:
			// Another socket connected with the same session. Stop instead of
			// fighting over the connection (mirrors the conflict handling of
			// other WhatsApp Web clients).
			logger.Errorf(ctx, "[WhatsApp] Stream replaced by another client for channel %s; disconnecting", a.channelID)
			go a.client.Disconnect()
		}
	}
}

func (a *Adapter) selfJIDString() string {
	if id := a.client.Store.ID; id != nil {
		return id.String()
	}
	return ""
}

// isSelfJID reports whether j refers to the linked account, matching both the
// phone-number JID and the newer LID identity.
func (a *Adapter) isSelfJID(j types.JID) bool {
	if id := a.client.Store.ID; id != nil && j.User == id.User {
		return true
	}
	if lid := a.client.Store.LID; lid.User != "" && j.User == lid.User {
		return true
	}
	return false
}

// senderPhone resolves the sender's phone number, preferring the PN alias
// when the message arrived with a LID (hidden user) address. Some events
// (e.g. unavailable-message retries) carry a LID with no SenderAlt; fall
// back to the LID→PN mapping whatsmeow persists from history sync.
func (a *Adapter) senderPhone(ctx context.Context, info *types.MessageInfo) string {
	sender := info.Sender
	if sender.Server == types.HiddenUserServer {
		if info.SenderAlt.User != "" {
			sender = info.SenderAlt
		} else if pn, err := a.client.Store.LIDs.GetPNForLID(ctx, sender); err == nil && !pn.IsEmpty() {
			sender = pn
		}
	}
	return sender.User
}

func normalizePhone(s string) string {
	return strings.NewReplacer("+", "", " ", "", "-", "").Replace(strings.TrimSpace(s))
}

// parseAllowFrom parses the "allow_from" credential (comma-separated phone
// numbers, or "*" to allow everyone) into a lookup set.
func parseAllowFrom(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" {
			out["*"] = true
			continue
		}
		if n := normalizePhone(part); n != "" {
			out[n] = true
		}
	}
	return out
}

func (a *Adapter) dmAllowed(phone string) bool {
	return a.allowFrom["*"] || a.allowFrom[normalizePhone(phone)]
}

// parseMessage filters and converts a whatsmeow message event into the
// unified IncomingMessage. Returns nil for anything that must not reach QA.
func (a *Adapter) parseMessage(ctx context.Context, v *events.Message) *im.IncomingMessage {
	info := v.Info
	if info.IsFromMe || v.Message == nil {
		return nil
	}
	// Status updates, broadcast lists and channels (newsletters) are not chats.
	switch info.Chat.Server {
	case types.BroadcastServer, types.NewsletterServer:
		return nil
	}
	// Ignore history delivered on (re)connect, beyond a grace window for
	// genuine offline catch-up. Redis-side dedup also guards replays.
	if info.Timestamp.Before(a.startedAt.Add(-historyGrace)) {
		return nil
	}

	msg := v.Message
	text := extractText(msg)
	image := msg.GetImageMessage()
	document := msg.GetDocumentMessage()
	if text == "" && image == nil && document == nil {
		// Reactions, receipts, polls, calls, protocol messages, …
		return nil
	}

	isGroup := info.Chat.Server == types.GroupServer
	phone := a.senderPhone(ctx, &info)
	if isGroup {
		// Group messages only trigger the bot when it is @mentioned or the
		// quoted message is the bot's own reply.
		if !a.groupTriggered(msg) {
			return nil
		}
	} else if !a.dmAllowed(phone) {
		if info.Sender.Server == types.HiddenUserServer && phone == info.Sender.User {
			// senderPhone could not resolve this LID to a phone number, so the
			// allowlist can never match. Fail closed, but loudly: a legitimate
			// sender is being rejected until the LID→PN mapping syncs
			// (allow_from="*" is the escape hatch).
			logger.Warnf(ctx, "[WhatsApp] DM from unresolved LID %s dropped (allow_from cannot match hidden-user addresses) channel=%s", phone, a.channelID)
		} else {
			logger.Debugf(ctx, "[WhatsApp] DM from %s dropped (not in allow_from) channel=%s", phone, a.channelID)
		}
		return nil
	}

	incoming := &im.IncomingMessage{
		Platform:    im.PlatformWhatsApp,
		MessageType: im.MessageTypeText,
		UserID:      phone,
		UserName:    info.PushName,
		ChatType:    im.ChatTypeDirect,
		Content:     text,
		MessageID:   string(info.ID),
		Extra: map[string]string{
			extraChatJID:   info.Chat.String(),
			extraSenderJID: info.Sender.String(),
		},
	}
	if isGroup {
		incoming.ChatType = im.ChatTypeGroup
		incoming.ChatID = info.Chat.String()
		incoming.Content = a.stripSelfMentions(text)
	}

	switch {
	case image != nil:
		incoming.MessageType = im.MessageTypeImage
		incoming.FileKey = mediaKey(&info)
		incoming.FileName = "photo.jpg"
		incoming.FileSize = int64(image.GetFileLength())
		a.media.put(incoming.FileKey, msg, incoming.FileName)
	case document != nil:
		incoming.MessageType = im.MessageTypeFile
		incoming.FileKey = mediaKey(&info)
		incoming.FileName = document.GetFileName()
		incoming.FileSize = int64(document.GetFileLength())
		a.media.put(incoming.FileKey, msg, incoming.FileName)
	}

	if quote := a.parseQuote(msg); quote != nil {
		incoming.Quote = quote
	}
	return incoming
}

// extractText pulls the text content (or media caption) out of a message.
func extractText(msg *waE2E.Message) string {
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation()
	case msg.GetExtendedTextMessage().GetText() != "":
		return msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage().GetCaption() != "":
		return msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage().GetCaption() != "":
		return msg.GetVideoMessage().GetCaption()
	case msg.GetDocumentMessage().GetCaption() != "":
		return msg.GetDocumentMessage().GetCaption()
	}
	return ""
}

// extractContextInfo finds the ContextInfo on whichever sub-message carries it.
func extractContextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	if ci := msg.GetExtendedTextMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := msg.GetImageMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := msg.GetVideoMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := msg.GetDocumentMessage().GetContextInfo(); ci != nil {
		return ci
	}
	return nil
}

// groupTriggered reports whether a group message addresses the bot: either a
// native @mention of the linked account or a reply to one of its messages.
func (a *Adapter) groupTriggered(msg *waE2E.Message) bool {
	ci := extractContextInfo(msg)
	if ci == nil {
		return false
	}
	for _, mention := range ci.GetMentionedJID() {
		if j, err := types.ParseJID(mention); err == nil && a.isSelfJID(j) {
			return true
		}
	}
	if participant := ci.GetParticipant(); participant != "" && ci.GetStanzaID() != "" {
		if j, err := types.ParseJID(participant); err == nil && a.isSelfJID(j) {
			return true
		}
	}
	return false
}

// stripSelfMentions removes "@<own number>" tokens so the QA input reads like
// a plain question.
func (a *Adapter) stripSelfMentions(text string) string {
	if id := a.client.Store.ID; id != nil {
		text = strings.ReplaceAll(text, "@"+id.User, "")
	}
	if lid := a.client.Store.LID; lid.User != "" {
		text = strings.ReplaceAll(text, "@"+lid.User, "")
	}
	return strings.TrimSpace(text)
}

// parseQuote extracts the quoted/replied message, if any.
func (a *Adapter) parseQuote(msg *waE2E.Message) *im.QuotedMessage {
	ci := extractContextInfo(msg)
	if ci == nil || ci.GetStanzaID() == "" || ci.GetQuotedMessage() == nil {
		return nil
	}
	quoted := ci.GetQuotedMessage()
	quote := &im.QuotedMessage{
		MessageID: ci.GetStanzaID(),
		Content:   extractText(quoted),
		SenderID:  ci.GetParticipant(),
	}
	if j, err := types.ParseJID(ci.GetParticipant()); err == nil {
		quote.IsBotMessage = a.isSelfJID(j)
	}
	if quote.Content == "" {
		switch {
		case quoted.GetImageMessage() != nil:
			quote.NonTextType = "image"
		case quoted.GetVideoMessage() != nil:
			quote.NonTextType = "video"
		case quoted.GetDocumentMessage() != nil:
			quote.NonTextType = "file"
		case quoted.GetAudioMessage() != nil:
			quote.NonTextType = "audio"
		}
	}
	return quote
}

// ── Send reply ──

func (a *Adapter) SendReply(ctx context.Context, incoming *im.IncomingMessage, reply *im.ReplyMessage) error {
	chatJID, err := a.replyTarget(incoming)
	if err != nil {
		return err
	}

	text := im.FormatIMDisplayContent(reply.Content, im.StreamDisplayFinal)
	segments := splitMessage(toWhatsAppMarkup(text), maxSegmentRunes)
	if len(segments) == 0 {
		return nil
	}

	// Best-effort typing indicator; failures must not block the reply.
	if err := a.client.SendChatPresence(ctx, chatJID, types.ChatPresenceComposing, types.ChatPresenceMediaText); err != nil {
		logger.Debugf(ctx, "[WhatsApp] typing indicator failed: %v", err)
	}
	defer func() {
		_ = a.client.SendChatPresence(ctx, chatJID, types.ChatPresencePaused, types.ChatPresenceMediaText)
	}()

	for i, segment := range segments {
		var msg *waE2E.Message
		if i == 0 && incoming.ChatType == im.ChatTypeGroup {
			// Quote the triggering message so the reply is legible in a busy group.
			msg = a.buildQuotedReply(segment, incoming)
		} else {
			msg = &waE2E.Message{Conversation: proto.String(segment)}
		}
		if _, err := a.client.SendMessage(ctx, chatJID, msg); err != nil {
			return fmt.Errorf("whatsapp send segment %d/%d: %w", i+1, len(segments), err)
		}
	}
	return nil
}

// replyTarget resolves the chat JID to reply to. The full JID travels in
// Extra because IncomingMessage.UserID only holds the bare phone number.
func (a *Adapter) replyTarget(incoming *im.IncomingMessage) (types.JID, error) {
	if raw := incoming.Extra[extraChatJID]; raw != "" {
		return types.ParseJID(raw)
	}
	if incoming.ChatType == im.ChatTypeGroup && incoming.ChatID != "" {
		return types.ParseJID(incoming.ChatID)
	}
	if incoming.UserID != "" {
		return types.NewJID(incoming.UserID, types.DefaultUserServer), nil
	}
	return types.JID{}, fmt.Errorf("no reply target on incoming message")
}

func (a *Adapter) buildQuotedReply(segment string, incoming *im.IncomingMessage) *waE2E.Message {
	senderJID := incoming.Extra[extraSenderJID]
	if incoming.MessageID == "" || senderJID == "" {
		return &waE2E.Message{Conversation: proto.String(segment)}
	}
	preview := incoming.Content
	if runes := []rune(preview); len(runes) > quotePreviewLimit {
		preview = string(runes[:quotePreviewLimit]) + "…"
	}
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(segment),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String(incoming.MessageID),
				Participant:   proto.String(senderJID),
				QuotedMessage: &waE2E.Message{Conversation: proto.String(preview)},
			},
		},
	}
}

// ── FileDownloader ──

// maxDownloadBytes caps the bytes actually fetched for one attachment. The
// FileLength the caller pre-checks comes from the sender-controlled protobuf,
// so the real payload can be arbitrarily large regardless of that check; the
// cap must be enforced on the stream itself. Slightly above the service-side
// limit to leave room for encryption overhead (padding + MAC).
const maxDownloadBytes = im.MaxIMAttachmentBytes + 1024

var errMediaTooLarge = fmt.Errorf("whatsapp media exceeds the %d MiB attachment limit", im.MaxIMAttachmentBytes>>20)

func (a *Adapter) DownloadFile(ctx context.Context, msg *im.IncomingMessage) (io.ReadCloser, string, error) {
	if msg.FileKey == "" {
		return nil, "", fmt.Errorf("file_key is required")
	}
	cached, ok := a.media.get(msg.FileKey)
	if !ok {
		return nil, "", fmt.Errorf("media for message %s is no longer available (cache expired)", msg.FileKey)
	}
	downloadable := downloadableFrom(cached.msg)
	if downloadable == nil {
		return nil, "", fmt.Errorf("message %s has no downloadable media", msg.FileKey)
	}
	// Stream to a size-capped temp file instead of buffering in memory: the
	// in-memory DownloadAny would allocate the full (untrusted) payload before
	// the caller's LimitReader could see a single byte.
	tmp, err := os.CreateTemp("", "weknora-wa-media-*")
	if err != nil {
		return nil, "", fmt.Errorf("create temp file for whatsapp media: %w", err)
	}
	if err := a.client.DownloadToFile(ctx, downloadable, &limitedFile{File: tmp, limit: maxDownloadBytes}); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		if strings.Contains(err.Error(), errMediaTooLarge.Error()) {
			return nil, "", errMediaTooLarge
		}
		return nil, "", fmt.Errorf("download whatsapp media: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, "", fmt.Errorf("rewind whatsapp media file: %w", err)
	}
	name := msg.FileName
	if name == "" {
		name = cached.fileName
	}
	return &tempFileReadCloser{File: tmp}, name, nil
}

// downloadableFrom picks the downloadable part out of a cached message,
// mirroring the order whatsmeow's DownloadAny uses.
func downloadableFrom(msg *waE2E.Message) whatsmeow.DownloadableMessage {
	if msg == nil {
		return nil
	}
	switch {
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage()
	}
	return nil
}

// limitedFile hard-caps how many bytes whatsmeow may write while downloading,
// aborting oversized transfers instead of filling the disk.
type limitedFile struct {
	*os.File
	limit int64
}

func (f *limitedFile) Write(p []byte) (int, error) {
	pos, err := f.File.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if pos+int64(len(p)) > f.limit {
		return 0, errMediaTooLarge
	}
	return f.File.Write(p)
}

func (f *limitedFile) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.limit {
		return 0, errMediaTooLarge
	}
	return f.File.WriteAt(p, off)
}

// tempFileReadCloser deletes the backing temp file on Close.
type tempFileReadCloser struct {
	*os.File
}

func (t *tempFileReadCloser) Close() error {
	err := t.File.Close()
	if rmErr := os.Remove(t.File.Name()); rmErr != nil && err == nil {
		err = rmErr
	}
	return err
}

// ── media cache ──

// mediaKey scopes a media-cache entry by chat and sender: the stanza ID alone
// is chosen by the sending client, so another participant could reuse a
// victim's ID and hijack the cache entry between receipt and download.
func mediaKey(info *types.MessageInfo) string {
	return info.Chat.String() + "|" + info.Sender.String() + "|" + string(info.ID)
}

// mediaCache keeps recently received media messages so DownloadFile can
// decrypt them (WhatsApp media requires the message's own media key).
type cachedMedia struct {
	msg      *waE2E.Message
	fileName string
	addedAt  time.Time
}

type mediaCache struct {
	mu      sync.Mutex
	entries map[string]cachedMedia
}

func newMediaCache() *mediaCache {
	return &mediaCache{entries: make(map[string]cachedMedia)}
}

func (c *mediaCache) put(id string, msg *waE2E.Message, fileName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[id] = cachedMedia{msg: msg, fileName: fileName, addedAt: time.Now()}
}

func (c *mediaCache) get(id string) (cachedMedia, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	return entry, ok
}

// reap evicts expired entries until ctx is cancelled.
func (c *mediaCache) reap(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-mediaCacheTTL)
			c.mu.Lock()
			for id, entry := range c.entries {
				if entry.addedAt.Before(cutoff) {
					delete(c.entries, id)
				}
			}
			c.mu.Unlock()
		}
	}
}
