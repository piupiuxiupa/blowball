package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	jwtpkg "github.com/golang-jwt/jwt/v5"
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

	h := NewWorkspaceHandler(fsSvc, testMaxUploadBytes, OnlyOfficeSettings{})

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
	r.PUT("/api/v1/workspace/files/*path", h.Rename)
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

// TestList_HiddenExcludedByDefault verifies hidden files and directories are
// omitted when include_hidden is absent.
func TestList_HiddenExcludedByDefault(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".hidden-dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".hidden-file"), []byte("secret"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "visible.txt"), []byte("hello"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files", nil)
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
	assert.Equal(t, "visible.txt", resp.Files[0].Name)
}

// TestList_IncludeHidden verifies hidden files and directories are returned
// when include_hidden=true.
func TestList_IncludeHidden(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".hidden-dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".hidden-file"), []byte("secret"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "visible.txt"), []byte("hello"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files?include_hidden=true", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Files []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 3)

	names := make([]string, len(resp.Files))
	for i, f := range resp.Files {
		names[i] = f.Name
	}
	assert.ElementsMatch(t, []string{".hidden-dir", ".hidden-file", "visible.txt"}, names)

	// Directories still sort before files.
	assert.Equal(t, ".hidden-dir", resp.Files[0].Name)
	assert.Equal(t, "dir", resp.Files[0].Type)
}

// TestList_IncludeHiddenOne verifies the numeric truthy form include_hidden=1
// is accepted and returns hidden entries.
func TestList_IncludeHiddenOne(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".hidden-file"), []byte("secret"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "visible.txt"), []byte("hello"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files?include_hidden=1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 2)

	names := make([]string, len(resp.Files))
	for i, f := range resp.Files {
		names[i] = f.Name
	}
	assert.ElementsMatch(t, []string{".hidden-file", "visible.txt"}, names)
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

	h := NewWorkspaceHandler(fsSvc, testMaxUploadBytes, OnlyOfficeSettings{})
	secret := "ws-token-secret"

	r := gin.New()
	r.Use(middleware.TraceMiddleware())
	// Replicate production routing: a single catch-all dispatches
	// /workspace/files/download/*path to TokenDownload.
	r.GET("/api/v1/workspace/files/*path", middleware.QueryTokenAuthMiddleware(secret), func(c *gin.Context) {
		raw := strings.TrimPrefix(c.Param("path"), "/")
		if raw != tokenDownloadPath && !strings.HasPrefix(raw, tokenDownloadPath+"/") {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "NOT_FOUND", "message": "not found"}})
			return
		}
		filePath := strings.TrimPrefix(raw, tokenDownloadPath)
		filePath = strings.TrimPrefix(filePath, "/")
		c.Params = []gin.Param{{Key: "path", Value: filePath}}
		h.TokenDownload(c)
	})
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
	if inline {
		q.Set("inline", "1")
	}
	return "/api/v1/workspace/files/download/" + url.PathEscape(path) + "?" + q.Encode()
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

// TestTokenDownload_NestedPath_EncodedSlashes verifies that a deeply nested file
// addressed by URL-encoded slashes (the wire form the React frontend emits via
// encodeURIComponent) round-trips through the catch-all to the correct file.
func TestTokenDownload_NestedPath_EncodedSlashes(t *testing.T) {
	env := newTokenDownloadTestEnv(t)
	ws := env.wsRoot()

	require.NoError(t, os.MkdirAll(filepath.Join(ws, "sub", "deep"), 0o755))
	nested := filepath.Join(ws, "sub", "deep", "note.md")
	rootDecoy := filepath.Join(ws, "note.md")
	require.NoError(t, os.WriteFile(nested, []byte("nested"), 0o644))
	require.NoError(t, os.WriteFile(rootDecoy, []byte("root"), 0o644))

	token := env.sign(t, "user-td", time.Hour)
	req := httptest.NewRequest(http.MethodGet, env.downloadURL("sub/deep/note.md", token, false), nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "nested", w.Body.String())
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

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/download/report.md", nil)
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
	h := NewWorkspaceHandler(fsSvc, testMaxUploadBytes, OnlyOfficeSettings{})
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
		SessionUpdateTitle:     func(*gin.Context) {},
		WorkspaceList:          h.List,
		WorkspaceUpload:        h.Upload,
		WorkspaceDownload:      h.Download,
		WorkspaceContent:       h.Content,
		WorkspaceDelete:        h.Delete,
		WorkspaceRename:        h.Rename,
		MCPTools:               func(*gin.Context) {},
		SkillsList:             func(*gin.Context) {},
	})

	token, err := jwt.Sign(secret, "user-prod", time.Hour)
	require.NoError(t, err)

	q := url.Values{}
	q.Set("token", token)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/download/multiply_1_to_100.py?"+q.Encode(), nil)
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

