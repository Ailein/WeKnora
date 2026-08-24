package handler

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// inboxHeartbeatInterval keeps idle SSE connections alive through proxies.
const inboxHeartbeatInterval = 25 * time.Second

// ListIMInbox lists the tenant's IM conversations for the operator inbox.
// GET /api/v1/im-inbox
//
// ListIMInbox godoc
// @Summary      查询运营者收件箱会话列表
// @Description  列出当前租户全部 IM 对话：人工接管中的置顶，其余按最近消息排序；含未读数、最近消息预览与渠道信息
// @Tags         IM 渠道
// @Produce      json
// @Param        filter         query     string  false  "过滤：human（人工接管中）/ unread（有未读），默认全部"
// @Param        im_channel_id  query     string  false  "仅列出指定 IM 渠道"
// @Param        page           query     int     false  "页码（默认 1）"
// @Param        page_size      query     int     false  "每页数量（默认 100，最大 200）"
// @Success      200            {object}  map[string]interface{}  "items/total/unread_total"
// @Failure      401            {object}  map[string]interface{}  "未认证"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-inbox [get]
func (h *IMHandler) ListIMInbox(c *gin.Context) {
	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	opts := im.InboxListOptions{
		Filter:      c.Query("filter"),
		IMChannelID: c.Query("im_channel_id"),
	}
	if v, err := strconv.Atoi(c.DefaultQuery("page", "1")); err == nil {
		opts.Page = v
	}
	if v, err := strconv.Atoi(c.DefaultQuery("page_size", "0")); err == nil {
		opts.PageSize = v
	}

	list, err := h.imService.ListInbox(c.Request.Context(), tenantID, opts)
	if err != nil {
		logger.Errorf(c.Request.Context(), "IM inbox list failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list IM inbox"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// MarkIMInboxRead clears the unread counter of one conversation.
// POST /api/v1/im-inbox/sessions/:session_id/read
//
// MarkIMInboxRead godoc
// @Summary      标记收件箱会话已读
// @Description  运营者打开会话后清零其未读计数；幂等，返回租户剩余未读总数
// @Tags         IM 渠道
// @Produce      json
// @Param        session_id  path      string                  true  "会话 ID"
// @Success      200         {object}  map[string]interface{}  "unread_total"
// @Failure      401         {object}  map[string]interface{}  "未认证"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-inbox/sessions/{session_id}/read [post]
func (h *IMHandler) MarkIMInboxRead(c *gin.Context) {
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

	total, err := h.imService.MarkInboxRead(c.Request.Context(), tenantID, sessionID)
	if err != nil {
		logger.Errorf(c.Request.Context(), "IM inbox mark-read failed for session %s: %v", sessionID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark conversation read"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"unread_total": total}})
}

// StreamIMInbox pushes realtime inbox events over SSE.
// GET /api/v1/im-inbox/stream
//
// StreamIMInbox godoc
// @Summary      订阅收件箱实时事件（SSE）
// @Description  Server-Sent Events 流：连接后先收到 type=ready（含未读总数），此后每条 IM 消息/已读操作推送 type=session 事件，item 为更新后的会话条目
// @Tags         IM 渠道
// @Produce      text/event-stream
// @Success      200  {string}  string  "SSE 事件流"
// @Failure      401  {object}  map[string]interface{}  "未认证"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-inbox/stream [get]
func (h *IMHandler) StreamIMInbox(c *gin.Context) {
	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	events, cancel := h.imService.SubscribeInbox(tenantID)
	defer cancel()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// Handshake carries the current badge total so the client needs no extra
	// round-trip before rendering it.
	unread, err := h.imService.InboxUnreadTotal(c.Request.Context(), tenantID)
	if err != nil {
		logger.Warnf(c.Request.Context(), "IM inbox stream failed to load unread total: %v", err)
	}
	c.SSEvent("message", im.InboxEvent{Type: "ready", UnreadTotal: unread})
	c.Writer.Flush()

	heartbeat := time.NewTicker(inboxHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			// SSE comment line: ignored by clients, defeats proxy idle timeouts.
			if _, err := io.WriteString(c.Writer, ": ping\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case evt, ok := <-events:
			if !ok {
				return
			}
			c.SSEvent("message", evt)
			c.Writer.Flush()
		}
	}
}

// GetIMQuickReplies returns the tenant's canned inbox phrases.
// GET /api/v1/im-inbox/quick-replies
//
// GetIMQuickReplies godoc
// @Summary      查询收件箱快捷短语
// @Description  返回当前租户的快捷短语列表（运营者共享）
// @Tags         IM 渠道
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "items"
// @Failure      401  {object}  map[string]interface{}  "未认证"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-inbox/quick-replies [get]
func (h *IMHandler) GetIMQuickReplies(c *gin.Context) {
	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	items, err := h.imService.GetQuickReplies(c.Request.Context(), tenantID)
	if err != nil {
		logger.Errorf(c.Request.Context(), "IM quick replies load failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quick replies"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items}})
}

// PutIMQuickReplies replaces the tenant's canned inbox phrases.
// PUT /api/v1/im-inbox/quick-replies
//
// PutIMQuickReplies godoc
// @Summary      更新收件箱快捷短语
// @Description  整体替换当前租户的快捷短语列表（最多 50 条，每条 500 字符内；空白项自动剔除）
// @Tags         IM 渠道
// @Accept       json
// @Produce      json
// @Param        request  body      map[string]interface{}  true  "items：字符串数组"
// @Success      200      {object}  map[string]interface{}  "规范化后的 items"
// @Failure      400      {object}  map[string]interface{}  "超出数量或长度限制"
// @Failure      401      {object}  map[string]interface{}  "未认证"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-inbox/quick-replies [put]
func (h *IMHandler) PutIMQuickReplies(c *gin.Context) {
	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req struct {
		Items []string `json:"items"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	items, err := h.imService.SetQuickReplies(c.Request.Context(), tenantID, req.Items)
	if err != nil {
		switch {
		case errors.Is(err, im.ErrQuickRepliesTooMany), errors.Is(err, im.ErrQuickReplyTooLong):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			logger.Errorf(c.Request.Context(), "IM quick replies save failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save quick replies"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items}})
}
