package service

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHistoryMessageRepo serves a fixed message list; every other repository
// method panics via the embedded nil interface.
type fakeHistoryMessageRepo struct {
	interfaces.MessageRepository
	rows []*types.Message
}

func (f *fakeHistoryMessageRepo) GetRecentMessagesBySession(
	_ context.Context, _ string, _ int,
) ([]*types.Message, error) {
	return f.rows, nil
}

// TestLoadAgentHistory_AppendsManualReplies verifies that operator manual
// replies (channel im_manual, no paired user message) are folded into the
// preceding turn's final answer with the operator prefix, media are summarized
// as text, and a reply older than every retained turn is dropped rather than
// emitted as a leading assistant message.
func TestLoadAgentHistory_AppendsManualReplies(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{Role: "assistant", RequestID: "m0", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "开场人工", CreatedAt: base.Add(-time.Minute)},
		{Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{Role: "assistant", RequestID: "r1", Content: "回答一", IsCompleted: true, CreatedAt: base.Add(time.Second)},
		{Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "人工补充",
			Images:  types.MessageImages{{URL: "data:image/png;base64,xx"}},
			CreatedAt: base.Add(2 * time.Second)},
		{Role: "user", RequestID: "r2", Content: "问题二", CreatedAt: base.Add(3 * time.Second)},
		{Role: "assistant", RequestID: "r2", Content: "回答二", IsCompleted: true, CreatedAt: base.Add(4 * time.Second)},
	}

	got, err := LoadAgentHistory(context.Background(), &fakeHistoryMessageRepo{rows: rows}, "s1", 10)
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
// maxRounds cuts off the turn a manual reply belongs to, the reply must not be
// re-attached to a later turn (that would reorder the conversation) — it is
// dropped together with its turn.
func TestLoadAgentHistory_ManualReplyOutsideWindowDropped(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{Role: "assistant", RequestID: "r1", Content: "回答一", IsCompleted: true, CreatedAt: base.Add(time.Second)},
		{Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "人工补充", CreatedAt: base.Add(2 * time.Second)},
		{Role: "user", RequestID: "r2", Content: "问题二", CreatedAt: base.Add(3 * time.Second)},
		{Role: "assistant", RequestID: "r2", Content: "回答二", IsCompleted: true, CreatedAt: base.Add(4 * time.Second)},
	}

	got, err := LoadAgentHistory(context.Background(), &fakeHistoryMessageRepo{rows: rows}, "s1", 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "问题二", got[0].Content)
	assert.Equal(t, "回答二", got[1].Content)
}

// TestLoadAgentHistory_ManualReplyWithEmptyFinalAnswer covers the edge where
// the preceding turn's assistant row is completed but has no text: the manual
// reply must still surface, as its own assistant message.
func TestLoadAgentHistory_ManualReplyWithEmptyFinalAnswer(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{Role: "assistant", RequestID: "r1", Content: "", IsCompleted: true, CreatedAt: base.Add(time.Second)},
		{Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "人工顶上", CreatedAt: base.Add(2 * time.Second)},
	}

	got, err := LoadAgentHistory(context.Background(), &fakeHistoryMessageRepo{rows: rows}, "s1", 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "问题一", got[0].Content)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Equal(t, "[人工客服回复] 人工顶上", got[1].Content)
}
