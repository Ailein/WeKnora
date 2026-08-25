package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/models/codexauth"
)

// newCodexOAuthTestHandler binds an ephemeral port instead of the real :1455
// so tests never collide with a developer's local `codex login`.
func newCodexOAuthTestHandler() *CodexOAuthHandler {
	h := NewCodexOAuthHandler()
	h.listenAddr = "127.0.0.1:0"
	return h
}

func newCodexOAuthTestRouter(h *CodexOAuthHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.POST("/codex/oauth/start", h.Start)
	r.GET("/codex/oauth/status", h.Status)
	r.POST("/codex/oauth/exchange", h.Exchange)
	return r
}

func codexDoJSON(t *testing.T, r *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var parsed map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &parsed)
	return w, parsed
}

func TestCodexOAuthStartAndPendingStatus(t *testing.T) {
	h := newCodexOAuthTestHandler()
	r := newCodexOAuthTestRouter(h)

	w, resp := codexDoJSON(t, r, http.MethodPost, "/codex/oauth/start", "")
	if w.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", w.Code, w.Body.String())
	}
	data := resp["data"].(map[string]any)
	state, _ := data["state"].(string)
	if state == "" {
		t.Fatal("start response missing state")
	}
	if u, _ := data["authorization_url"].(string); !strings.HasPrefix(u, codexauth.AuthorizeEndpoint) {
		t.Errorf("authorization_url = %q, want prefix %q", u, codexauth.AuthorizeEndpoint)
	}
	if listening, _ := data["callback_listening"].(bool); !listening {
		t.Error("expected callback_listening=true on an ephemeral port")
	}
	h.mu.Lock()
	if h.server == nil {
		t.Error("listener should be running while a flow is pending")
	}
	h.mu.Unlock()

	w, resp = codexDoJSON(t, r, http.MethodGet, "/codex/oauth/status?state="+state, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := resp["data"].(map[string]any)["status"]; got != "pending" {
		t.Errorf("status = %v, want pending", got)
	}

	// Unknown state → 404 envelope.
	w, _ = codexDoJSON(t, r, http.MethodGet, "/codex/oauth/status?state=nope", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown state status = %d, want 404", w.Code)
	}
}

