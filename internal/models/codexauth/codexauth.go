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

	"github.com/Tencent/WeKnora/internal/logger"
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

// tokenRetryDelays are the waits before the 2nd/3rd attempt of a token POST.
// Overridable in tests.
var tokenRetryDelays = []time.Duration{200 * time.Millisecond, 600 * time.Millisecond}

// postTokenForm posts an OAuth grant to tokenEndpoint and returns the HTTP
// status plus body. Transport-level failures (dial/TLS/EOF/reset) are retried:
// behind proxy chains the Cloudflare edge intermittently kills connections
// before responding, and since no response means the server never processed
// the grant, the one-shot code/refresh token has not been consumed. If the
// server *did* process a lost response, the retry surfaces a clear
// invalid_grant instead of a bare connection error.
func postTokenForm(ctx context.Context, form url.Values) (status int, body []byte, err error) {
	encoded := form.Encode()
	for attempt := 0; ; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(encoded))
		if reqErr != nil {
			return 0, nil, fmt.Errorf("create token request: %w", reqErr)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, doErr := refreshHTTPClient.Do(req)
		if doErr == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return resp.StatusCode, b, nil
		}
		if attempt >= len(tokenRetryDelays) || ctx.Err() != nil {
			return 0, nil, fmt.Errorf("连接 OpenAI 授权服务器失败（已重试 %d 次，多为网络/代理链路抖动，可稍后再试）: %w",
				attempt+1, doErr)
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(tokenRetryDelays[attempt]):
		}
	}
}

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
	statusCode, body, err := postTokenForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("refresh Codex token: %w", err)
	}

	if statusCode != http.StatusOK {
		lower := strings.ToLower(string(body))
		if statusCode == http.StatusBadRequest || statusCode == http.StatusUnauthorized ||
			strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "refresh_token_reused") {
			return nil, fmt.Errorf("%w (HTTP %d: %s)", ErrReauthRequired, statusCode, truncate(string(body), 300))
		}
		return nil, fmt.Errorf("refresh Codex token: HTTP %d: %s", statusCode, truncate(string(body), 300))
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

// Loader reads the token pair currently stored for a model. A source consults
// it before spending its own refresh token so a rotation made by another
// process (a second replica, or one whose persist this process never saw) is
// adopted instead of colliding with it — the old token is single-use, and a
// second refresh with it is rejected as reuse. Empty strings mean "nothing
// stored".
type Loader func(ctx context.Context, modelID string) (accessToken, refreshToken string, err error)

var (
	hooksMu   sync.RWMutex
	persister Persister
	loader    Loader
)

// SetPersister registers the storage write-back used after every refresh.
// Called once at container assembly time.
func SetPersister(p Persister) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	persister = p
}

// SetLoader registers the storage read used before every refresh. Optional;
// without it a source trusts its in-memory pair only.
func SetLoader(l Loader) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	loader = l
}

func persist(ctx context.Context, modelID, access, refresh string) error {
	hooksMu.RLock()
	p := persister
	hooksMu.RUnlock()
	if p == nil || modelID == "" {
		return nil
	}
	return p(ctx, modelID, access, refresh)
}

func load(ctx context.Context, modelID string) (access, refresh string, ok bool) {
	hooksMu.RLock()
	l := loader
	hooksMu.RUnlock()
	if l == nil || modelID == "" {
		return "", "", false
	}
	access, refresh, err := l(ctx, modelID)
	if err != nil || refresh == "" {
		return "", "", false
	}
	return access, refresh, true
}

// refreshDetachTimeout bounds one detached refresh (token POST with its
// transport retries, plus the write-back).
const refreshDetachTimeout = 2 * time.Minute

