package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// makeJWT builds an unsigned-but-well-formed JWT with the OpenAI auth claim.
func makeJWT(t *testing.T, accountID string, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"exp": exp.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestParseAccessToken(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	token := makeJWT(t, "acc-123", exp)

	claims, err := ParseAccessToken(token)
	if err != nil {
		t.Fatalf("ParseAccessToken: %v", err)
	}
	if claims.AccountID != "acc-123" {
		t.Errorf("AccountID = %q, want acc-123", claims.AccountID)
	}
	if !claims.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want %v", claims.ExpiresAt, exp)
	}
}

func TestParseAccessTokenRejectsNonJWT(t *testing.T) {
	if _, err := ParseAccessToken("sk-proj-notajwt"); err == nil {
		t.Fatal("expected error for a non-JWT API key")
	}
	if _, err := ParseAccessToken(makeJWT(t, "", time.Now())); err == nil {
		t.Fatal("expected error when chatgpt_account_id claim is missing")
	}
}

func TestParseAuthJSON(t *testing.T) {
	// Codex CLI ~/.codex/auth.json shape.
	raw := `{"OPENAI_API_KEY":null,"auth_mode":"chatgpt","tokens":{"access_token":"at","refresh_token":"rt","account_id":"acc","id_token":"idt"},"last_refresh":"2026-08-25T00:00:00Z"}`
	access, refresh, err := ParseAuthJSON(raw)
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	if access != "at" || refresh != "rt" {
		t.Errorf("got (%q, %q), want (at, rt)", access, refresh)
	}

	// Flat shape.
	access, refresh, err = ParseAuthJSON(`{"access_token":"a2","refresh_token":"r2"}`)
	if err != nil {
		t.Fatalf("ParseAuthJSON flat: %v", err)
	}
	if access != "a2" || refresh != "r2" {
		t.Errorf("flat got (%q, %q), want (a2, r2)", access, refresh)
	}

	if _, _, err := ParseAuthJSON(`{"foo":1}`); err == nil {
		t.Error("expected error for JSON without tokens")
	}
	if _, _, err := ParseAuthJSON(`not json`); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// withTokenServer points refreshes at a test server for the test's duration.
func withTokenServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	old := tokenEndpoint
	tokenEndpoint = srv.URL
	t.Cleanup(func() {
		tokenEndpoint = old
		srv.Close()
	})
}

func TestRefreshSuccessAndRotation(t *testing.T) {
	newAccess := makeJWT(t, "acc-9", time.Now().Add(time.Hour))
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("client_id"); got != ClientID {
			t.Errorf("client_id = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "old-rt" {
			t.Errorf("refresh_token = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": newAccess, "refresh_token": "new-rt", "expires_in": 3600,
		})
	})

	res, err := Refresh(context.Background(), "old-rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken != newAccess || res.RefreshToken != "new-rt" {
		t.Errorf("unexpected rotation result: %+v", res)
	}
}

func TestRefreshInvalidGrantMapsToReauth(t *testing.T) {
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh_token_reused"}`)
	})

	_, err := Refresh(context.Background(), "dead-rt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errorsIs(err, ErrReauthRequired) {
		t.Errorf("error %v should wrap ErrReauthRequired", err)
	}
}

func errorsIs(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func TestTokenSourceServesValidTokenWithoutRefresh(t *testing.T) {
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("refresh endpoint should not be called for a fresh token")
	})
	access := makeJWT(t, "acc-1", time.Now().Add(2*time.Hour))
	src := GetTokenSource("", access, "rt-fresh-1")

	token, accountID, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != access || accountID != "acc-1" {
		t.Errorf("got (%q, %q)", token, accountID)
	}
}

