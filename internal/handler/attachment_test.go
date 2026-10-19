package handler

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/store/fs"
)

// pngBytes is a minimal PNG-magic payload — sniffing checks the leading
// signature only, so this suffices without being a real decodable image.
var pngBytes = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, 1, 2, 3, 4)

type fakeAttachmentStore struct {
	rows map[string]model.Attachment
}

func (f *fakeAttachmentStore) InsertAttachment(_ context.Context, a model.Attachment) error {
	if f.rows == nil {
		f.rows = map[string]model.Attachment{}
	}
	f.rows[a.ID] = a
	return nil
}

func (f *fakeAttachmentStore) GetAttachment(_ context.Context, id, userID, sessionID string) (*model.Attachment, error) {
	a, ok := f.rows[id]
	if !ok || a.UserID != userID || a.SessionID != sessionID {
		return nil, nil
	}
	cp := a
	return &cp, nil
}

type fakeSessionGetter struct {
	sessions map[string]model.Session
}

func (f *fakeSessionGetter) GetSessionByID(_ context.Context, sessionID string) (*model.Session, error) {
	s, ok := f.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	return &s, nil
}

func newAttachmentTestEnv(t *testing.T, owner string) (*AttachmentHandler, *fakeAttachmentStore, *fs.Store, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := &fakeAttachmentStore{}
	sessions := &fakeSessionGetter{sessions: map[string]model.Session{
		"s-1": {SessionID: "s-1", UserID: owner},
	}}
	fsStore, err := fs.New(t.TempDir())
	require.NoError(t, err)
	h := NewAttachmentHandler(store, sessions, fsStore)

	r := gin.New()
	v1 := r.Group("/api/v1")
	// Bearer partition (auth simulated by publishing the owner's id).
	authed := v1.Group("/")
	authed.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, owner)
		c.Next()
	})
	authed.POST("/sessions/:session_id/attachments", h.Upload)
	// Query-token partition (same simulation).
	v1.GET("/sessions/:session_id/attachments/:attachment_id/content", func(c *gin.Context) {
		c.Set(middleware.UserIDKey, owner)
		c.Next()
	}, h.Content)
	return h, store, fsStore, r
}

func postAttachment(t *testing.T, r *gin.Engine, sessionID, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write(content)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAttachmentUpload_Success(t *testing.T) {
	_, store, fsStore, r := newAttachmentTestEnv(t, "u-1")

	w := postAttachment(t, r, "s-1", "../evil name.png", pngBytes)
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		AttachmentID string `json:"attachment_id"`
		Filename     string `json:"filename"`
		MIME         string `json:"mime"`
		Size         int64  `json:"size"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "image/png", body.MIME)
	assert.Equal(t, int64(len(pngBytes)), body.Size)
	assert.Equal(t, "evil name.png", body.Filename, "client filename is basename-sanitized display metadata")

	// Registry row + on-disk bytes at the id-derived path (never the client name).
	row, ok := store.rows[body.AttachmentID]
	require.True(t, ok)
	assert.Equal(t, "u-1", row.UserID)
	assert.Equal(t, "s-1", row.SessionID)
	dst := filepath.Join(fsStore.UserAttachmentDir("u-1", "s-1"), body.AttachmentID+".png")
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, pngBytes, got)
}

func TestAttachmentUpload_NonImageRejected(t *testing.T) {
	_, store, fsStore, r := newAttachmentTestEnv(t, "u-1")

	w := postAttachment(t, r, "s-1", "doc.pdf", []byte("%PDF-1.7 fake"))
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "UNSUPPORTED_IMAGE_TYPE")
	assert.Empty(t, store.rows)
	// Rejection happens before any directory is created — nothing touched disk.
	_, err := os.Stat(fsStore.UserAttachmentDir("u-1", "s-1"))
	require.True(t, os.IsNotExist(err))
}

func TestAttachmentUpload_TooLarge(t *testing.T) {
	_, store, _, r := newAttachmentTestEnv(t, "u-1")

	big := append(append([]byte{}, pngBytes...), make([]byte, maxAttachmentBytes)...)
	w := postAttachment(t, r, "s-1", "big.png", big)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Empty(t, store.rows)
}

func TestAttachmentUpload_NotOwner(t *testing.T) {
	_, store, _, r := newAttachmentTestEnv(t, "u-1")

	w := postAttachment(t, r, "s-other", "shot.png", pngBytes)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, store.rows)
}

func TestAttachmentContent_ETagAnd304(t *testing.T) {
	_, _, _, r := newAttachmentTestEnv(t, "u-1")

	w := postAttachment(t, r, "s-1", "shot.png", pngBytes)
	require.Equal(t, http.StatusOK, w.Code)
	var up struct {
		AttachmentID string `json:"attachment_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &up))

	url := "/api/v1/sessions/s-1/attachments/" + up.AttachmentID + "/content"

	req := httptest.NewRequest(http.MethodGet, url, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.Equal(t, `"`+up.AttachmentID+`"`, w.Header().Get("ETag"))
	assert.Equal(t, "private, max-age=31536000, immutable", w.Header().Get("Cache-Control"))
	assert.Equal(t, pngBytes, w.Body.Bytes())

	req = httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("If-None-Match", `"`+up.AttachmentID+`"`)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotModified, w.Code)
	assert.Empty(t, w.Body.Bytes())
}

func TestAttachmentContent_CrossUserAndBadID(t *testing.T) {
	_, store, fsStore, r := newAttachmentTestEnv(t, "u-1")
	// Plant a foreign row: u-2's attachment inside u-1's tree cannot be read
	// even if the file existed — the registry triple rejects it first.
	id := "01953c5e-7f9e-7cc8-8db5-2f4a5b6c7d8e"
	store.rows = map[string]model.Attachment{}
	store.rows[id] = model.Attachment{ID: id, UserID: "u-2", SessionID: "s-1", Filename: "x.png", MIME: "image/png"}

	for _, aid := range []string{id, "../../etc/passwd", "not-a-uuid"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/s-1/attachments/"+aid+"/content", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "id %q", aid)
	}
	_ = fsStore
}
