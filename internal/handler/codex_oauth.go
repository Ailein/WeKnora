package handler

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/codexauth"
)

// CodexOAuthHandler drives the "Sign in with ChatGPT" flow for the Codex
// (ChatGPT subscription) model provider.
//
// Unlike the MCP OAuth flow, the redirect URI here is fixed by OpenAI at
// http://localhost:1455/auth/callback — it can never point at WeKnora's own
// public URL. So while a flow is active this handler runs a dedicated
// listener on :1455 (reachable when the browser sits on the same machine as
// the server, e.g. docker-compose on a laptop with the port mapped) and the
// frontend polls Status until the callback lands. When the listener is
// unreachable (remote deployment, port taken), the user pastes the callback
// URL from the browser's address bar into Exchange instead.
type CodexOAuthHandler struct {
	mu    sync.Mutex
	flows map[string]*codexOAuthFlow

	listener   net.Listener
	server     *http.Server
	flowTTL    time.Duration
	listenAddr string
}

type codexOAuthFlow struct {
	verifier  string
	createdAt time.Time
	// status: pending → exchanging → complete | error
	status string
	result *codexauth.ExchangeResult
	errMsg string
}

// NewCodexOAuthHandler constructs the handler.
func NewCodexOAuthHandler() *CodexOAuthHandler {
	return &CodexOAuthHandler{
		flows:      map[string]*codexOAuthFlow{},
		flowTTL:    15 * time.Minute,
		listenAddr: codexauth.CallbackAddr,
	}
}

// Start begins an authorization attempt.
//
// Start godoc
// @Summary      发起 Codex (ChatGPT 订阅) OAuth 授权
// @Description  生成 PKCE 授权链接并在 :1455 启动本地回调监听；前端打开链接后轮询 status
// @Tags         模型
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "{state, authorization_url, callback_listening}"
// @Security     Bearer
// @Router       /codex/oauth/start [post]
func (h *CodexOAuthHandler) Start(c *gin.Context) {
	ctx := c.Request.Context()
	flow, err := codexauth.NewLoginFlow()
	if err != nil {
		c.Error(errors.NewInternalServerError("failed to start Codex OAuth flow: " + err.Error()))
		return
	}

	h.mu.Lock()
	h.sweepLocked()
	h.flows[flow.State] = &codexOAuthFlow{
		verifier:  flow.Verifier,
		createdAt: time.Now(),
		status:    "pending",
	}
	listening := h.ensureListenerLocked(ctx)
	h.mu.Unlock()

	// Expire the flow (and free the port) even if the user abandons the tab
	// and the frontend never polls again.
	time.AfterFunc(h.flowTTL+time.Second, func() {
		h.mu.Lock()
		h.sweepLocked()
		h.stopListenerIfIdleLocked(context.Background())
		h.mu.Unlock()
	})

	logger.Infof(ctx, "[CodexOAuth] flow started, state=%s, callback_listening=%v", flow.State, listening)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"state":              flow.State,
		"authorization_url":  flow.URL,
		"callback_listening": listening,
	}})
}

// Status reports one attempt's progress; terminal states are returned exactly
// once (the flow is deleted with the response) since they carry the tokens.
//
// Status godoc
// @Summary      查询 Codex OAuth 授权进度
// @Description  pending 时继续轮询；complete 时一次性返回 access/refresh token（随后即失效）
// @Tags         模型
// @Produce      json
// @Param        state  query  string  true  "start 返回的 state"
// @Success      200  {object}  map[string]interface{}  "{status, access_token?, refresh_token?, email?, plan?, error?}"
// @Security     Bearer
// @Router       /codex/oauth/status [get]
func (h *CodexOAuthHandler) Status(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.Error(errors.NewValidationError("state is required"))
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked()
	flow, ok := h.flows[state]
	if !ok {
		c.Error(errors.NewNotFoundError("authorization flow not found or expired"))
		return
	}
	switch flow.status {
	case "complete":
		data := flowResultJSON(flow.result)
		delete(h.flows, state)
		h.stopListenerIfIdleLocked(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
	case "error":
		msg := flow.errMsg
		delete(h.flows, state)
		h.stopListenerIfIdleLocked(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"status": "error", "error": msg}})
	default: // pending / exchanging
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"status": "pending"}})
	}
}

type codexOAuthExchangeRequest struct {
	State string `json:"state" binding:"required"`
	// Input is whatever the user pasted: the full localhost:1455 callback URL,
	// its query string, or the bare authorization code.
	Input string `json:"input" binding:"required"`
}

