// Codex is the ChatGPT-subscription channel (fork addition, not upstream).
//
// Facts (codex-cli / pi-ai reference clients; verified against the live
// endpoint on a Plus subscription):
//   - the only endpoint is POST {base}/responses under
//     https://chatgpt.com/backend-api/codex, speaking an OpenAI Responses
//     dialect that only streams, rejects store:true and output caps, and
//     requires instructions;
//   - credentials are a rotating OAuth pair (access + single-use refresh
//     token) from "Sign in with ChatGPT", not an API key — see
//     internal/models/codexauth. The access token rides in the row's api_key
//     and the refresh token in parameters.refresh_token;
//   - chat and vision only: the subscription exposes no embedding, rerank or
//     ASR surface.
//
// The wire client is internal/models/chat/codex_responses.go; this definition
// only makes the vendor visible to the catalog (editor picker, capability
// resolution, URL detection). Its catalog entries are derived from OpenAI's
// reviewed metadata in runtime/fork_providers.go, so the generated catalog
// stays byte-identical to upstream.
package providers

import (
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/types"
)

// CodexID is the provider identifier stored on model rows.
const CodexID = "codex"

// CodexBaseURL is the ChatGPT-backend Codex API root (subscription channel).
const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// CodexModels lists the models the subscription channel serves, first one
// being the editor's default. The live list (codexauth.ListModels) supersedes
// it in the editor whenever the backend is reachable.
var CodexModels = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
}

// NewCodexProvider returns the Codex vendor definition.
func NewCodexProvider() *Definition {
	return &Definition{
		ID:    CodexID,
		Name:  "OpenAI Codex (ChatGPT subscription)",
		Names: map[string]string{"zh-CN": "OpenAI Codex（ChatGPT 订阅）"},
		Description: "ChatGPT Plus/Pro subscription via Codex OAuth: gpt-5.6-sol, gpt-5.6-terra, " +
			"gpt-5.6-luna, gpt-5.5 (chat and vision)",
		Descriptions: map[string]string{
			"zh-CN": "通过 ChatGPT 账号授权（Codex OAuth）使用 Plus/Pro 订阅额度：gpt-5.6-sol、gpt-5.6-terra、gpt-5.6-luna、gpt-5.5（对话与视觉）",
		},
		Website: "https://chatgpt.com/codex",
		Icon:    openaiIcon,
		// Informational: the chat factory routes this vendor to its own
		// client before any protocol client is built.
		API:   api.APIOpenAIResponses,
		Order: 31,
		// The credential is an OAuth token pair obtained by the editor's
		// "Sign in with ChatGPT" flow, never a typed API key.
		RequiresAuth: false,
		Auth:         AuthNone,
		URLPatterns:  []string{"chatgpt.com"},
		DefaultBaseURLs: map[types.ModelType]string{
			types.ModelTypeKnowledgeQA: CodexBaseURL,
			types.ModelTypeVLLM:        CodexBaseURL,
		},
		ModelTypes: []types.ModelType{
			types.ModelTypeKnowledgeQA,
			types.ModelTypeVLLM,
		},
		Validate: func(cfg *Config) error {
			if cfg == nil {
				return fmt.Errorf("config is nil")
			}
			if strings.TrimSpace(cfg.ModelName) == "" {
				return fmt.Errorf("model name is required")
			}
			return nil
		},
	}
}
