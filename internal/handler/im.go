package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/im/whatsapp"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// validIMPlatforms is the set of supported IM platforms.
var validIMPlatforms = map[string]bool{
	"wecom": true, "feishu": true, "lark": true, "slack": true, "telegram": true, "dingtalk": true,
	"mattermost": true, "wechat": true, "qqbot": true, "yunzhijia": true, "whatsapp": true,
}

// invalidIMPlatformError is the 400 message listing the accepted platforms. It
// is derived from validIMPlatforms so the two cannot drift apart as platforms
// are added.
var invalidIMPlatformError = func() string {
	names := make([]string, 0, len(validIMPlatforms))
	for p := range validIMPlatforms {
		names = append(names, "'"+p+"'")
	}
	sort.Strings(names)
	return "platform must be one of: " + strings.Join(names, ", ")
}()

// IMHandler handles IM platform callback requests and channel CRUD.
type IMHandler struct {
	imService       *im.Service
	whatsappPairing *whatsapp.PairingService
	// db backs durable status probes (e.g. whatsmeow device existence) for
	// channels that have no live runtime to ask.
	db *gorm.DB
}

// NewIMHandler creates a new IM handler.
func NewIMHandler(imService *im.Service, db *gorm.DB, redisClient *redis.Client) *IMHandler {
	return &IMHandler{
		imService:       imService,
		whatsappPairing: whatsapp.NewPairingService(db, redisClient),
		db:              db,
	}
}

// enforcePlatformFixedModes pins mode/output_mode for platforms whose
// transport is not user-selectable (wechat: long-poll only; whatsapp:
// leader-elected websocket with full output). Create and Update both apply
// it so the invariant cannot be bypassed by a later PUT.
func enforcePlatformFixedModes(channel *im.IMChannel) {
	switch channel.Platform {
	case "wechat":
		channel.Mode = "longpoll"
		channel.OutputMode = "full"
	case "whatsapp":
		channel.Mode = "websocket"
		channel.OutputMode = "full"
	}
}

// ── Channel CRUD handlers ──

