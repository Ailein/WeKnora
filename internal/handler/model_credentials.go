package handler

import (
	"net/http"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/handler/dto"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

// ModelCredentialsHandler handles secret credentials for models via the
// dedicated /models/:id/credentials subresource. See mcp_credentials.go for
// the rationale; this handler mirrors that contract for Model resources.
//
// Recognized fields: "api_key" (every provider), "app_secret" (WeKnora Cloud).
type ModelCredentialsHandler struct {
	svc interfaces.ModelService
}

func NewModelCredentialsHandler(svc interfaces.ModelService) *ModelCredentialsHandler {
	return &ModelCredentialsHandler{svc: svc}
}

type modelCredentialsPutRequest struct {
	APIKey    *string `json:"api_key,omitempty"`
	AppSecret *string `json:"app_secret,omitempty"`
	// RefreshToken 是 OAuth 型厂商（OpenAI Codex / ChatGPT 订阅）的刷新令牌；
	// 与 api_key（此时承载 access token）配对更新。
	RefreshToken *string `json:"refresh_token,omitempty"`
}

func (h *ModelCredentialsHandler) Put(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	tenantID := c.GetUint64(types.TenantIDContextKey.String())
	if tenantID == 0 {
		c.Error(errors.NewBadRequestError("Workspace ID cannot be empty"))
		return
	}

	var req modelCredentialsPutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}
	if req.APIKey == nil && req.AppSecret == nil && req.RefreshToken == nil {
		m, err := h.svc.GetModelByID(ctx, id)
		if err != nil || m == nil {
			c.Error(errors.NewNotFoundError("Model not found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "data": dto.CredentialsResponse{
			Fields: map[string]dto.CredentialFieldMetadata{
				"api_key":       {Configured: m.Parameters.APIKey != ""},
				"app_secret":    {Configured: m.Parameters.AppSecret != ""},
				"refresh_token": {Configured: m.Parameters.RefreshToken != ""},
			},
		}})
		return
	}

	updated, err := h.svc.UpdateModelCredentials(ctx, id, req.APIKey, req.AppSecret, req.RefreshToken)
	if err != nil {
		if err == service.ErrModelNotFound {
			c.Error(errors.NewNotFoundError("Model not found"))
			return
		}
		if appErr, ok := errors.IsAppError(err); ok {
			c.Error(appErr)
			return
		}
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"model_id": secutils.SanitizeForLog(id)})
		c.Error(errors.NewInternalServerError("failed to update credentials: " + err.Error()))
		return
	}

	resp := dto.CredentialsResponse{
		Fields: map[string]dto.CredentialFieldMetadata{
			"api_key":       {Configured: updated.Parameters.APIKey != ""},
			"app_secret":    {Configured: updated.Parameters.AppSecret != ""},
			"refresh_token": {Configured: updated.Parameters.RefreshToken != ""},
		},
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

func (h *ModelCredentialsHandler) DeleteField(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	field := c.Param("field")
	tenantID := c.GetUint64(types.TenantIDContextKey.String())
	if tenantID == 0 {
		c.Error(errors.NewBadRequestError("Workspace ID cannot be empty"))
		return
	}
	if field != "api_key" && field != "app_secret" && field != "refresh_token" {
		c.Error(errors.NewBadRequestError("unknown credential field: " + secutils.SanitizeForLog(field)))
		return
	}
	if err := h.svc.ClearModelCredential(ctx, id, field); err != nil {
		if err == service.ErrModelNotFound {
			c.Error(errors.NewNotFoundError("Model not found"))
			return
		}
		if appErr, ok := errors.IsAppError(err); ok {
			c.Error(appErr)
			return
		}
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"model_id": secutils.SanitizeForLog(id),
			"field":    field,
		})
		c.Error(errors.NewInternalServerError("failed to clear credential: " + err.Error()))
		return
	}
	c.Status(http.StatusNoContent)
}