// TokenSource owns the live token pair for one credential and serializes
// refreshes. Client instances are rebuilt per call by the service layer, so
// sources live in a process-wide registry keyed by model id.
type TokenSource struct {
	// mu serializes refreshes and is held across the token POST.
	mu      sync.Mutex
	modelID string

	// identMu guards the fields below for readers that must not wait on an
	// in-flight refresh (GetTokenSource runs under the registry lock, and a
	// refresh can take half a minute behind a flaky proxy). Writers hold mu
	// and identMu; readers hold either.
	identMu sync.Mutex
	// seedAccess/seedRefresh are the tokens this source was created from;
	// known records every refresh token it has held since. Together they
	// recognize "same credential" whatever stored generation a caller
	// presents — the stored pair lags the live one whenever a persist failed.
	seedAccess   string
	seedRefresh  string
	known        map[string]bool
	accessToken  string
	refreshToken string
	claims       *Claims

	// lastPersistErr keeps the last write-back failure for tests/diagnostics;
	// refreshLocked already logs it.
	lastPersistErr error
}

func newTokenSource(modelID, accessToken, refreshToken string) *TokenSource {
	src := &TokenSource{
		modelID:     modelID,
		seedAccess:  accessToken,
		seedRefresh: refreshToken,
		known:       map[string]bool{},
	}
	var claims *Claims
	if accessToken != "" {
		if parsed, err := ParseAccessToken(accessToken); err == nil {
			claims = parsed
		}
	}
	src.setPairLocked(accessToken, refreshToken, claims)
	return src
}

// setPairLocked installs a token pair. Caller holds mu (or the source is not
// published yet).
func (s *TokenSource) setPairLocked(access, refresh string, claims *Claims) {
	s.identMu.Lock()
	defer s.identMu.Unlock()
	s.accessToken, s.refreshToken, s.claims = access, refresh, claims
	if refresh != "" {
		s.known[refresh] = true
	}
}

// snapshot returns the live pair without waiting on an in-flight refresh.
func (s *TokenSource) snapshot() (access, refresh string, claims *Claims) {
	s.identMu.Lock()
	defer s.identMu.Unlock()
	return s.accessToken, s.refreshToken, s.claims
}

