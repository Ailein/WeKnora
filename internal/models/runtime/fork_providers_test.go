package runtime_test

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/providers"
	modelruntime "github.com/Tencent/WeKnora/internal/models/runtime"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestCodexVendorRegisteredWithSubscriptionCatalog(t *testing.T) {
	v, ok := modelruntime.Get(providers.CodexID)
	if !ok {
		t.Fatal("codex vendor is not registered")
	}
	if v.RequiresAuth || v.Auth != providers.AuthNone {
		t.Errorf("codex must not ask for an API key: requiresAuth=%v auth=%s", v.RequiresAuth, v.Auth)
	}
	if got := modelruntime.DetectByURL(providers.CodexBaseURL); got != providers.CodexID {
		t.Errorf("DetectByURL(%q) = %q", providers.CodexBaseURL, got)
	}

	picker := v.ModelsByType(types.ModelTypeKnowledgeQA)
	if len(picker) != len(providers.CodexModels) {
		t.Fatalf("picker lists %d models, want %d", len(picker), len(providers.CodexModels))
	}
	for i, id := range providers.CodexModels {
		if picker[i].ID != id {
			t.Errorf("picker[%d] = %q, want %q (order is the editor default)", i, picker[i].ID, id)
		}
		if picker[i].Cost != nil {
			t.Errorf("%s carries API pricing on a subscription vendor", id)
		}
	}
	if vlms := v.ModelsByType(types.ModelTypeVLLM); len(vlms) != len(providers.CodexModels) {
		t.Errorf("VLM picker lists %d models, want every subscription model", len(vlms))
	}

	// A model only the live list knows still resolves through the family rule.
	for _, name := range append([]string{"gpt-5.4-mini"}, providers.CodexModels...) {
		r, err := modelruntime.Resolve(modelruntime.Ref{Provider: providers.CodexID, Model: name})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !r.Cataloged || !r.Spec.Reasoning || !r.Spec.AcceptsImages() {
			t.Errorf("%s: cataloged=%v reasoning=%v images=%v", name, r.Cataloged, r.Spec.Reasoning, r.Spec.AcceptsImages())
		}
		if r.Spec.ContextWindow != 272000 {
			t.Errorf("%s: context window %d, want the Codex 272k cap", name, r.Spec.ContextWindow)
		}
		if r.BaseURL != providers.CodexBaseURL {
			t.Errorf("%s: base URL %q", name, r.BaseURL)
		}
		levels := r.Capabilities().ThinkingLevels
		for _, l := range levels {
			if l == api.ReasoningMax {
				t.Errorf("%s: offers max, which the Codex client folds onto xhigh", name)
			}
		}
	}
	if err := modelruntime.ValidateRow("gpt-5.6-sol", types.ModelTypeKnowledgeQA,
		&types.ModelParameters{Provider: providers.CodexID}); err != nil {
		t.Errorf("ValidateRow: %v", err)
	}
}
