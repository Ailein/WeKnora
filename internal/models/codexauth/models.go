package codexauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// modelsClientVersion is the codex-cli version reported to the /models
// endpoint, which rejects requests below each model's minimal_client_version.
// Bump alongside official CLI releases when upstream raises the floor — a
// stale value degrades model listing to the static fallback, chat is
// unaffected.
const modelsClientVersion = "0.149.0"

// UpstreamModel is one entry of the ChatGPT-backend Codex model catalog.
type UpstreamModel struct {
	Slug        string
	DisplayName string
	Description string
	Vision      bool
}

// ListModels fetches the live model catalog for the subscription channel
// (GET {base}/models). Only user-facing API models are returned: entries with
// visibility "hide" (internal models like auto-review) or supported_in_api ==
// false are dropped. Transport-level failures are retried like token POSTs —
// same flaky proxy→Cloudflare path.
func ListModels(ctx context.Context, baseURL, accessToken, accountID string) ([]UpstreamModel, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/models?client_version=" + url.QueryEscape(modelsClientVersion)

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("create models request: %w", err)
		}
		ApplyHeaders(req.Header, accessToken, accountID, "")

		resp, doErr := refreshHTTPClient.Do(req)
		if doErr == nil {
			// The catalog embeds full instruction templates per model — allow
			// several MB.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("list Codex models: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
			}
			return parseModelsResponse(body)
		}
		if attempt >= len(tokenRetryDelays) || ctx.Err() != nil {
			return nil, fmt.Errorf("list Codex models: %w", doErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(tokenRetryDelays[attempt]):
		}
	}
}

func parseModelsResponse(body []byte) ([]UpstreamModel, error) {
	var parsed struct {
		Models []struct {
			Slug            string   `json:"slug"`
			DisplayName     string   `json:"display_name"`
			Description     string   `json:"description"`
			Visibility      string   `json:"visibility"`
			SupportedInAPI  *bool    `json:"supported_in_api"`
			InputModalities []string `json:"input_modalities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}
	result := make([]UpstreamModel, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if m.Slug == "" || m.Visibility != "list" {
			continue
		}
		if m.SupportedInAPI != nil && !*m.SupportedInAPI {
			continue
		}
		vision := false
		for _, mod := range m.InputModalities {
			if mod == "image" {
				vision = true
				break
			}
		}
		result = append(result, UpstreamModel{
			Slug:        m.Slug,
			DisplayName: m.DisplayName,
			Description: m.Description,
			Vision:      vision,
		})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("models response contained no listable models")
	}
	return result, nil
}
