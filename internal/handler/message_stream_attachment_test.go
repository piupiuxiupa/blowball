package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/store/fs"
	"github.com/lush/blowball/internal/stream"
)

// visionSelectionConfig is testSelectionConfig with the catalog entry marked
// vision-capable (add-image-attachments).
func visionSelectionConfig() ModelSelectionConfig {
	return ModelSelectionConfig{
		Catalog: []config.ModelCatalogEntry{
			{Name: "test-model", MaxContextTokens: 128000, Thinking: false, Vision: true},
		},
		Default:       "test-model",
		DefaultEffort: "none",
	}
}

type attachmentSendEnv struct {
	engine  *gin.Engine
	stub    *stubOrchestrator
	atts    *fakeAttachmentStore
	redis   *handlerFakeRedis
	dataDir string
	runs    *run.Manager
}

func newAttachmentSendEnv(t *testing.T) *attachmentSendEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "ok"),
		stream.AgentEndEvent(stream.AgentConfucius),
	}}
	mysql := &handlerFakeMySQL{getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1"}}
	redis := &handlerFakeRedis{}
	fs := newHandlerFakeFS()
	deps := sessionDeps(mysql, redis, fs)
	sessSvc := newSessionSvc(deps)
	msgSvc := newMessageSvc(deps)

	atts := &fakeAttachmentStore{rows: map[string]model.Attachment{}}
	dataDir := t.TempDir()
	runMgr := run.NewManager(run.NewMemStore(), run.NewRegistry())
	streamH := NewMessageStreamHandler(sessSvc, msgSvc, nil, nil, nil, stub, dataDir,
		runMgr, visionSelectionConfig(), 0).
		WithAttachmentStore(atts)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", streamH.SendMessage)
	return &attachmentSendEnv{engine: r, stub: stub, atts: atts, redis: redis, dataDir: dataDir, runs: runMgr}
}

func (e *attachmentSendEnv) plant(t *testing.T, id, filename, mime string, data []byte, userID string) {
	t.Helper()
	e.atts.rows[id] = model.Attachment{ID: id, UserID: userID, SessionID: ptrStr("sess-1"), Filename: filename, MIME: mime, Size: int64(len(data))}
	if userID == "user-1" {
		dir := filepath.Join(e.dataDir, userID, "attachments", "sess-1")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".png"), data, 0o644))
	}
}

func (e *attachmentSendEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

const att1 = "01953c5e-7f9e-7cc8-8db5-2f4a5b6c7d8e"
const att2 = "01953c5e-8a11-7c22-9f00-1b2c3d4e5f60"
const att3 = "01953c5e-9b22-7d33-8f11-2c3d4e5f6a71"
const att4 = "01953c5e-ac33-7e44-9f22-3d4e5f6a7b82"
const att5 = "01953c5e-bd44-7f55-8f33-4e5f6a7b8c93"

func attachmentBody(ids ...string) string {
	b, _ := json.Marshal(map[string]any{"content": "look at this", "attachment_ids": ids})
	return string(b)
}

func TestSendMessage_Attachments_CurrentTurnInline(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.plant(t, att1, "shot.png", "image/png", pngBytes, "user-1")

	w := env.post(t, attachmentBody(att1))
	require.Equal(t, http.StatusOK, w.Code)

	msgs := env.stub.gotMessages
	require.NotEmpty(t, msgs)
	last := msgs[len(msgs)-1]
	assert.Equal(t, "user", last.Role)
	require.Len(t, last.Images, 1)
	assert.Equal(t, att1, last.Images[0].AttachmentID)
	assert.Equal(t, "image/png", last.Images[0].MIMEType)
	assert.Equal(t, pngBytes, last.Images[0].Data)

	// The persisted user row carries the structured references (the fake
	// records the send-time dual-write batch).
	require.NotEmpty(t, env.redis.dualBatches)
	var userRow *model.Message
	for _, batch := range env.redis.dualBatches {
		for i := range batch {
			if batch[i].Role == model.RoleUser {
				userRow = &batch[i]
			}
		}
	}
	require.NotNil(t, userRow)
	require.Len(t, userRow.Attachments, 1)
	assert.Equal(t, att1, userRow.Attachments[0].ID)
}

func TestSendMessage_Attachments_ForeignAttachmentRejected(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.plant(t, att1, "shot.png", "image/png", pngBytes, "user-2") // someone else's

	w := env.post(t, attachmentBody(att1))
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "attachment not found")
	assert.Empty(t, env.redis.dualBatches, "rejection must precede any persistence")
}

