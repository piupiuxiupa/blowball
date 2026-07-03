package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/pkg/jwt"
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
	r.DELETE("/api/v1/workspace/files/*path", h.Delete)
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
// tokenDownloadTestEnv is a test harness for the query-token download endpoint.
// It wires WorkspaceHandler.TokenDownload behind the real
// QueryTokenAuthMiddleware so both auth and file-serving logic are exercised.
type tokenDownloadTestEnv struct {
	handler *WorkspaceHandler
	engine  *gin.Engine
	dataDir string
	secret  string
}

func newTokenDownloadTestEnv(t *testing.T) *tokenDownloadTestEnv {
	t.Helper()
	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "user-td", "workspace"), 0o755))

	h := NewWorkspaceHandler(fsSvc, testMaxUploadBytes)
	secret := "ws-token-secret"

	r := gin.New()
	r.Use(middleware.TraceMiddleware())
	r.GET("/api/v1/workspace/files/download", middleware.QueryTokenAuthMiddleware(secret), h.TokenDownload)
	return &tokenDownloadTestEnv{handler: h, engine: r, dataDir: dataDir, secret: secret}
}

func (e *tokenDownloadTestEnv) wsRoot() string {
	return filepath.Join(e.dataDir, "user-td", "workspace")
}

func (e *tokenDownloadTestEnv) sign(t *testing.T, userID string, expire time.Duration) string {
	t.Helper()
	tok, err := jwt.Sign(e.secret, userID, expire)
	require.NoError(t, err)
	return tok
}

func (e *tokenDownloadTestEnv) downloadURL(path, token string, inline bool) string {
	q := url.Values{}
	q.Set("token", token)
	q.Set("path", path)
	if inline {
		q.Set("inline", "1")
	}
	return "/api/v1/workspace/files/download?" + q.Encode()
}

func TestTokenDownload_ExistingFile_Attachment(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "report.md"), []byte("# report"), 0o644))

	token := env.sign(t, "user-td", time.Hour)
	req := httptest.NewRequest(http.MethodGet, env.downloadURL("report.md", token, false), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "# report", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, w.Header().Get("Content-Disposition"), `filename="report.md"`)
	assert.Contains(t, w.Header().Get("Content-Disposition"), "filename*=utf-8''report.md")
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
}

func TestTokenDownload_Inline(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "pic.png"), []byte("png"), 0o644))

	token := env.sign(t, "user-td", time.Hour)
	req := httptest.NewRequest(http.MethodGet, env.downloadURL("pic.png", token, true), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Disposition"), "inline")
	assert.Contains(t, w.Header().Get("Content-Disposition"), `filename="pic.png"`)
}

func TestTokenDownload_ChineseFilename(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	ws := env.wsRoot()
	name := "中文报告.md"
	require.NoError(t, os.WriteFile(filepath.Join(ws, name), []byte("content"), 0o644))

	token := env.sign(t, "user-td", time.Hour)
	req := httptest.NewRequest(http.MethodGet, env.downloadURL(name, token, false), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	disp := w.Header().Get("Content-Disposition")
	assert.Contains(t, disp, "filename*=utf-8''%E4%B8%AD%E6%96%87%E6%8A%A5%E5%91%8A.md")
}

func TestTokenDownload_MissingPath(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	token := env.sign(t, "user-td", time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/download?token="+url.QueryEscape(token), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "BAD_REQUEST", resp.Error.Code)
}

func TestTokenDownload_MissingToken(t *testing.T) {
	env := newTokenDownloadTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/download?path=report.md", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "missing token", resp.Error.Message)
}

func TestTokenDownload_InvalidToken(t *testing.T) {
	env := newTokenDownloadTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/download?token=bad-token&path=report.md", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "invalid token", resp.Error.Message)
}

func TestTokenDownload_ExpiredToken(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	token := env.sign(t, "user-td", -time.Minute)

	req := httptest.NewRequest(http.MethodGet, env.downloadURL("report.md", token, false), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "token expired", resp.Error.Message)
}

func TestTokenDownload_PathOutsideWorkspace_403(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	token := env.sign(t, "user-td", time.Hour)

	req := httptest.NewRequest(http.MethodGet, env.downloadURL("../../../etc/passwd", token, false), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "FORBIDDEN", resp.Error.Code)
}

func TestTokenDownload_Directory_400(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "folder"), 0o755))

	token := env.sign(t, "user-td", time.Hour)
	req := httptest.NewRequest(http.MethodGet, env.downloadURL("folder", token, false), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "BAD_REQUEST", resp.Error.Code)
}

func TestTokenDownload_NotFound_404(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	token := env.sign(t, "user-td", time.Hour)

	req := httptest.NewRequest(http.MethodGet, env.downloadURL("missing.txt", token, false), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "NOT_FOUND", resp.Error.Code)
}

