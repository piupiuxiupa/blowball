package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	cursorpkg "github.com/lush/blowball/internal/pkg/cursor"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
	"github.com/lush/blowball/internal/stream"
)

// stubOrchestrator is a canned OrchestratorRunner for handler tests. It writes
// the configured events to the hub, optionally sleeps to simulate work, then
// returns the same events + error. The recorded call args let tests assert the
// handler wired the orchestrator correctly.
type stubOrchestrator struct {
	mu            sync.Mutex
	gotWorkspace  string
	gotSkillsDir  string
	gotMessage    string
	eventsToEmit  []stream.StreamEvent
	returnErr     error
	preCloseSleep time.Duration
}

func (s *stubOrchestrator) Handle(ctx context.Context, workspaceRoot, skillsDir, userID, userMessage string, hub *stream.Hub) ([]stream.StreamEvent, error) {
	s.mu.Lock()
	s.gotWorkspace = workspaceRoot
	s.gotSkillsDir = skillsDir
	s.gotMessage = userMessage
	s.mu.Unlock()

	for _, e := range s.eventsToEmit {
		if !hub.SendCtx(ctx, e) {
			break
		}
	}
	if s.preCloseSleep > 0 {
		select {
		case <-time.After(s.preCloseSleep):
		case <-ctx.Done():
		}
	}
	// Mimic the production adapter contract: done is forwarded to the hub for
	// the SSE wire but is not part of the persisted event stream.
	var out []stream.StreamEvent
	for _, e := range s.eventsToEmit {
		if e.Type != stream.EventDone {
			out = append(out, e)
		}
	}
	return out, s.returnErr
}

// msgRecorder is a handler-package-local fake MySQLStore/FSStore/RedisStore
// combo for driving *service.SessionService through the real persistence path.
// It captures the messages handed to SaveMessage / SaveMessagesBatch so handler
// tests can assert on them.
//
// We reuse the existing service-package fakeMySQLStore etc. by exposing them
// through a tiny set of in-handler-package types — but those are private, so
// we re-declare minimal fakes here that satisfy service.MySQLStore /
// service.RedisStore / service.FSStore.
type handlerFakeMySQL struct {
	mu                  sync.Mutex
	createSessionCalls  int
	createSessionArg    model.Session
	createSessionErr    error
	getSessionByIDFound *model.Session
	getSessionIDErr     error
	listSessionsRows    []mysqlstore.SessionWithTitle
	listSessionsErr     error
	upsertTitleCalls    int
	upsertTitleArg      model.Title
	appendMessageCalls  int
	appendMessageArg    model.Message
	appendMessageErr    error
	appendMessagesCalls int
	appendMessagesArg   []model.Message
	appendMessagesErr   error
	listMessagesRows    []model.Message
	listMessagesErr     error
}

func (m *handlerFakeMySQL) CreateSession(_ context.Context, sess model.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createSessionCalls++
	m.createSessionArg = sess
	return m.createSessionErr
}
func (m *handlerFakeMySQL) GetSessionByID(_ context.Context, _ string) (*model.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getSessionIDErr != nil {
		return nil, m.getSessionIDErr
	}
	if m.getSessionByIDFound == nil {
		return nil, nil
	}
	cp := *m.getSessionByIDFound
	return &cp, nil
}
func (m *handlerFakeMySQL) ListSessionsWithTitle(_ context.Context, _ string) ([]mysqlstore.SessionWithTitle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listSessionsErr != nil {
		return nil, m.listSessionsErr
	}
	out := make([]mysqlstore.SessionWithTitle, len(m.listSessionsRows))
	copy(out, m.listSessionsRows)
	return out, nil
}
func (m *handlerFakeMySQL) UpsertTitle(_ context.Context, t model.Title) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertTitleCalls++
	m.upsertTitleArg = t
	return nil
}
func (m *handlerFakeMySQL) GetTitle(_ context.Context, _ string) (*model.Title, error) {
	return nil, nil
}
func (m *handlerFakeMySQL) AppendMessage(_ context.Context, msg model.Message) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendMessageCalls++
	m.appendMessageArg = msg
	if m.appendMessageErr != nil {
		return 0, m.appendMessageErr
	}
	return int64(m.appendMessageCalls), nil
}
func (m *handlerFakeMySQL) AppendMessages(_ context.Context, msgs []model.Message) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendMessagesCalls++
	m.appendMessagesArg = msgs
	if m.appendMessagesErr != nil {
		return nil, m.appendMessagesErr
	}
	ids := make([]int64, len(msgs))
	for i := range msgs {
		m.appendMessageCalls++
		ids[i] = int64(m.appendMessageCalls)
	}
	return ids, nil
}
func (m *handlerFakeMySQL) ListMessages(_ context.Context, _ string) ([]model.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listMessagesErr != nil {
		return nil, m.listMessagesErr
	}
	out := make([]model.Message, len(m.listMessagesRows))
	copy(out, m.listMessagesRows)
	return out, nil
}

