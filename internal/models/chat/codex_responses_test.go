package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func testCodexJWT(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]any{
		"exp": time.Now().Add(2 * time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-test",
		},
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func newTestCodexChat(t *testing.T) *CodexChat {
	t.Helper()
	c, err := NewCodexChat(&ChatConfig{
		ModelName: "gpt-5.6-terra",
		ModelID:   "codex-test-model",
		APIKey:    testCodexJWT(t),
		Provider:  "codex",
	})
	if err != nil {
		t.Fatalf("NewCodexChat: %v", err)
	}
	return c
}

func TestNewCodexChatRequiresCredentials(t *testing.T) {
	if _, err := NewCodexChat(&ChatConfig{ModelName: "gpt-5.6-terra"}); err == nil {
		t.Fatal("expected error without access/refresh token")
	}
}

func TestCodexEndpointResolution(t *testing.T) {
	cases := map[string]string{
		"":                                      "https://chatgpt.com/backend-api/codex/responses",
		"https://chatgpt.com/backend-api/codex": "https://chatgpt.com/backend-api/codex/responses",
		"https://chatgpt.com/backend-api/codex/responses": "https://chatgpt.com/backend-api/codex/responses",
		"https://chatgpt.com/backend-api":                 "https://chatgpt.com/backend-api/codex/responses",
	}
	for baseURL, want := range cases {
		// Constructed directly: NewCodexChat's SSRF check resolves DNS, which
		// is environment-dependent (fake-IP proxies map chatgpt.com into
		// 198.18.0.0/15) and irrelevant to URL-shape resolution.
		c := &CodexChat{baseURL: strings.TrimRight(baseURL, "/")}
		if c.baseURL == "" {
			c.baseURL = "https://chatgpt.com/backend-api/codex"
		}
		if got := c.endpoint(); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", baseURL, got, want)
		}
	}
}

