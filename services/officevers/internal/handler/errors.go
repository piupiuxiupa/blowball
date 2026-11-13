package handler

import (
	"errors"
	"net/http"

	miniostore "github.com/lush/blowball/services/officevers/internal/minio"

	"github.com/gin-gonic/gin"
)

// writeError maps a domain error to an HTTP status code and writes a JSON error
// body. The mapping is:
//   - not found                  -> 404
//   - validation error (bad input) -> 400
//   - everything else            -> 500
func writeError(c *gin.Context, err error) {
	if err == nil {
		return
	}

	switch {
	case errors.Is(err, miniostore.ErrNotFound):
		c.JSON(http.StatusNotFound, apiError{Error: apiErrorBody{Message: "not found"}})
	case miniostore.IsValidationError(err):
		c.JSON(http.StatusBadRequest, apiError{Error: apiErrorBody{Message: err.Error()}})
	case isMinioNotFound(err):
		c.JSON(http.StatusNotFound, apiError{Error: apiErrorBody{Message: "not found"}})
	default:
		c.JSON(http.StatusInternalServerError, apiError{Error: apiErrorBody{Message: err.Error()}})
	}
}

// isMinioNotFound reports whether err is a MinIO/S3 not-found error that slipped
// up from the storage layer without being wrapped in ErrNotFound.
func isMinioNotFound(err error) bool {
	return errors.Is(err, miniostore.ErrNotFound)
}
