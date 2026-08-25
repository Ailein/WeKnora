package vlm

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/provider"
)

// CodexVLM serves the VLM interface over the ChatGPT-subscription Codex
// channel by delegating to the chat package's Responses-API client — the
// backend has a single endpoint for both text and vision.
type CodexVLM struct {
	inner *chat.CodexChat
}

// NewCodexVLM 创建基于 ChatGPT 订阅（Codex OAuth）的视觉模型实例。
func NewCodexVLM(config *Config) (*CodexVLM, error) {
	inner, err := chat.NewCodexChat(&chat.ChatConfig{
		ModelID:       config.ModelID,
		ModelName:     config.ModelName,
		BaseURL:       config.BaseURL,
		APIKey:        config.APIKey,
		RefreshToken:  config.RefreshToken,
		Provider:      string(provider.ProviderCodex),
		CustomHeaders: config.CustomHeaders,
		ExtraConfig:   stringAnyMapToStringMap(config.Extra),
	})
	if err != nil {
		return nil, err
	}
	return &CodexVLM{inner: inner}, nil
}

func stringAnyMapToStringMap(in map[string]any) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// Predict sends the images plus prompt as one user turn. Reasoning is pinned
// low: OCR/captioning wants extraction, not deliberation, and the VLM path
// has no budget for long thinking phases.
func (v *CodexVLM) Predict(ctx context.Context, imgBytesList [][]byte, prompt string) (string, error) {
	parts := []chat.MessageContentPart{{Type: "text", Text: prompt}}
	for _, imgBytes := range imgBytesList {
		if len(imgBytes) == 0 {
			continue
		}
		mimeType := detectImageMIME(imgBytes)
		dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(imgBytes))
		parts = append(parts, chat.MessageContentPart{
			Type:     "image_url",
			ImageURL: &chat.ImageURL{URL: dataURI, Detail: "auto"},
		})
	}

	logger.Infof(ctx, "[VLM] Calling Codex (ChatGPT subscription) API, model=%s, numImages=%d",
		v.inner.GetModelName(), len(imgBytesList))

	thinking := false
	resp, err := v.inner.Chat(ctx, []chat.Message{{Role: "user", MultiContent: parts}},
		&chat.ChatOptions{Thinking: &thinking})
	if err != nil {
		return "", fmt.Errorf("Codex VLM request: %w", err)
	}
	if strings.TrimSpace(resp.Content) == "" {
		return "", fmt.Errorf("Codex VLM returned no content (finish_reason=%s)", resp.FinishReason)
	}
	return resp.Content, nil
}

func (v *CodexVLM) GetModelName() string { return v.inner.GetModelName() }
func (v *CodexVLM) GetModelID() string   { return v.inner.GetModelID() }
