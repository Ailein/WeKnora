// Package codexauth manages OpenAI Codex (ChatGPT subscription) OAuth
// credentials: parsing the JWT access token, refreshing it with the rotating
// refresh token, and sharing one in-flight token per model across the many
// short-lived chat/vlm client instances the service layer creates.
//
// The refresh token is SINGLE USE (RFC 6749 rotation): every refresh returns a
// new refresh token and invalidates the old one. All refreshes therefore go
// through a per-model TokenSource with a mutex, and the rotated pair is pushed
// back to persistent storage via the registered Persister so restarts and
// other replicas pick it up.
package codexauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// ClientID is the public OAuth client id used by the official Codex CLI
	// (and every third-party harness: pi/openclaw, Hermes, opencode).
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// TokenURL is the OAuth token endpoint for refresh-token grants.
	TokenURL = "https://auth.openai.com/oauth/token"
	// DefaultBaseURL is the ChatGPT-backend Codex API root; the chat client
	// appends "/responses".
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
	// Originator must be a Cloudflare-allowlisted client identifier or
	// requests from datacenter IPs get challenged; codex_cli_rs is the
	// official CLI's value (Hermes ships the same one for this reason).
	Originator = "codex_cli_rs"
	// UserAgent identifies WeKnora while keeping the allowlisted CLI prefix.
	UserAgent = "codex_cli_rs/0.0.0 (WeKnora)"

	// jwtClaimNamespace is where OpenAI nests auth metadata inside the
	// access-token JWT payload.
	jwtClaimNamespace = "https://api.openai.com/auth"

	// refreshSkew refreshes the token this long before its JWT exp so a
	// long streaming call started near the boundary still authenticates.
	refreshSkew = 5 * time.Minute
)

// ErrReauthRequired means the refresh token itself was rejected (revoked,
// rotated away by another client such as a local Codex CLI, or expired).
// The only fix is importing fresh credentials, so surface that clearly.
var ErrReauthRequired = errors.New(
	"Codex 凭证已失效（refresh token 被拒绝，可能已被其他 Codex 客户端轮换或撤销），请在模型设置中重新导入 ~/.codex/auth.json")

var refreshHTTPClient = &http.Client{Timeout: 30 * time.Second}

// tokenEndpoint is TokenURL, overridable in tests.
var tokenEndpoint = TokenURL

// Claims is the subset of the access-token JWT payload we need.
type Claims struct {
	AccountID string
	ExpiresAt time.Time
}

// ParseAccessToken decodes the JWT payload without verifying the signature
// (we are the client, not the resource server) and extracts the ChatGPT
// account id plus expiry.
func ParseAccessToken(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("access token is not a JWT (expected ChatGPT OAuth token from ~/.codex/auth.json)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT payload: %w", err)
	}
	var body struct {
		Exp  float64 `json:"exp"`
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("parse JWT payload: %w", err)
	}
	if body.Auth.ChatGPTAccountID == "" {
		return nil, fmt.Errorf("JWT payload missing %s.chatgpt_account_id claim", jwtClaimNamespace)
	}
	return &Claims{
		AccountID: body.Auth.ChatGPTAccountID,
		ExpiresAt: time.Unix(int64(body.Exp), 0),
	}, nil
}

// ParseAuthJSON extracts the token pair from the content of a Codex CLI
// ~/.codex/auth.json file (or a raw {access_token, refresh_token} object).
func ParseAuthJSON(raw string) (accessToken, refreshToken string, err error) {
	var file struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(raw), &file); err != nil {
		return "", "", fmt.Errorf("invalid auth JSON: %w", err)
	}
	access := file.Tokens.AccessToken
	refresh := file.Tokens.RefreshToken
	if access == "" && refresh == "" {
		access = file.AccessToken
		refresh = file.RefreshToken
	}
	if access == "" && refresh == "" {
		return "", "", fmt.Errorf("auth JSON contains no tokens.access_token / tokens.refresh_token")
	}
	return access, refresh, nil
}

// RefreshResult is a freshly rotated token pair.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Refresh exchanges the (single-use) refresh token for a new pair.
func Refresh(ctx context.Context, refreshToken string) (*RefreshResult, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := refreshHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh Codex token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		lower := strings.ToLower(string(body))
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized ||
			strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "refresh_token_reused") {
			return nil, fmt.Errorf("%w (HTTP %d: %s)", ErrReauthRequired, resp.StatusCode, truncate(string(body), 300))
		}
		return nil, fmt.Errorf("refresh Codex token: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}
	if parsed.AccessToken == "" || parsed.RefreshToken == "" {
		return nil, fmt.Errorf("refresh response missing access_token/refresh_token")
	}
	expires := time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	if claims, err := ParseAccessToken(parsed.AccessToken); err == nil {
		expires = claims.ExpiresAt
	}
	return &RefreshResult{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    expires,
	}, nil
}

// Persister writes a rotated token pair back to durable storage (the model's
// encrypted parameters). modelID may be empty for unsaved "test connection"
// models — implementations must treat that as a no-op.
type Persister func(ctx context.Context, modelID, accessToken, refreshToken string) error

