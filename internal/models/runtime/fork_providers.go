package runtime

import (
	"strings"

	"github.com/Tencent/WeKnora/internal/models"
	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/catalog"
	"github.com/Tencent/WeKnora/internal/models/providers"
	"github.com/Tencent/WeKnora/internal/types"
)

// registerForkProviders adds the vendors this fork ships on top of upstream's
// built-ins. They are registered here rather than in providers.Builtins() so
// the generated catalog (internal/models/catalog/data) stays identical to
// upstream: catalog.BuiltinModels panics for a vendor it has no data for.
func registerForkProviders(rt *Runtime) {
	rt.Register(providers.NewCodexProvider(), codexCatalog()...)
}

// codexContextWindow is the window Codex sessions are sized against. The
// subscription backend publishes no window of its own and codex-cli sizes
// gpt-5 family sessions at 272k, well under the API's 1M+, so history is
// compacted against the smaller figure.
const codexContextWindow = 272000

// codexThinkingLevels mirrors what the Codex client sends (see
// chat/codex_responses.go): the backend takes low…xhigh; "off" is sent as
// low because the effort cannot be switched off there, minimal folds onto
// low and max is not offered.
var codexThinkingLevels = api.ThinkingLevelMap{
	api.ReasoningOff:     api.Ptr("low"),
	api.ReasoningMinimal: api.Ptr("low"),
	api.ReasoningXHigh:   api.Ptr("xhigh"),
	api.ReasoningMax:     nil,
}

// codexCatalog derives the subscription catalog from OpenAI's reviewed
// metadata for the same models, so upstream catalog updates flow through. A
// gpt-5* family rule keeps models the live list adds (gpt-5.4, …) resolving
// as reasoning vision models.
func codexCatalog() []models.ModelSpec {
	byID := map[string]models.ModelSpec{}
	var family *models.ModelSpec
	for _, m := range catalog.BuiltinModels(providers.OpenaiID) {
		if catalog.EntryType(m.Type) != types.ModelTypeKnowledgeQA {
			continue
		}
		if m.ID != "" {
			byID[strings.ToLower(m.ID)] = m
		} else if strings.EqualFold(m.Match, "gpt-5*") {
			rule := m
			family = &rule
		}
	}
	out := make([]models.ModelSpec, 0, len(providers.CodexModels)+1)
	for _, id := range providers.CodexModels {
		m, ok := byID[id]
		if !ok {
			m = models.ModelSpec{ID: id, Reasoning: true, Input: []string{"text", "image"}}
		}
		out = append(out, codexEntry(m))
	}
	if family == nil {
		family = &models.ModelSpec{Name: "GPT-5 family", Match: "gpt-5*", Reasoning: true, Input: []string{"text", "image"}}
	}
	return append(out, codexEntry(*family))
}

func codexEntry(m models.ModelSpec) models.ModelSpec {
	m.Type = ""
	m.API = api.APIOpenAIResponses
	// Subscription quota, not metered API pricing.
	m.Cost = nil
	// OpenAI's compat targets its Chat Completions entries; the Codex client
	// has its own fixed dialect (no sampling parameters, no output cap).
	m.Compat = nil
	m.ThinkingLevels = codexThinkingLevels
	if m.ContextWindow == 0 || m.ContextWindow > codexContextWindow {
		m.ContextWindow = codexContextWindow
	}
	return m
}
