package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/codexauth"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
)

// CodexChat implements Chat over the ChatGPT-subscription Codex channel
// (chatgpt.com/backend-api/codex/responses). The wire protocol is the OpenAI
// Responses API — not Chat Completions — with rotating OAuth credentials, so
// it gets its own implementation alongside AnthropicChat rather than a
// providerAdapter on RemoteAPIChat.
//
// The backend only serves streaming responses, so the non-streaming Chat()
// aggregates the SSE stream into a single ChatResponse.
type CodexChat struct {
	modelName     string
	modelID       string
	baseURL       string
	tokenSource   *codexauth.TokenSource
	customHeaders map[string]string
	extraConfig   map[string]string
	// sessionID feeds both the session_id header and prompt_cache_key so
	// multi-turn calls from one client instance share the server-side cache.
	sessionID string
}

// NewCodexChat 创建 ChatGPT 订阅（Codex OAuth）聊天实例。
// config.APIKey 承载 access token，config.RefreshToken 承载轮换刷新令牌。
func NewCodexChat(config *ChatConfig) (*CodexChat, error) {
	if config.BaseURL != "" {
		if err := secutils.ValidateURLForSSRF(config.BaseURL); err != nil {
			return nil, fmt.Errorf("baseURL SSRF check failed: %w", err)
		}
	}
	if strings.TrimSpace(config.APIKey) == "" && strings.TrimSpace(config.RefreshToken) == "" {
		return nil, fmt.Errorf("Codex provider: 请导入 ChatGPT 订阅凭证（~/.codex/auth.json 的 access_token / refresh_token）")
	}

	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = codexauth.DefaultBaseURL
	}

	return &CodexChat{
		modelName:     config.ModelName,
		modelID:       config.ModelID,
		baseURL:       baseURL,
		tokenSource:   codexauth.GetTokenSource(config.ModelID, strings.TrimSpace(config.APIKey), strings.TrimSpace(config.RefreshToken)),
		customHeaders: config.CustomHeaders,
		extraConfig:   config.ExtraConfig,
		sessionID:     uuid.New().String(),
	}, nil
}

func (c *CodexChat) GetModelName() string { return c.modelName }
func (c *CodexChat) GetModelID() string   { return c.modelID }

// endpoint mirrors pi-ai's resolveCodexUrl: accept a bare host, a /codex
// root, or a full /codex/responses URL.
func (c *CodexChat) endpoint() string {
	base := strings.TrimRight(c.baseURL, "/")
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	if strings.HasSuffix(base, "/codex") {
		return base + "/responses"
	}
	return base + "/codex/responses"
}

// ---------------------------------------------------------------------------
// Request building (Responses API)
// ---------------------------------------------------------------------------

type codexContentPart struct {
	Type     string `json:"type"` // input_text | input_image | output_text
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// Annotations must be present (even empty) on output_text parts.
	Annotations *[]any `json:"annotations,omitempty"`
}

type codexInputItem struct {
	// Message items: role + content. Function items: type + call fields.
	Type    string             `json:"type,omitempty"` // message | function_call | function_call_output
	Role    string             `json:"role,omitempty"`
	Content []codexContentPart `json:"content,omitempty"`
	Status  string             `json:"status,omitempty"`
	ID      string             `json:"id,omitempty"`
	CallID  string             `json:"call_id,omitempty"`
	Name    string             `json:"name,omitempty"`
	// Arguments (function_call) and Output (function_call_output) are plain
	// strings on the wire.
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type codexTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict"`
}

type codexReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