func TestSendMessage_Attachments_TooManyRejected(t *testing.T) {
	env := newAttachmentSendEnv(t)
	for _, id := range []string{att1, att2, att3, att4, att5} {
		env.plant(t, id, "s.png", "image/png", pngBytes, "user-1")
	}

	w := env.post(t, attachmentBody(att1, att2, att3, att4, att5))
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "at most 4 attachments")
}

func TestSendMessage_Attachments_NonVisionModelRejected(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.plantStaged(t, att1, "shot.png", pngBytes)

	// Rebuild the engine with the default (non-vision) selection config.
	gin.SetMode(gin.TestMode)
	mysql := &handlerFakeMySQL{getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1"}}
	deps := sessionDeps(mysql, &handlerFakeRedis{}, newHandlerFakeFS())
	streamH := NewMessageStreamHandler(newSessionSvc(deps), newMessageSvc(deps), nil, nil, nil,
		&stubOrchestrator{}, t.TempDir(), run.NewManager(run.NewMemStore(), run.NewRegistry()), testSelectionConfig(), 0).
		WithAttachmentStore(env.atts)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", streamH.SendMessage)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/messages", strings.NewReader(attachmentBody(att1)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "MODEL_NOT_VISION")
	assert.Nil(t, env.atts.rows[att1].SessionID, "capability rejection must not claim a staged attachment")
}

func TestSendMessage_Attachments_MissingFileDegradesToPlaceholder(t *testing.T) {
	env := newAttachmentSendEnv(t)
	// Registry row exists but the file was deleted underneath it.
	env.atts.rows[att1] = model.Attachment{ID: att1, UserID: "user-1", SessionID: ptrStr("sess-1"), Filename: "gone.png", MIME: "image/png"}

	w := env.post(t, attachmentBody(att1))
	require.Equal(t, http.StatusOK, w.Code)

	last := env.stub.gotMessages[len(env.stub.gotMessages)-1]
	assert.Empty(t, last.Images)
	assert.Contains(t, last.Content, "look at this")
	assert.Contains(t, last.Content, "[图片: gone.png]")
}

func TestMessagesToAgentMessages_AttachmentPlaceholderStable(t *testing.T) {
	row := UserMessage("sess-1", "trace-1", "look at this", time.Now(),
		model.MessageAttachmentRef{ID: att1, Filename: "shot.png", MIME: "image/png"})

	first, _, err := MessagesToAgentMessagesIndexed([]model.Message{row}, nil)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, "look at this\n[图片: shot.png]", first[0].Content)
	assert.Empty(t, first[0].Images)

	// Byte-stable across repeated reconstruction (append-only replay).
	second, _, err := MessagesToAgentMessagesIndexed([]model.Message{row}, nil)
	require.NoError(t, err)
	assert.Equal(t, first[0].Content, second[0].Content)
}

// plantStaged plants an uploaded-but-unbound attachment (session_id NULL) and
// its file under the _staging sentinel directory (add-staged-attachment-
// upload). data nil plants the registry row only (no file).
func (e *attachmentSendEnv) plantStaged(t *testing.T, id, filename string, data []byte) {
	t.Helper()
	e.atts.rows[id] = model.Attachment{ID: id, UserID: "user-1", SessionID: nil, Filename: filename, MIME: "image/png", Size: int64(len(data))}
	if data != nil {
		dir := filepath.Join(e.dataDir, "user-1", "attachments", fs.AttachmentStagingScope)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".png"), data, 0o644))
	}
}

// TestSendMessage_Attachments_StagedClaimed: a staged id sent to a (possibly
// brand-new) session is claimed atomically, the file moves into the session
// directory, and the image rides the current turn inline — the whole point of
// add-staged-attachment-upload.
func TestSendMessage_Attachments_StagedClaimed(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.plantStaged(t, att1, "shot.png", pngBytes)

	w := env.post(t, attachmentBody(att1))
	require.Equal(t, http.StatusOK, w.Code)

	// Row bound to the session; file moved out of staging.
	row := env.atts.rows[att1]
	require.NotNil(t, row.SessionID)
	assert.Equal(t, "sess-1", *row.SessionID)
	stagedPath := filepath.Join(env.dataDir, "user-1", "attachments", fs.AttachmentStagingScope, att1+".png")
	_, err := os.Stat(stagedPath)
	require.True(t, os.IsNotExist(err), "claimed file must leave the staging dir")
	boundPath := filepath.Join(env.dataDir, "user-1", "attachments", "sess-1", att1+".png")
	got, err := os.ReadFile(boundPath)
	require.NoError(t, err)
	assert.Equal(t, pngBytes, got)

	// The turn carried the image inline.
	last := env.stub.gotMessages[len(env.stub.gotMessages)-1]
	require.Len(t, last.Images, 1)
	assert.Equal(t, att1, last.Images[0].AttachmentID)
	assert.Equal(t, pngBytes, last.Images[0].Data)
}

