package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CORS adds a permissive Cross-Origin policy so browser front-ends (e.g.
// blowball-frontend) can call the API directly from a different origin.
//
// office-version-service is unauthenticated by design (see README "Future
// work"), so it does NOT send credentials and therefore may safely allow any
// origin: when the request carries an Origin header we reflect it (with a
// `Vary: Origin` so caches key by origin), otherwise we fall back to `*`.
//
// `OPTIONS` preflight requests are short-circuited with 204 No Content.
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
		} else {
			c.Header("Access-Control-Allow-Origin", "*")
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, X-Requested-With")
		c.Header("Access-Control-Max-Age", "600")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
