package handler

import (
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
	streamH := NewMessageStreamHandler(sessSvc, msgSvc, nil, nil, nil, stub, dataDir,
		run.NewManager(run.NewMemStore(), run.NewRegistry()), visionSelectionConfig(), 0).
		WithAttachmentStore(atts)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", streamH.SendMessage)
	return &attachmentSendEnv{engine: r, stub: stub, atts: atts, redis: redis, dataDir: dataDir}
}

func (e *attachmentSendEnv) plant(t *testing.T, id, filename, mime string, data []byte, userID string) {
	t.Helper()
	e.atts.rows[id] = model.Attachment{ID: id, UserID: userID, SessionID: "sess-1", Filename: filename, MIME: mime, Size: int64(len(data))}
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
	env.plant(t, att1, "shot.png", "image/png", pngBytes, "user-1")

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
}

func TestSendMessage_Attachments_MissingFileDegradesToPlaceholder(t *testing.T) {
	env := newAttachmentSendEnv(t)
	// Registry row exists but the file was deleted underneath it.
	env.atts.rows[att1] = model.Attachment{ID: att1, UserID: "user-1", SessionID: "sess-1", Filename: "gone.png", MIME: "image/png"}

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