// TestSendMessage_Attachments_BoundToOtherSessionRejected: once claimed, an
// attachment is that session's forever — another session's reference 404s.
func TestSendMessage_Attachments_BoundToOtherSessionRejected(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.atts.rows[att1] = model.Attachment{ID: att1, UserID: "user-1", SessionID: ptrStr("sess-2"), Filename: "x.png", MIME: "image/png"}

	w := env.post(t, attachmentBody(att1))
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "attachment not found")
	assert.Empty(t, env.redis.dualBatches, "rejection must precede any persistence")
}

// TestSendMessage_Attachments_StagedClaimRaceLost: the row looked staged at
// lookup time but the atomic claim lost (a concurrent send in another
// session won) — indistinguishable from a foreign id by contract.
func TestSendMessage_Attachments_StagedClaimRaceLost(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.plantStaged(t, att1, "shot.png", pngBytes)
	env.atts.claimHook = func(_, _, _ string) (bool, error) { return false, nil }

	w := env.post(t, attachmentBody(att1))
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, env.redis.dualBatches, "a lost claim must not occupy the turn or persist")
	assert.Nil(t, env.atts.rows[att1].SessionID, "lost claim leaves the row staged")
}

// TestSendMessage_Attachments_StagedBindFailureDegrades: the claim won but
// the file vanished before the rename — the claim rolls back and the turn
// continues with the deterministic placeholder (attachments never kill a
// turn).
func TestSendMessage_Attachments_StagedBindFailureDegrades(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.plantStaged(t, att1, "gone.png", nil) // row only, no staged file

	w := env.post(t, attachmentBody(att1))
	require.Equal(t, http.StatusOK, w.Code)

	last := env.stub.gotMessages[len(env.stub.gotMessages)-1]
	assert.Empty(t, last.Images)
	assert.Contains(t, last.Content, "[图片: gone.png]")
	assert.Nil(t, env.atts.rows[att1].SessionID, "failed bind must roll the claim back to staged")
}

func TestSendMessage_Attachments_PartialClaimFailureRollsBack(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.plantStaged(t, att1, "first.png", pngBytes)
	env.plantStaged(t, att2, "second.png", pngBytes)
	claims := 0
	env.atts.claimHook = func(id, userID, sessionID string) (bool, error) {
		claims++
		if claims > 1 {
			return false, nil
		}
		row := env.atts.rows[id]
		row.SessionID = ptrStr(sessionID)
		env.atts.rows[id] = row
		return true, nil
	}

	w := env.post(t, attachmentBody(att1, att2))
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, env.redis.dualBatches)
	assert.Nil(t, env.atts.rows[att1].SessionID, "first successful claim must be rolled back")
	assert.Nil(t, env.atts.rows[att2].SessionID)

	staged := filepath.Join(env.dataDir, "user-1", "attachments", fs.AttachmentStagingScope, att1+".png")
	bound := filepath.Join(env.dataDir, "user-1", "attachments", "sess-1", att1+".png")
	_, err := os.Stat(staged)
	assert.NoError(t, err, "rolled-back file must return to staging")
	_, err = os.Stat(bound)
	assert.True(t, os.IsNotExist(err))
}

func TestSendMessage_Attachments_SessionBusyRollsBackClaims(t *testing.T) {
	env := newAttachmentSendEnv(t)
	env.plantStaged(t, att1, "first.png", pngBytes)
	_, claimed, err := env.runs.Store.ClaimSession(context.Background(), "sess-1", "other-run")
	require.NoError(t, err)
	require.True(t, claimed)

	w := env.post(t, attachmentBody(att1))
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Nil(t, env.atts.rows[att1].SessionID, "SESSION_BUSY must roll the attachment claim back")
	staged := filepath.Join(env.dataDir, "user-1", "attachments", fs.AttachmentStagingScope, att1+".png")
	_, err = os.Stat(staged)
	assert.NoError(t, err)
}
