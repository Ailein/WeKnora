package codexauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestNewLoginFlow(t *testing.T) {
	flow, err := NewLoginFlow()
	if err != nil {
		t.Fatalf("NewLoginFlow: %v", err)
	}
	if l := len(flow.Verifier); l < 43 || l > 128 {
		t.Errorf("verifier length = %d, want 43..128 (RFC 7636)", l)
	}

	u, err := url.Parse(flow.URL)
	if err != nil {
		t.Fatalf("authorize URL invalid: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != AuthorizeEndpoint {
		t.Errorf("authorize endpoint = %q, want %q", got, AuthorizeEndpoint)
	}
	q := u.Query()
	sum := sha256.Sum256([]byte(flow.Verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
	for key, want := range map[string]string{
		"response_type":             "code",
		"client_id":                 ClientID,
		"redirect_uri":              RedirectURI,
		"scope":                     "openid profile email offline_access",
		"code_challenge":            wantChallenge,
		"code_challenge_method":     "S256",
		"state":                     flow.State,
		"codex_cli_simplified_flow": "true",
		"originator":                Originator,
	} {
		if q.Get(key) != want {
			t.Errorf("authorize URL param %s = %q, want %q", key, q.Get(key), want)
		}
	}

	// Two flows must never share state or verifier.
	flow2, err := NewLoginFlow()
	if err != nil {
		t.Fatal(err)
	}
	if flow2.State == flow.State || flow2.Verifier == flow.Verifier {
		t.Error("flows must use fresh random state/verifier")
	}
}

func TestParseAuthorizationInput(t *testing.T) {
	cases := []struct {
		in        string
		code, sta string
	}{
		{"http://localhost:1455/auth/callback?code=abc&state=st1", "abc", "st1"},
		{"  http://localhost:1455/auth/callback?code=abc ", "abc", ""},
		{"?code=xyz&state=st2", "xyz", "st2"},
		{"code=xyz&state=st2", "xyz", "st2"},
		{"rawcode#st3", "rawcode", "st3"},
		{"barecode", "barecode", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		code, state := ParseAuthorizationInput(tc.in)
		if code != tc.code || state != tc.sta {
			t.Errorf("ParseAuthorizationInput(%q) = (%q, %q), want (%q, %q)",
				tc.in, code, state, tc.code, tc.sta)
		}
	}
}

// makeIDToken builds an id_token-style JWT carrying email + plan claims.
func makeIDToken(t *testing.T, email, plan string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type": plan,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestExchangeCode(t *testing.T) {
	access := makeJWT(t, "acc-ex", time.Now().Add(time.Hour))
	idToken := makeIDToken(t, "joe@example.com", "plus")
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"grant_type":    "authorization_code",
			"client_id":     ClientID,
			"code":          "the-code",
			"code_verifier": "the-verifier",
			"redirect_uri":  RedirectURI,
		} {
			if r.PostFormValue(key) != want {
				t.Errorf("exchange form %s = %q, want %q", key, r.PostFormValue(key), want)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  access,
			"refresh_token": "new-refresh",
			"id_token":      idToken,
			"expires_in":    3600,
		})
	})

	res, err := ExchangeCode(context.Background(), "the-code", "the-verifier")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if res.AccessToken != access || res.RefreshToken != "new-refresh" {
		t.Error("exchange result tokens mismatch")
	}
	if res.Email != "joe@example.com" || res.Plan != "plus" {
		t.Errorf("profile = (%q, %q), want (joe@example.com, plus)", res.Email, res.Plan)
	}
}

func TestExchangeCodeHTTPError(t *testing.T) {
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	})
	if _, err := ExchangeCode(context.Background(), "expired-code", "v"); err == nil {
		t.Fatal("expected error for HTTP 400 exchange response")
	}
}

func TestProfileFromTokensFallsBackToAccessToken(t *testing.T) {
	// No id_token; plan claim only present in the access token, no email at all.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro"},
	})
	access := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".x"
	email, plan := profileFromTokens("", access)
	if email != "" || plan != "pro" {
		t.Errorf("profile = (%q, %q), want (\"\", pro)", email, plan)
	}
}
