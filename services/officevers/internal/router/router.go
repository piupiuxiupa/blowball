// Package router wires the HTTP routes for the office-version-service.
package router

import (
	"github.com/lush/blowball/services/officevers/internal/handler"

	"github.com/gin-gonic/gin"
)

// New builds the Gin engine with all routes wired up. The supplied store must
// implement handler.Storage (the concrete *minio.Client does).
func New(store handler.Storage) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	// Permissive CORS so browser front-ends can call the API directly. The
	// service is unauthenticated, so this is safe (no credentials are sent).
	r.Use(CORS())

	h := handler.New(store)

	docs := r.Group("/documents")
	{
		// GET /documents              -> list UUID directories
		docs.GET("", h.ListDirectories)
		// File-level operations. The *filepath catch-all is the file's logical
		// path (it may contain "/"); the operation is selected by HTTP method and
		// the ?action= query parameter inside the handler:
		//   POST   /documents/:uuid/*filepath                  -> upload a new version
		//   POST   /documents/:uuid/*filepath?action=rollback  -> rollback to a version
		//   GET    /documents/:uuid/*filepath                  -> download latest
		//   GET    /documents/:uuid/*filepath?action=versions  -> list versions
		//   GET    /documents/:uuid/*filepath?action=version&versionId=<vid> -> download a version
		//   DELETE /documents/:uuid/*filepath                  -> delete file + versions
		docs.GET("/:uuid/*filepath", h.Document)
		docs.POST("/:uuid/*filepath", h.Document)
		docs.DELETE("/:uuid/*filepath", h.Document)
	}

	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })

	return r
}