// TestTokenDownload_ProductionRouting verifies that the production route
// registration (catch-all /*path with workspaceFileAuthMW) correctly dispatches
// /workspace/files/download to TokenDownload using the query token. This is a
// regression guard for the gin static/wildcard sibling limitation.
func TestTokenDownload_ProductionRouting(t *testing.T) {
	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "user-prod", "workspace"), 0o755))

	secret := "prod-routing-secret"
	h := NewWorkspaceHandler(fsSvc, testMaxUploadBytes)
	ws := filepath.Join(dataDir, "user-prod", "workspace")
	require.NoError(t, os.WriteFile(filepath.Join(ws, "multiply_1_to_100.py"), []byte("print(1)"), 0o644))

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.TraceIDKey, "trace-prod")
		c.Next()
	})
	RegisterRoutes(r, RouteDeps{
		AuthMW:                 func(c *gin.Context) { c.AbortWithStatusJSON(401, gin.H{"error": gin.H{"code": "UNAUTHORIZED", "message": "header auth required"}}) },
		QueryTokenAuthMW:       middleware.QueryTokenAuthMiddleware(secret),
		WorkspaceTokenDownload: h.TokenDownload,
		Login:                  func(*gin.Context) {},
		SessionList:            func(*gin.Context) {},
		SessionCreate:          func(*gin.Context) {},
		SessionMessages:        func(*gin.Context) {},
		SendMessage:            func(*gin.Context) {},
		SessionDelete:          func(*gin.Context) {},
		WorkspaceList:          h.List,
		WorkspaceUpload:        h.Upload,
		WorkspaceDownload:      h.Download,
		WorkspaceContent:       h.Content,
		WorkspaceDelete:        h.Delete,
		MCPTools:               func(*gin.Context) {},
		SkillsList:             func(*gin.Context) {},
	})

	token, err := jwt.Sign(secret, "user-prod", time.Hour)
	require.NoError(t, err)

	q := url.Values{}
	q.Set("token", token)
	q.Set("path", "multiply_1_to_100.py")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/download?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "print(1)", w.Body.String())
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), "filename*=utf-8''multiply_1_to_100.py")
}

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

// TestDelete_File_204 deletes a single file and verifies it is gone.
func TestDelete_File_204(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	target := filepath.Join(ws, "note.txt")
	require.NoError(t, os.WriteFile(target, []byte("bye"), 0o644))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspace/files/note.txt", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist, "file must be removed from disk")
}

// TestDelete_Directory_Recursive deletes a directory and verifies its contents
// are removed with it.
func TestDelete_Directory_Recursive(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	dir := filepath.Join(ws, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hi"), 0o644))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspace/files/project", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist, "directory and its contents must be removed")
}

// TestDelete_NestedPath_EncodedSlashes deletes a deeply nested file addressed
// by a catch-all path with URL-encoded slashes — the exact wire form the React
// frontend emits (encodeURIComponent("sub/deep/note.md") -> "sub%2Fdeep%2Fnote.md").
// It asserts the encoded slashes round-trip through gin's catch-all to the
// nested file, AND that a distinct same-named file at the workspace root is
// left untouched (no wrong-file deletion).
func TestDelete_NestedPath_EncodedSlashes(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()

	// A nested file plus a decoy at the root sharing the basename.
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "sub", "deep"), 0o755))
	nested := filepath.Join(ws, "sub", "deep", "note.md")
	require.NoError(t, os.WriteFile(nested, []byte("nested"), 0o644))
	rootDecoy := filepath.Join(ws, "note.md")
	require.NoError(t, os.WriteFile(rootDecoy, []byte("root"), 0o644))

	// Frontend encoding: encodeURIComponent on the full relative path. Go's
	// url.PathEscape escapes "/" the same way (to %2F), reproducing the wire form.
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/workspace/files/"+url.PathEscape("sub/deep/note.md"), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	_, err := os.Stat(nested)
	require.ErrorIs(t, err, os.ErrNotExist, "nested file must be removed")
	_, err = os.Stat(rootDecoy)
	require.NoError(t, err, "root decoy must NOT be deleted (wrong-file regression)")
}

// TestDelete_NestedPath_LiteralSlashes is the same scenario but with literal
// (unencoded) slashes in the catch-all, which the router must also accept.
func TestDelete_NestedPath_LiteralSlashes(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "a", "b"), 0o755))
	nested := filepath.Join(ws, "a", "b", "c.txt")
	require.NoError(t, os.WriteFile(nested, []byte("c"), 0o644))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspace/files/a/b/c.txt", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	_, err := os.Stat(nested)
	require.ErrorIs(t, err, os.ErrNotExist, "nested file must be removed")
}

// TestDelete_PathOutsideWorkspace_403 verifies a traversal attempt is rejected.
func TestDelete_PathOutsideWorkspace_403(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspace/files/../../etc/passwd", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "FORBIDDEN", resp.Error.Code)
}

// TestDelete_NotFound_404 verifies deleting a missing target returns 404.
func TestDelete_NotFound_404(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspace/files/nope.txt", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "NOT_FOUND", resp.Error.Code)
}

// TestDelete_WorkspaceRoot_400 verifies a path that resolves to the workspace
// root (".", "./", "foo/..") is refused — never wiping the whole workspace —
// and that the root survives.
func TestDelete_WorkspaceRoot_400(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("survive"), 0o644))

	for _, p := range []string{".", "./", "foo/.."} {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspace/files/"+p, nil)
		w := httptest.NewRecorder()
		env.engine.ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code, "path %q: body: %s", p, w.Body.String())
		var resp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "BAD_REQUEST", resp.Error.Code, "path %q", p)
	}

	// The workspace root and its contents are intact.
	_, err := os.Stat(ws)
	require.NoError(t, err, "workspace root must not be deleted")
	data, err := os.ReadFile(filepath.Join(ws, "keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "survive", string(data))
}