type codexRequest struct {
	Model             string           `json:"model"`
	Store             bool             `json:"store"`
	Stream            bool             `json:"stream"`
	Instructions      string           `json:"instructions,omitempty"`
	Input             []codexInputItem `json:"input"`
	Tools             []codexTool      `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	ParallelToolCalls bool             `json:"parallel_tool_calls"`
	Reasoning         *codexReasoning  `json:"reasoning,omitempty"`
	PromptCacheKey    string           `json:"prompt_cache_key,omitempty"`
}

var codexReasoningEfforts = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

func (c *CodexChat) buildRequest(messages []Message, opts *ChatOptions) *codexRequest {
	req := &codexRequest{
		Model:             c.modelName,
		Store:             false, // the Codex backend rejects store:true
		Stream:            true,  // the backend only streams; Chat() aggregates
		ParallelToolCalls: true,
		PromptCacheKey:    c.sessionID,
	}

	var systemParts []string
	msgIndex := 0
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			if text := messageText(msg); text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			parts := userContentParts(msg)
			if len(parts) > 0 {
				req.Input = append(req.Input, codexInputItem{Type: "message", Role: "user", Content: parts})
			}
		case "assistant":
			if text := messageText(msg); text != "" {
				empty := []any{}
				req.Input = append(req.Input, codexInputItem{
					Type:   "message",
					Role:   "assistant",
					Status: "completed",
					ID:     fmt.Sprintf("msg_%d", msgIndex),
					Content: []codexContentPart{{
						Type: "output_text", Text: text, Annotations: &empty,
					}},
				})
			}
			// Prior-turn tool calls replay as function_call items. The item id
			// is deliberately omitted — with store:false the server would try
			// to pair a supplied fc_* id with a reasoning item we don't replay.
			for _, tc := range msg.ToolCalls {
				req.Input = append(req.Input, codexInputItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		case "tool":
			output := msg.Content
			if output == "" {
				output = messageText(msg)
			}
			req.Input = append(req.Input, codexInputItem{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: output,
			})
		}
		msgIndex++
	}
	req.Instructions = strings.Join(systemParts, "\n\n")

	if opts != nil {
		for _, tool := range opts.Tools {
			strict := false
			req.Tools = append(req.Tools, codexTool{
				Type:        "function",
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  tool.Function.Parameters,
				Strict:      &strict,
			})
		}
		if len(opts.Tools) > 0 {
			switch opts.ToolChoice {
			case "", "auto":
				req.ToolChoice = "auto"
			case "none", "required":
				req.ToolChoice = opts.ToolChoice
			default: // a specific function name
				req.ToolChoice = map[string]string{"type": "function", "name": opts.ToolChoice}
			}
		}
		if opts.ParallelToolCalls != nil {
			req.ParallelToolCalls = *opts.ParallelToolCalls
		}
		// JSON-format requests follow the Chat Completions path's convention:
		// inject the schema into the last message instead of using strict
		// text.format (which the Codex backend gates behind strict schemas).
		if len(opts.Format) > 0 && len(req.Input) > 0 {
			last := &req.Input[len(req.Input)-1]
			if len(last.Content) > 0 && last.Content[len(last.Content)-1].Type == "input_text" {
				last.Content[len(last.Content)-1].Text += fmt.Sprintf("\nUse this JSON schema: %s", opts.Format)
			} else if last.Type == "function_call_output" {
				last.Output += fmt.Sprintf("\nUse this JSON schema: %s", opts.Format)
			}
		}
	}
	req.Reasoning = c.reasoningFor(opts)

	// max_tokens / max_output_tokens is intentionally NOT sent: the Codex
	// backend rejects output caps (both Hermes and pi/openclaw omit it).
	// Sampling params (temperature/top_p) are likewise unsupported there.
	return req
}

// reasoningFor maps WeKnora's Thinking switch onto Responses reasoning
// effort. extra_config.reasoning_effort overrides the "thinking on" effort.
func (c *CodexChat) reasoningFor(opts *ChatOptions) *codexReasoning {
	base := "medium"
	if c.extraConfig != nil {
		if v := strings.ToLower(strings.TrimSpace(c.extraConfig["reasoning_effort"])); codexReasoningEfforts[v] {
			base = v
		}
	}
	if base == "minimal" { // gpt-5.2+ rejects minimal; clamp like pi-ai does
		base = "low"
	}
	if opts != nil && opts.Thinking != nil && !*opts.Thinking {
		return &codexReasoning{Effort: "low"}
	}
	return &codexReasoning{Effort: base, Summary: "auto"}
}

func messageText(msg Message) string {
	if strings.TrimSpace(msg.Content) != "" {
		return msg.Content
	}
	return textFromMultiContent(msg.MultiContent)
}

func userContentParts(msg Message) []codexContentPart {
	var parts []codexContentPart
	if len(msg.MultiContent) > 0 {
		for _, p := range msg.MultiContent {
			switch p.Type {
			case "text":
				if p.Text != "" {
					parts = append(parts, codexContentPart{Type: "input_text", Text: p.Text})
				}
			case "image_url":
				if p.ImageURL != nil && p.ImageURL.URL != "" {
					detail := p.ImageURL.Detail
					if detail == "" {
						detail = "auto"
					}
					parts = append(parts, codexContentPart{
						Type: "input_image", ImageURL: resolveImageURLForLLM(p.ImageURL.URL), Detail: detail,
					})
				}
			}
		}
		return parts
	}
	for _, img := range msg.Images {
		parts = append(parts, codexContentPart{
			Type: "input_image", ImageURL: resolveImageURLForLLM(img), Detail: "auto",
		})
	}
	if msg.Content != "" {
		parts = append(parts, codexContentPart{Type: "input_text", Text: msg.Content})
	}
	return parts
}

// ---------------------------------------------------------------------------
// HTTP + auth
// ---------------------------------------------------------------------------

// doRequest sends the request with a fresh token, force-refreshing once on
// 401 (the provider can revoke access tokens before their JWT exp).
func (c *CodexChat) doRequest(ctx context.Context, jsonBody []byte) (*http.Response, error) {
	endpoint := c.endpoint()
	if err := secutils.ValidateURLForSSRF(endpoint); err != nil {
		return nil, fmt.Errorf("endpoint SSRF check failed: %w", err)
	}

	send := func() (*http.Response, error) {
		token, accountID, err := c.tokenSource.Token(ctx)
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		codexauth.ApplyHeaders(httpReq.Header, token, accountID, c.sessionID)
		secutils.ApplyCustomHeaders(httpReq, c.customHeaders)
		return rawHTTPClient.Do(httpReq)
	}

	resp, err := send()
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		logger.Warnf(ctx, "[Codex] 401 from backend, force-refreshing token and retrying once")
		if err := c.tokenSource.ForceRefresh(ctx); err != nil {
			return nil, err
		}
		resp, err = send()
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, codexAPIError(resp.StatusCode, body)
	}
	return resp, nil
}

// codexAPIError turns backend errors into actionable messages — most
// importantly the subscription usage-limit shape.
func codexAPIError(status int, body []byte) error {
	var parsed struct {
		Error struct {
			Code     string  `json:"code"`
			Type     string  `json:"type"`
			Message  string  `json:"message"`
			PlanType string  `json:"plan_type"`
			ResetsAt float64 `json:"resets_at"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	code := parsed.Error.Code
	if code == "" {
		code = parsed.Error.Type
	}
	lower := strings.ToLower(code)
	if status == http.StatusTooManyRequests ||
		strings.Contains(lower, "usage_limit_reached") ||
		strings.Contains(lower, "usage_not_included") ||
		strings.Contains(lower, "rate_limit_exceeded") {
		plan := ""
		if parsed.Error.PlanType != "" {
			plan = fmt.Sprintf("（%s 套餐）", strings.ToLower(parsed.Error.PlanType))
		}
		return fmt.Errorf("已达 ChatGPT 订阅用量上限%s，请稍后再试或升级套餐 (HTTP %d: %s)",
			plan, status, firstNonEmpty(parsed.Error.Message, string(truncateBytes(body, 200))))
	}
	if parsed.Error.Message != "" {
		return fmt.Errorf("API request failed with status %d: %s", status, parsed.Error.Message)
	}
	return fmt.Errorf("API request failed with status %d: %s", status, truncateBytes(body, 500))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func truncateBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// ---------------------------------------------------------------------------
// SSE event model
// ---------------------------------------------------------------------------

type codexItem struct {
	Type      string `json:"type"` // message | reasoning | function_call
	ID        string `json:"id"`
	Status    string `json:"status"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

type codexResponsePayload struct {
	ID     string      `json:"id"`
	Status string      `json:"status"`
	Output []codexItem `json:"output"`
	Usage  *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		TotalTokens        int `json:"total_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

type codexStreamEvent struct {
	Type      string                `json:"type"`
	Delta     string                `json:"delta"`
	ItemID    string                `json:"item_id"`
	Arguments string                `json:"arguments"`
	Item      *codexItem            `json:"item"`
	Response  *codexResponsePayload `json:"response"`
	Code      string                `json:"code"`
	Message   string                `json:"message"`
}

// codexStreamState accumulates one response across SSE events.
type codexStreamState struct {
	thinkingEmitter
	content      strings.Builder
	reasoning    strings.Builder
	toolOrder    []string
	toolByItem   map[string]*types.LLMToolCall
	argsDone     map[string]bool
	notified     map[string]bool
	usage        *types.TokenUsage
	finishReason string
	completed    bool
	err          error
}

func newCodexStreamState() *codexStreamState {
	return &codexStreamState{
		toolByItem: map[string]*types.LLMToolCall{},
		argsDone:   map[string]bool{},
		notified:   map[string]bool{},
	}
}

func (s *codexStreamState) orderedToolCalls() []types.LLMToolCall {
	if len(s.toolOrder) == 0 {
		return nil
	}
	out := make([]types.LLMToolCall, 0, len(s.toolOrder))
	for _, id := range s.toolOrder {
		if tc := s.toolByItem[id]; tc != nil && tc.Function.Name != "" {
			out = append(out, *tc)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// apply folds one event into the state. When streamChan is non-nil the
// user-visible chunks (thinking / answer deltas, tool-call markers) are
// emitted with the same semantics as processRawHTTPStream.
func (s *codexStreamState) apply(event *codexStreamEvent, streamChan chan types.StreamResponse) {
	switch event.Type {
	case "response.output_item.added":
		if event.Item != nil && event.Item.Type == "function_call" {
			itemID := event.Item.ID
			if itemID == "" {
				itemID = event.Item.CallID
			}
			if _, ok := s.toolByItem[itemID]; !ok {
				s.toolByItem[itemID] = &types.LLMToolCall{
					ID:   event.Item.CallID,
					Type: "function",
					Function: types.FunctionCall{
						Name:      event.Item.Name,
						Arguments: event.Item.Arguments,
					},
				}
				s.toolOrder = append(s.toolOrder, itemID)
			}
			s.notifyToolCall(itemID, streamChan)
		}

	case "response.reasoning_summary_text.delta":
		if event.Delta != "" {
			s.reasoning.WriteString(event.Delta)
			if streamChan != nil {
				s.emit(streamChan, event.Delta)
			}
		}

	case "response.reasoning_summary_part.done":
		s.reasoning.WriteString("\n\n")
		if streamChan != nil && s.active {
			s.emit(streamChan, "\n\n")
		}

	case "response.output_text.delta", "response.refusal.delta":
		if event.Delta != "" {
			s.content.WriteString(event.Delta)
			if streamChan != nil {
				s.finish(streamChan)
				streamChan <- types.StreamResponse{
					ResponseType: types.ResponseTypeAnswer,
					Content:      event.Delta,
				}
			}
		}

	case "response.function_call_arguments.delta":
		if tc := s.toolByItem[event.ItemID]; tc != nil && !s.argsDone[event.ItemID] {
			tc.Function.Arguments += event.Delta
		}

	case "response.function_call_arguments.done":
		if tc := s.toolByItem[event.ItemID]; tc != nil && event.Arguments != "" {
			tc.Function.Arguments = event.Arguments
			s.argsDone[event.ItemID] = true
		}

	case "response.output_item.done":
		if event.Item != nil && event.Item.Type == "function_call" {
			itemID := event.Item.ID
			if itemID == "" {
				itemID = event.Item.CallID
			}
			tc := s.toolByItem[itemID]
			if tc == nil {
				tc = &types.LLMToolCall{Type: "function"}
				s.toolByItem[itemID] = tc
				s.toolOrder = append(s.toolOrder, itemID)
			}
			if event.Item.CallID != "" {
				tc.ID = event.Item.CallID
			}
			if event.Item.Name != "" {
				tc.Function.Name = event.Item.Name
			}
			if event.Item.Arguments != "" {
				tc.Function.Arguments = event.Item.Arguments
				s.argsDone[itemID] = true
			}
			s.notifyToolCall(itemID, streamChan)
		}

	case "response.completed", "response.done", "response.incomplete":
		s.completed = true
		if event.Response != nil {
			s.absorbFinalResponse(event.Response)
		}
		if s.finishReason == "" {
			s.finishReason = "stop"
		}
		if len(s.orderedToolCalls()) > 0 && s.finishReason == "stop" {
			s.finishReason = "tool_calls"
		}

	case "response.failed":
		msg := "Codex response failed"
		if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
			msg = event.Response.Error.Message
		}
		s.err = fmt.Errorf("API stream error: %s", msg)

	case "error":
		msg := event.Message
		if msg == "" {
			msg = event.Code
		}
		s.err = fmt.Errorf("API stream error: %s", msg)
	}
}

// notifyToolCall emits the one-time ResponseTypeToolCall marker once both the
// function name and call id are known — same contract as processToolCallsDelta.
func (s *codexStreamState) notifyToolCall(itemID string, streamChan chan types.StreamResponse) {
	tc := s.toolByItem[itemID]
	if streamChan == nil || tc == nil || s.notified[itemID] || tc.Function.Name == "" || tc.ID == "" {
		return
	}
	s.notified[itemID] = true
	streamChan <- types.StreamResponse{
		ResponseType: types.ResponseTypeToolCall,
		Data: map[string]interface{}{
			"tool_name":    tc.Function.Name,
			"tool_call_id": tc.ID,
		},
	}
}

// absorbFinalResponse reconciles state with the authoritative response object
// from response.completed: usage, status, and any function_call item the
// incremental events missed.
func (s *codexStreamState) absorbFinalResponse(resp *codexResponsePayload) {
	if resp.Usage != nil {
		cached := resp.Usage.InputTokensDetails.CachedTokens
		usage := types.TokenUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		usage.SetPromptCacheUsage(cached, 0, max(0, resp.Usage.InputTokens-cached), true)
		s.usage = &usage
	}
	switch resp.Status {
	case "incomplete":
		s.finishReason = "length"
		if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason != "" {
			s.finishReason = resp.IncompleteDetails.Reason
		}
	case "failed", "cancelled":
		if s.err == nil {
			msg := resp.Status
			if resp.Error != nil && resp.Error.Message != "" {
				msg = resp.Error.Message
			}
			s.err = fmt.Errorf("API stream error: %s", msg)
		}
	default:
		s.finishReason = "stop"
	}
	for _, item := range resp.Output {
		if item.Type != "function_call" {
			continue
		}
		itemID := item.ID
		if itemID == "" {
			itemID = item.CallID
		}
		tc := s.toolByItem[itemID]
		if tc == nil {
			tc = &types.LLMToolCall{Type: "function"}
			s.toolByItem[itemID] = tc
			s.toolOrder = append(s.toolOrder, itemID)
		}
		if item.CallID != "" {
			tc.ID = item.CallID
		}
		if item.Name != "" {
			tc.Function.Name = item.Name
		}
		if item.Arguments != "" {
			tc.Function.Arguments = item.Arguments
		}
	}
	// Fallback for aggregation callers: if no output_text deltas arrived,
	// take the final message text from the response object.
	if s.content.Len() == 0 {
		for _, item := range resp.Output {
			if item.Type != "message" {
				continue
			}
			for _, part := range item.Content {
				if part.Text != "" {
					s.content.WriteString(part.Text)
				} else if part.Refusal != "" {
					s.content.WriteString(part.Refusal)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Chat interface
// ---------------------------------------------------------------------------

// Chat 非流式：请求仍以流式发出（后端只支持流式），本地聚合成完整响应。
func (c *CodexChat) Chat(ctx context.Context, messages []Message, opts *ChatOptions) (*types.ChatResponse, error) {
	ctx, cancel := withLLMTimeout(ctx, defaultChatTimeout)
	defer cancel()

	jsonBody, err := json.Marshal(c.buildRequest(messages, opts))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	logger.Infof(ctx, "[Codex Request] endpoint=%s, model=%s, request:\n%s",
		c.endpoint(), c.modelName, secutils.CompactImageDataURLForLog(string(jsonBody)))

	resp, err := c.doRequest(ctx, jsonBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	state := newCodexStreamState()
	reader := NewSSEReader(resp.Body)
	for {
		event, err := reader.ReadEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read SSE response: %w", err)
		}
		if event == nil || event.Done || len(event.Data) == 0 {
			if event != nil && event.Done {
				break
			}
			continue
		}
		var streamEvent codexStreamEvent
		if err := json.Unmarshal(event.Data, &streamEvent); err != nil {
			logger.Warnf(ctx, "[Codex] skip unparsable SSE event: %v", err)
			continue
		}
		state.apply(&streamEvent, nil)
		if state.err != nil {
			return nil, state.err
		}
		if state.completed {
			break
		}
	}
	if state.err != nil {
		return nil, state.err
	}

	usage := types.TokenUsage{}
	if state.usage != nil {
		usage = *state.usage
	}
	result := &types.ChatResponse{
		Content:          state.content.String(),
		ReasoningContent: strings.TrimSpace(state.reasoning.String()),
		ToolCalls:        state.orderedToolCalls(),
		FinishReason:     state.finishReason,
		Usage:            usage,
	}
	logUsage(ctx, c.modelName, &result.Usage)
	return result, nil
}

// ChatStream 流式聊天。
func (c *CodexChat) ChatStream(ctx context.Context, messages []Message, opts *ChatOptions) (<-chan types.StreamResponse, error) {
	timeoutCtx, cancel := withLLMTimeout(ctx, defaultStreamTimeout)

	jsonBody, err := json.Marshal(c.buildRequest(messages, opts))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	logger.Infof(timeoutCtx, "[Codex Stream Request] endpoint=%s, model=%s, request:\n%s",
		c.endpoint(), c.modelName, secutils.CompactImageDataURLForLog(string(jsonBody)))

	resp, err := c.doRequest(timeoutCtx, jsonBody)
	if err != nil {
		cancel()
		return nil, err
	}

	streamChan := make(chan types.StreamResponse)
	go func() {
		defer cancel()
		defer close(streamChan)
		defer resp.Body.Close()
		c.processCodexStream(timeoutCtx, resp.Body, streamChan)
	}()
	return streamChan, nil
}

func (c *CodexChat) processCodexStream(ctx context.Context, body io.Reader, streamChan chan types.StreamResponse) {
	state := newCodexStreamState()
	reader := NewSSEReader(body)

	finish := func() {
		state.finish(streamChan)
		logUsage(ctx, c.modelName, state.usage)
		streamChan <- types.StreamResponse{
			ResponseType: types.ResponseTypeAnswer,
			Done:         true,
			ToolCalls:    state.orderedToolCalls(),
			Usage:        state.usage,
			FinishReason: state.finishReason,
		}
	}

	for {
		event, err := reader.ReadEvent()
		if err != nil {
			if err == io.EOF {
				finish()
			} else {
				logger.Errorf(ctx, "[Codex] stream read error: %v", err)
				streamChan <- types.StreamResponse{
					ResponseType: types.ResponseTypeError,
					Content:      err.Error(),
					Done:         true,
				}
			}
			return
		}
		if event == nil {
			continue
		}
		if event.Done {
			finish()
			return
		}
		if len(event.Data) == 0 {
			continue
		}

		var streamEvent codexStreamEvent
		if err := json.Unmarshal(event.Data, &streamEvent); err != nil {
			logger.Warnf(ctx, "[Codex] skip unparsable SSE event: %v", err)
			continue
		}
		state.apply(&streamEvent, streamChan)
		if state.err != nil {
			streamChan <- types.StreamResponse{
				ResponseType: types.ResponseTypeError,
				Content:      state.err.Error(),
				Done:         true,
			}
			return
		}
		if state.completed {
			finish()
			return
		}
	}
}