// Exchange completes the flow from a manually pasted callback URL/code — the
// fallback for deployments where the browser's redirect to localhost:1455
// cannot reach this process.
//
// Exchange godoc
// @Summary      手动提交 Codex OAuth 回调
// @Description  浏览器跳转 localhost:1455 失败时，把回调 URL（或授权码）粘贴到此端点完成令牌交换
// @Tags         模型
// @Accept       json
// @Produce      json
// @Param        request  body  map[string]interface{}  true  "{state: string, input: string}"
// @Success      200  {object}  map[string]interface{}  "{status, access_token?, refresh_token?, email?, plan?}"
// @Security     Bearer
// @Router       /codex/oauth/exchange [post]
func (h *CodexOAuthHandler) Exchange(c *gin.Context) {
	ctx := c.Request.Context()
	var req codexOAuthExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}
	code, pastedState := codexauth.ParseAuthorizationInput(req.Input)
	if code == "" {
		c.Error(errors.NewValidationError("no authorization code found in the pasted input"))
		return
	}
	if pastedState != "" && pastedState != req.State {
		c.Error(errors.NewBadRequestError("state mismatch: the pasted URL belongs to a different login attempt"))
		return
	}

	h.mu.Lock()
	h.sweepLocked()
	flow, ok := h.flows[req.State]
	if !ok {
		h.mu.Unlock()
		c.Error(errors.NewNotFoundError("authorization flow not found or expired"))
		return
	}
	if flow.status != "pending" {
		// The :1455 callback beat the paste (or a double submit): let the
		// poller pick the terminal result up via Status.
		h.mu.Unlock()
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"status": "pending"}})
		return
	}
	flow.status = "exchanging"
	verifier := flow.verifier
	h.mu.Unlock()

	result, err := codexauth.ExchangeCode(ctx, code, verifier)

	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		flow.status = "error"
		flow.errMsg = err.Error()
		logger.Errorf(ctx, "[CodexOAuth] manual exchange failed: %v", err)
		c.Error(errors.NewBadRequestError("token exchange failed: " + err.Error()))
		return
	}
	logger.Infof(ctx, "[CodexOAuth] manual exchange complete, state=%s", req.State)
	data := flowResultJSON(result)
	delete(h.flows, req.State)
	h.stopListenerIfIdleLocked(ctx)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func flowResultJSON(res *codexauth.ExchangeResult) gin.H {
	return gin.H{
		"status":        "complete",
		"access_token":  res.AccessToken,
		"refresh_token": res.RefreshToken,
		"email":         res.Email,
		"plan":          res.Plan,
		"expires_at":    res.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// sweepLocked drops expired flows. Callers hold h.mu.
func (h *CodexOAuthHandler) sweepLocked() {
	cutoff := time.Now().Add(-h.flowTTL)
	for state, flow := range h.flows {
		if flow.createdAt.Before(cutoff) {
			delete(h.flows, state)
		}
	}
}

// ensureListenerLocked lazily binds :1455. Returns whether a listener is up.
// Callers hold h.mu. Bind failure is not fatal — the paste fallback still
// works — so it only logs.
func (h *CodexOAuthHandler) ensureListenerLocked(ctx context.Context) bool {
	if h.listener != nil {
		return true
	}
	ln, err := net.Listen("tcp", h.listenAddr)
	if err != nil {
		logger.Warnf(ctx, "[CodexOAuth] cannot listen on %s (%v); manual paste fallback only", h.listenAddr, err)
		return false
	}
	srv := &http.Server{Handler: h.callbackHandler(), ReadHeaderTimeout: 10 * time.Second}
	h.listener = ln
	h.server = srv
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			logger.Warnf(context.Background(), "[CodexOAuth] callback listener exited: %v", serveErr)
		}
		h.mu.Lock()
		if h.server == srv {
			h.listener = nil
			h.server = nil
		}
		h.mu.Unlock()
	}()
	logger.Infof(ctx, "[CodexOAuth] callback listener started on %s", h.listenAddr)
	return true
}

// stopListenerIfIdleLocked releases :1455 once no live flow needs it, so a
// local `codex login` on the same machine can grab the port again. Callers
// hold h.mu.
func (h *CodexOAuthHandler) stopListenerIfIdleLocked(ctx context.Context) {
	if h.server == nil {
		return
	}
	for _, flow := range h.flows {
		if flow.status == "pending" || flow.status == "exchanging" {
			return
		}
	}
	srv := h.server
	h.listener = nil
	h.server = nil
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Infof(ctx, "[CodexOAuth] callback listener stopped (no active flows)")
}