func TestCodexOAuthCallbackDeniedThenStatusOneShot(t *testing.T) {
	h := newCodexOAuthTestHandler()
	r := newCodexOAuthTestRouter(h)

	_, resp := codexDoJSON(t, r, http.MethodPost, "/codex/oauth/start", "")
	state := resp["data"].(map[string]any)["state"].(string)

	// Browser lands with an error param (user clicked deny).
	cb := httptest.NewRecorder()
	h.callbackHandler().ServeHTTP(cb, httptest.NewRequest(http.MethodGet,
		codexauth.CallbackPath+"?state="+state+"&error=access_denied", nil))
	if cb.Code != http.StatusBadRequest {
		t.Fatalf("callback denied status = %d, want 400", cb.Code)
	}

	// First poll returns the terminal error…
	w, respErr := codexDoJSON(t, r, http.MethodGet, "/codex/oauth/status?state="+state, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	data := respErr["data"].(map[string]any)
	if data["status"] != "error" || !strings.Contains(data["error"].(string), "access_denied") {
		t.Errorf("terminal data = %#v, want error/access_denied", data)
	}
	// …and consumes the flow: the second poll must 404 and the port is freed.
	w, _ = codexDoJSON(t, r, http.MethodGet, "/codex/oauth/status?state="+state, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("second poll status = %d, want 404 (one-shot)", w.Code)
	}
	h.mu.Lock()
	if h.server != nil {
		t.Error("listener should stop once the last flow is consumed")
	}
	h.mu.Unlock()
}

func TestCodexOAuthCallbackRejectsUnknownAndForeignState(t *testing.T) {
	h := newCodexOAuthTestHandler()
	cb := httptest.NewRecorder()
	h.callbackHandler().ServeHTTP(cb, httptest.NewRequest(http.MethodGet,
		codexauth.CallbackPath+"?state=unknown&code=x", nil))
	if cb.Code != http.StatusBadRequest {
		t.Errorf("unknown state callback = %d, want 400", cb.Code)
	}

	// Wrong path → branded 404 page, never a token exchange.
	nf := httptest.NewRecorder()
	h.callbackHandler().ServeHTTP(nf, httptest.NewRequest(http.MethodGet, "/other", nil))
	if nf.Code != http.StatusNotFound {
		t.Errorf("wrong path = %d, want 404", nf.Code)
	}
}

func TestCodexOAuthCompleteStatusReturnsTokensOnce(t *testing.T) {
	h := newCodexOAuthTestHandler()
	r := newCodexOAuthTestRouter(h)

	// Seed a completed flow directly (the exchange itself is covered by
	// codexauth's ExchangeCode tests).
	h.mu.Lock()
	h.flows["st-done"] = &codexOAuthFlow{
		createdAt: time.Now(),
		status:    "complete",
		result: &codexauth.ExchangeResult{
			RefreshResult: codexauth.RefreshResult{
				AccessToken:  "at-1",
				RefreshToken: "rt-1",
				ExpiresAt:    time.Now().Add(time.Hour),
			},
			Email: "joe@example.com",
			Plan:  "plus",
		},
	}
	h.mu.Unlock()

	w, resp := codexDoJSON(t, r, http.MethodGet, "/codex/oauth/status?state=st-done", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	data := resp["data"].(map[string]any)
	if data["status"] != "complete" || data["access_token"] != "at-1" ||
		data["refresh_token"] != "rt-1" || data["email"] != "joe@example.com" || data["plan"] != "plus" {
		t.Errorf("complete payload = %#v", data)
	}

	if w, _ := codexDoJSON(t, r, http.MethodGet, "/codex/oauth/status?state=st-done", ""); w.Code != http.StatusNotFound {
		t.Errorf("tokens must be one-shot; second read = %d, want 404", w.Code)
	}

	// A late browser callback for the already-consumed state gets the
	// "expired" page rather than a fresh exchange.
	cb := httptest.NewRecorder()
	h.callbackHandler().ServeHTTP(cb, httptest.NewRequest(http.MethodGet,
		codexauth.CallbackPath+"?state=st-done&code=x", nil))
	if cb.Code != http.StatusBadRequest {
		t.Errorf("late callback = %d, want 400", cb.Code)
	}
}

func TestCodexOAuthExchangeValidation(t *testing.T) {
	h := newCodexOAuthTestHandler()
	r := newCodexOAuthTestRouter(h)

	_, resp := codexDoJSON(t, r, http.MethodPost, "/codex/oauth/start", "")
	state := resp["data"].(map[string]any)["state"].(string)

	// Pasted URL from a DIFFERENT login attempt must be rejected.
	w, _ := codexDoJSON(t, r, http.MethodPost, "/codex/oauth/exchange",
		`{"state":"`+state+`","input":"http://localhost:1455/auth/callback?code=c&state=other"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("foreign-state exchange = %d, want 400", w.Code)
	}

	// Paste without any code.
	w, _ = codexDoJSON(t, r, http.MethodPost, "/codex/oauth/exchange",
		`{"state":"`+state+`","input":"http://localhost:1455/auth/callback?state=`+state+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("codeless exchange = %d, want 400", w.Code)
	}

	// Unknown flow.
	w, _ = codexDoJSON(t, r, http.MethodPost, "/codex/oauth/exchange",
		`{"state":"missing","input":"code=x"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown flow exchange = %d, want 404", w.Code)
	}
}

func TestCodexOAuthFlowExpiry(t *testing.T) {
	h := newCodexOAuthTestHandler()
	h.flowTTL = 10 * time.Millisecond
	r := newCodexOAuthTestRouter(h)

	_, resp := codexDoJSON(t, r, http.MethodPost, "/codex/oauth/start", "")
	state := resp["data"].(map[string]any)["state"].(string)
	time.Sleep(30 * time.Millisecond)

	if w, _ := codexDoJSON(t, r, http.MethodGet, "/codex/oauth/status?state="+state, ""); w.Code != http.StatusNotFound {
		t.Errorf("expired flow status = %d, want 404", w.Code)
	}
}