func TestTokenSourceRefreshesExpiredAndPersists(t *testing.T) {
	rotated := makeJWT(t, "acc-2", time.Now().Add(time.Hour))
	var calls int
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": rotated, "refresh_token": "rt-2b", "expires_in": 3600,
		})
	})

	var mu sync.Mutex
	persisted := map[string][2]string{}
	SetPersister(func(ctx context.Context, modelID, access, refresh string) error {
		mu.Lock()
		defer mu.Unlock()
		persisted[modelID] = [2]string{access, refresh}
		return nil
	})
	t.Cleanup(func() { SetPersister(nil) })

	expired := makeJWT(t, "acc-2", time.Now().Add(-time.Minute))
	src := GetTokenSource("model-refresh-1", expired, "rt-2a")

	token, accountID, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != rotated || accountID != "acc-2" {
		t.Errorf("got (%q…, %q)", token[:16], accountID)
	}
	if calls != 1 {
		t.Errorf("refresh calls = %d, want 1", calls)
	}
	mu.Lock()
	got := persisted["model-refresh-1"]
	mu.Unlock()
	if got != [2]string{rotated, "rt-2b"} {
		t.Errorf("persisted = %v", got)
	}

	// Second call: token now valid, no second refresh.
	if _, _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("refresh calls after warm hit = %d, want 1", calls)
	}
}

func TestGetTokenSourceReusesAcrossRotation(t *testing.T) {
	access := makeJWT(t, "acc-3", time.Now().Add(time.Hour))
	src := GetTokenSource("model-reuse-1", access, "seed-rt")
	src.mu.Lock()
	src.refreshToken = "rotated-rt" // simulate an in-memory rotation
	src.mu.Unlock()

	// Caller passes the stored (stale) seed pair: must reuse, not reset.
	if again := GetTokenSource("model-reuse-1", access, "seed-rt"); again != src {
		t.Error("stale seed credential should reuse the rotated source")
	}
	// Caller passes the rotated pair (persisted successfully): also reuse.
	if again := GetTokenSource("model-reuse-1", access, "rotated-rt"); again != src {
		t.Error("current credential should reuse the source")
	}
	// A genuinely new credential replaces the source.
	if again := GetTokenSource("model-reuse-1", access, "brand-new-rt"); again == src {
		t.Error("new credential should build a fresh source")
	}
}

func TestTokenSourceAccessOnlyExpired(t *testing.T) {
	expired := makeJWT(t, "acc-4", time.Now().Add(-time.Hour))
	src := GetTokenSource("", expired, "")
	if _, _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected error for expired access token without refresh token")
	}
}

func TestApplyHeaders(t *testing.T) {
	h := http.Header{}
	ApplyHeaders(h, "tok", "acc", "sess")
	for key, want := range map[string]string{
		"Authorization":      "Bearer tok",
		"chatgpt-account-id": "acc",
		"originator":         Originator,
		"OpenAI-Beta":        "responses=experimental",
		"session_id":         "sess",
		"User-Agent":         UserAgent,
	} {
		if got := h.Get(key); got != want {
			t.Errorf("header %s = %q, want %q", key, got, want)
		}
	}
}

// withFastRetries shrinks the retry backoff for tests.
func withFastRetries(t *testing.T) {
	t.Helper()
	old := tokenRetryDelays
	tokenRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { tokenRetryDelays = old })
}

// hijackClose kills the connection before any response bytes, so the client
// sees the same transport-level EOF the Cloudflare edge produces.
func hijackClose(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("test server does not support hijacking")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestPostTokenFormRetriesTransportErrors(t *testing.T) {
	withFastRetries(t)
	newAccess := makeJWT(t, "acc-retry", time.Now().Add(time.Hour))
	var mu sync.Mutex
	attempts := 0
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			hijackClose(t, w)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt-new","expires_in":3600}`, newAccess)
	})

	res, err := Refresh(context.Background(), "rt-old")
	if err != nil {
		t.Fatalf("Refresh should survive two transport failures: %v", err)
	}
	if res.RefreshToken != "rt-new" {
		t.Errorf("RefreshToken = %q, want rt-new", res.RefreshToken)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestPostTokenFormNoRetryOnHTTPError(t *testing.T) {
	withFastRetries(t)
	var mu sync.Mutex
	attempts := 0
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	})

	_, err := Refresh(context.Background(), "rt-dead")
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err = %v, want ErrReauthRequired", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (HTTP errors must not be retried)", attempts)
	}
}

func TestPostTokenFormExhaustsRetries(t *testing.T) {
	withFastRetries(t)
	var mu sync.Mutex
	attempts := 0
	withTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		hijackClose(t, w)
	})

	_, err := ExchangeCode(context.Background(), "code-x", "verifier-y")
	if err == nil {
		t.Fatal("expected error after all attempts fail")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if !strings.Contains(err.Error(), "已重试 3 次") {
		t.Errorf("error should mention retry count, got: %v", err)
	}
}
