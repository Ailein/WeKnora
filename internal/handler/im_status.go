package handler

import (
	"net/http"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/im/whatsapp"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// GetIMChannelStatus reports the runtime health of an IM channel.
// GET /api/v1/im-channels/:id/status
//
// GetIMChannelStatus godoc
// @Summary      查询 IM 渠道运行状态
// @Description  返回渠道运行时健康状态（connected/connecting/logged_out/needs_pairing/…）；长连接平台为实时状态，其余按持久化状态推断
// @Tags         IM 渠道
// @Produce      json
// @Param        id   path      string                  true  "渠道 ID"
// @Success      200  {object}  map[string]interface{}  "状态（state/detail/since）"
// @Failure      404  {object}  map[string]interface{}  "渠道不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-channels/{id}/status [get]
func (h *IMHandler) GetIMChannelStatus(c *gin.Context) {
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

	c.JSON(http.StatusOK, gin.H{"data": h.resolveChannelStatus(c, channel)})
}

// resolveChannelStatus layers the answer: disabled beats everything, then the
// live runtime (local adapter or the Redis mirror of another replica), then
// durable evidence for channels nothing is running.
func (h *IMHandler) resolveChannelStatus(c *gin.Context, channel *im.IMChannel) *im.ChannelStatus {
	if !channel.Enabled {
		return &im.ChannelStatus{State: im.ChannelStateDisabled}
	}
	if st, ok := h.imService.ChannelRuntimeStatus(channel.ID); ok {
		return st
	}

	st := &im.ChannelStatus{
		State:  im.ChannelStateNotRunning,
		Detail: h.imService.LastStartError(channel.ID),
	}
	if channel.Platform != string(im.PlatformWhatsApp) {
		return st
	}

	// WhatsApp: the session store tells us definitively whether the pairing
	// is still usable, even when no instance has the channel running.
	creds, err := im.ParseCredentials(channel.Credentials)
	if err != nil {
		return st
	}
	deviceJID := im.GetString(creds, "device_jid")
	if deviceJID == "" {
		return &im.ChannelStatus{State: im.ChannelStateNeedsPairing, Detail: "no device paired yet; scan the QR code"}
	}
	exists, err := whatsapp.DeviceExists(c.Request.Context(), h.db, deviceJID)
	if err != nil {
		logger.Warnf(c.Request.Context(), "[IM] WhatsApp device probe failed for channel %s: %v", channel.ID, err)
		return st
	}
	if !exists {
		return &im.ChannelStatus{
			State:  im.ChannelStateNeedsPairing,
			Detail: "the linked device was removed from the WhatsApp account; re-scan the QR code",
		}
	}
	return st
}
