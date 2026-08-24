package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// agentHistoryFetchMultiplier controls how many raw DB messages to fetch
// when assembling history. Each turn contributes ~2 rows (user + assistant);
// we ask for a generous multiple so we never under-fetch when some pairs are
// incomplete (e.g. an in-flight turn).
const agentHistoryFetchMultiplier = 4

// agentHistoryFetchMin is the floor for the DB fetch limit, used when
// maxRounds is small or unset.
const agentHistoryFetchMin = 50

var agentHistoryThinkTagRegex = regexp.MustCompile(`(?s)<think>.*?</think>`)

// LoadAgentHistory rebuilds the multi-turn LLM context for an Agent-mode
// session directly from the persistent messages table. The result is a
// chronologically ordered list of chat.Message entries suitable for prepending
// to the current turn (without system prompt; the engine adds that itself).
//
// For each historical turn it emits:
//  1. A user message (RenderedContent if present, else Content, plus any
//     image captions appended).
//  2. For each AgentStep with non-terminal tool calls (i.e. excluding
//     final_answer), an assistant message carrying the step's thought and
//     tool_calls, followed by one tool message per tool result.
//  3. A final assistant message with the canonical answer (msg.Content with
//     <think> blocks stripped).
//
// Turns lacking either user or assistant content are skipped. The newest
// maxRounds turns are returned in chronological order.
//
// DB is treated as the single source of truth — there is no Redis/in-memory
// cache layer above this function. Callers are expected to invoke it once
// per turn before handing the messages to the agent engine.
func LoadAgentHistory(
	ctx context.Context,
	messageRepo interfaces.MessageRepository,
	sessionID string,
	maxRounds int,
) ([]chat.Message, error) {
	if maxRounds <= 0 {
		return []chat.Message{}, nil
	}

	fetchLimit := maxRounds * agentHistoryFetchMultiplier
	if fetchLimit < agentHistoryFetchMin {
		fetchLimit = agentHistoryFetchMin
	}

	rows, err := messageRepo.GetRecentMessagesBySession(ctx, sessionID, fetchLimit)
	if err != nil {
		return nil, fmt.Errorf("load agent history: %w", err)
	}
	if len(rows) == 0 {
		return []chat.Message{}, nil
	}

	type pair struct {
		user      *types.Message
		assistant *types.Message
		createdAt time.Time
	}
	pairs := make(map[string]*pair)
	var sideNotes []*types.Message
	for _, msg := range rows {
		if types.IsIMSideNote(msg) {
			// Operator manual replies and takeover-period user messages never
			// have a paired counterpart; they are appended to the preceding
			// turn's answer below instead.
			sideNotes = append(sideNotes, msg)
			continue
		}
		p, ok := pairs[msg.RequestID]
		if !ok {
			p = &pair{}
			pairs[msg.RequestID] = p
		}
		switch msg.Role {
		case "user":
			p.user = msg
			if p.createdAt.IsZero() || msg.CreatedAt.Before(p.createdAt) {
				p.createdAt = msg.CreatedAt
			}
		case "assistant":
			p.assistant = msg
		}
	}

	completePairs := make([]*pair, 0, len(pairs))
	for _, p := range pairs {
		if p.user != nil && p.assistant != nil && p.assistant.IsCompleted {
			completePairs = append(completePairs, p)
		}
	}

	sort.Slice(completePairs, func(i, j int) bool {
		return completePairs[i].createdAt.Before(completePairs[j].createdAt)
	})

	if len(completePairs) > maxRounds {
		completePairs = completePairs[len(completePairs)-maxRounds:]
	}

	turnStarts := make([]time.Time, len(completePairs))
	for i, p := range completePairs {
		turnStarts[i] = p.createdAt
	}
	suffixes := imSideNoteSuffixes(turnStarts, sideNotes)

	out := make([]chat.Message, 0, len(completePairs)*4)
	for i, p := range completePairs {
		out = append(out, buildUserHistoryMessage(p.user))
		msgs := buildAssistantHistoryMessages(p.assistant)
		if suffixes[i] != "" {
			msgs = appendManualReplySuffix(msgs, suffixes[i])
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// imSideNoteSuffixes assigns each IM side note (operator manual reply or
// takeover-period user message) to the latest retained turn that started at or
// before it and returns one ready-to-append suffix per turn, in chronological
// order. Notes older than every retained turn are dropped: a lone leading
// assistant message would break providers that require strictly alternating
// roles, and the same policy keeps both history pipelines consistent.
func imSideNoteSuffixes(turnStarts []time.Time, sideNotes []*types.Message) []string {
	suffixes := make([]string, len(turnStarts))
	if len(turnStarts) == 0 || len(sideNotes) == 0 {
		return suffixes
	}
	sort.SliceStable(sideNotes, func(i, j int) bool {
		return sideNotes[i].CreatedAt.Before(sideNotes[j].CreatedAt)
	})
	for _, m := range sideNotes {
		idx := -1
		for i, start := range turnStarts {
			if !start.After(m.CreatedAt) {
				idx = i
			}
		}
		if idx < 0 {
			continue
		}
		text := types.IMSideNoteHistoryText(m)
		if text == "" {
			continue
		}
		suffixes[idx] += "\n\n" + text
	}
	return suffixes
}

// appendManualReplySuffix folds an IM side-note suffix into the turn's final
// answer message, keeping the assistant/user alternation intact. When the turn
// has no plain assistant message to extend (empty final answer), the suffix
// becomes its own trailing assistant message.
func appendManualReplySuffix(msgs []chat.Message, suffix string) []chat.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) == 0 {
			msgs[i].Content += suffix
			return msgs
		}
	}
	return append(msgs, chat.Message{Role: "assistant", Content: strings.TrimSpace(suffix)})
}

