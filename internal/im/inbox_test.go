package im

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ── 纯函数 ──

func TestInboxPreview(t *testing.T) {
	if got := inboxPreview("  你好\n\n  世界\t! "); got != "你好 世界 !" {
		t.Fatalf("inboxPreview 应压平空白，got %q", got)
	}
	long := strings.Repeat("长", inboxPreviewMaxRunes+30)
	if got := inboxPreview(long); len([]rune(got)) != inboxPreviewMaxRunes {
		t.Fatalf("inboxPreview 应截断到 %d 字符，got %d", inboxPreviewMaxRunes, len([]rune(got)))
	}
}

// ── 活动记录 ──

func inboxTestChannelSession(id, sessionID string, tenantID uint64) *ChannelSession {
	return &ChannelSession{
		ID:          id,
		Platform:    "whatsapp",
		UserID:      "8613800138000",
		SessionID:   sessionID,
		TenantID:    tenantID,
		AgentID:     "agent-1",
		IMChannelID: "wa-channel",
	}
}

// drainInboxEvent returns the next buffered event or fails; the local hub
// publishes synchronously, so no waiting is involved.
func drainInboxEvent(t *testing.T, ch <-chan InboxEvent) InboxEvent {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	default:
		t.Fatal("期望收到收件箱事件，但通道为空")
		return InboxEvent{}
	}
}

func assertNoInboxEvent(t *testing.T, ch <-chan InboxEvent) {
	t.Helper()
	select {
	case evt := <-ch:
		t.Fatalf("不应收到事件，got %+v", evt)
	default:
	}
}

func TestNoteInboxActivityLifecycle(t *testing.T) {
	svc, _, _, _ := newManualReplyFixture(t)
	cs := inboxTestChannelSession("cs-note", "sess-note", 1)
	createManualReplySession(t, svc.db, cs)

	ctx := context.Background()
	svc.noteInboxActivity(ctx, "sess-note", inboxNote{Role: InboxRoleUser, Preview: "第一条  问题"})
	got := reloadChannelSession(t, svc, "cs-note")
	if got.OperatorUnreadCount != 1 || got.LastMessageRole != InboxRoleUser ||
		got.LastMessagePreview != "第一条 问题" || got.LastMessageAt == nil {
		t.Fatalf("用户消息应计未读并写入预览，got %+v", got)
	}

	svc.noteInboxActivity(ctx, "sess-note", inboxNote{Role: InboxRoleUser, Preview: "第二条"})
	if got = reloadChannelSession(t, svc, "cs-note"); got.OperatorUnreadCount != 2 {
		t.Fatalf("未读应累加到 2，got %d", got.OperatorUnreadCount)
	}

	// 机器人回复：预览更新，未读不变。
	svc.noteInboxActivity(ctx, "sess-note", inboxNote{Role: InboxRoleAssistant, Preview: "机器人答案"})
	got = reloadChannelSession(t, svc, "cs-note")
	if got.OperatorUnreadCount != 2 || got.LastMessageRole != InboxRoleAssistant {
		t.Fatalf("机器人回复不应改未读，got unread=%d role=%s", got.OperatorUnreadCount, got.LastMessageRole)
	}

	// 人工回复：预览更新且未读清零。
	svc.noteInboxActivity(ctx, "sess-note", inboxNote{Role: InboxRoleOperator, Preview: "人工回复", ResetUnread: true})
	got = reloadChannelSession(t, svc, "cs-note")
	if got.OperatorUnreadCount != 0 || got.LastMessageRole != InboxRoleOperator {
		t.Fatalf("人工回复应清零未读，got unread=%d role=%s", got.OperatorUnreadCount, got.LastMessageRole)
	}

	// 未绑定 IM 的会话：静默无操作。
	svc.noteInboxActivity(ctx, "sess-unknown", inboxNote{Role: InboxRoleUser, Preview: "x"})
}

