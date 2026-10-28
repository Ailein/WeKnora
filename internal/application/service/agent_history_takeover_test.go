package service

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadAgentHistory_AppendsTakeoverUserMessages verifies that user messages
// recorded while an operator held the conversation (channel im_takeover, no
// paired assistant answer) are folded into the preceding turn's answer,
// chronologically interleaved with operator manual replies, so the bot resumes
// with the full picture of the human conversation.
func TestLoadAgentHistory_AppendsTakeoverUserMessages(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{ID: "u1", Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{ID: "a1", Role: "assistant", RequestID: "r1", Content: "回答一", IsCompleted: true, CreatedAt: base.Add(time.Second)},
		// Operator takes over: the user keeps talking, the operator answers.
		{ID: "t1", Role: "user", RequestID: "t1", Channel: types.ChannelIMTakeover, IsCompleted: true,
			Content: "转人工，我要改订单", CreatedAt: base.Add(2 * time.Second)},
		{ID: "m1", Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "好的，已为您修改", CreatedAt: base.Add(3 * time.Second)},
		{ID: "t2", Role: "user", RequestID: "t2", Channel: types.ChannelIMTakeover, IsCompleted: true,
			Content: "谢谢", CreatedAt: base.Add(4 * time.Second)},
	}

	got, _, err := LoadAgentHistory(context.Background(), &historyRepo{rows: rows}, "s1", unlimitedBudget, false)
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.Equal(t, "问题一", got[0].Content)
	assert.Equal(t, "回答一"+
		"\n\n"+types.TakeoverUserHistoryPrefix+"转人工，我要改订单"+
		"\n\n"+types.ManualReplyHistoryPrefix+"好的，已为您修改"+
		"\n\n"+types.TakeoverUserHistoryPrefix+"谢谢",
		got[1].Content)
}

// TestLoadAgentHistory_TakeoverMessageOutsideWindowDropped mirrors the manual
// reply truncation policy: a takeover-period user message whose turn is not
// replayed must not leak into a later turn.
func TestLoadAgentHistory_TakeoverMessageOutsideWindowDropped(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{ID: "t0", Role: "user", RequestID: "t0", Channel: types.ChannelIMTakeover, IsCompleted: true,
			Content: "最早的人工期消息", CreatedAt: base.Add(-time.Minute)},
		{ID: "u1", Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{ID: "a1", Role: "assistant", RequestID: "r1", Content: "回答一", IsCompleted: true, CreatedAt: base.Add(time.Second)},
	}

	got, _, err := LoadAgentHistory(context.Background(), &historyRepo{rows: rows}, "s1", unlimitedBudget, false)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, m := range got {
		assert.NotContains(t, m.Content, "最早的人工期消息")
	}
}