// CreateIMChannel creates a new IM channel for an agent.
func (h *IMHandler) CreateIMChannel(c *gin.Context) {
	agentID := c.Param("id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent_id is required"})
		return
	}

	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req struct {
		Platform        string     `json:"platform" binding:"required"`
		Name            string     `json:"name"`
		Mode            string     `json:"mode"`
		OutputMode      string     `json:"output_mode"`
		SessionMode     string     `json:"session_mode"`
		KnowledgeBaseID string     `json:"knowledge_base_id"`
		Credentials     types.JSON `json:"credentials"`
		Enabled         *bool      `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !validIMPlatforms[req.Platform] {
		c.JSON(http.StatusBadRequest, gin.H{"error": invalidIMPlatformError})
		return
	}

	channel := &im.IMChannel{
		TenantID:        tenantID,
		AgentID:         agentID,
		Platform:        req.Platform,
		Name:            req.Name,
		Mode:            req.Mode,
		OutputMode:      req.OutputMode,
		SessionMode:     req.SessionMode,
		KnowledgeBaseID: req.KnowledgeBaseID,
		Credentials:     req.Credentials,
		Enabled:         true,
	}
	if req.Enabled != nil {
		channel.Enabled = *req.Enabled
	}
	// WeChat only works over long-polling; WhatsApp is connection-based
	// (leader-elected socket) and does not stream: frequent message edits
	// are conspicuous automation on an unofficial client.
	if req.Platform == "wechat" || req.Platform == "whatsapp" {
		enforcePlatformFixedModes(channel)
	} else {
		if channel.Mode == "" {
			if channel.Platform == "mattermost" || channel.Platform == "yunzhijia" {
				channel.Mode = "webhook"
			} else {
				channel.Mode = "websocket"
			}
		}
		if channel.OutputMode == "" {
			channel.OutputMode = "stream"
		}
	}
	if channel.Credentials == nil {
		channel.Credentials = types.JSON("{}")
	}

	if err := h.imService.CreateChannel(channel); err != nil {
		logger.Errorf(c.Request.Context(), "[IM] Create channel failed: %v", err)
		if strings.HasPrefix(err.Error(), "duplicate_bot:") {
			c.JSON(http.StatusConflict, gin.H{"error": strings.TrimPrefix(err.Error(), "duplicate_bot: ")})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create channel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": channel})
}

// ListIMChannels lists all IM channels for an agent.
func (h *IMHandler) ListIMChannels(c *gin.Context) {
	agentID := c.Param("id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent_id is required"})
		return
	}

	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	channels, err := h.imService.ListChannelsByAgent(agentID, tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list channels"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": im.SummarizeIMChannels(channels)})
}

// ListAllIMChannels lists every IM channel in the current tenant, across
// agents, for the cross-agent overview page. Credentials are intentionally
// NOT included in the response.
func (h *IMHandler) ListAllIMChannels(c *gin.Context) {
	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	channels, err := h.imService.ListChannelsByTenant(tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list channels"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": channels})
}

// UpdateIMChannel updates an IM channel.
//
// UpdateIMChannel godoc
// @Summary      更新 IM 渠道
// @Description  更新指定 IM 渠道的名称、模式、知识库、凭证或启用状态
// @Tags         IM 渠道
// @Accept       json
// @Produce      json
// @Param        id       path      string                  true  "渠道 ID"
// @Param        request  body      map[string]interface{}  true  "更新字段（name/mode/output_mode/knowledge_base_id/credentials/enabled）"
// @Success      200      {object}  map[string]interface{}  "更新后的渠道"
// @Failure      400      {object}  map[string]interface{}  "请求参数错误"
// @Failure      404      {object}  map[string]interface{}  "渠道不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-channels/{id} [put]
func (h *IMHandler) UpdateIMChannel(c *gin.Context) {
	channelID := c.Param("id")
	if channelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel id is required"})
		return
	}

	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	channel, err := h.imService.GetChannelByIDAndTenant(channelID, tenantID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}

	var req struct {
		Name            *string    `json:"name"`
		Mode            *string    `json:"mode"`
		OutputMode      *string    `json:"output_mode"`
		SessionMode     *string    `json:"session_mode"`
		KnowledgeBaseID *string    `json:"knowledge_base_id"`
		Credentials     types.JSON `json:"credentials"`
		Enabled         *bool      `json:"enabled"`
		AgentID         *string    `json:"agent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name != nil {
		channel.Name = *req.Name
	}
	if req.Mode != nil {
		channel.Mode = *req.Mode
	}
	if req.OutputMode != nil {
		channel.OutputMode = *req.OutputMode
	}
	// Platform-fixed transports must survive updates too, or a PUT could flip
	// e.g. whatsapp to "webhook": the factory would still open its socket on
	// every replica (webhook mode skips leader election), and concurrent
	// connections with one WhatsApp session corrupt its Signal state.
	enforcePlatformFixedModes(channel)
	if req.SessionMode != nil {
		channel.SessionMode = *req.SessionMode
	}
	if req.KnowledgeBaseID != nil {
		channel.KnowledgeBaseID = *req.KnowledgeBaseID
	}
	if req.Credentials != nil {
		channel.Credentials = im.MergeUpdatedCredentials(channel.Platform, channel.Credentials, req.Credentials)
	}
	if req.Enabled != nil {
		channel.Enabled = *req.Enabled
	}
	if req.AgentID != nil {
		newAgentID := strings.TrimSpace(*req.AgentID)
		if newAgentID != "" && newAgentID != channel.AgentID {
			if err := h.imService.SetChannelAgentID(c.Request.Context(), channel, newAgentID); err != nil {
				logger.Errorf(c.Request.Context(), "[IM] Update channel agent failed: %v", err)
				c.JSON(http.StatusBadRequest, gin.H{"error": "agent not found"})
				return
			}
		}
	}

	if err := h.imService.UpdateChannel(channel); err != nil {
		logger.Errorf(c.Request.Context(), "[IM] Update channel failed: %v", err)
		if strings.HasPrefix(err.Error(), "duplicate_bot:") {
			c.JSON(http.StatusConflict, gin.H{"error": strings.TrimPrefix(err.Error(), "duplicate_bot: ")})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update channel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": channel})
}

// DeleteIMChannel deletes an IM channel.
//
// DeleteIMChannel godoc
// @Summary      删除 IM 渠道
// @Description  删除指定 IM 渠道
// @Tags         IM 渠道
// @Produce      json
// @Param        id   path      string                  true  "渠道 ID"
// @Success      200  {object}  map[string]interface{}  "success: true"
// @Failure      400  {object}  map[string]interface{}  "请求参数错误"
// @Failure      404  {object}  map[string]interface{}  "渠道不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-channels/{id} [delete]
func (h *IMHandler) DeleteIMChannel(c *gin.Context) {
	channelID := c.Param("id")
	if channelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel id is required"})
		return
	}

	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	if err := h.imService.DeleteChannel(channelID, tenantID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete channel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ToggleIMChannel toggles the enabled state of an IM channel.
//
// ToggleIMChannel godoc
// @Summary      启用/停用 IM 渠道
// @Description  切换指定 IM 渠道的启用状态
// @Tags         IM 渠道
// @Produce      json
// @Param        id   path      string                  true  "渠道 ID"
// @Success      200  {object}  map[string]interface{}  "更新后的渠道"
// @Failure      400  {object}  map[string]interface{}  "请求参数错误"
// @Failure      404  {object}  map[string]interface{}  "渠道不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-channels/{id}/toggle [post]
func (h *IMHandler) ToggleIMChannel(c *gin.Context) {
	channelID := c.Param("id")
	if channelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel id is required"})
		return
	}

	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	channel, err := h.imService.ToggleChannel(channelID, tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to toggle channel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": channel})
}

// SendIMSessionReply delivers an operator-typed manual reply into the IM
// conversation bound to a session (human takeover). Admin-only: the reply is
// sent to an external IM user under the bot's identity.
//
// SendIMSessionReply godoc
// @Summary      发送 IM 人工回复
// @Description  以人工身份向指定会话绑定的 IM 对话直接发送消息（不触发 AI 回答），并记入会话历史。支持 JSON（纯文本）或 multipart/form-data（content 字段 + images/attachments 文件）
// @Tags         IM 渠道
// @Accept       json
// @Accept       multipart/form-data
// @Produce      json
// @Param        session_id  path      string                  true  "会话 ID"
// @Param        request     body      map[string]interface{}  true  "消息内容（content）"
// @Success      200         {object}  map[string]interface{}  "已持久化的消息"
// @Failure      400         {object}  map[string]interface{}  "内容为空/过长、附件超限或平台不支持"
// @Failure      404         {object}  map[string]interface{}  "会话未绑定 IM 对话"
// @Failure      409         {object}  map[string]interface{}  "渠道运行时不在本实例"
// @Failure      502         {object}  map[string]interface{}  "IM 平台投递失败"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-sessions/{session_id}/messages [post]
func (h *IMHandler) SendIMSessionReply(c *gin.Context) {
	sessionID := c.Param("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session id is required"})
		return
	}

	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	content, attachments, err := parseManualReplyRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msg, err := h.imService.SendManualReply(c.Request.Context(), tenantID, sessionID, content, attachments)
	if err != nil {
		switch {
		case errors.Is(err, im.ErrManualReplyEmptyContent),
			errors.Is(err, im.ErrManualReplyTooLong),
			errors.Is(err, im.ErrManualReplyUnsupported),
			errors.Is(err, im.ErrManualReplyTooManyAttachments),
			errors.Is(err, im.ErrManualReplyAttachmentTooLarge),
			errors.Is(err, im.ErrManualReplyBadAttachment),
			errors.Is(err, im.ErrManualReplyMediaUnsupported):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, im.ErrManualReplyNotIMSession):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, im.ErrManualReplyNotRunning):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, im.ErrManualReplyDelivery):
			logger.Errorf(c.Request.Context(), "[IM] Manual reply delivery failed for session %s: %v", sessionID, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		default:
			logger.Errorf(c.Request.Context(), "[IM] Manual reply failed for session %s: %v", sessionID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send manual reply"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": msg})
}