func TestRememberPeerName(t *testing.T) {
	svc, _, _, _ := newManualReplyFixture(t)
	cs := inboxTestChannelSession("cs-peer", "sess-peer", 1)
	createManualReplySession(t, svc.db, cs)

	ctx := context.Background()
	svc.rememberPeerName(ctx, cs, "  张三  ")
	if got := reloadChannelSession(t, svc, "cs-peer"); got.PeerName != "张三" {
		t.Fatalf("应捕获去空白后的昵称，got %q", got.PeerName)
	}
	if cs.PeerName != "张三" {
		t.Fatalf("内存中的行也应更新，got %q", cs.PeerName)
	}

	// 空名与未变化的名字不产生写入；超长名截断。
	svc.rememberPeerName(ctx, cs, "")
	if got := reloadChannelSession(t, svc, "cs-peer"); got.PeerName != "张三" {
		t.Fatalf("空昵称不应覆盖已有值，got %q", got.PeerName)
	}
	long := strings.Repeat("名", inboxPeerNameMaxRunes+10)
	svc.rememberPeerName(ctx, cs, long)
	if got := reloadChannelSession(t, svc, "cs-peer"); len([]rune(got.PeerName)) != inboxPeerNameMaxRunes {
		t.Fatalf("超长昵称应截断到 %d 字符，got %d", inboxPeerNameMaxRunes, len([]rune(got.PeerName)))
	}
}

// ── 列表 ──

