package handler

import (
	"net/http"
	"strconv"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// GetIMAnalytics reports IM traffic analytics for the current tenant.
// GET /api/v1/im-analytics
//
// GetIMAnalytics godoc
// @Summary      查询 IM 分析看板数据
// @Description  按自然日聚合当前租户 IM 渠道的会话、消息、接管与响应时长指标；仅统计 IM 渠道消息，不含控制台/网页流量
// @Tags         IM 渠道
// @Produce      json
// @Param        days               query     int     false  "统计天数（含今天，1-90，默认 7）"
// @Param        tz_offset_minutes  query     int     false  "查看者相对 UTC 的偏移分钟数（东为正，默认 0）"
// @Param        im_channel_id      query     string  false  "仅统计指定 IM 渠道"
// @Success      200                {object}  map[string]interface{}  "看板数据（totals/daily/channels/top_users）"
// @Failure      401                {object}  map[string]interface{}  "未认证"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /im-analytics [get]
func (h *IMHandler) GetIMAnalytics(c *gin.Context) {
	tenantID, ok := c.Request.Context().Value(types.TenantIDContextKey).(uint64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	q := im.AnalyticsQuery{IMChannelID: c.Query("im_channel_id")}
	if v, err := strconv.Atoi(c.DefaultQuery("days", "0")); err == nil {
		q.Days = v
	}
	if v, err := strconv.Atoi(c.DefaultQuery("tz_offset_minutes", "0")); err == nil {
		q.TZOffsetMinutes = v
	}

	result, err := h.imService.ChannelAnalytics(c.Request.Context(), tenantID, q)
	if err != nil {
		logger.Errorf(c.Request.Context(), "IM analytics failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build IM analytics"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": result})
}
