// Package minio wraps the MinIO client and exposes the storage operations the
// service needs: versioned upload/download/list/delete and non-destructive
// rollback. MinIO's native S3 Object Versioning is the sole mechanism for
// retaining history; this package keeps no state of its own.
package minio

import (
	"io"
	"time"
)

// Version describes a single historical version of an object.
type Version struct {
	VersionID      string    `json:"versionId"`
	LastModified   time.Time `json:"lastModified"`
	Size           int64     `json:"size"`
	IsLatest       bool      `json:"isLatest"`
	IsDeleteMarker bool      `json:"isDeleteMarker,omitempty"`
}

// Download is the result of a download operation: the object stream together
// with metadata about the version being streamed. The caller must close Reader.
type Download struct {
	Reader       io.ReadCloser
	VersionID    string
	LastModified time.Time
	Size         int64
	ContentType  string
}
