package parity

import "github.com/Tencent/WeKnora/internal/models/providers"

// forkVendor reports vendors this fork registers on top of the upstream
// catalog (runtime/fork_providers.go). Codex speaks the ChatGPT backend's own
// Responses dialect, which rejects output caps, and is covered by
// chat/codex_responses_test.go; it is not part of the upstream migration
// baseline or the cross-vendor request-shape invariants.
func forkVendor(id string) bool { return id == providers.CodexID }
