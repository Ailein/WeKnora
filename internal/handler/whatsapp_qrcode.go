package handler

import (
	"net/http"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/gin-gonic/gin"
)

// WhatsAppStartPairing starts a WhatsApp QR pairing session.
// POST /api/v1/whatsapp/qrcode
//
// WhatsAppStartPairing godoc
// @Summary      获取 WhatsApp 扫码配对二维码
// @Description  启动一次 WhatsApp 设备配对，返回本地渲染的二维码 PNG（data URL）与会话标识（无请求体）
// @Tags         IM 渠道
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "配对会话（session_id + qr_png）"
// @Failure      500  {object}  map[string]interface{}  "配对启动失败"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /whatsapp/qrcode [post]
func (h *IMHandler) WhatsAppStartPairing(c *gin.Context) {
	ctx := c.Request.Context()

	status, err := h.whatsappPairing.StartPairing(ctx)
	if err != nil {
		logger.Errorf(ctx, "[WhatsApp] Failed to start pairing: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start pairing: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": status})
}

// WhatsAppPollPairing checks the status of a WhatsApp pairing session.
// POST /api/v1/whatsapp/qrcode/status
//
// WhatsAppPollPairing godoc
// @Summary      轮询 WhatsApp 配对状态
// @Description  查询配对会话状态；二维码轮换时返回新的 qr_png，success 时返回 device_jid 凭证
// @Tags         IM 渠道
// @Accept       json
// @Produce      json
// @Param        request  body      map[string]interface{}  true  "{session_id: string}"
// @Success      200      {object}  map[string]interface{}  "配对状态"
// @Failure      400      {object}  map[string]interface{}  "请求参数错误"
// @Failure      404      {object}  map[string]interface{}  "会话不存在或已过期"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /whatsapp/qrcode/status [post]
func (h *IMHandler) WhatsAppPollPairing(c *gin.Context) {
	ctx := c.Request.Context()

	var req struct {
		SessionID string `json:"session_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
		return
	}

	status, err := h.whatsappPairing.Poll(req.SessionID)
	if err != nil {
		logger.Warnf(ctx, "[WhatsApp] Poll pairing failed: %v", err)
		c.JSON(http.StatusNotFound, gin.H{"error": "pairing session not found or expired"})
		return
	}

	resp := gin.H{
		"session_id": status.SessionID,
		"status":     status.Status,
	}
	if status.QRPNG != "" {
		resp["qr_png"] = status.QRPNG
	}
	if status.Error != "" {
		resp["error"] = status.Error
	}
	// Only include credentials when pairing succeeded
	if status.Status == "success" {
		resp["credentials"] = gin.H{
			"device_jid": status.DeviceJID,
		}
		resp["phone"] = status.Phone
	}

	c.JSON(http.StatusOK, gin.H{"data": resp})
}