// buildUserHistoryMessage converts a stored user message into the chat.Message
// form that should appear in LLM history. It deliberately ignores
// RenderedContent: that field is a snapshot of the old prompt and retrieval
// context format, which must not be mixed into the current request protocol.
// Image captions and attachments are reconstructed from their canonical DB
// columns so useful user-provided context is retained without stale RAG data.
func buildUserHistoryMessage(m *types.Message) chat.Message {
	content := m.Content
	if captions := extractImageCaptionsFromMessage(m.Images); captions != "" {
		content += "\n\n[用户上传图片内容]\n" + captions
	}
	if len(m.Attachments) > 0 {
		content += m.Attachments.BuildPrompt()
	}
	return chat.Message{Role: "user", Content: content}
}

// buildAssistantHistoryMessages reconstructs the assistant side of one
// historical turn. It walks AgentSteps to expand intermediate tool calls into
// proper OpenAI-shaped assistant + tool messages, then emits the canonical
// final answer as a trailing assistant message.
//
// AgentSteps from KnowledgeQA-mode turns are empty, in which case the result
// is just the single final-answer assistant message — exactly mirroring how
// the KnowledgeQA pipeline replays history today.
func buildAssistantHistoryMessages(m *types.Message) []chat.Message {
	msgs := make([]chat.Message, 0, len(m.AgentSteps)*2+1)
	for _, step := range m.AgentSteps {
		nonTerminalCalls := filterNonTerminalToolCalls(step.ToolCalls)
		if len(nonTerminalCalls) == 0 {
			continue
		}
		assistantMsg := chat.Message{
			Role:             "assistant",
			Content:          step.Thought,
			ReasoningContent: step.ReasoningContent,
			ToolCalls:        make([]chat.ToolCall, 0, len(nonTerminalCalls)),
		}
		for _, tc := range nonTerminalCalls {
			argsJSON, _ := json.Marshal(tc.Args)
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, chat.ToolCall{
				ID:               tc.ID,
				Type:             "function",
				ProviderMetadata: tc.ProviderMetadata,
				Function: chat.FunctionCall{
					Name:      tc.Name,
					Arguments: string(argsJSON),
				},
			})
		}
		msgs = append(msgs, assistantMsg)
		for _, tc := range nonTerminalCalls {
			msgs = append(msgs, chat.Message{
				Role:       "tool",
				Content:    toolCallOutput(tc),
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
		}
	}

	finalContent := agentHistoryThinkTagRegex.ReplaceAllString(m.Content, "")
	finalContent = strings.TrimSpace(finalContent)
	if finalContent != "" {
		msgs = append(msgs, chat.Message{Role: "assistant", Content: finalContent})
	}
	return msgs
}

// legacyFinalAnswerToolName is the name of the now-removed final_answer tool.
// It is retained here only to filter such calls out of OLD persisted agent
// histories: pre-existing conversations recorded a final_answer tool call as
// the terminal step, and the canonical answer text is replayed via the
// trailing assistant message instead. Re-injecting it would duplicate the
// answer or confuse the model into thinking the previous turn is mid-flight.
const legacyFinalAnswerToolName = "final_answer"

// filterNonTerminalToolCalls drops legacy final_answer entries from historical
// tool calls (see legacyFinalAnswerToolName), plus the pipeline stages a
// fast-answer turn records for its own timeline (see
// types.PipelineToolCallIDPrefix): the model never issued those, so replaying
// them would attribute calls to it that it cannot answer for.
func filterNonTerminalToolCalls(calls []types.ToolCall) []types.ToolCall {
	out := make([]types.ToolCall, 0, len(calls))
	for _, tc := range calls {
		if tc.Name == legacyFinalAnswerToolName || types.IsPipelineToolCallID(tc.ID) {
			continue
		}
		out = append(out, tc)
	}
	return out
}

// toolCallOutput returns the textual content to use for a historical tool
// message: the recorded Output on success, or an "Error: …" line otherwise so
// the model can tell that an earlier tool call failed.
func toolCallOutput(tc types.ToolCall) string {
	if tc.Result == nil {
		return ""
	}
	if !tc.Result.Success {
		if tc.Result.Error != "" {
			return "Error: " + tc.Result.Error
		}
		return "Error: tool call failed"
	}
	return agenttools.CompactToolOutputForHistory(tc.Name, tc.Result)
}

// extractImageCaptionsFromMessage concatenates non-empty Caption fields from
// stored message images. Mirrors the helper used in chat_pipeline so both
// modes surface previous-turn image descriptions identically.
func extractImageCaptionsFromMessage(images types.MessageImages) string {
	var parts []string
	for _, img := range images {
		if img.Caption != "" {
			parts = append(parts, img.Caption)
		}
	}
	return strings.Join(parts, "\n")
}