func (m *handlerFakeMySQL) ListMessagesPaged(_ context.Context, _, cursorStr string, pageSize int, order string) ([]model.Message, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listMessagesErr != nil {
		return nil, "", m.listMessagesErr
	}
	rows := make([]model.Message, len(m.listMessagesRows))
	copy(rows, m.listMessagesRows)

	less := func(i, j int) bool {
		if rows[i].MsgTime.Equal(rows[j].MsgTime) {
			if rows[i].MsgIndex == rows[j].MsgIndex {
				return rows[i].ID < rows[j].ID
			}
			return rows[i].MsgIndex < rows[j].MsgIndex
		}
		return rows[i].MsgTime.Before(rows[j].MsgTime)
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if !less(i, j) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if order == "desc" {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}

	start := 0
	if cursorStr != "" {
		for i, msg := range rows {
			enc, err := cursorpkg.Encode(cursorpkg.Cursor{MsgTime: msg.MsgTime, MsgIndex: msg.MsgIndex, ID: msg.ID})
			if err != nil {
				return nil, "", err
			}
			if enc == cursorStr {
				start = i + 1
				break
			}
		}
	}
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	page := rows[start:end]
	if len(page) == 0 {
		return page, "", nil
	}
	if end >= len(rows) {
		return page, "", nil
	}
	last := page[len(page)-1]
	next, err := cursorpkg.Encode(cursorpkg.Cursor{MsgTime: last.MsgTime, MsgIndex: last.MsgIndex, ID: last.ID})
	if err != nil {
		return nil, "", err
	}
	return page, next, nil
}

type handlerFakeRedis struct {
	mu                  sync.Mutex
	appendCalls         int
	appendErr           error
	appendMessagesCalls int
	appendMessagesArgs  [][]byte
	appendMessagesErr   error
	getCalls            int
	getResult           [][]byte
	getErr              error
	setCalls            int
	setErr              error
}

func (r *handlerFakeRedis) AppendMessage(_ context.Context, _ string, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendCalls++
	return r.appendErr
}
func (r *handlerFakeRedis) AppendMessages(_ context.Context, _ string, raws [][]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendMessagesCalls++
	r.appendMessagesArgs = make([][]byte, len(raws))
	for i, b := range raws {
		r.appendMessagesArgs[i] = append([]byte(nil), b...)
	}
	return r.appendMessagesErr
}
func (r *handlerFakeRedis) GetMessages(_ context.Context, _ string) ([][]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getCalls++
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.getResult, nil
}
func (r *handlerFakeRedis) SetMessages(_ context.Context, _ string, _ [][]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCalls++
	return r.setErr
}

type handlerFakeFS struct {
	mu          sync.Mutex
	writeCalls  int
	writeData   map[string][]byte
	writeErr    error
	ensureCalls int
	ensureErr   error
}

func newHandlerFakeFS() *handlerFakeFS {
	return &handlerFakeFS{writeData: map[string][]byte{}}
}

func (f *handlerFakeFS) WriteSession(_ context.Context, userID, sessionID string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeCalls++
	f.writeData[userID+"/"+sessionID] = append([]byte(nil), data...)
	return f.writeErr
}
func (f *handlerFakeFS) ReadSession(_ context.Context, _, _ string) ([]byte, error) {
	return nil, nil
}
func (f *handlerFakeFS) DeleteSession(_ context.Context, _, _ string) error { return nil }
func (f *handlerFakeFS) EnsureUserDirs(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	return f.ensureErr
}

// sessionHandlerTestEnv bundles a configured SessionHandler with its backing
// fakes so each test can construct the harness with one helper.
type sessionHandlerTestEnv struct {
	h      *SessionHandler
	mysql  *handlerFakeMySQL
	redis  *handlerFakeRedis
	fs     *handlerFakeFS
	stub   *stubOrchestrator
	engine *gin.Engine
}

func newSessionHandlerEnv(t *testing.T, stub *stubOrchestrator) *sessionHandlerTestEnv {
	t.Helper()
	if stub == nil {
		stub = &stubOrchestrator{
			eventsToEmit: []stream.StreamEvent{
				stream.AgentStartEvent(stream.AgentConfuse),
				stream.TokenEvent(stream.AgentConfuse, "Hello"),
				stream.AgentEndEvent(stream.AgentConfuse),
			},
		}
	}
	mysql := &handlerFakeMySQL{
		getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1"},
	}
	redis := &handlerFakeRedis{}
	fs := newHandlerFakeFS()
	deps := sessionDeps(mysql, redis, fs)
	sessSvc := newSessionSvc(deps)
	msgSvc := newMessageSvc(deps)
	h := NewSessionHandler(sessSvc, msgSvc, nil, stub, "/tmp/blowball-test-data")

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions", h.CreateSession)
	r.GET("/api/v1/sessions", h.ListSessions)
	r.GET("/api/v1/sessions/:session_id/messages", h.GetSessionMessages)
	r.POST("/api/v1/sessions/:session_id/messages", h.SendMessage)
	return &sessionHandlerTestEnv{h: h, mysql: mysql, redis: redis, fs: fs, stub: stub, engine: r}
}

// TestSendMessage_DirectAnswer_PersistsUserAndAssistantEvents_SSE drives the
// full SSE path with a stub orchestrator that emits a start/token/end/done
// sequence then returns. The test verifies (a) the SSE body has the spec wire
// format for every event, (b) the user message is persisted synchronously, and
// (c) the assistant events are persisted as a batch after the orchestrator
// succeeds.
func TestSendMessage_DirectAnswer_PersistsUserAndAssistantEvents_SSE(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfuse),
			stream.TokenEvent(stream.AgentConfuse, "Hello, "),
			stream.TokenEvent(stream.AgentConfuse, "world!"),
			stream.AgentEndEvent(stream.AgentConfuse),
			stream.DoneEvent(map[string]any{"total_tokens": 10}),
		},
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"hi there"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// SSE headers must be present so the client knows this is a stream.
	require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))

	// Every emitted event must show up as a properly formatted SSE block.
	body := w.Body.String()
	require.Contains(t, body, "event: agent_start\n")
	require.Contains(t, body, "event: token\n")
	require.Contains(t, body, "event: agent_end\n")
	require.Contains(t, body, "event: done\n")
	// Each block must end with the SSE terminator.
	for block := range strings.SplitSeq(body, "\n\n") {
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "event: ") {
			continue
		}
		// data line must contain valid JSON.
		dataLine := ""
		for line := range strings.SplitSeq(block, "\n") {
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				dataLine = after
				break
			}
		}
		require.NotEmpty(t, dataLine, "expected data line in block %q", block)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(dataLine), &parsed), "block %q", block)
	}

	// Orchestrator saw the user message + a workspace rooted under the user's
	// data dir.
	env.stub.mu.Lock()
	require.Equal(t, "hi there", env.stub.gotMessage)
	require.Contains(t, env.stub.gotWorkspace, "user-1/workspace")
	env.stub.mu.Unlock()

	// Two FS writes happened: one synchronous user message, one batched
	// assistant event stream.
	require.Eventually(t, func() bool {
		env.fs.mu.Lock()
		defer env.fs.mu.Unlock()
		return env.fs.writeCalls == 2
	}, time.Second, 10*time.Millisecond, "expected two FS writes (user + assistant batch)")

	// MySQL: two batch appends — one for the user message (batch of 1) and one
	// for the assistant event stream (batch of 4, done excluded).
	require.Eventually(t, func() bool {
		env.mysql.mu.Lock()
		defer env.mysql.mu.Unlock()
		return env.mysql.appendMessagesCalls == 2
	}, time.Second, 10*time.Millisecond, "expected user batch + assistant batch")

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	require.Len(t, env.mysql.appendMessagesArg, 4, "assistant batch must contain all events except done")

	// User batch (first call) is a single message row.
	// We cannot observe the exact first batch args here because appendMessagesArg
	// holds the last call, but the user message fields are asserted in the
	// dedicated user-message tests below.
}

