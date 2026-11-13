package minio

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	miniogo "github.com/minio/minio-go/v7"
)

// ErrNotFound is returned when an object or version does not exist.
var ErrNotFound = errors.New("not found")

// officeTypes maps common Office and document extensions to their MIME types.
// Go's built-in mime table does not include OOXML types, so they are registered
// here to provide an appropriate content type on download.
var officeTypes = map[string]string{
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".doc":  "application/msword",
	".xls":  "application/vnd.ms-excel",
	".ppt":  "application/vnd.ms-powerpoint",
	".pdf":  "application/pdf",
	".txt":  "text/plain; charset=utf-8",
	".csv":  "text/csv; charset=utf-8",
	".json": "application/json",
	".zip":  "application/zip",
}

// contentTypeFor resolves an appropriate content type for a stored leaf name
// (the hashed leaf preserves the original extension), using the Office extension
// table first, then Go's mime table, then octet-stream.
func contentTypeFor(leaf string) string {
	ext := strings.ToLower(filepath.Ext(leaf))
	if ct, ok := officeTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// Upload stores a file as a new version of the object at the logical path's
// derived storage key and returns the new version id and last-modified
// timestamp. Each upload produces a new version; prior versions are retained by
// MinIO Object Versioning.
// (Requirement: Upload a file as a new version)
func (c *Client) Upload(ctx context.Context, uuid, path string, reader io.Reader, size int64) (string, time.Time, error) {
	if err := ValidateUUID(uuid); err != nil {
		return "", time.Time{}, err
	}
	leaf, err := StorageLeaf(path)
	if err != nil {
		return "", time.Time{}, err
	}
	key := ObjectKey(uuid, leaf)
	info, err := c.raw.PutObject(ctx, c.Bucket, key, reader, size, miniogo.PutObjectOptions{
		ContentType: contentTypeFor(leaf),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	// S3 PutObject responses do not include Last-Modified, so minio-go leaves
	// UploadInfo.LastModified as the zero value. Stat the new version to obtain
	// the authoritative timestamp the server recorded.
	lastModified := info.LastModified
	if lastModified.IsZero() {
		stat, statErr := c.raw.StatObject(ctx, c.Bucket, key, miniogo.StatObjectOptions{VersionID: info.VersionID})
		if statErr == nil {
			lastModified = stat.LastModified
		}
	}
	return info.VersionID, lastModified, nil
}

// ListVersions returns the version history of the object at a logical path's
// derived storage key, ordered newest-first (as returned by S3
// ListObjectVersions). Each entry includes the version id, last-modified
// timestamp, size, and whether it is the latest version. It returns ErrNotFound
// when the file has never been uploaded.
// (Requirement: List version history of a file)
func (c *Client) ListVersions(ctx context.Context, uuid, path string) ([]Version, error) {
	if err := ValidateUUID(uuid); err != nil {
		return nil, err
	}
	leaf, err := StorageLeaf(path)
	if err != nil {
		return nil, err
	}
	var versions []Version
	found := false
	for obj := range c.raw.ListObjects(ctx, c.Bucket, miniogo.ListObjectsOptions{
		Prefix:       ObjectKey(uuid, leaf),
		WithVersions: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		found = true
		versions = append(versions, Version{
			VersionID:      obj.VersionID,
			LastModified:   obj.LastModified,
			Size:           obj.Size,
			IsLatest:       obj.IsLatest,
			IsDeleteMarker: obj.IsDeleteMarker,
		})
	}
	if !found {
		return nil, ErrNotFound
	}
	// S3 ListObjectVersions returns versions newest-first with exactly one
	// marked IsLatest; trust that server-provided ordering and flag.
	return versions, nil
}

// DownloadLatest returns the content of the current latest version of the
// object at a logical path's derived storage key.
// (Requirement: Download the latest version)
func (c *Client) DownloadLatest(ctx context.Context, uuid, path string) (*Download, error) {
	return c.download(ctx, uuid, path, "")
}

// DownloadVersion returns the content of a specific version of the object at a
// logical path's derived storage key, identified by its version id.
// (Requirement: Download a specific historical version)
func (c *Client) DownloadVersion(ctx context.Context, uuid, path, versionID string) (*Download, error) {
	if versionID == "" {
		return nil, ErrEmptyVersionID
	}
	return c.download(ctx, uuid, path, versionID)
}

func (c *Client) download(ctx context.Context, uuid, path, versionID string) (*Download, error) {
	if err := ValidateUUID(uuid); err != nil {
		return nil, err
	}
	leaf, err := StorageLeaf(path)
	if err != nil {
		return nil, err
	}
	obj, err := c.raw.GetObject(ctx, c.Bucket, ObjectKey(uuid, leaf), miniogo.GetObjectOptions{VersionID: versionID})
	if err != nil {
		if isNotFoundErr(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	stat, err := obj.Stat()
	if err != nil {
		obj.Close()
		if isNotFoundErr(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	contentType := stat.ContentType
	if contentType == "" {
		contentType = contentTypeFor(leaf)
	}
	return &Download{
		Reader:       obj,
		VersionID:    stat.VersionID,
		LastModified: stat.LastModified,
		Size:         stat.Size,
		ContentType:  contentType,
	}, nil
}

// ListDirectories returns the UUID directories (top-level prefixes) that contain
// at least one file.
// (Requirement: List UUID directories)
func (c *Client) ListDirectories(ctx context.Context) ([]string, error) {
	var dirs []string
	for obj := range c.raw.ListObjects(ctx, c.Bucket, miniogo.ListObjectsOptions{
		Recursive: false, // "/" delimiter collapses each {uuid}/ to a common prefix
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if !isPrefixEntry(obj) {
			continue
		}
		dir := strings.TrimSuffix(obj.Key, "/")
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// Delete removes the object at a logical path's derived storage key together
// with every historical version of that object. It is idempotent: deleting a
// non-existent object succeeds.
// (Requirement: Delete a file and all its versions)
func (c *Client) Delete(ctx context.Context, uuid, path string) error {
	if err := ValidateUUID(uuid); err != nil {
		return err
	}
	leaf, err := StorageLeaf(path)
	if err != nil {
		return err
	}
	// ForceDelete is a MinIO extension that removes the object and ALL of its
	// versions (and delete markers) in a single request.
	return c.raw.RemoveObject(ctx, c.Bucket, ObjectKey(uuid, leaf), miniogo.RemoveObjectOptions{ForceDelete: true})
}

// isPrefixEntry reports whether an ObjectInfo emitted by ListObjects represents
// a common prefix (directory) rather than an actual object. Common-prefix
// entries are emitted by minio-go as ObjectInfo{Key: "prefix/"} with zero
// metadata (no ETag, no LastModified).
func isPrefixEntry(obj miniogo.ObjectInfo) bool {
	if obj.LastModified.IsZero() && obj.ETag == "" {
		return strings.HasSuffix(obj.Key, "/")
	}
	return false
}

// isNotFoundErr reports whether err is an S3/MinIO not-found error
// (NoSuchKey / NoSuchVersion / 404).
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var resp miniogo.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.Code {
		case "NoSuchKey", "NoSuchVersion", "NotFound":
			return true
		}
		if resp.StatusCode == http.StatusNotFound {
			return true
		}
	}
	return false
}