// callbackHandler serves the :1455 redirect target. It is unauthenticated by
// design (the browser carries no WeKnora bearer); the single-use state is the
// authenticator, exactly like the MCP OAuth callback route.
func (h *CodexOAuthHandler) callbackHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(codexauth.CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		state := r.URL.Query().Get("state")
		code := r.URL.Query().Get("code")
		providerErr := r.URL.Query().Get("error")

		h.mu.Lock()
		h.sweepLocked()
		flow, ok := h.flows[state]
		if !ok {
			h.mu.Unlock()
			writeCodexOAuthPage(w, http.StatusBadRequest, false,
				"授权流程不存在或已过期 / Login attempt not found or expired",
				"请回到 WeKnora 页面重新点击授权按钮。Please restart the login from WeKnora.")
			return
		}
		if flow.status != "pending" {
			status := flow.status
			h.mu.Unlock()
			if status == "complete" {
				writeCodexOAuthPage(w, http.StatusOK, true,
					"授权已完成 / Already authorized",
					"回到 WeKnora 页面即可，此窗口可关闭。Return to WeKnora; you can close this window.")
			} else {
				writeCodexOAuthPage(w, http.StatusConflict, false,
					"授权正在处理或已失败 / Login already being processed or failed",
					"请回到 WeKnora 页面查看状态。Check the status back in WeKnora.")
			}
			return
		}
		if providerErr != "" || code == "" {
			flow.status = "error"
			if providerErr != "" {
				flow.errMsg = "authorization denied: " + providerErr
			} else {
				flow.errMsg = "callback carried no authorization code"
			}
			h.mu.Unlock()
			writeCodexOAuthPage(w, http.StatusBadRequest, false,
				"授权未完成 / Authorization failed",
				"请回到 WeKnora 页面重试。Please retry from WeKnora.")
			return
		}
		flow.status = "exchanging"
		verifier := flow.verifier
		h.mu.Unlock()

		// Detached context: the exchange must finish even if the user closes
		// the browser tab the instant the page starts loading.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		result, err := codexauth.ExchangeCode(ctx, code, verifier)

		h.mu.Lock()
		if err != nil {
			flow.status = "error"
			flow.errMsg = err.Error()
			h.mu.Unlock()
			logger.Errorf(ctx, "[CodexOAuth] callback exchange failed: %v", err)
			writeCodexOAuthPage(w, http.StatusBadGateway, false,
				"令牌交换失败 / Token exchange failed",
				html.EscapeString(err.Error()))
			return
		}
		flow.status = "complete"
		flow.result = result
		h.mu.Unlock()
		logger.Infof(ctx, "[CodexOAuth] callback exchange complete, state=%s", state)

		writeCodexOAuthPage(w, http.StatusOK, true,
			"授权成功 / Signed in",
			"回到 WeKnora 页面，令牌已自动填入。此窗口可关闭。Return to WeKnora — the tokens are filled in. You can close this window.")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeCodexOAuthPage(w, http.StatusNotFound, false,
			"路径不存在 / Not found",
			"这是 WeKnora 的 ChatGPT 授权回调端口。This port only serves the WeKnora ChatGPT OAuth callback.")
	})
	return mux
}

// writeCodexOAuthPage renders the minimal self-contained result page shown in
// the OAuth popup. window.close() succeeds because the popup was opened by
// script from the WeKnora tab.
func writeCodexOAuthPage(w http.ResponseWriter, status int, ok bool, title, detail string) {
	icon, color := "✓", "#07c05f"
	if !ok {
		icon, color = "✕", "#d54941"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>WeKnora · ChatGPT 授权</title>
<style>
body{margin:0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;background:#f5f7fa;color:#333}
.card{background:#fff;border-radius:12px;box-shadow:0 4px 24px rgba(0,0,0,.08);padding:48px 56px;text-align:center;max-width:440px}
.icon{width:56px;height:56px;line-height:56px;border-radius:50%%;background:%s;color:#fff;font-size:28px;margin:0 auto 20px}
h1{font-size:18px;margin:0 0 12px}p{font-size:14px;color:#666;line-height:1.7;margin:0}
</style></head><body><div class="card"><div class="icon">%s</div><h1>%s</h1><p>%s</p></div>
<script>if(%t){setTimeout(function(){try{window.close()}catch(e){}},1800)}</script>
</body></html>`, color, icon, html.EscapeString(title), detail, ok)
}