// TestSendMessage_BadRequest_NoBody verifies that an empty / malformed body
// yields 400 with the unified error shape.
func TestSendMessage_BadRequest_NoBody(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{not json`},
		{"missing content", `{"other":"x"}`},
		{"empty content", `{"content":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/sessions/sess-1/messages",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
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
			assert.Equal(t, "BAD_REQUEST", env2.Error.Code)
			assert.NotEmpty(t, env2.Error.Message)
		})
	}
}

// TestSendMessage_SessionNotFound_404 verifies that posting a message to a
// non-existent session returns 404 and never invokes the orchestrator.
func TestSendMessage_SessionNotFound_404(t *testing.T) {
	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{stream.TokenEvent(stream.AgentConfuse, "should not run")}}
	env := newSessionHandlerEnv(t, stub)
	env.mysql.getSessionByIDFound = nil // session does not exist

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
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

	env.stub.mu.Lock()
	defer env.stub.mu.Unlock()
	assert.Equal(t, "", env.stub.gotMessage, "orchestrator must NOT be called when session not found")
}

// TestSendMessage_WrongOwner_404 verifies that a user cannot send messages to
// another user's session.
func TestSendMessage_WrongOwner_404(t *testing.T) {
	stub := &stubOrchestrator{}
	env := newSessionHandlerEnv(t, stub)
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "sess-1", UserID: "other-user"}

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
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

