package chatpipeline

import (
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

// TestAttachIMSideNotesToHistory covers the KnowledgeQA-mode folding of IM
// side notes: operator manual replies and takeover-period user messages are
// appended to the answer of the latest retained turn, chronologically, and a
// note older than every retained turn is dropped.
func TestAttachIMSideNotesToHistory(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	history := []*types.History{
		{Query: "问题一", Answer: "回答一", CreateAt: base},
	}
	notes := []*types.Message{
		{Role: "user", Channel: types.ChannelIMTakeover, IsCompleted: true,
			Content: "转人工", CreatedAt: base.Add(2 * time.Second)},
		{Role: "assistant", Channel: types.ChannelIMManual, IsCompleted: true,
			Content: "人工在", CreatedAt: base.Add(3 * time.Second)},
		{Role: "user", Channel: types.ChannelIMTakeover, IsCompleted: true,
			Content: "早于所有轮次", CreatedAt: base.Add(-time.Minute)},
	}

	attachIMSideNotesToHistory(history, notes)

	assert.Equal(t, "回答一"+
		"\n\n"+types.TakeoverUserHistoryPrefix+"转人工"+
		"\n\n"+types.ManualReplyHistoryPrefix+"人工在",
		history[0].Answer)
	assert.NotContains(t, history[0].Answer, "早于所有轮次")
}