func TestCodexBuildRequestShape(t *testing.T) {
	c := newTestCodexChat(t)
	thinking := true
	req := c.buildRequest([]Message{
		{Role: "system", Content: "You are a helpful CS agent."},
		{Role: "user", Content: "What is on the menu?", Images: []string{"data:image/png;base64,AAA"}},
		{Role: "assistant", Content: "Let me check.", ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: FunctionCall{Name: "search_kb", Arguments: `{"q":"menu"}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Name: "search_kb", Content: "menu: burgers"},
	}, &ChatOptions{
		Thinking: &thinking,
		Tools: []Tool{{Type: "function", Function: FunctionDef{
			Name: "search_kb", Description: "search", Parameters: json.RawMessage(`{"type":"object"}`),
		}}},
		ToolChoice: "auto",
	})

	if req.Store {
		t.Error("store must be false on the Codex channel")
	}
	if !req.Stream {
		t.Error("stream must always be true")
	}
	if req.Instructions != "You are a helpful CS agent." {
		t.Errorf("instructions = %q", req.Instructions)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "medium" || req.Reasoning.Summary != "auto" {
		t.Errorf("reasoning = %+v, want medium/auto", req.Reasoning)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "search_kb" || req.Tools[0].Type != "function" {
		t.Errorf("tools = %+v", req.Tools)
	}
	if req.Tools[0].Strict == nil || *req.Tools[0].Strict {
		t.Error("tools.strict must be false (non-strict schemas)")
	}
	if req.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v", req.ToolChoice)
	}

	// input: user message (image + text), assistant message, function_call, function_call_output
	if len(req.Input) != 4 {
		t.Fatalf("len(input) = %d, want 4: %+v", len(req.Input), req.Input)
	}
	user := req.Input[0]
	if user.Role != "user" || len(user.Content) != 2 ||
		user.Content[0].Type != "input_image" || user.Content[1].Type != "input_text" {
		t.Errorf("user item = %+v", user)
	}
	asst := req.Input[1]
	if asst.Role != "assistant" || asst.Content[0].Type != "output_text" || asst.Content[0].Annotations == nil {
		t.Errorf("assistant item = %+v", asst)
	}
	fc := req.Input[2]
	if fc.Type != "function_call" || fc.CallID != "call_1" || fc.Name != "search_kb" || fc.ID != "" {
		t.Errorf("function_call item = %+v (id must be omitted)", fc)
	}
	fco := req.Input[3]
	if fco.Type != "function_call_output" || fco.CallID != "call_1" || fco.Output != "menu: burgers" {
		t.Errorf("function_call_output item = %+v", fco)
	}
}

func TestCodexReasoningMapping(t *testing.T) {
	c := newTestCodexChat(t)

	off := false
	if r := c.reasoningFor(&ChatOptions{Thinking: &off}); r.Effort != "low" || r.Summary != "" {
		t.Errorf("thinking=false → %+v, want low without summary", r)
	}
	if r := c.reasoningFor(nil); r.Effort != "medium" || r.Summary != "auto" {
		t.Errorf("default → %+v, want medium/auto", r)
	}

	c.extraConfig = map[string]string{"reasoning_effort": "xhigh"}
	if r := c.reasoningFor(nil); r.Effort != "xhigh" {
		t.Errorf("extra_config override → %+v, want xhigh", r)
	}
	c.extraConfig = map[string]string{"reasoning_effort": "minimal"}
	if r := c.reasoningFor(nil); r.Effort != "low" {
		t.Errorf("minimal must clamp to low, got %+v", r)
	}
}

// sseStream renders events the way the Codex backend does.
func sseStream(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: ")
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	return b.String()
}

const codexCompletedEvent = `{"type":"response.completed","response":{"id":"resp_1","status":"completed",` +
	`"usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150,"input_tokens_details":{"cached_tokens":100}},` +
	`"output":[]}}`

func collectStream(t *testing.T, c *CodexChat, sse string) []types.StreamResponse {
	t.Helper()
	ch := make(chan types.StreamResponse, 64)
	go func() {
		defer close(ch)
		c.processCodexStream(context.Background(), strings.NewReader(sse), ch)
	}()
	var out []types.StreamResponse
	for r := range ch {
		out = append(out, r)
	}
	return out
}

func TestCodexStreamAnswerWithThinking(t *testing.T) {
	c := newTestCodexChat(t)
	sse := sseStream(
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"pondering"}`,
		`{"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hello "}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"world"}`,
		codexCompletedEvent,
	)
	out := collectStream(t, c, sse)

	var thinking, answer strings.Builder
	var thinkingDone bool
	var final *types.StreamResponse
	for i := range out {
		r := out[i]
		switch r.ResponseType {
		case types.ResponseTypeThinking:
			if r.Done {
				thinkingDone = true
			} else {
				thinking.WriteString(r.Content)
			}
		case types.ResponseTypeAnswer:
			answer.WriteString(r.Content)
			if r.Done {
				final = &out[i]
			}
		case types.ResponseTypeError:
			t.Fatalf("unexpected error frame: %s", r.Content)
		}
	}
	if thinking.String() != "pondering" || !thinkingDone {
		t.Errorf("thinking = %q done=%v", thinking.String(), thinkingDone)
	}
	if answer.String() != "Hello world" {
		t.Errorf("answer = %q", answer.String())
	}
	if final == nil {
		t.Fatal("missing final Done frame")
	}
	if final.FinishReason != "stop" {
		t.Errorf("finish = %q", final.FinishReason)
	}
	if final.Usage == nil || final.Usage.PromptTokens != 120 || final.Usage.CompletionTokens != 30 ||
		final.Usage.CacheReadTokens != 100 {
		t.Errorf("usage = %+v", final.Usage)
	}
}

func TestCodexStreamToolCalls(t *testing.T) {
	c := newTestCodexChat(t)
	sse := sseStream(
		`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"search_kb","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"q\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"menu\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"q\":\"menu\"}"}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"search_kb","arguments":"{\"q\":\"menu\"}"}}`,
		codexCompletedEvent,
	)
	out := collectStream(t, c, sse)

	var marker *types.StreamResponse
	var final *types.StreamResponse
	for i := range out {
		switch out[i].ResponseType {
		case types.ResponseTypeToolCall:
			marker = &out[i]
		case types.ResponseTypeAnswer:
			if out[i].Done {
				final = &out[i]
			}
		case types.ResponseTypeError:
			t.Fatalf("unexpected error frame: %s", out[i].Content)
		}
	}
	if marker == nil {
		t.Fatal("missing ResponseTypeToolCall marker")
	}
	if marker.Data["tool_name"] != "search_kb" || marker.Data["tool_call_id"] != "call_9" {
		t.Errorf("marker data = %+v", marker.Data)
	}
	if final == nil {
		t.Fatal("missing final frame")
	}
	if final.FinishReason != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", final.FinishReason)
	}
	if len(final.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", final.ToolCalls)
	}
	tc := final.ToolCalls[0]
	if tc.ID != "call_9" || tc.Function.Name != "search_kb" || tc.Function.Arguments != `{"q":"menu"}` {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestCodexStreamFailureEvent(t *testing.T) {
	c := newTestCodexChat(t)
	sse := sseStream(
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`,
	)
	out := collectStream(t, c, sse)
	last := out[len(out)-1]
	if last.ResponseType != types.ResponseTypeError || !strings.Contains(last.Content, "boom") {
		t.Errorf("last frame = %+v, want error containing boom", last)
	}
}

// TestCodexAggregateChatFromState verifies the shared state machine used by
// the non-streaming Chat(): tool calls missing from incremental events are
// reconciled from the authoritative response.completed payload, and message
// text falls back to the final output when no deltas arrived.
func TestCodexAggregateFromFinalResponse(t *testing.T) {
	state := newCodexStreamState()
	final := `{"type":"response.completed","response":{"id":"resp_2","status":"completed",` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":0}},` +
		`"output":[` +
		`{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"final text"}]},` +
		`{"type":"function_call","id":"fc_2","call_id":"call_2","name":"fn","arguments":"{}"}` +
		`]}}`
	var ev codexStreamEvent
	if err := json.Unmarshal([]byte(final), &ev); err != nil {
		t.Fatal(err)
	}
	state.apply(&ev, nil)

	if !state.completed {
		t.Fatal("state not completed")
	}
	if state.content.String() != "final text" {
		t.Errorf("content = %q", state.content.String())
	}
	calls := state.orderedToolCalls()
	if len(calls) != 1 || calls[0].ID != "call_2" || calls[0].Function.Name != "fn" {
		t.Errorf("tool calls = %+v", calls)
	}
	if state.finishReason != "tool_calls" {
		t.Errorf("finish = %q", state.finishReason)
	}
	if state.usage == nil || state.usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", state.usage)
	}
}

func TestCodexAPIErrorUsageLimit(t *testing.T) {
	err := codexAPIError(429, []byte(`{"error":{"code":"usage_limit_reached","message":"limit","plan_type":"PLUS","resets_at":1}}`))
	if !strings.Contains(err.Error(), "用量上限") || !strings.Contains(err.Error(), "plus") {
		t.Errorf("usage limit error = %v", err)
	}
	err = codexAPIError(500, []byte(`{"error":{"message":"internal"}}`))
	if !strings.Contains(err.Error(), "status 500") || !strings.Contains(err.Error(), "internal") {
		t.Errorf("generic error = %v", err)
	}
}

func TestCodexIncompleteMapsToLength(t *testing.T) {
	state := newCodexStreamState()
	raw := `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`
	var ev codexStreamEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	state.apply(&ev, nil)
	if state.finishReason != "max_output_tokens" {
		t.Errorf("finish = %q", state.finishReason)
	}
}