// TestListSessions_ReturnsSessionsArray verifies the response shape and that
// sessions are surfaced in service-determined order with RFC3339 timestamps.
func TestListSessions_ReturnsSessionsArray(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.listSessionsRows = []mysqlstore.SessionWithTitle{
		{SessionID: "s-1", UserID: "user-1", Title: "Alpha", UpdateTime: time.Unix(1_700_000_010, 0).UTC()},
		{SessionID: "s-2", UserID: "user-1", Title: "Beta", UpdateTime: time.Unix(1_700_000_005, 0).UTC()},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Sessions []struct {
			SessionID  string `json:"session_id"`
			Title      string `json:"title"`
			UpdateTime string `json:"update_time"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Sessions, 2)
	assert.Equal(t, "s-1", resp.Sessions[0].SessionID)
	assert.Equal(t, "Alpha", resp.Sessions[0].Title)
	assert.Equal(t, "s-2", resp.Sessions[1].SessionID)
	// RFC3339 timestamps must parse cleanly.
	for _, s := range resp.Sessions {
		_, err := time.Parse(time.RFC3339, s.UpdateTime)
		assert.NoError(t, err, "update_time %q must be RFC3339", s.UpdateTime)
	}
}

// TestListSessions_EmptyArray verifies an empty result returns 200 with a
// JSON array (not null).
func TestListSessions_EmptyArray(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Sessions []any `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// The sessions array must be present (not null) and empty.
	assert.NotNil(t, resp.Sessions)
	assert.Empty(t, resp.Sessions)
}

// TestCreateSession_ReturnsUUIDv7SessionID verifies the new session endpoint
// returns a 36-character UUID v7 session_id and persists the row.
func TestCreateSession_ReturnsUUIDv7SessionID(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.SessionID, 36, "session_id must be 36-char UUID")
	assert.Equal(t, byte('7'), resp.SessionID[14], "session_id must be UUID v7")

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	require.Equal(t, 1, env.mysql.createSessionCalls)
	assert.Equal(t, resp.SessionID, env.mysql.createSessionArg.SessionID)
	assert.Equal(t, "user-1", env.mysql.createSessionArg.UserID)
	assert.Equal(t, "trace-1", env.mysql.createSessionArg.TraceID)
}

// TestGetSessionMessages_ReturnsPaginatedMessages verifies the messages endpoint
// returns the page from MySQL and a next_page_token when more pages exist.
func TestGetSessionMessages_ReturnsPaginatedMessages(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	base := time.Unix(1_700_000_000, 0).UTC()
	env.mysql.listMessagesRows = []model.Message{
		{ID: 1, SessionID: "sess-1", MsgTime: base, MsgIndex: 0, Content: "first"},
		{ID: 2, SessionID: "sess-1", MsgTime: base.Add(time.Second), MsgIndex: 0, Content: "second"},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1/messages?page_size=1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Messages      []model.Message `json:"messages"`
		NextPageToken string          `json:"next_page_token"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Messages, 1)
	assert.Equal(t, "first", resp.Messages[0].Content)
	assert.NotEmpty(t, resp.NextPageToken, "next_page_token must be present when more pages exist")
}

// TestGetSessionMessages_WrongOwner_404 verifies that a user cannot read
// another user's session messages.
func TestGetSessionMessages_WrongOwner_404(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "sess-1", UserID: "other-user"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1/messages", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestSendMessage_FirstTurnFiresTitle verifies that when there are no prior
// messages, title generation is invoked. TitleService is nil in the env above,
// so we configure one with a fake LLM and assert the title is upserted.
func TestSendMessage_FirstTurnFiresTitle(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	// Wire a real TitleService backed by a fake LLM and the env's MySQL fake.
	deps := sessionDeps(env.mysql, env.redis, env.fs)
	env.h.titleSvc = newTitleSvcWithFake(t, deps, "Generated Title")

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-new/messages",
		strings.NewReader(`{"content":"first message"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Title generation is fire-and-forget; give it time to land.
	require.Eventually(t, func() bool {
		env.mysql.mu.Lock()
		defer env.mysql.mu.Unlock()
		return env.mysql.upsertTitleCalls == 1
	}, time.Second, 10*time.Millisecond, "expected one title upsert on first turn")

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, "sess-new", env.mysql.upsertTitleArg.SessionID)
}

// TestSendMessage_NotFirstTurnDoesNotFireTitle verifies that title generation
// is suppressed when prior messages already exist for the session.
func TestSendMessage_NotFirstTurnDoesNotFireTitle(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	// Seed the Redis tier so RecoverMessages returns a non-empty list —
	// making the handler treat this as a non-first turn.
	priorMsg := model.Message{
		SessionID: "sess-old",
		Agent:     model.AgentUser,
		MsgIndex:  0,
		Role:      model.RoleUser,
		EventType: model.EventTypeMessage,
		Content:   "old",
	}
	raw, err := json.Marshal(priorMsg)
	require.NoError(t, err)
	env.redis.getResult = [][]byte{raw}

	env.h.titleSvc = newTitleSvcWithFake(t, sessionDeps(env.mysql, env.redis, env.fs), "should not be used")

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-old/messages",
		strings.NewReader(`{"content":"second message"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Give the fire-and-forget goroutine plenty of time it would NOT be
	// scheduled; assert no upsert ever happened.
	time.Sleep(50 * time.Millisecond)
	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 0, env.mysql.upsertTitleCalls, "title generation must NOT fire on non-first turn")
}

// TestSendMessage_ProducesDeterministicSSESequence asserts the exact SSE byte
// sequence a client sees for a direct-answer turn. Phase 12 integration tests
// can copy this assertion verbatim.
func TestSendMessage_ProducesDeterministicSSESequence(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfuse),
			stream.TokenEvent(stream.AgentConfuse, "Hello"),
			stream.AgentEndEvent(stream.AgentConfuse),
			stream.DoneEvent(nil),
		},
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-seq/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Parse every SSE block (delimited by \n\n) into structured form so we can
	// assert the exact event sequence: agent_start, token, agent_end, done.
	blocks := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n\n")
	require.Len(t, blocks, 4, "expected exactly 4 SSE events")

	types := make([]string, 0, 4)
	for _, b := range blocks {
		lines := strings.Split(b, "\n")
		require.True(t, strings.HasPrefix(lines[0], "event: "), "block %q", b)
		types = append(types, strings.TrimPrefix(lines[0], "event: "))
		require.Len(t, lines, 2, "expected event + data line per block; got %q", b)
		require.True(t, strings.HasPrefix(lines[1], "data: "), "block %q", b)
	}
	assert.Equal(t, []string{"agent_start", "token", "agent_end", "done"}, types)
}

// TestSendMessage_EventStreamIncludesMarkersAndToolCall verifies that a turn
// with agent lifecycle markers and a tool_call event is persisted as multiple
// rows, with correct event_type / role / agent values, and that the done event
// is excluded from persistence.
func TestSendMessage_EventStreamIncludesMarkersAndToolCall(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfuse),
			stream.TokenEvent(stream.AgentConfuse, "Thinking"),
			stream.ToolCallEvent(stream.AgentConfuse, "invoke_chongzhi", map[string]any{"task": "compute"}),
			stream.AgentStartEvent(stream.AgentChongzhi),
			stream.TokenEvent(stream.AgentChongzhi, "42"),
			stream.AgentEndEvent(stream.AgentChongzhi),
			stream.AgentEndEvent(stream.AgentConfuse),
			stream.DoneEvent(nil),
		},
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-events/messages",
		strings.NewReader(`{"content":"do it"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		env.mysql.mu.Lock()
		defer env.mysql.mu.Unlock()
		return env.mysql.appendMessagesCalls == 2
	}, time.Second, 10*time.Millisecond, "expected user batch + assistant batch")

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()

	// Assistant event stream: 7 events, done excluded.
	require.Len(t, env.mysql.appendMessagesArg, 7)
	wantTypes := []string{
		model.EventTypeAgentStart,
		model.EventTypeToken,
		model.EventTypeToolCall,
		model.EventTypeAgentStart,
		model.EventTypeToken,
		model.EventTypeAgentEnd,
		model.EventTypeAgentEnd,
	}
	for i, want := range wantTypes {
		assert.Equal(t, want, env.mysql.appendMessagesArg[i].EventType, "event %d", i)
	}

	// Token/tool_call rows carry the assistant role; markers have empty role.
	for _, m := range env.mysql.appendMessagesArg {
		switch m.EventType {
		case model.EventTypeToken, model.EventTypeToolCall:
			assert.Equal(t, model.RoleAssistant, m.Role, "event_type=%s", m.EventType)
		default:
			assert.Empty(t, m.Role, "event_type=%s", m.EventType)
		}
	}

	// Agent column is preserved from the event.
	assert.Equal(t, stream.AgentConfuse, env.mysql.appendMessagesArg[0].Agent)
	assert.Equal(t, stream.AgentChongzhi, env.mysql.appendMessagesArg[4].Agent)

	// Tool_call content is JSON with name and args.
	toolMsg := env.mysql.appendMessagesArg[2]
	require.Equal(t, model.EventTypeToolCall, toolMsg.EventType)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(toolMsg.Content), &payload))
	assert.Equal(t, "invoke_chongzhi", payload["name"])
}

// TestSendMessage_OrchestratorFailure_PersistsUserOnly verifies that when the
// orchestrator returns an error, only the user message is persisted and no
// assistant event rows are written.
func TestSendMessage_OrchestratorFailure_PersistsUserOnly(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfuse),
			stream.TokenEvent(stream.AgentConfuse, "oops"),
		},
		returnErr: errors.New("orchestrator blew up"),
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-fail/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Wait for the synchronous user save; the assistant batch must NOT run.
	require.Eventually(t, func() bool {
		env.mysql.mu.Lock()
		defer env.mysql.mu.Unlock()
		return env.mysql.appendMessagesCalls == 1
	}, time.Second, 10*time.Millisecond, "expected exactly one user message batch")

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	require.Len(t, env.mysql.appendMessagesArg, 1)
	userMsg := env.mysql.appendMessagesArg[0]
	assert.Equal(t, model.AgentUser, userMsg.Agent)
	assert.Equal(t, model.EventTypeMessage, userMsg.EventType)
	assert.Equal(t, model.RoleUser, userMsg.Role)
	assert.Equal(t, 0, userMsg.MsgIndex)
}
