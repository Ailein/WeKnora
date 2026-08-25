package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/integrationassets"
)

// serveIntegrationAssets exposes integrations/<name>/assets/<file> as public
// static files. Registered BEFORE the auth middleware: browsers load these
// via bare <img> tags (chat markdown), which cannot attach credentials.
// Path safety and the extension whitelist live in integrationassets.Resolve.
func serveIntegrationAssets(r *gin.Engine) {
	r.GET("/api/v1/integration-assets/:integration/:file", func(c *gin.Context) {
		fullPath, contentType, err := integrationassets.Resolve(
			c.Param("integration"), c.Param("file"))
		if err != nil {
			c.String(http.StatusNotFound, "not found")
			return
		}
		c.Header("Content-Type", contentType)
		c.Header("Cache-Control", "public, max-age=3600")
		c.File(fullPath)
	})
}