var (
	persisterMu sync.RWMutex
	persister   Persister
)

// SetPersister registers the storage write-back used after every refresh.
// Called once at container assembly time.
func SetPersister(p Persister) {
	persisterMu.Lock()
	defer persisterMu.Unlock()
	persister = p
}

func persist(ctx context.Context, modelID, access, refresh string) error {
	persisterMu.RLock()
	p := persister
	persisterMu.RUnlock()
	if p == nil || modelID == "" {
		return nil
	}
	return p(ctx, modelID, access, refresh)
}

// TokenSource owns the live token pair for one credential and serializes
// refreshes. Client instances are rebuilt per call by the service layer, so
// sources live in a process-wide registry keyed by model id.
type TokenSource struct {
	mu      sync.Mutex
	modelID string
	// seedRefresh is the refresh token this source was created from; used to
	// recognize "same credential" when a caller passes stored values that
	// predate an in-memory rotation whose persistence failed.
	seedRefresh string

	accessToken  string
	refreshToken string
	claims       *Claims

	// onPersistErr logs are the caller's concern; we keep the last error for
	// tests/diagnostics only.
	lastPersistErr error
}

var (
	registryMu sync.Mutex
	registry   = map[string]*TokenSource{}
)

// sourceKey prefers the stable model id; unsaved test models fall back to a
// hash of the credential so repeated tests share one source too.
func sourceKey(modelID, accessToken, refreshToken string) string {
	if modelID != "" {
		return "model:" + modelID
	}
	sum := sha256.Sum256([]byte(refreshToken + "|" + accessToken))
	return "cred:" + hex.EncodeToString(sum[:8])
}

// GetTokenSource returns the shared source for this credential, creating or
// replacing it when the caller presents a credential the source has never
// seen (i.e. the user imported new tokens).
func GetTokenSource(modelID, accessToken, refreshToken string) *TokenSource {
	key := sourceKey(modelID, accessToken, refreshToken)
	registryMu.Lock()
	defer registryMu.Unlock()
	if src, ok := registry[key]; ok {
		src.mu.Lock()
		known := refreshToken == "" || refreshToken == src.seedRefresh || refreshToken == src.refreshToken
		src.mu.Unlock()
		if known {
			return src
		}
	}
	src := &TokenSource{
		modelID:      modelID,
		seedRefresh:  refreshToken,
		accessToken:  accessToken,
		refreshToken: refreshToken,
	}
	if accessToken != "" {
		if claims, err := ParseAccessToken(accessToken); err == nil {
			src.claims = claims
		}
	}
	registry[key] = src
	return src
}

// Token returns a currently valid access token plus its account id,
// refreshing (and persisting the rotation) when the cached one is expired or
// missing.
func (s *TokenSource) Token(ctx context.Context) (accessToken, accountID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accessToken != "" && s.claims != nil && time.Until(s.claims.ExpiresAt) > refreshSkew {
		return s.accessToken, s.claims.AccountID, nil
	}
	if err := s.refreshLocked(ctx); err != nil {
		return "", "", err
	}
	return s.accessToken, s.claims.AccountID, nil
}

// ForceRefresh discards the cached access token (e.g. after an upstream 401)
// and fetches a new pair.
func (s *TokenSource) ForceRefresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(ctx)
}

func (s *TokenSource) refreshLocked(ctx context.Context) error {
	if s.refreshToken == "" {
		if s.accessToken == "" {
			return fmt.Errorf("Codex 凭证缺失：请粘贴 ~/.codex/auth.json 中的 access_token / refresh_token")
		}
		// Access token only (no refresh): usable until it expires, then dead.
		if s.claims == nil {
			claims, err := ParseAccessToken(s.accessToken)
			if err != nil {
				return err
			}
			s.claims = claims
		}
		if time.Until(s.claims.ExpiresAt) <= 0 {
			return fmt.Errorf("Codex access token 已过期且未配置 refresh token，请重新导入 ~/.codex/auth.json")
		}
		return nil
	}
	res, err := Refresh(ctx, s.refreshToken)
	if err != nil {
		return err
	}
	claims, err := ParseAccessToken(res.AccessToken)
	if err != nil {
		return fmt.Errorf("refreshed token invalid: %w", err)
	}
	s.accessToken = res.AccessToken
	s.refreshToken = res.RefreshToken
	s.claims = claims
	// Persist the rotation; a write failure must not fail the call (the
	// in-memory pair keeps working), but the next process restart would then
	// need the seedRefresh fallback in GetTokenSource.
	s.lastPersistErr = persist(ctx, s.modelID, res.AccessToken, res.RefreshToken)
	return nil
}

// ApplyHeaders sets every header the ChatGPT Codex backend requires.
func ApplyHeaders(h http.Header, accessToken, accountID, sessionID string) {
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("chatgpt-account-id", accountID)
	h.Set("originator", Originator)
	h.Set("User-Agent", UserAgent)
	h.Set("OpenAI-Beta", "responses=experimental")
	if sessionID != "" {
		h.Set("session_id", sessionID)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
