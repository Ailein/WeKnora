package provider

import (
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

// CodexBaseURL is the ChatGPT-backend Codex API root (subscription channel).
// Kept here (not only in codexauth) so the provider registry can hand it to
// the frontend as the default Base URL.
const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// CodexProvider 通过 ChatGPT 订阅（Codex OAuth 凭证）调用 GPT-5.x 系列模型。
// 认证不是 API Key，而是会轮换的 OAuth token 对（access + refresh），协议是
// OpenAI Responses API 而非 Chat Completions，因此 chat 层有独立实现
// （internal/models/chat/codex_responses.go）。
type CodexProvider struct{}

func init() {
	Register(&CodexProvider{})
}

// Info 返回 Codex provider 的元数据
func (p *CodexProvider) Info() ProviderInfo {
	return ProviderInfo{
		Name:        ProviderCodex,
		DisplayName: "OpenAI Codex (ChatGPT 订阅)",
		Description: "ChatGPT Plus/Pro subscription via Codex OAuth (gpt-5.6 family)",
		DefaultURLs: map[types.ModelType]string{
			types.ModelTypeKnowledgeQA: CodexBaseURL,
			types.ModelTypeVLLM:        CodexBaseURL,
		},
		// 该通道只有一个 responses 端点：对话与视觉可用，
		// embedding/rerank/ASR 上游不存在。
		ModelTypes: []types.ModelType{
			types.ModelTypeKnowledgeQA,
			types.ModelTypeVLLM,
		},
		RequiresAuth: false, // 凭证走 OAuth token 对，不是 API key
	}
}

// ValidateConfig 验证 Codex provider 配置
func (p *CodexProvider) ValidateConfig(config *Config) error {
	if config.ModelName == "" {
		return fmt.Errorf("model name is required")
	}
	return nil
}