// TestRename_File_Success renames a file within the workspace root.
func TestRename_File_Success(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "old.md"), []byte("content"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/old.md",
		strings.NewReader(`{"new_path":"new.md"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp renameResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "old.md", resp.OldPath)
	assert.Equal(t, "new.md", resp.NewPath)

	_, err := os.Stat(filepath.Join(ws, "old.md"))
	require.ErrorIs(t, err, os.ErrNotExist, "source must be gone")
	data, err := os.ReadFile(filepath.Join(ws, "new.md"))
	require.NoError(t, err)
	assert.Equal(t, "content", string(data))
}

// TestRename_Directory_Success renames a directory recursively.
func TestRename_Directory_Success(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "old-dir", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "old-dir", "sub", "f.txt"), []byte("x"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/old-dir",
		strings.NewReader(`{"new_path":"new-dir"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp renameResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "old-dir", resp.OldPath)
	assert.Equal(t, "new-dir", resp.NewPath)

	_, err := os.Stat(filepath.Join(ws, "old-dir"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(filepath.Join(ws, "new-dir", "sub", "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
}

// TestRename_MoveToSubdirectory moves a file into an existing subdirectory.
func TestRename_MoveToSubdirectory(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a.md"), []byte("move me"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/a.md",
		strings.NewReader(`{"new_path":"subdir/b.md"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp renameResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "subdir/b.md", resp.NewPath)

	_, err := os.Stat(filepath.Join(ws, "a.md"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(filepath.Join(ws, "subdir", "b.md"))
	require.NoError(t, err)
	assert.Equal(t, "move me", string(data))
}

// TestRename_DestinationExists_409 verifies no overwrite when destination exists.
func TestRename_DestinationExists_409(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "src.md"), []byte("src"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "dst.md"), []byte("dst"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/src.md",
		strings.NewReader(`{"new_path":"dst.md"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct{ Code string `json:"code"` } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ALREADY_EXISTS", body.Error.Code)

	// Source and destination are untouched.
	srcData, err := os.ReadFile(filepath.Join(ws, "src.md"))
	require.NoError(t, err)
	assert.Equal(t, "src", string(srcData))
	dstData, err := os.ReadFile(filepath.Join(ws, "dst.md"))
	require.NoError(t, err)
	assert.Equal(t, "dst", string(dstData))
}

// TestRename_SourceMissing_404 verifies renaming a missing source returns 404.
func TestRename_SourceMissing_404(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/missing.md",
		strings.NewReader(`{"new_path":"x.md"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct{ Code string `json:"code"` } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "NOT_FOUND", body.Error.Code)
}

// TestRename_PathOutsideWorkspace_403 verifies traversal attempts for either
// source or destination are rejected.
func TestRename_PathOutsideWorkspace_403(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a.md"), []byte("a"), 0o644))

	cases := []struct {
		name    string
		path    string
		newPath string
	}{
		{"source outside", "../../etc/passwd", "x.md"},
		{"destination outside", "a.md", "../../etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut,
				"/api/v1/workspace/files/"+tc.path,
				strings.NewReader(`{"new_path":"`+tc.newPath+`"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)

			require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
			var body struct {
				Error struct{ Code string `json:"code"` } `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, "FORBIDDEN", body.Error.Code)
		})
	}
}

// TestRename_WorkspaceRoot_400 verifies the workspace root cannot be renamed.
func TestRename_WorkspaceRoot_400(t *testing.T) {
	env := newWSTestEnv(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/.",
		strings.NewReader(`{"new_path":"new-root"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

// TestRename_MissingNewPath_400 verifies a missing or empty new_path is rejected.
func TestRename_MissingNewPath_400(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a.md"), []byte("a"), 0o644))

	cases := []string{
		`{}`,
		`{"new_path":""}`,
		`{"new_path":"   "}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/workspace/files/a.md",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		env.engine.ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	}
}

// TestRename_NestedPath_EncodedSlashes verifies renaming a deeply nested file
// via URL-encoded slashes (the frontend wire form) hits the right file and
// leaves a root decoy untouched.
func TestRename_NestedPath_EncodedSlashes(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()

	require.NoError(t, os.MkdirAll(filepath.Join(ws, "sub", "deep"), 0o755))
	nested := filepath.Join(ws, "sub", "deep", "note.md")
	rootDecoy := filepath.Join(ws, "note.md")
	require.NoError(t, os.WriteFile(nested, []byte("nested"), 0o644))
	require.NoError(t, os.WriteFile(rootDecoy, []byte("root"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/"+url.PathEscape("sub/deep/note.md"),
		strings.NewReader(`{"new_path":"sub/deep/renamed.md"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	_, err := os.Stat(nested)
	require.ErrorIs(t, err, os.ErrNotExist, "nested source must be gone")
	_, err = os.Stat(rootDecoy)
	require.NoError(t, err, "root decoy must remain")
	data, err := os.ReadFile(filepath.Join(ws, "sub", "deep", "renamed.md"))
	require.NoError(t, err)
	assert.Equal(t, "nested", string(data))
}

// TestRename_LiteralSlashes moves a file using literal slashes in the catch-all.
func TestRename_LiteralSlashes(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a", "b", "c.txt"), []byte("c"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/a/b/c.txt",
		strings.NewReader(`{"new_path":"a/b/d.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	_, err := os.Stat(filepath.Join(ws, "a", "b", "c.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(filepath.Join(ws, "a", "b", "d.txt"))
	require.NoError(t, err)
	assert.Equal(t, "c", string(data))
}

// TestRename_CreateDestinationParent verifies a move into a not-yet-existing
// subdirectory creates the parent directories.
func TestRename_CreateDestinationParent(t *testing.T) {
	env := newWSTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "f.txt"), []byte("f"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/f.txt",
		strings.NewReader(`{"new_path":"new/sub/f.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	data, err := os.ReadFile(filepath.Join(ws, "new", "sub", "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "f", string(data))
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

// --- OnlyOffice editor-config + save-callback tests ---

// ooTestEnv wires a WorkspaceHandler with OnlyOffice enabled behind a gin engine
// whose routing mirrors production: the config endpoint hangs off the catch-all
// behind AuthMiddleware (Bearer), and the callback endpoint is a standalone POST
// behind QueryTokenAuthMiddleware. A backing httptest.Server plays the
// DocumentServer for the callback download (its URL is also ServerURL, so the
// host allowlist accepts its result URLs).
type ooTestEnv struct {
	engine    *gin.Engine
	dataDir   string
	jwtSecret string // blowball user-JWT secret (AuthMiddleware)
	ooSecret  string // OnlyOffice editor-config signing secret
	serverURL string // OnlyOffice ServerURL = backing server URL
	backend   string // InternalBackend (host the container reaches the backend at)
	dlServer  *httptest.Server
}

func newOnlyOfficeTestEnv(t *testing.T) *ooTestEnv {
	t.Helper()
	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "user-1", "workspace"), 0o755))

	// Backing "DocumentServer": serves the edited result the callback downloads.
	// Branch on path so one server covers the persist / 404 / oversize cases.
	mux := http.NewServeMux()
	mux.HandleFunc("/edited", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("EDITED-CONTENT"))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/oversize", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), testMaxUploadBytes+10))
	})
	dlServer := httptest.NewServer(mux)
	t.Cleanup(dlServer.Close)

	jwtSecret := "oo-user-jwt-secret"
	ooSecret := "oo-editor-secret"
	h := NewWorkspaceHandler(fsSvc, testMaxUploadBytes, OnlyOfficeSettings{
		Secret:          ooSecret,
		ServerURL:       dlServer.URL,
		InternalBackend: "http://oo-backend.local",
	})

	r := gin.New()
	r.Use(middleware.TraceMiddleware())
	// Config endpoint: catch-all with /onlyoffice-config suffix, Bearer auth.
	r.GET("/api/v1/workspace/files/*path", middleware.AuthMiddleware(jwtSecret), func(c *gin.Context) {
		raw := strings.TrimPrefix(c.Param("path"), "/")
		if strings.HasSuffix(raw, onlyOfficeConfigSuffix) {
			c.Params = []gin.Param{{Key: "path", Value: strings.TrimSuffix(raw, onlyOfficeConfigSuffix)}}
			h.OnlyOfficeConfig(c)
			return
		}
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "NOT_FOUND", "message": "not found"}})
	})
	// Callback endpoint: standalone POST with query-token auth.
	r.POST("/api/v1/workspace/onlyoffice-callback", middleware.QueryTokenAuthMiddleware(jwtSecret), h.OnlyOfficeCallback)

	return &ooTestEnv{
		engine: r, dataDir: dataDir, jwtSecret: jwtSecret, ooSecret: ooSecret,
		serverURL: dlServer.URL, backend: "http://oo-backend.local", dlServer: dlServer,
	}
}

func (e *ooTestEnv) wsRoot() string { return filepath.Join(e.dataDir, "user-1", "workspace") }

// userJWT signs a blowball user JWT for user-1 (the configured workspace owner).
func (e *ooTestEnv) userJWT(t *testing.T) string {
	t.Helper()
	tok, err := jwt.Sign(e.jwtSecret, "user-1", time.Hour)
	require.NoError(t, err)
	return tok
}

// postCallback issues a callback POST with the given JSON body and file path.
func (e *ooTestEnv) postCallback(t *testing.T, rel, body string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{}
	q.Set("path", rel)
	q.Set("token", e.userJWT(t))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/onlyoffice-callback?"+q.Encode(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

// onlyOfficeConfigBody decodes the editor-config response into a typed struct.
type onlyOfficeConfigBody struct {
	ServerURL string                 `json:"server_url"`
	Config    map[string]any         `json:"config"`
	Token     string                 `json:"token"`
}

// TestOnlyOfficeConfig_SignedConfig verifies the happy path: the endpoint
// returns an edit-mode config with a random key, document.url and callbackUrl
// rooted at InternalBackend and carrying the user JWT, and a token that the same
// secret verifies and whose payload equals the returned config.
func TestOnlyOfficeConfig_SignedConfig(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	ws := env.wsRoot()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "report.docx"), []byte("orig"), 0o644))

	// doConfigGET uses userJWT(nil); sign directly here instead.
	target := "/api/v1/workspace/files/" + url.PathEscape("report.docx") + onlyOfficeConfigSuffix
	userTok := env.userJWT(t)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+userTok)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp onlyOfficeConfigBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, env.serverURL, resp.ServerURL)

	doc := resp.Config["document"].(map[string]any)
	editor := resp.Config["editorConfig"].(map[string]any)
	assert.Equal(t, "word", resp.Config["documentType"])
	assert.Equal(t, "docx", doc["fileType"])
	assert.Equal(t, "report.docx", doc["title"])
	assert.NotEmpty(t, doc["key"], "key must be randomly generated")
	perms := doc["permissions"].(map[string]any)
	assert.Equal(t, true, perms["edit"])
	assert.Equal(t, true, perms["download"])
	assert.Equal(t, "edit", editor["mode"])
	custom := editor["customization"].(map[string]any)
	assert.Equal(t, true, custom["forcesave"])

	// document.url and callbackUrl must be rooted at InternalBackend and embed
	// the user JWT as the query token.
	docURL := doc["url"].(string)
	cbURL := editor["callbackUrl"].(string)
	assert.Contains(t, docURL, env.backend+"/api/v1/workspace/files/download/report.docx")
	assert.Contains(t, docURL, "token="+url.QueryEscape(userTok))
	assert.Contains(t, cbURL, env.backend+"/api/v1/workspace/onlyoffice-callback")
	assert.Contains(t, cbURL, "path=report.docx")
	assert.Contains(t, cbURL, "token="+url.QueryEscape(userTok))

	// The token must verify with the same secret and carry the config as payload.
	claims := jwtpkg.MapClaims{}
	parsed, err := jwtpkg.ParseWithClaims(resp.Token, claims, func(t *jwtpkg.Token) (any, error) {
		return []byte(env.ooSecret), nil
	})
	require.NoError(t, err, "token must verify with the onlyoffice secret")
	require.True(t, parsed.Valid)
	// documentType round-trips through both the JSON response and the JWT payload.
	assert.Equal(t, "word", claims["documentType"])
	// The full config object must be the JWT payload (OnlyOffice verifies it).
	assert.True(t, reflect.DeepEqual(normalizeJSON(resp.Config), normalizeJSON(map[string]any(claims))),
		"token payload must equal the returned config")
}

// TestOnlyOfficeConfig_PathOutsideWorkspace_403 verifies a traversal attempt is
// rejected with 403 before any config is built.
func TestOnlyOfficeConfig_PathOutsideWorkspace_403(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	target := "/api/v1/workspace/files/" + url.PathEscape("../../etc/passwd") + onlyOfficeConfigSuffix
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+env.userJWT(t))
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct{ Code string `json:"code"` } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "FORBIDDEN", body.Error.Code)
}

// TestOnlyOfficeConfig_NotFound_404 verifies a missing file returns 404.
func TestOnlyOfficeConfig_NotFound_404(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	target := "/api/v1/workspace/files/missing.docx" + onlyOfficeConfigSuffix
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+env.userJWT(t))
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct{ Code string `json:"code"` } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "NOT_FOUND", body.Error.Code)
}