// writeIMHandlingError maps takeover-state failures onto precise status codes
// shared by the GET and PUT endpoints.
func writeIMHandlingError(c *gin.Context, sessionID string, err error) {
	switch {
	case errors.Is(err, im.ErrHandlingUnsupported),
		errors.Is(err, im.ErrHandlingInvalidMode),
		errors.Is(err, im.ErrHandlingInvalidTimeout):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, im.ErrHandlingNotIMSession):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, im.ErrHandlingConversationGone):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		logger.Errorf(c.Request.Context(), "[IM] Session handling request failed for session %s: %v", sessionID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process session handling"})
	}
}

// GetIMSessionHandling reports whether the conversation bound to a session is
// answered by the bot or held by a human operator.
//
// GetIMSessionHandling godoc
// @Summary      查询 IM 会话接管状态
// @Description  返回会话绑定的 IM 对话当前由谁应答（bot/human）、接管到期时间与窗口时长
// @Tags         IM 渠道
// @Produce      json
// @Param        session_id  path      string                  true  "会话 ID"
// @Success      200         {object}  map[string]interface{}  "接管状态"
// @Failure      400         {object}  map[string]interface{}  "平台不支持接管"
// @Failure      404         {object}  map[string]interface{}  "会话未绑定 IM 对话"
// @Failure      409         {object}  map[string]interface{}  "对话已重置且无活跃后继"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-sessions/{session_id}/handling [get]
func (h *IMHandler) GetIMSessionHandling(c *gin.Context) {
	sessionID := c.Param("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session id is required"})
		return
	}
	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	handling, err := h.imService.GetSessionHandling(c.Request.Context(), tenantID, sessionID)
	if err != nil {
		writeIMHandlingError(c, sessionID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": handling})
}

// SetIMSessionHandling switches the conversation bound to a session between
// bot handling and human takeover. Admin-only, like manual replies: it mutes
// or unmutes the bot for an external IM user.
//
// SetIMSessionHandling godoc
// @Summary      切换 IM 会话接管状态
// @Description  mode=human 时机器人对该对话静默（消息仍记录进会话），可选 timeout_minutes 设置无人工活动后自动恢复；mode=bot 立即交还机器人
// @Tags         IM 渠道
// @Accept       json
// @Produce      json
// @Param        session_id  path      string                  true  "会话 ID"
// @Param        request     body      map[string]interface{}  true  "mode（bot/human）与可选 timeout_minutes（0=不过期，5-1440）"
// @Success      200         {object}  map[string]interface{}  "更新后的接管状态"
// @Failure      400         {object}  map[string]interface{}  "mode/timeout 非法或平台不支持"
// @Failure      404         {object}  map[string]interface{}  "会话未绑定 IM 对话"
// @Failure      409         {object}  map[string]interface{}  "对话已重置且无活跃后继"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-sessions/{session_id}/handling [put]
func (h *IMHandler) SetIMSessionHandling(c *gin.Context) {
	sessionID := c.Param("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session id is required"})
		return
	}
	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req struct {
		Mode string `json:"mode"`
		// Pointer distinguishes "omitted" (default window) from an explicit 0
		// (takeover with no expiry).
		TimeoutMinutes *int `json:"timeout_minutes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	timeout := -1 // service substitutes the default window
	if req.TimeoutMinutes != nil {
		timeout = *req.TimeoutMinutes
	}
	handling, err := h.imService.SetSessionHandling(c.Request.Context(), tenantID, sessionID, req.Mode, timeout)
	if err != nil {
		writeIMHandlingError(c, sessionID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": handling})
}

// parseManualReplyRequest accepts both the JSON body used for text-only
// replies and multipart/form-data — a "content" field plus files under
// "images" (must be image/*, delivered inline) and "attachments" (delivered
// as documents).
func parseManualReplyRequest(c *gin.Context) (string, []*im.ReplyAttachment, error) {
	if !strings.HasPrefix(c.ContentType(), "multipart/") {
		var req struct {
			Content string `json:"content"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			return "", nil, errors.New("invalid request body")
		}
		return req.Content, nil, nil
	}

	form, err := c.MultipartForm()
	if err != nil {
		return "", nil, errors.New("invalid multipart form")
	}
	var content string
	if vals := form.Value["content"]; len(vals) > 0 {
		content = vals[0]
	}
	var attachments []*im.ReplyAttachment
	for _, fh := range form.File["images"] {
		att, err := readManualReplyFile(fh, true)
		if err != nil {
			return "", nil, err
		}
		attachments = append(attachments, att)
	}
	for _, fh := range form.File["attachments"] {
		att, err := readManualReplyFile(fh, false)
		if err != nil {
			return "", nil, err
		}
		attachments = append(attachments, att)
	}
	return content, attachments, nil
}