func TestListInboxPinningAndFilters(t *testing.T) {
	svc, _, _, _ := newManualReplyFixture(t)
	ctx := context.Background()
	now := time.Now()
	if err := svc.db.Model(&IMChannel{}).Where("id = ?", "wa-channel").
		Update("name", "客服号").Error; err != nil {
		t.Fatalf("rename channel: %v", err)
	}

	// A：人工接管中（未过期），未读 2，最近消息较旧 → 仍应置顶。
	future := now.Add(30 * time.Minute)
	oldTime := now.Add(-time.Hour)
	a := inboxTestChannelSession("cs-a", "sess-a", 1)
	a.HandlingMode = HandlingModeHuman
	a.HandlingExpiresAt = &future
	a.OperatorUnreadCount = 2
	a.LastMessageAt = &oldTime
	a.LastMessagePreview = "转人工"
	a.PeerName = "张三"
	createManualReplySession(t, svc.db, a)

	// B：机器人处理、telegram、无渠道绑定，最近消息最新。
	newest := now.Add(-time.Minute)
	b := inboxTestChannelSession("cs-b", "sess-b", 1)
	b.Platform = "telegram"
	b.IMChannelID = ""
	b.LastMessageAt = &newest
	createManualReplySession(t, svc.db, b)

	// C：接管已过期、未读 1、最近消息最旧 → 不置顶且显示为 bot。
	expired := now.Add(-time.Minute)
	oldest := now.Add(-2 * time.Hour)
	c := inboxTestChannelSession("cs-c", "sess-c", 1)
	c.HandlingMode = HandlingModeHuman
	c.HandlingExpiresAt = &expired
	c.OperatorUnreadCount = 1
	c.LastMessageAt = &oldest
	createManualReplySession(t, svc.db, c)

	// 其他租户的行必须不可见。
	other := inboxTestChannelSession("cs-other", "sess-other", 2)
	other.OperatorUnreadCount = 9
	createManualReplySession(t, svc.db, other)

	list, err := svc.ListInbox(ctx, 1, InboxListOptions{})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if list.Total != 3 || len(list.Items) != 3 {
		t.Fatalf("租户 1 应有 3 条会话，got total=%d items=%d", list.Total, len(list.Items))
	}
	if list.UnreadTotal != 3 {
		t.Fatalf("未读总数应为 3（2+1），got %d", list.UnreadTotal)
	}
	order := []string{list.Items[0].SessionID, list.Items[1].SessionID, list.Items[2].SessionID}
	if order[0] != "sess-a" || order[1] != "sess-b" || order[2] != "sess-c" {
		t.Fatalf("排序应为 接管置顶→按时间倒序，got %v", order)
	}

	itemA := list.Items[0]
	if itemA.ChannelName != "客服号" {
		t.Fatalf("应联出渠道名称，got %q", itemA.ChannelName)
	}
	if !itemA.ManualReplySupported || itemA.HandlingMode != HandlingModeHuman || itemA.PeerName != "张三" {
		t.Fatalf("A 条目字段异常：%+v", itemA)
	}
	if itemA.Title != "wa chat" {
		t.Fatalf("应联出会话标题，got %q", itemA.Title)
	}
	if list.Items[1].ManualReplySupported {
		t.Fatal("telegram 会话不应支持控制台回复")
	}
	if itemC := list.Items[2]; itemC.HandlingMode != HandlingModeBot || itemC.HandlingExpiresAt != nil {
		t.Fatalf("过期接管应显示为 bot，got %+v", itemC)
	}

	// filter=human 只含未过期接管。
	human, err := svc.ListInbox(ctx, 1, InboxListOptions{Filter: "human"})
	if err != nil {
		t.Fatalf("ListInbox human: %v", err)
	}
	if len(human.Items) != 1 || human.Items[0].SessionID != "sess-a" {
		t.Fatalf("human 过滤应只剩 sess-a，got %+v", human.Items)
	}

	// filter=unread 含 A 与 C。
	unread, err := svc.ListInbox(ctx, 1, InboxListOptions{Filter: "unread"})
	if err != nil {
		t.Fatalf("ListInbox unread: %v", err)
	}
	if len(unread.Items) != 2 {
		t.Fatalf("unread 过滤应有 2 条，got %d", len(unread.Items))
	}

	// 渠道过滤。
	byChannel, err := svc.ListInbox(ctx, 1, InboxListOptions{IMChannelID: "wa-channel"})
	if err != nil {
		t.Fatalf("ListInbox by channel: %v", err)
	}
	if len(byChannel.Items) != 2 {
		t.Fatalf("wa-channel 过滤应有 2 条（A、C），got %d", len(byChannel.Items))
	}

	// 分页。
	page2, err := svc.ListInbox(ctx, 1, InboxListOptions{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("ListInbox page2: %v", err)
	}
	if page2.Total != 3 || len(page2.Items) != 1 || page2.Items[0].SessionID != "sess-c" {
		t.Fatalf("第 2 页应只剩 sess-c，got %+v", page2.Items)
	}
}

// ── 已读与事件 ──

func TestMarkInboxReadAndEvent(t *testing.T) {
	svc, _, _, _ := newManualReplyFixture(t)
	ctx := context.Background()
	cs := inboxTestChannelSession("cs-read", "sess-read", 1)
	cs.OperatorUnreadCount = 5
	createManualReplySession(t, svc.db, cs)

	events, cancel := svc.SubscribeInbox(1)
	defer cancel()

	total, err := svc.MarkInboxRead(ctx, 1, "sess-read")
	if err != nil {
		t.Fatalf("MarkInboxRead: %v", err)
	}
	if total != 0 {
		t.Fatalf("清零后未读总数应为 0，got %d", total)
	}
	if got := reloadChannelSession(t, svc, "cs-read"); got.OperatorUnreadCount != 0 {
		t.Fatalf("未读应清零，got %d", got.OperatorUnreadCount)
	}
	evt := drainInboxEvent(t, events)
	if evt.Type != "session" || evt.Item == nil || evt.Item.SessionID != "sess-read" || evt.Item.UnreadCount != 0 {
		t.Fatalf("应推送清零后的会话事件，got %+v", evt)
	}

	// 幂等：已读会话再次标记不产生事件。
	if _, err := svc.MarkInboxRead(ctx, 1, "sess-read"); err != nil {
		t.Fatalf("重复 MarkInboxRead: %v", err)
	}
	assertNoInboxEvent(t, events)

	// 错误租户不可清零他人数据。
	cs2 := inboxTestChannelSession("cs-read2", "sess-read2", 1)
	cs2.OperatorUnreadCount = 1
	createManualReplySession(t, svc.db, cs2)
	if _, err := svc.MarkInboxRead(ctx, 42, "sess-read2"); err != nil {
		t.Fatalf("跨租户 MarkInboxRead: %v", err)
	}
	if got := reloadChannelSession(t, svc, "cs-read2"); got.OperatorUnreadCount != 1 {
		t.Fatalf("跨租户标记不应生效，got %d", got.OperatorUnreadCount)
	}
}

func TestInboxSubscribePublishOnActivity(t *testing.T) {
	svc, _, _, _ := newManualReplyFixture(t)
	ctx := context.Background()
	cs := inboxTestChannelSession("cs-evt", "sess-evt", 1)
	createManualReplySession(t, svc.db, cs)

	events, cancel := svc.SubscribeInbox(1)
	otherTenant, cancelOther := svc.SubscribeInbox(2)
	defer cancelOther()

	svc.noteInboxActivity(ctx, "sess-evt", inboxNote{Role: InboxRoleUser, Preview: "有人吗"})
	evt := drainInboxEvent(t, events)
	if evt.Type != "session" || evt.Item == nil {
		t.Fatalf("应收到 session 事件，got %+v", evt)
	}
	if evt.Item.LastMessagePreview != "有人吗" || evt.Item.UnreadCount != 1 || evt.UnreadTotal != 1 {
		t.Fatalf("事件应携带最新条目与未读总数，got %+v", evt)
	}
	// 事件按租户隔离。
	assertNoInboxEvent(t, otherTenant)

	// 退订后不再接收。
	cancel()
	svc.noteInboxActivity(ctx, "sess-evt", inboxNote{Role: InboxRoleUser, Preview: "还在吗"})
	assertNoInboxEvent(t, events)
}

// ── 快捷短语 ──

func TestQuickRepliesRoundTripAndValidation(t *testing.T) {
	svc, _, _, _ := newManualReplyFixture(t)
	ctx := context.Background()

	items, err := svc.GetQuickReplies(ctx, 1)
	if err != nil || len(items) != 0 {
		t.Fatalf("未设置时应返回空列表，got %v err=%v", items, err)
	}

	saved, err := svc.SetQuickReplies(ctx, 1, []string{"  您好，请稍等 ", "", "订单请提供单号"})
	if err != nil {
		t.Fatalf("SetQuickReplies: %v", err)
	}
	if len(saved) != 2 || saved[0] != "您好，请稍等" {
		t.Fatalf("应去空白并剔除空项，got %v", saved)
	}
	if items, err = svc.GetQuickReplies(ctx, 1); err != nil || len(items) != 2 || items[1] != "订单请提供单号" {
		t.Fatalf("回读应一致，got %v err=%v", items, err)
	}

	// 整体替换。
	if _, err = svc.SetQuickReplies(ctx, 1, []string{"新短语"}); err != nil {
		t.Fatalf("替换失败：%v", err)
	}
	if items, _ = svc.GetQuickReplies(ctx, 1); len(items) != 1 || items[0] != "新短语" {
		t.Fatalf("应整体替换，got %v", items)
	}

	// 租户隔离。
	if items, _ = svc.GetQuickReplies(ctx, 2); len(items) != 0 {
		t.Fatalf("其他租户应为空，got %v", items)
	}

	// 超限校验。
	tooMany := make([]string, maxQuickReplies+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	if _, err = svc.SetQuickReplies(ctx, 1, tooMany); !errors.Is(err, ErrQuickRepliesTooMany) {
		t.Fatalf("超量应报 ErrQuickRepliesTooMany，got %v", err)
	}
	if _, err = svc.SetQuickReplies(ctx, 1, []string{strings.Repeat("长", maxQuickReplyRunes+1)}); !errors.Is(err, ErrQuickReplyTooLong) {
		t.Fatalf("超长应报 ErrQuickReplyTooLong，got %v", err)
	}
}

// ── 与人工回复的联动 ──

func TestManualReplyResetsInboxUnread(t *testing.T) {
	svc, _, _, _ := newManualReplyFixture(t)
	cs := inboxTestChannelSession("cs-mr", "sess-mr", 1)
	cs.OperatorUnreadCount = 3
	createManualReplySession(t, svc.db, cs)

	if _, err := svc.SendManualReply(context.Background(), 1, "sess-mr", "好的，马上处理", nil); err != nil {
		t.Fatalf("SendManualReply: %v", err)
	}
	got := reloadChannelSession(t, svc, "cs-mr")
	if got.OperatorUnreadCount != 0 || got.LastMessageRole != InboxRoleOperator ||
		got.LastMessagePreview != "好的，马上处理" {
		t.Fatalf("人工回复应清零未读并更新预览，got %+v", got)
	}
}