// TestOnlyOfficeConfig_Unauthenticated_401 verifies a request without a Bearer
// token is rejected by AuthMiddleware.
func TestOnlyOfficeConfig_Unauthenticated_401(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	target := "/api/v1/workspace/files/report.docx" + onlyOfficeConfigSuffix
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

// TestOnlyOfficeConfig_Disabled_503 verifies that when the secret is empty the
// endpoint returns 503 instead of signing an unverifiable config.
func TestOnlyOfficeConfig_Disabled_503(t *testing.T) {
	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "user-1", "workspace"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "user-1", "workspace", "report.docx"), []byte("x"), 0o644))
	h := NewWorkspaceHandler(fsSvc, testMaxUploadBytes, OnlyOfficeSettings{}) // disabled

	r := gin.New()
	r.GET("/api/v1/workspace/files/*path", func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Params = []gin.Param{{Key: "path", Value: strings.TrimPrefix(c.Param("path"), "/")}}
		h.OnlyOfficeConfig(c)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/files/report.docx"+onlyOfficeConfigSuffix, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct{ Code string `json:"code"` } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ONLYOFFICE_DISABLED", body.Error.Code)
}

// TestOnlyOfficeCallback_SaveStatus_Persists verifies status 2 and 6 download the
// result file and atomically overwrite the workspace file, returning error:0.
func TestOnlyOfficeCallback_SaveStatus_Persists(t *testing.T) {
	for _, status := range []int{2, 6} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			env := newOnlyOfficeTestEnv(t)
			ws := env.wsRoot()
			target := filepath.Join(ws, "report.docx")
			require.NoError(t, os.WriteFile(target, []byte("ORIGINAL"), 0o644))

			body := fmt.Sprintf(`{"status":%d,"key":"k","url":%q}`, status, env.dlServer.URL+"/edited")
			w := env.postCallback(t, "report.docx", body)

			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			var resp struct {
				Error int `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, 0, resp.Error, "save must ack error:0")

			data, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, "EDITED-CONTENT", string(data), "workspace file must be overwritten")
		})
	}
}

// TestOnlyOfficeCallback_NonSaveStatus_NoWrite verifies status 1 and 4 do not
// touch disk but still ack error:0.
func TestOnlyOfficeCallback_NonSaveStatus_NoWrite(t *testing.T) {
	for _, status := range []int{1, 4} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			env := newOnlyOfficeTestEnv(t)
			ws := env.wsRoot()
			target := filepath.Join(ws, "report.docx")
			require.NoError(t, os.WriteFile(target, []byte("ORIGINAL"), 0o644))

			body := fmt.Sprintf(`{"status":%d,"key":"k","url":%q}`, status, env.dlServer.URL+"/edited")
			w := env.postCallback(t, "report.docx", body)

			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			var resp struct {
				Error int `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, 0, resp.Error)

			data, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, "ORIGINAL", string(data), "non-save status must not modify the file")
		})
	}
}