// recognizes reports whether the presented credential is one this source
// has held: the seed pair, the live pair, or any rotation in between. An
// access-only credential must match an access token this source knows.
func (s *TokenSource) recognizes(accessToken, refreshToken string) bool {
	s.identMu.Lock()
	defer s.identMu.Unlock()
	if refreshToken == "" {
		return accessToken == s.seedAccess || accessToken == s.accessToken
	}
	return refreshToken == s.seedRefresh || s.known[refreshToken]
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
//
// A model-keyed source seeded from a credential that an anonymous
// (unsaved "test connection") source already rotated adopts that rotation:
// the seed refresh token is spent, so starting from it would fail the first
// real call with ErrReauthRequired right after a successful test. The adopted
// pair is persisted immediately so a restart does not resurrect the dead seed.
func GetTokenSource(modelID, accessToken, refreshToken string) *TokenSource {
	src, adopted := getOrCreateTokenSource(modelID, accessToken, refreshToken)
	if adopted {
		access, refresh, _ := src.snapshot()
		if err := persist(context.Background(), modelID, access, refresh); err != nil {
			logger.Warnf(context.Background(),
				"[Codex] persist adopted token rotation for model %s failed: %v", modelID, err)
		}
	}
	return src
}

func getOrCreateTokenSource(modelID, accessToken, refreshToken string) (src *TokenSource, adopted bool) {
	key := sourceKey(modelID, accessToken, refreshToken)
	registryMu.Lock()
	defer registryMu.Unlock()
	if existing, ok := registry[key]; ok && existing.recognizes(accessToken, refreshToken) {
		return existing, false
	}
	src = newTokenSource(modelID, accessToken, refreshToken)
	if modelID != "" && refreshToken != "" {
		if anon, ok := registry[sourceKey("", accessToken, refreshToken)]; ok && anon.recognizes(accessToken, refreshToken) {
			if access, refresh, claims := anon.snapshot(); refresh != "" && refresh != refreshToken {
				src.setPairLocked(access, refresh, claims)
				adopted = true
			}
		}
	}
	registry[key] = src
	return src, adopted
}

// Forget drops the cached source for a model so the next client rebuilds it
// from the stored credentials. Call whenever the stored pair is replaced or
// cleared through the credentials API — otherwise the old source keeps using
// (and on rotation writes back) tokens the user just removed.
func Forget(modelID string) {
	if modelID == "" {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, "model:"+modelID)
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
// and fetches a new pair. staleAccess is the token the caller was rejected
// with: when a concurrent caller already rotated past it, that rotation is
// served as-is instead of spending a second refresh token on the same 401.
func (s *TokenSource) ForceRefresh(ctx context.Context, staleAccess string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if staleAccess != "" && s.accessToken != "" && s.accessToken != staleAccess &&
		s.claims != nil && time.Until(s.claims.ExpiresAt) > 0 {
		return nil
	}
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
			s.setPairLocked(s.accessToken, "", claims)
		}
		if time.Until(s.claims.ExpiresAt) <= 0 {
			return fmt.Errorf("Codex access token 已过期且未配置 refresh token，请重新导入 ~/.codex/auth.json")
		}
		return nil
	}

	// The grant and its write-back must outlive the caller. A client that
	// disconnects mid-refresh would otherwise cancel the POST after the
	// server already rotated (old token spent, new pair lost) or cancel the
	// persist after a successful rotation (the row keeps a spent token the
	// next restart tries to use). Either way a perfectly good credential
	// ends in ErrReauthRequired. Log values from ctx are kept.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshDetachTimeout)
	defer cancel()

	// Another process may already hold a newer rotation of this credential.
	if s.adoptStoredLocked(rctx) && time.Until(s.claims.ExpiresAt) > refreshSkew {
		return nil
	}
	res, err := Refresh(rctx, s.refreshToken)
	if err != nil {
		// Our token may have been spent by another replica between the load
		// above and this POST; its rotation is in the store and usable.
		if errors.Is(err, ErrReauthRequired) && s.adoptStoredLocked(rctx) && time.Until(s.claims.ExpiresAt) > 0 {
			return nil
		}
		return err
	}
	claims, err := ParseAccessToken(res.AccessToken)
	if err != nil {
		return fmt.Errorf("refreshed token invalid: %w", err)
	}
	s.setPairLocked(res.AccessToken, res.RefreshToken, claims)
	// Persist the rotation; a write failure must not fail the call (the
	// in-memory pair keeps working), but the next process restart would then
	// start from the spent stored refresh token and need a re-import. Say so
	// loudly instead of failing silently later.
	s.lastPersistErr = persist(rctx, s.modelID, res.AccessToken, res.RefreshToken)
	if s.lastPersistErr != nil {
		logger.Warnf(ctx,
			"[Codex] persist rotated token for model %s failed (in-memory pair still valid; re-import ~/.codex/auth.json if this keeps failing): %v",
			s.modelID, s.lastPersistErr)
	}
	return nil
}

// adoptStoredLocked replaces the live pair with the stored one when the store
// holds a newer rotation. Every refresh issues a fresh access token with a
// later expiry, so the expiry orders rotations: a stored pair that expires no
// later than the live one is the older generation (this process rotated past
// it but its persist failed) and is ignored. Caller holds mu.
func (s *TokenSource) adoptStoredLocked(ctx context.Context) bool {
	access, refresh, ok := load(ctx, s.modelID)
	if !ok || refresh == s.refreshToken || s.known[refresh] {
		return false
	}
	claims, err := ParseAccessToken(access)
	if err != nil {
		return false
	}
	if s.claims != nil && !claims.ExpiresAt.After(s.claims.ExpiresAt) {
		return false
	}
	logger.Infof(ctx, "[Codex] adopting token rotation stored by another process for model %s", s.modelID)
	s.setPairLocked(access, refresh, claims)
	return true
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
