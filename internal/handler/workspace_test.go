package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/store/fs"
)

const testMaxUploadBytes = 1 << 20 // 1 MiB cap for tests

// wsTestEnv sets up a temp data dir, fs.Store, and WorkspaceHandler wired to
// a gin engine with auth stubs that inject user-1.
type wsTestEnv struct {
	handler *WorkspaceHandler
	engine  *gin.Engine
	dataDir string
	fsSvc   *fs.Store
}

func newWSTestEnv(t *testing.T) *wsTestEnv {
	t.Helper()
	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)
	// Create user workspace so List/Download can succeed.
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "user-1", "workspace"), 0o755))

	h := NewWorkspaceHandler(fsSvc, testMaxUploadBytes)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-ws")
		c.Next()
	})
	r.GET("/api/v1/workspace/files", h.List)
	r.POST("/api/v1/workspace/upload", h.Upload)
	r.GET("/api/v1/workspace/files/*path", func(c *gin.Context) {
		raw := c.Param("path")
		if len(raw) > 8 && raw[len(raw)-8:] == "/content" {
			trimmed := raw[:len(raw)-8]
			c.Params = []gin.Param{{Key: "path", Value: trimmed}}
			h.Content(c)
			return
		}
		h.Download(c)
	})
	return &wsTestEnv{handler: h, engine: r, dataDir: dataDir, fsSvc: fsSvc}
}

func (e *wsTestEnv) wsRoot() string {
	return filepath.Join(e.dataDir, "user-1", "workspace")
}

// TestList_Root verifies listing the workspace root returns all entries with
// correct shape (dirs first, then files, sorted by name).
func TestList_Root(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "b.txt"), []byte("world"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Files []struct {
			Name       string `json:"name"`
			Type       string `json:"type"`
			Size       int64  `json:"size"`
			UpdateTime string `json:"update_time"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 3)
	// dirs first, then files.
	assert.Equal(t, "src", resp.Files[0].Name)
	assert.Equal(t, "dir", resp.Files[0].Type)
	assert.Equal(t, "a.txt", resp.Files[1].Name)
	assert.Equal(t, "file", resp.Files[1].Type)
	assert.Equal(t, int64(5), resp.Files[1].Size)
	assert.Equal(t, "b.txt", resp.Files[2].Name)
}

// TestList_Subdirectory verifies the ?path= query parameter scopes the listing
// to a subdirectory.
func TestList_Subdirectory(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("package main"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files?path=src", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 1)
	assert.Equal(t, "main.go", resp.Files[0].Name)
}

// TestList_PathOutsideWorkspace_403 verifies that a path traversal attempt is
// rejected with 403.
func TestList_PathOutsideWorkspace_403(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files?path=../../../etc", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	var env2 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env2))
	assert.Equal(t, "FORBIDDEN", env2.Error.Code)
}

// TestUpload_Success writes a small file to the workspace root and verifies
// the response contains the path and size.
func TestUpload_Success(t *testing.T) {
	env := newWSTestEnv(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "test.txt")
	require.NoError(t, err)
	_, err = io.Copy(part, bytes.NewReader([]byte("upload content")))
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("path", ""))
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "test.txt", resp.Path)
	assert.Equal(t, int64(14), resp.Size)

	// File actually exists on disk.
	data, err := os.ReadFile(filepath.Join(env.wsRoot(), "test.txt"))
	require.NoError(t, err)
	assert.Equal(t, "upload content", string(data))
}

// TestUpload_PathOutsideWorkspace_403 verifies a path traversal in the upload
// destination is rejected.
func TestUpload_PathOutsideWorkspace_403(t *testing.T) {
	env := newWSTestEnv(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "evil.txt")
	require.NoError(t, err)
	_, err = io.Copy(part, bytes.NewReader([]byte("x")))
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("path", "../../etc"))
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// TestUpload_FileTooLarge_413 creates a body slightly larger than the limit
// and verifies 413 is returned.
func TestUpload_FileTooLarge_413(t *testing.T) {
	env := newWSTestEnv(t)

	// Write a 1-byte-over-limit payload into the multipart field.
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "big.bin")
	require.NoError(t, err)
	_, err = io.Copy(part, bytes.NewReader(make([]byte, testMaxUploadBytes+1)))
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("path", ""))
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
	var env2 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env2))
	assert.Equal(t, "FILE_TOO_LARGE", env2.Error.Code)
}

// TestDownload_ExistingFile serves an existing file and verifies the status
// and body content match.
func TestDownload_ExistingFile(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("download me"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/hello.txt", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "download me", w.Body.String())
}

// TestDownload_NonExistent_404 verifies a missing file returns 404.
func TestDownload_NonExistent_404(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/nope.txt", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	var env2 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env2))
	assert.Equal(t, "NOT_FOUND", env2.Error.Code)
}

// TestContent_TextFile returns text content as JSON.
func TestContent_TextFile(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "readme.md"), []byte("# Title"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/readme.md/content", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Size    int    `json:"size"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "readme.md", resp.Path)
	assert.Equal(t, "# Title", resp.Content)
	assert.Equal(t, 7, resp.Size)
}

// TestContent_BinaryFile_400 verifies a binary file is rejected with the
// BINARY_FILE error code and the client is told to use the download endpoint.
func TestContent_BinaryFile_400(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	// Write a file with a NUL byte in the first 1024 bytes.
	binaryData := append([]byte{0x89, 0x50, 0x4E, 0x47}, make([]byte, 100)...)
	binaryData[10] = 0x00 // NUL byte triggers binary detection
	require.NoError(t, os.WriteFile(filepath.Join(ws, "image.png"), binaryData, 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/image.png/content", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var env2 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env2))
	assert.Equal(t, "BINARY_FILE", env2.Error.Code)
	assert.Contains(t, env2.Error.Message, "download")
}

// TestDownload_PathOutsideWorkspace_403 verifies the download path is
// validated for traversal.
func TestDownload_PathOutsideWorkspace_403(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/../../etc/passwd", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// TestContent_PathOutsideWorkspace_403 verifies the content path is validated.
func TestContent_PathOutsideWorkspace_403(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/../../etc/passwd/content", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// TestList_EmptyWorkspace verifies an empty workspace returns 200 with an
// empty array.
func TestList_EmptyWorkspace(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Files []any `json:"files"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Files)
}

// TestUpload_Subdirectory uploads a file into a nested subdirectory and
// verifies auto-creation.
func TestUpload_Subdirectory(t *testing.T) {
	env := newWSTestEnv(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "nested.txt")
	require.NoError(t, err)
	_, err = io.Copy(part, bytes.NewReader([]byte("nested content")))
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("path", "sub/dir"))
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	data, err := os.ReadFile(filepath.Join(env.wsRoot(), "sub", "dir", "nested.txt"))
	require.NoError(t, err)
	assert.Equal(t, "nested content", string(data))
}