// TestOnlyOfficeCallback_HostNotAllowed verifies a result URL whose host differs
// from ServerURL is rejected (SSRF mitigation) with error:1 and no write.
func TestOnlyOfficeCallback_HostNotAllowed(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	ws := env.wsRoot()
	target := filepath.Join(ws, "report.docx")
	require.NoError(t, os.WriteFile(target, []byte("ORIGINAL"), 0o644))

	body := fmt.Sprintf(`{"status":2,"key":"k","url":%q}`, "http://evil.example/edited")
	w := env.postCallback(t, "report.docx", body)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error int `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Error, "disallowed host must error:1")

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "ORIGINAL", string(data), "rejected callback must not modify the file")
}

// TestOnlyOfficeCallback_DownloadFails verifies a non-2xx download leaves the
// original file intact and replies error:1.
func TestOnlyOfficeCallback_DownloadFails(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	ws := env.wsRoot()
	target := filepath.Join(ws, "report.docx")
	require.NoError(t, os.WriteFile(target, []byte("ORIGINAL"), 0o644))

	body := fmt.Sprintf(`{"status":2,"key":"k","url":%q}`, env.dlServer.URL+"/missing")
	w := env.postCallback(t, "report.docx", body)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error int `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Error)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "ORIGINAL", string(data), "failed download must leave the original intact")
}

