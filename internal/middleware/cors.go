package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORSConfig describes the policy CORS applies on every response.
type CORSConfig struct {
	// AllowOrigins is the explicit list of origins permitted to make
	// credentialed cross-site requests. A single "*" entry short-circuits to
	// the permissive wildcard (no credentials carried that way, but the spec
	// for this service uses Authorization via Bearer rather than cookies, so
	// the wildcard is acceptable for local dev).
	AllowOrigins []string
	// AllowMethods lists the HTTP verbs advertised on preflight responses.
	AllowMethods []string
	// AllowHeaders lists the request headers advertised on preflight
	// responses.
	AllowHeaders []string
	// AllowCredentials mirrors Access-Control-Allow-Credentials.
	AllowCredentials bool
	// MaxAge is the seconds clients may cache a preflight result.
	MaxAge int
}

// DefaultCORSConfig returns a permissive policy suitable for local
// development: any origin, standard verbs, the headers this service's clients
// send (Content-Type, Authorization), credentials allowed, and a 24h preflight
// cache.
func DefaultCORSConfig() CORSConfig {
	return CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowHeaders:     []string{"Content-Type", "Authorization"},
		AllowCredentials: true,
		MaxAge:           86400,
	}
}

// CORS returns a gin middleware built from DefaultCORSConfig.
func CORS() gin.HandlerFunc {
	return CORSWithConfig(DefaultCORSConfig())
}

// CORSWithConfig returns a gin middleware that applies cfg. For preflight
// OPTIONS requests it short-circuits the chain with 204 after writing the
// CORS headers; for all other requests it writes the headers and continues.
//
// When AllowOrigins contains the wildcard "*", Access-Control-Allow-Origin is
// echoed as "*" and the request's own Origin is echoed otherwise (so the
// Allow-Credentials: true header remains meaningful to browsers).
func CORSWithConfig(cfg CORSConfig) gin.HandlerFunc {
	methods := strings.Join(cfg.AllowMethods, ", ")
	headers := strings.Join(cfg.AllowHeaders, ", ")
	maxAge := strconv.Itoa(cfg.MaxAge)
	wildcard := contains(cfg.AllowOrigins, "*")

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		switch {
		case wildcard:
			c.Header("Access-Control-Allow-Origin", "*")
		case origin != "" && contains(cfg.AllowOrigins, origin):
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
		case origin != "":
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
		}

		if methods != "" {
			c.Header("Access-Control-Allow-Methods", methods)
		}
		if headers != "" {
			c.Header("Access-Control-Allow-Headers", headers)
		}
		if cfg.AllowCredentials {
			c.Header("Access-Control-Allow-Credentials", "true")
		}
		if cfg.MaxAge > 0 {
			c.Header("Access-Control-Max-Age", maxAge)
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// contains reports whether haystack includes needle.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
