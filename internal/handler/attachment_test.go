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

	"github.com/google/uuid"
)

// pngBytes is a minimal PNG-magic payload — sniffing checks the leading
// signature only, so this suffices without being a real decodable image.
var pngBytes = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, 1, 2, 3, 4)

// ptrStr returns a *string for nullable-session fixtures.
func ptrStr(s string) *string { return &s }

type fakeAttachmentStore struct {
	rows map[string]model.Attachment
	// claimHook overrides ClaimAttachment when set (race injection for tests).
	claimHook         func(id, userID, sessionID string) (bool, error)
	insertStagedHook  func(a model.Attachment) (bool, error)
	unclaimErr        error
	deleteStagedError error
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
	if !ok || a.UserID != userID || a.SessionID == nil || *a.SessionID != sessionID {
		return nil, nil
	}
	cp := a
	return &cp, nil
}

func (f *fakeAttachmentStore) CountStagedAttachments(_ context.Context, userID string) (int, error) {
	n := 0
	for _, a := range f.rows {
		if a.UserID == userID && a.SessionID == nil {
			n++
		}
	}
	return n, nil
}

func (f *fakeAttachmentStore) InsertStagedAttachment(_ context.Context, a model.Attachment, limit int) (bool, error) {
	if f.insertStagedHook != nil {
		return f.insertStagedHook(a)
	}
	if f.rows == nil {
		f.rows = map[string]model.Attachment{}
	}
	if limit > 0 {
		n := 0
		for _, row := range f.rows {
			if row.UserID == a.UserID && row.SessionID == nil {
				n++
			}
		}
		if n >= limit {
			return false, nil
		}
	}
	f.rows[a.ID] = a
	return true, nil
}

func (f *fakeAttachmentStore) DeleteStagedAttachment(_ context.Context, id string) (bool, error) {
	if f.deleteStagedError != nil {
		return false, f.deleteStagedError
	}
	row, ok := f.rows[id]
	if !ok || row.SessionID != nil {
		return false, nil
	}
	delete(f.rows, id)
	return true, nil
}

func (f *fakeAttachmentStore) GetAttachmentByUser(_ context.Context, id, userID string) (*model.Attachment, error) {
	a, ok := f.rows[id]
	if !ok || a.UserID != userID {
		return nil, nil
	}
	cp := a
	return &cp, nil
}

func (f *fakeAttachmentStore) ClaimAttachment(_ context.Context, id, userID, sessionID string) (bool, error) {
	if f.claimHook != nil {
		return f.claimHook(id, userID, sessionID)
	}
	a, ok := f.rows[id]
	if !ok || a.UserID != userID || a.SessionID != nil {
		return false, nil
	}
	a.SessionID = ptrStr(sessionID)
	f.rows[id] = a
	return true, nil
}

func (f *fakeAttachmentStore) UnclaimAttachment(_ context.Context, id, userID, sessionID string) error {
	if f.unclaimErr != nil {
		return f.unclaimErr
	}
	a, ok := f.rows[id]
	if !ok || a.UserID != userID || a.SessionID == nil || *a.SessionID != sessionID {
		return nil
	}
	a.SessionID = nil
	f.rows[id] = a
	return nil
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
	authed.POST("/attachments", h.UploadStaged)
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
	require.NotNil(t, row.SessionID)
	assert.Equal(t, "s-1", *row.SessionID)
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
	store.rows[id] = model.Attachment{ID: id, UserID: "u-2", SessionID: ptrStr("s-1"), Filename: "x.png", MIME: "image/png"}

	for _, aid := range []string{id, "../../etc/passwd", "not-a-uuid"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/s-1/attachments/"+aid+"/content", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "id %q", aid)
	}
	_ = fsStore
}

func postStagedAttachment(t *testing.T, r *gin.Engine, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write(content)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAttachmentUploadStaged_Success verifies the session-independent upload
// (add-staged-attachment-upload): no session lookup, the row is born staged
// (nil session) and the file lands under the _staging sentinel directory.
func TestAttachmentUploadStaged_Success(t *testing.T) {
	_, store, fsStore, r := newAttachmentTestEnv(t, "u-1")

	w := postStagedAttachment(t, r, "shot.png", pngBytes)
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

	row, ok := store.rows[body.AttachmentID]
	require.True(t, ok)
	assert.Equal(t, "u-1", row.UserID)
	assert.Nil(t, row.SessionID, "staged upload must be born unbound")
	dst := filepath.Join(fsStore.UserAttachmentStagingDir("u-1"), body.AttachmentID+".png")
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, pngBytes, got)
}

// TestAttachmentUploadStaged_QuotaExceeded: the per-user staged quota rejects
// the 21st unbound upload with 429 and leaves disk/table untouched.
func TestAttachmentUploadStaged_QuotaExceeded(t *testing.T) {
	_, store, fsStore, r := newAttachmentTestEnv(t, "u-1")
	store.rows = map[string]model.Attachment{}
	for range maxStagedAttachmentsPerUser {
		id := uuid.NewString()
		store.rows[id] = model.Attachment{ID: id, UserID: "u-1", SessionID: nil, Filename: "x.png", MIME: "image/png"}
	}
	// A bound row and another user's staged row must not count against u-1.
	store.rows["bound"] = model.Attachment{ID: "bound", UserID: "u-1", SessionID: ptrStr("s-1"), MIME: "image/png"}
	store.rows["other"] = model.Attachment{ID: "other", UserID: "u-2", SessionID: nil, MIME: "image/png"}

	w := postStagedAttachment(t, r, "shot.png", pngBytes)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "TOO_MANY_STAGED_ATTACHMENTS")
	entries, err := os.ReadDir(fsStore.UserAttachmentStagingDir("u-1"))
	require.True(t, err != nil || len(entries) == 0, "no file must be written past the quota")
}

func TestAttachmentUploadStaged_AtomicReservationLosesRace(t *testing.T) {
	_, store, fsStore, r := newAttachmentTestEnv(t, "u-1")
	store.insertStagedHook = func(model.Attachment) (bool, error) {
		return false, nil
	}

	w := postStagedAttachment(t, r, "shot.png", pngBytes)

	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "TOO_MANY_STAGED_ATTACHMENTS")
	require.Empty(t, store.rows)
	entries, err := os.ReadDir(fsStore.UserAttachmentStagingDir("u-1"))
	require.True(t, err != nil || len(entries) == 0,
		"a lost atomic reservation must not write a staged file")
}

// TestAttachmentUploadStaged_RejectsBadImage: the staged endpoint enforces
// the same magic-byte sniffing as the session-scoped one.
func TestAttachmentUploadStaged_RejectsBadImage(t *testing.T) {
	_, store, _, r := newAttachmentTestEnv(t, "u-1")

	w := postStagedAttachment(t, r, "evil.png", []byte("%PDF-1.7 fake"))
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "UNSUPPORTED_IMAGE_TYPE")
	assert.Empty(t, store.rows)
}