// TestOnlyOfficeCallback_Oversize verifies a result larger than maxUploadBytes is
// rejected with error:1 and the original file is untouched.
func TestOnlyOfficeCallback_Oversize(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	ws := env.wsRoot()
	target := filepath.Join(ws, "report.docx")
	require.NoError(t, os.WriteFile(target, []byte("ORIGINAL"), 0o644))

	body := fmt.Sprintf(`{"status":2,"key":"k","url":%q}`, env.dlServer.URL+"/oversize")
	w := env.postCallback(t, "report.docx", body)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error int `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Error)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "ORIGINAL", string(data), "oversize result must leave the original intact")
}

// TestOnlyOfficeCallback_MissingToken_401 verifies the query-token middleware
// rejects a callback without a token.
func TestOnlyOfficeCallback_MissingToken_401(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/onlyoffice-callback?path=report.docx", strings.NewReader(`{"status":2}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct{ Message string `json:"message"` } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "missing token", body.Error.Message)
}

// TestOnlyOfficeCallback_PathOutsideWorkspace_403 verifies a traversal attempt in
// the path query is rejected with 403 even with a valid token.
func TestOnlyOfficeCallback_PathOutsideWorkspace_403(t *testing.T) {
	env := newOnlyOfficeTestEnv(t)
	w := env.postCallback(t, "../../../etc/passwd", `{"status":2}`)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct{ Code string `json:"code"` } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "FORBIDDEN", body.Error.Code)
}

// normalizeJSON normalizes a value decoded from JSON so two decodings of the
// same object compare equal. It recurses into nested maps and slices; leaves are
// returned as-is. (Numbers in this config are absent, so no float coercion.)
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizeJSON(val)
		}
		return out
	default:
		return v
	}
}
