package codexauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth authorization-code login (the "Sign in with ChatGPT" flow). The
// redirect URI is FIXED at http://localhost:1455/auth/callback — that is the
// only URI registered for the public Codex client id, so the browser always
// lands on the user's own machine. Deployments where WeKnora runs elsewhere
// fall back to pasting the callback URL manually (the code inside it is all
// the exchange needs; the redirect target never has to be reachable).
const (
	// AuthorizeEndpoint is the ChatGPT OAuth authorization page.
	AuthorizeEndpoint = "https://auth.openai.com/oauth/authorize"
	// RedirectURI is the only redirect registered for ClientID.
	RedirectURI = "http://localhost:1455/auth/callback"
	// CallbackPath is the path portion of RedirectURI.
	CallbackPath = "/auth/callback"
	// CallbackAddr is where the local callback listener must bind so the
	// browser redirect to RedirectURI reaches it (docker-compose maps it).
	CallbackAddr = ":1455"

	oauthScope = "openid profile email offline_access"
)

// LoginFlow is one in-progress authorization attempt: the state that ties the
// browser redirect back to it and the PKCE verifier needed for the exchange.
type LoginFlow struct {
	State    string
	Verifier string
	URL      string
}

// NewLoginFlow generates PKCE material plus state and builds the authorize URL.
func NewLoginFlow() (*LoginFlow, error) {
	verifierRaw := make([]byte, 64)
	if _, err := rand.Read(verifierRaw); err != nil {
		return nil, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	stateRaw := make([]byte, 16)
	if _, err := rand.Read(stateRaw); err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	state := hex.EncodeToString(stateRaw)

	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {ClientID},
		"redirect_uri":               {RedirectURI},
		"scope":                      {oauthScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {Originator},
	}
	return &LoginFlow{
		State:    state,
		Verifier: verifier,
		URL:      AuthorizeEndpoint + "?" + q.Encode(),
	}, nil
}

// ExchangeResult is the token pair from an authorization-code exchange plus
// display-only profile hints for the UI ("已授权 xx@yy (plus)").
type ExchangeResult struct {
	RefreshResult
	Email string
	Plan  string
}

// ExchangeCode swaps the authorization code for the initial token pair.
func ExchangeCode(ctx context.Context, code, verifier string) (*ExchangeResult, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {RedirectURI},
	}
	statusCode, body, err := postTokenForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("exchange Codex authorization code: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange Codex authorization code: HTTP %d: %s",
			statusCode, truncate(string(body), 300))
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse exchange response: %w", err)
	}
	if parsed.AccessToken == "" || parsed.RefreshToken == "" {
		return nil, fmt.Errorf("exchange response missing access_token/refresh_token")
	}

	expires := time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	if claims, err := ParseAccessToken(parsed.AccessToken); err == nil {
		expires = claims.ExpiresAt
	}
	email, plan := profileFromTokens(parsed.IDToken, parsed.AccessToken)
	return &ExchangeResult{
		RefreshResult: RefreshResult{
			AccessToken:  parsed.AccessToken,
			RefreshToken: parsed.RefreshToken,
			ExpiresAt:    expires,
		},
		Email: email,
		Plan:  plan,
	}, nil
}

// ParseAuthorizationInput accepts whatever the user pasted after the browser
// redirect failed to reach a listener: the full callback URL, a raw query
// string, "code#state", or a bare code.
func ParseAuthorizationInput(input string) (code, state string) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", ""
	}
	if u, err := url.Parse(value); err == nil && u.Scheme != "" {
		return u.Query().Get("code"), u.Query().Get("state")
	}
	if strings.Contains(value, "code=") {
		if params, err := url.ParseQuery(strings.TrimPrefix(value, "?")); err == nil {
			return params.Get("code"), params.Get("state")
		}
	}
	if before, after, ok := strings.Cut(value, "#"); ok {
		return before, after
	}
	return value, ""
}

// profileFromTokens pulls email + subscription plan out of the id/access
// token JWT payloads (unverified — display only). Either may be missing.
func profileFromTokens(idToken, accessToken string) (email, plan string) {
	for _, token := range []string{idToken, accessToken} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			continue
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var body struct {
			Email string `json:"email"`
			Auth  struct {
				ChatGPTPlanType string `json:"chatgpt_plan_type"`
			} `json:"https://api.openai.com/auth"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			continue
		}
		if email == "" {
			email = body.Email
		}
		if plan == "" {
			plan = body.Auth.ChatGPTPlanType
		}
	}
	return email, plan
}