// manualReplyImageMimes are the image formats accepted for inline manual-reply
// images (matching what WhatsApp renders inline).
var manualReplyImageMimes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// readManualReplyFile buffers one uploaded file. The hard read cap only guards
// process memory; the precise per-kind size limits live in the IM service.
func readManualReplyFile(fh *multipart.FileHeader, isImage bool) (*im.ReplyAttachment, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment %q", fh.Filename)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(im.MaxIMAttachmentBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment %q", fh.Filename)
	}

	mime := fh.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		if len(data) > 0 {
			mime = http.DetectContentType(data)
		}
	}
	mime = strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])
	kind := im.MessageTypeFile
	if isImage {
		// Whitelist instead of an image/* prefix: it keeps script-capable
		// SVG out of the inline data-URI store and rejects formats WhatsApp
		// cannot render inline anyway.
		if !manualReplyImageMimes[mime] {
			return nil, fmt.Errorf("file %q is not a supported image (jpeg/png/gif/webp)", fh.Filename)
		}
		kind = im.MessageTypeImage
	}
	return &im.ReplyAttachment{
		Kind:     kind,
		FileName: filepath.Base(fh.Filename),
		MimeType: mime,
		Data:     data,
	}, nil
}

func writeIMCallbackACK(c *gin.Context, platform string) {
	if platform == "yunzhijia" {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data": gin.H{
				"type":    2,
				"content": "",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ── Callback handlers ──

// IMCallback handles IM platform callback requests for a specific channel.
// Route: POST /api/v1/im/callback/:channel_id
//
// IMCallback godoc
// @Summary      IM 平台回调
// @Description  接收各 IM 平台的事件回调；走平台自身签名校验，不使用 API Key
// @Tags         IM 回调
// @Accept       json
// @Produce      json
// @Param        channel_id  path      string                  true  "渠道 ID"
// @Success      200         {object}  map[string]interface{}  "处理结果"
// @Failure      400         {object}  map[string]interface{}  "请求参数错误"
// @Failure      401         {object}  map[string]interface{}  "签名校验失败"
// @Router       /im/callback/{channel_id} [get]
// @Router       /im/callback/{channel_id} [post]
func (h *IMHandler) IMCallback(c *gin.Context) {
	ctx := c.Request.Context()
	channelID := c.Param("channel_id")

	// Always validate the durable row before using a cached webhook adapter.
	// This is the correctness fallback when a replica missed Redis invalidation
	// while disconnected, and prevents stale credentials/config from being used.
	adapter, channel, err := h.imService.EnsureChannelAdapter(channelID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			logger.Errorf(ctx, "[IM] Channel not found for callback: %s", channelID)
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}
		if errors.Is(err, im.ErrChannelDisabled) {
			logger.Errorf(ctx, "[IM] Channel disabled for callback: %s", channelID)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "channel is disabled"})
			return
		}
		logger.Errorf(ctx, "[IM] Channel unavailable for callback %s: %v", channelID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "channel not available"})
		return
	}

	logger.Infof(ctx, "[IM] Callback received platform=%s path_channel_id=%s", channel.Platform, channelID)

	// Handle URL verification
	if adapter.HandleURLVerification(c) {
		return
	}

	// Verify callback signature
	if err := adapter.VerifyCallback(c); err != nil {
		logger.Errorf(ctx, "[IM] Callback verification failed for channel %s: %v", channelID, err)
		c.JSON(http.StatusForbidden, gin.H{"error": "verification failed"})
		return
	}

	// Parse the callback message
	msg, err := adapter.ParseCallback(c)
	if err != nil {
		logger.Errorf(ctx, "[IM] Parse callback failed for channel %s: %v", channelID, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "parse failed"})
		return
	}

	// If nil, it's a non-message event - just acknowledge
	if msg == nil {
		if channel.Platform == "mattermost" {
			logger.Infof(ctx, "[IM] Mattermost callback ignored (no message): path_channel_id=%s — check: (1) trigger word must be the *first word* of the post; (2) if channel+trigger are both set, post must be in that channel; (3) bot_user_id must not match the sender", channelID)
		} else {
			logger.Infof(ctx, "[IM] Callback parsed no message to process platform=%s path_channel_id=%s", channel.Platform, channelID)
		}
		writeIMCallbackACK(c, channel.Platform)
		return
	}

	// Respond immediately to avoid platform timeout
	writeIMCallbackACK(c, channel.Platform)

	// Detach from gin request context
	asyncCtx := context.WithoutCancel(ctx)

	// Process message asynchronously
	go func() {
		if err := h.imService.HandleMessage(asyncCtx, msg, channelID); err != nil {
			logger.Errorf(asyncCtx, "[IM] Handle message error for channel %s: %v", channelID, err)
		}
	}()
}
