package chatpipeline

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHistoryMessageService serves a fixed message list for history loading;
// every other MessageService method panics via the embedded nil interface.
type fakeHistoryMessageService struct {
	interfaces.MessageService
	rows []*types.Message
}

func (f *fakeHistoryMessageService) GetRecentMessagesBySession(
	_ context.Context, _ string, _ int,
) ([]*types.Message, error) {
	return f.rows, nil
}

// TestLoadAndProcessHistory_AppendsManualReplies mirrors the Agent-mode test:
// operator manual replies fold into the preceding turn's Answer with the
// operator prefix, and a reply with no preceding retained turn is dropped.
func TestLoadAndProcessHistory_AppendsManualReplies(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{Role: "assistant", RequestID: "m0", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "开场人工", CreatedAt: base.Add(-time.Minute)},
		{Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{Role: "assistant", RequestID: "r1", Content: "回答一", CreatedAt: base.Add(time.Second)},
		{Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content:     "人工补充",
			Attachments: types.MessageAttachments{{FileName: "合同.pdf"}},
			CreatedAt:   base.Add(2 * time.Second)},
		{Role: "user", RequestID: "r2", Content: "问题二", CreatedAt: base.Add(3 * time.Second)},
		{Role: "assistant", RequestID: "r2", Content: "回答二", CreatedAt: base.Add(4 * time.Second)},
	}

	got, err := loadAndProcessHistory(context.Background(), &fakeHistoryMessageService{rows: rows}, "s1", 10, 50)
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.Equal(t, "问题一", got[0].Query)
	assert.Equal(t, "回答一\n\n[人工客服回复] 人工补充 (附文件: 合同.pdf)", got[0].Answer)
	assert.Equal(t, "问题二", got[1].Query)
	assert.Equal(t, "回答二", got[1].Answer)
}

// TestLoadAndProcessHistory_ManualReplyOutsideWindowDropped verifies that a
// manual reply belonging to a truncated turn disappears with it instead of
// being re-attached to a later turn.
func TestLoadAndProcessHistory_ManualReplyOutsideWindowDropped(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	rows := []*types.Message{
		{Role: "user", RequestID: "r1", Content: "问题一", CreatedAt: base},
		{Role: "assistant", RequestID: "r1", Content: "回答一", CreatedAt: base.Add(time.Second)},
		{Role: "assistant", RequestID: "m1", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "人工补充", CreatedAt: base.Add(2 * time.Second)},
		{Role: "user", RequestID: "r2", Content: "问题二", CreatedAt: base.Add(3 * time.Second)},
		{Role: "assistant", RequestID: "r2", Content: "回答二", CreatedAt: base.Add(4 * time.Second)},
	}

	got, err := loadAndProcessHistory(context.Background(), &fakeHistoryMessageService{rows: rows}, "s1", 1, 50)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "问题二", got[0].Query)
	assert.Equal(t, "回答二", got[0].Answer)
}
