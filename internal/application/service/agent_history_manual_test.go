package service

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadAgentHistory_AppendsManualReplies verifies that operator manual
// replies (channel im_manual, no paired user message) are folded into the
// preceding turn's final answer with the operator prefix, media are summarized
// as text, and a reply older than every replayed turn is dropped rather than
// emitted as a leading assistant message.
func TestLoadAgentHistory_AppendsManualReplies(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{ID: "m0", Role: "assistant", RequestID: "m0", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "开场人工", CreatedAt: base.Add(-time.Minute)},
		{ID: "u1", Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{ID: "a1", Role: "assistant", RequestID: "r1", Content: "回答一", IsCompleted: true, CreatedAt: base.Add(time.Second)},
		{ID: "m1", Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content:   "人工补充",
			Images:    types.MessageImages{{URL: "data:image/png;base64,xx"}},
			CreatedAt: base.Add(2 * time.Second)},
		{ID: "u2", Role: "user", RequestID: "r2", Content: "问题二", CreatedAt: base.Add(3 * time.Second)},
		{ID: "a2", Role: "assistant", RequestID: "r2", Content: "回答二", IsCompleted: true, CreatedAt: base.Add(4 * time.Second)},
	}

	got, _, err := LoadAgentHistory(context.Background(), &historyRepo{rows: rows}, "s1", unlimitedBudget, false)
	require.NoError(t, err)
	require.Len(t, got, 4)

	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "问题一", got[0].Content)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Equal(t, "回答一\n\n[人工客服回复] 人工补充 (附 1 张图片)", got[1].Content)
	assert.Equal(t, "问题二", got[2].Content)
	assert.Equal(t, "回答二", got[3].Content)
	for _, m := range got {
		assert.NotContains(t, m.Content, "开场人工")
	}
}

// TestLoadAgentHistory_ManualReplyOutsideWindowDropped covers truncation: when
// the turn a manual reply belongs to is not replayed (covered by a context
// checkpoint, or left out for the token budget), the reply must not be
// re-attached to a later turn (that would reorder the conversation) — it is
// dropped together with its turn.
func TestLoadAgentHistory_ManualReplyOutsideWindowDropped(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{ID: "u1", Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{ID: "a1", Role: "assistant", RequestID: "r1", Content: "回答一", IsCompleted: true, CreatedAt: base.Add(time.Second)},
		{ID: "m1", Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "人工补充", CreatedAt: base.Add(2 * time.Second)},
		{ID: "u2", Role: "user", RequestID: "r2", Content: "问题二", CreatedAt: base.Add(3 * time.Second)},
		{ID: "a2", Role: "assistant", RequestID: "r2", Content: "回答二", IsCompleted: true, CreatedAt: base.Add(4 * time.Second)},
	}

	// The budget keeps only the newest turn.
	got, _, err := LoadAgentHistory(context.Background(), &historyRepo{rows: rows}, "s1", 1, false)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "问题二", got[0].Content)
	assert.Equal(t, "回答二", got[1].Content)

	// A checkpoint on the first turn covers it, and with it the reply.
	got, _, err = LoadAgentHistory(context.Background(),
		&historyRepo{rows: rows, checkpoint: checkpointOn(rows[1], "摘要")}, "s1", unlimitedBudget, false)
	require.NoError(t, err)
	for _, m := range got {
		assert.NotContains(t, m.Content, "人工补充")
	}
	assert.Equal(t, []string{"问题二"}, questions(got))
}

// TestLoadAgentHistory_ManualReplyWithEmptyFinalAnswer covers the edge where
// the preceding turn's assistant row is completed but has no text: the manual
// reply must still surface, as its own assistant message.
func TestLoadAgentHistory_ManualReplyWithEmptyFinalAnswer(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{ID: "u1", Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{ID: "a1", Role: "assistant", RequestID: "r1", Content: "", IsCompleted: true, CreatedAt: base.Add(time.Second)},
		{ID: "m1", Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "人工顶上", CreatedAt: base.Add(2 * time.Second)},
	}

	got, _, err := LoadAgentHistory(context.Background(), &historyRepo{rows: rows}, "s1", unlimitedBudget, false)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "问题一", got[0].Content)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Equal(t, "[人工客服回复] 人工顶上", got[1].Content)
	assert.Equal(t, "a1", got[1].TurnID, "the reply rides on its turn for checkpointing")
}

// TestLoadAgentHistory_SideNotesAcrossPages pins the paging argument behind
// sideNoteSuffix: a turn's notes are all read by the time the turn is
// complete, even when the read spans several pages.
func TestLoadAgentHistory_SideNotesAcrossPages(t *testing.T) {
	rows := storedTurns(agentHistoryPageSize) // fills more than one page
	last := rows[len(rows)-1]
	rows = append(rows, &types.Message{
		ID: "m-last", Role: "assistant", RequestID: "m-last", Channel: types.ChannelIMManual, IsCompleted: true,
		Content: "人工收尾", CreatedAt: last.CreatedAt.Add(time.Second),
	}, &types.Message{
		ID: "m-first", Role: "assistant", RequestID: "m-first", Channel: types.ChannelIMManual, IsCompleted: true,
		Content: "第一轮后的人工", CreatedAt: rows[1].CreatedAt.Add(time.Millisecond),
	})

	repo := &historyRepo{rows: rows}
	got, _, err := LoadAgentHistory(context.Background(), repo, "s1", unlimitedBudget, false)
	require.NoError(t, err)
	require.Greater(t, repo.pages, 1)
	assert.Equal(t, "answer 1\n\n[人工客服回复] 第一轮后的人工", got[1].Content)
	assert.Equal(t, last.Content+"\n\n[人工客服回复] 人工收尾", got[len(got)-1].Content)
}
