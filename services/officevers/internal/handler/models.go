// Package handler implements the Gin HTTP handlers that expose the storage
// operations as a REST API under /documents.
package handler

import "time"

// uploadResponse is returned by the upload endpoint.
type uploadResponse struct {
	VersionID    string    `json:"versionId"`
	LastModified time.Time `json:"lastModified"`
}

// versionEntry is one element of a file's version history.
type versionEntry struct {
	VersionID      string    `json:"versionId"`
	LastModified   time.Time `json:"lastModified"`
	Size           int64     `json:"size"`
	IsLatest       bool      `json:"isLatest"`
	IsDeleteMarker bool      `json:"isDeleteMarker,omitempty"`
}

// versionsResponse wraps the version history of a file.
type versionsResponse struct {
	UUID     string         `json:"uuid"`
	Path     string         `json:"path"`
	Versions []versionEntry `json:"versions"`
}

// directoriesResponse wraps the top-level UUID directory listing.
type directoriesResponse struct {
	Directories []string `json:"directories"`
}

// rollbackResponse is returned after a successful rollback.
type rollbackResponse struct {
	VersionID string `json:"versionId"`
}

// rollbackRequest is the expected JSON body for a rollback request.
type rollbackRequest struct {
	VersionID string `json:"versionId"`
}

// apiError is the JSON body returned for all error responses.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Message string `json:"message"`
}
