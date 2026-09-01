package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	cursorpkg "github.com/lush/blowball/internal/pkg/cursor"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
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
	gotMessages   []agent.Message
	eventsToEmit  []stream.StreamEvent
	returnErr     error
	preCloseSleep time.Duration
	// interEventDelay parks BETWEEN emitted events (never after the last),
	// letting tests keep a turn in flight across incremental-persist ticks.
	interEventDelay time.Duration
}

func (s *stubOrchestrator) Handle(ctx context.Context, workspaceRoot, skillsDir, userID string, messages []agent.Message, hub *stream.Hub, hooks TurnHooks, _ agent.ModelOverride) ([]stream.StreamEvent, map[string]any, error) {
	s.mu.Lock()
	s.gotWorkspace = workspaceRoot
	s.gotSkillsDir = skillsDir
	s.gotMessages = messages
	s.mu.Unlock()

	// Serve the (now unconditional) turn tap like the production adapter
	// does, so the incremental persister can snapshot the stream so far
	// (incremental-message-persistence). Snapshots reply with the events
	// emitted up to that moment.
	var tapMu sync.Mutex
	var collected []stream.StreamEvent
	if tap := hooks.Tap; tap != nil {
		defer tap.close()
		reqs := tap.requests()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case req := <-reqs:
					tapMu.Lock()
					out := append([]stream.StreamEvent(nil), collected...)
					tapMu.Unlock()
					req <- out
				}
			}
		}()
	}

	for i, e := range s.eventsToEmit {
		if !hub.SendCtx(ctx, e) {
			break
		}
		tapMu.Lock()
		collected = append(collected, e)
		tapMu.Unlock()
		if s.interEventDelay > 0 && i < len(s.eventsToEmit)-1 {
			select {
			case <-time.After(s.interEventDelay):
			case <-ctx.Done():
			}
		}
	}
	if s.preCloseSleep > 0 {
		select {
		case <-time.After(s.preCloseSleep):
		case <-ctx.Done():
		}
	}
	// Mimic the production adapter contract: done is forwarded to the hub for
	// the SSE wire but is not part of the persisted event stream; its usage is
	// returned separately so the handler can persist per-agent cost.
	var out []stream.StreamEvent
	var usage map[string]any
	for _, e := range s.eventsToEmit {
		if e.Type == stream.EventDone {
			if u, ok := e.Meta[stream.MetaUsage].(map[string]any); ok {
				usage = u
			}
			continue
		}
		out = append(out, e)
	}
	return out, usage, s.returnErr
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
	deleteSessionCalls  int
	deleteSessionArg    string
	deleteSessionErr    error
	listSessionsRows    []mysqlstore.SessionWithTitle
	listSessionsErr     error
	getTitleFound       *model.Title
	getTitleErr         error
	upsertTitleCalls    int
	upsertTitleArg      model.Title
	appendMessagesCalls int
	appendMessagesArg   []model.Message
	appendMessagesErr   error
	// appendMessagesErrFirst fails only the first N AppendMessages calls
	// (harden-turn-persistence: send-time dual failure, turn-end recovery).
	appendMessagesErrFirst int
	listMessagesRows       []model.Message
	listMessagesErr        error
	saveTurnUsageCalls     int
	saveTurnUsageArg       model.TurnUsage
	saveTurnUsageErr       error

	// Compaction-storage recording (context-compaction capability).
	compactions                 []model.ContextCompaction
	insertCompactionErr         error
	latestCompactionErr         error
	updateSessionCompactedCalls int
	updateSessionCompactedErr   error
	latestContextTokens         int
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
func (m *handlerFakeMySQL) DeleteSession(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteSessionCalls++
	m.deleteSessionArg = sessionID
	return m.deleteSessionErr
}
func (m *handlerFakeMySQL) UpdateSessionTime(_ context.Context, _ string) error { return nil }
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
func (m *handlerFakeMySQL) UpsertTitleManual(_ context.Context, t model.Title) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertTitleCalls++
	m.upsertTitleArg = t
	return nil
}
func (m *handlerFakeMySQL) GetTitle(_ context.Context, _ string) (*model.Title, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getTitleErr != nil {
		return nil, m.getTitleErr
	}
	if m.getTitleFound == nil {
		return nil, nil
	}
	cp := *m.getTitleFound
	return &cp, nil
}
func (m *handlerFakeMySQL) AppendMessages(_ context.Context, msgs []model.Message) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendMessagesCalls++
	m.appendMessagesArg = msgs
	if m.appendMessagesErrFirst > 0 {
		m.appendMessagesErrFirst--
		return nil, errFakeHandler
	}
	if m.appendMessagesErr != nil {
		return nil, m.appendMessagesErr
	}
	ids := make([]int64, len(msgs))
	for i := range ids {
		ids[i] = int64(i + 1)
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

func (m *handlerFakeMySQL) SaveTurnUsage(_ context.Context, tu model.TurnUsage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveTurnUsageCalls++
	m.saveTurnUsageArg = tu
	return m.saveTurnUsageErr
}

// Compaction-storage members (context-compaction capability). compactions is
// the append-only record list; latestCompactionErr simulates read failures.
func (m *handlerFakeMySQL) InsertCompaction(_ context.Context, rec model.ContextCompaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.compactions = append(m.compactions, rec)
	return m.insertCompactionErr
}
func (m *handlerFakeMySQL) LatestCompaction(_ context.Context, _ string) (*model.ContextCompaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.latestCompactionErr != nil {
		return nil, m.latestCompactionErr
	}
	if len(m.compactions) == 0 {
		return nil, nil
	}
	cp := m.compactions[len(m.compactions)-1]
	return &cp, nil
}
func (m *handlerFakeMySQL) UpdateSessionCompacted(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateSessionCompactedCalls++
	return m.updateSessionCompactedErr
}
func (m *handlerFakeMySQL) LatestContextTokens(_ context.Context, _ string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latestContextTokens, nil
}

// handlerFakeRedis records the write-behind dual writes. The decoded rows
// (dualRows) are what handler tests assert on — with the Redis-first change
// the dual-write pipeline IS the persistence call, so the batch previously
// observed on the MySQL fake now lands here. dualBatches records each
// pipeline call's rows separately so tests can assert the batch SPLIT
// (send-time [user] + turn-end [events], send-time-user-message-persistence).
type handlerFakeRedis struct {
	mu          sync.Mutex
	dualCalls   int
	dualSID     string
	dualRows    []model.Message
	dualBatches [][]model.Message
	dualErr     error
	// dualErrFirst fails only the first N AppendMessagesDual calls
	// (harden-turn-persistence: send-time failure, turn-end recovery).
	dualErrFirst int
	clearCalls   int
	delSessErr   error
	getCalls     int
	getResult    [][]byte
	getErr       error
	setCalls     int
	setErr       error
	setArgRows   int

	// Compaction-cache recording (context-compaction capability).
	compactionCache    []byte
	setCompactionCalls int
	setCompactionErr   error
	getCompactionCalls int
}

func (r *handlerFakeRedis) AppendMessagesDual(_ context.Context, sessionID string, raws [][]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dualCalls++
	r.dualSID = sessionID
	batch := make([]model.Message, 0, len(raws))
	for _, b := range raws {
		var m model.Message
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		batch = append(batch, m)
	}
	r.dualRows = append(r.dualRows, batch...)
	r.dualBatches = append(r.dualBatches, batch)
	if r.dualErrFirst > 0 {
		r.dualErrFirst--
		return errFakeHandler
	}
	return r.dualErr
}
func (r *handlerFakeRedis) dualCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dualCalls
}
func (r *handlerFakeRedis) dualSnapshot() []model.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]model.Message, len(r.dualRows))
	copy(out, r.dualRows)
	return out
}

// dualBatchSnapshot returns a copy of the per-call batches in call order.
func (r *handlerFakeRedis) dualBatchSnapshot() [][]model.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]model.Message, len(r.dualBatches))
	copy(out, r.dualBatches)
	return out
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
func (r *handlerFakeRedis) SetMessages(_ context.Context, _ string, raws [][]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCalls++
	r.setArgRows = len(raws)
	return r.setErr
}
func (r *handlerFakeRedis) ClearMessages(_ context.Context, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearCalls++
	return nil
}
func (r *handlerFakeRedis) DelSessionCache(_ context.Context, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.delSessErr
}

// Compaction-cache members (context-compaction capability): a single-slot
// cache mirroring the whole-key overwrite semantics of the real store.
func (r *handlerFakeRedis) SetCompactionCache(_ context.Context, _ string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCompactionCalls++
	r.compactionCache = append([]byte(nil), data...)
	return r.setCompactionErr
}
func (r *handlerFakeRedis) GetCompactionCache(_ context.Context, _ string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getCompactionCalls++
	if r.compactionCache == nil {
		return nil, nil
	}
	return append([]byte(nil), r.compactionCache...), nil
}
func (r *handlerFakeRedis) DelCompactionCache(_ context.Context, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.compactionCache = nil
	return nil
}

// handlerFakeFS records EnsureUserDirs — the FS store's only remaining duty
// after the warm-tier removal.
type handlerFakeFS struct {
	mu          sync.Mutex
	ensureCalls int
	ensureErr   error
}

func newHandlerFakeFS() *handlerFakeFS {
	return &handlerFakeFS{}
}

func (f *handlerFakeFS) EnsureUserDirs(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	return f.ensureErr
}

// sessionHandlerTestEnv bundles a configured SessionHandler (CRUD) and
// MessageStreamHandler (streaming) with their backing fakes so each test can
// construct the harness with one helper. Mirrors the role split: the CRUD
// routes are served by h, the streaming route by stream.
type sessionHandlerTestEnv struct {
	h      *SessionHandler
	stream *MessageStreamHandler
	mysql  *handlerFakeMySQL
	redis  *handlerFakeRedis
	fs     *handlerFakeFS
	stub   *stubOrchestrator
	engine *gin.Engine
}

// errFakeHandler is the canned store error used by the fail-first-N fake
// knobs (harden-turn-persistence tests).
var errFakeHandler = errors.New("fake handler store failure")

func newSessionHandlerEnv(t *testing.T, stub *stubOrchestrator) *sessionHandlerTestEnv {
	t.Helper()
	if stub == nil {
		stub = &stubOrchestrator{
			eventsToEmit: []stream.StreamEvent{
				stream.AgentStartEvent(stream.AgentConfucius),
				stream.TokenEvent(stream.AgentConfucius, "Hello"),
				stream.AgentEndEvent(stream.AgentConfucius),
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
	titleSvc := service.NewTitleService(nil, mysql, config.OpenAIConfig{TitleModel: "title-model"})
	// SessionHandler owns CRUD only; MessageStreamHandler owns the streaming
	// endpoint and the orchestrator dependency. Both share the same services.
	h := NewSessionHandler(sessSvc, titleSvc, nil)
	stream := NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, nil, nil, stub, "/tmp/blowball-test-data", run.NewManager(run.NewMemStore(), run.NewRegistry()), testSelectionConfig(), 0)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions", h.CreateSession)
	r.GET("/api/v1/sessions", h.ListSessions)
	r.GET("/api/v1/sessions/:session_id", h.GetSession)
	r.GET("/api/v1/sessions/:session_id/messages", h.GetSessionMessages)
	r.POST("/api/v1/sessions/:session_id/messages", stream.SendMessage)
	r.PATCH("/api/v1/sessions/:session_id", h.UpdateTitle)
	r.DELETE("/api/v1/sessions/:session_id", h.DeleteSession)
	return &sessionHandlerTestEnv{h: h, stream: stream, mysql: mysql, redis: redis, fs: fs, stub: stub, engine: r}
}

// TestSendMessage_DirectAnswer_PersistsUserAndAssistantEvents_SSE drives the
// full SSE path with a stub orchestrator that emits a start/token/end/done
// sequence then returns. The test verifies (a) the SSE body has the spec wire
// format for every event, (b) the user message and assistant events are
// persisted together in a single batch after the orchestrator succeeds.
func TestSendMessage_DirectAnswer_PersistsUserAndAssistantEvents_SSE(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "Hello, "),
			stream.TokenEvent(stream.AgentConfucius, "world!"),
			stream.AgentEndEvent(stream.AgentConfucius),
			stream.DoneEvent(map[string]any{
				"total":    map[string]any{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10},
				"by_agent": map[string]any{"Confucius": map[string]any{"total_tokens": 10}},
				"meta":     map[string]any{"sub_agent_invocations": []string{}, "parallel": false},
			}),
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
	require.Len(t, env.stub.gotMessages, 1)
	require.Equal(t, "user", env.stub.gotMessages[0].Role)
	require.Equal(t, "hi there", env.stub.gotMessages[0].Content)
	require.Contains(t, env.stub.gotWorkspace, "user-1/workspace")
	env.stub.mu.Unlock()

	// TWO dual writes happened (send-time-user-message-persistence): the
	// send-time batch carrying the user row alone, then the turn-end batch
	// carrying the merged assistant events (the write-behind queue and the
	// read cache receive the same blobs in one pipeline per batch).
	require.Eventually(t, func() bool {
		return env.redis.dualCount() == 2
	}, time.Second, 10*time.Millisecond, "expected one send-time batch + one turn-end batch")

	batches := env.redis.dualBatchSnapshot()
	require.Len(t, batches, 2)
	// Batch 1 (send-time, synchronous before the turn started): the user row
	// alone.
	require.Len(t, batches[0], 1, "send-time batch must contain only the user row")
	userMsg := batches[0][0]
	assert.Equal(t, model.AgentUser, userMsg.Agent)
	assert.Equal(t, model.EventTypeMessage, userMsg.EventType)
	assert.Equal(t, model.RoleUser, userMsg.Role)
	assert.Equal(t, 0, userMsg.MsgIndex)
	assert.Equal(t, "hi there", userMsg.Content)
	require.NotEmpty(t, userMsg.ClientMsgID, "persisted rows carry the idempotency key")

	// Batch 2 (turn-end): the assistant events only — the user row must not
	// re-enter (msgs:{sid} has no dedup).
	for _, m := range batches[1] {
		assert.NotEqual(t, model.EventTypeMessage, m.EventType, "turn-end batch must not repeat the user row")
	}

	rows := env.redis.dualSnapshot()
	// Concatenated: user message followed by merged assistant events (agent_start, token, agent_end).
	require.Len(t, rows, 4, "batches must contain user + 3 merged assistant events")

	// Assistant event stream is merged: agent_start, merged token, agent_end (done excluded).
	wantTypes := []string{
		model.EventTypeAgentStart,
		model.EventTypeToken,
		model.EventTypeAgentEnd,
	}
	for i, want := range wantTypes {
		assert.Equal(t, want, rows[i+1].EventType, "assistant event %d", i)
	}
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

// newLimitTestEngine builds a scratch engine mounting a SendMessage handler
// with a caller-chosen maxInputTokens over the shared test fakes — the env's
// own streaming handler is constructed with the limit disabled (0), so the
// token-cap tests need their own wiring.
func newLimitTestEngine(t *testing.T, env *sessionHandlerTestEnv, stub *stubOrchestrator, maxInputTokens int) *gin.Engine {
	t.Helper()
	deps := sessionDeps(env.mysql, env.redis, env.fs)
	stream := NewMessageStreamHandler(
		newSessionSvc(deps), newMessageSvc(deps),
		service.NewTitleService(nil, env.mysql, config.OpenAIConfig{TitleModel: "title-model"}),
		nil, nil, stub, "/tmp/blowball-test-data",
		run.NewManager(run.NewMemStore(), run.NewRegistry()),
		testSelectionConfig(), maxInputTokens)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", stream.SendMessage)
	return r
}

// TestSendMessage_ContentTooLong_RejectedBeforeClaim verifies over-limit
// content is rejected with 400 CONTENT_TOO_LONG carrying the limit and the
// estimate, and that the rejection never reaches storage or the orchestrator
// nor claims the session's run slot (a rejected input must not produce a
// SESSION_BUSY interplay).
func TestSendMessage_ContentTooLong_RejectedBeforeClaim(t *testing.T) {
	env := newSessionHandlerEnv(t, &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{stream.TokenEvent(stream.AgentConfucius, "must not run")},
	})
	r := newLimitTestEngine(t, env, env.stub, 10)

	// 50 CJK runes → estimate 50 > limit 10.
	body := `{"content":"` + strings.Repeat("汉", 50) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "CONTENT_TOO_LONG", resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "10-token")
	assert.Contains(t, resp.Error.Message, "estimated 50")

	// No store I/O and no orchestrator invocation.
	env.mysql.mu.Lock()
	assert.Zero(t, env.mysql.appendMessagesCalls, "rejected input must not persist")
	env.mysql.mu.Unlock()
	assert.Zero(t, env.redis.dualCount(), "rejected input must not reach the dual-write path")
	env.stub.mu.Lock()
	assert.Empty(t, env.stub.gotMessages, "orchestrator must not run for rejected input")
	env.stub.mu.Unlock()
}

// TestSendMessage_ContentWithinLimit_Proceeds verifies an under-limit content
// flows through the ordinary path (SSE 200), i.e. the check does not disturb
// normal input.
func TestSendMessage_ContentWithinLimit_Proceeds(t *testing.T) {
	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "ok"),
		stream.AgentEndEvent(stream.AgentConfucius),
	}}
	env := newSessionHandlerEnv(t, stub)
	r := newLimitTestEngine(t, env, stub, 100)

	// 50 CJK runes → estimate 50 ≤ limit 100.
	body := `{"content":"` + strings.Repeat("汉", 50) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

// TestSendMessage_TokenLimitDisabled_PassesOverlong verifies a non-positive
// maxInputTokens (explicit config 0) skips the token check: over-limit content
// proceeds to the ordinary SSE path. The 1MB body cap is exercised separately
// below and is NOT disabled by this setting.
func TestSendMessage_TokenLimitDisabled_PassesOverlong(t *testing.T) {
	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "ok"),
		stream.AgentEndEvent(stream.AgentConfucius),
	}}
	env := newSessionHandlerEnv(t, stub)
	r := newLimitTestEngine(t, env, stub, 0)

	// Would estimate ~50 against a 10-token limit — but the limit is off.
	body := `{"content":"` + strings.Repeat("汉", 50) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

// TestSendMessage_DefaultLimitFromConfig_RejectsOversized closes the loop
// config resolution → handler behavior: a config with no messages block
// resolves the default 5000 via MaxInputTokensLimit, and a handler wired with
// that value rejects a >5000-token input.
func TestSendMessage_DefaultLimitFromConfig_RejectsOversized(t *testing.T) {
	env := newSessionHandlerEnv(t, &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{stream.TokenEvent(stream.AgentConfucius, "must not run")},
	})
	// The zero-value config carries no messages block — the production shape
	// when the operator omits it.
	cfg := &config.Config{}
	r := newLimitTestEngine(t, env, env.stub, cfg.Messages.MaxInputTokensLimit())

	// 5001 CJK runes → estimate 5001 > default 5000.
	body := `{"content":"` + strings.Repeat("汉", 5001) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "CONTENT_TOO_LONG")
	assert.Contains(t, w.Body.String(), "5000-token")
}

// TestSendMessage_OversizedBody_413 verifies the 1MB request-body backstop:
// an oversized body is rejected 413 REQUEST_TOO_LARGE during parsing, before
// any storage or orchestrator work.
func TestSendMessage_OversizedBody_413(t *testing.T) {
	env := newSessionHandlerEnv(t, &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{stream.TokenEvent(stream.AgentConfucius, "must not run")},
	})
	// The byte cap is independent of the token limit — keep the token limit
	// high so the ONLY trigger here is body size.
	r := newLimitTestEngine(t, env, env.stub, 10_000_000)

	// ASCII filler well past 1MB (token limit raised so it cannot fire first).
	body := `{"content":"` + strings.Repeat("a", maxMessageBodyBytes+2048) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "REQUEST_TOO_LARGE", resp.Error.Code)

	env.mysql.mu.Lock()
	assert.Zero(t, env.mysql.appendMessagesCalls, "oversized body must not persist")
	env.mysql.mu.Unlock()
	assert.Zero(t, env.redis.dualCount(), "oversized body must not reach the dual-write path")
	env.stub.mu.Lock()
	assert.Empty(t, env.stub.gotMessages, "orchestrator must not run for an oversized body")
	env.stub.mu.Unlock()
}

// TestSendMessage_SessionNotFound_404 verifies that posting a message to a
// non-existent session returns 404 and never invokes the orchestrator.
func TestSendMessage_SessionNotFound_404(t *testing.T) {
	stub := &stubOrchestrator{eventsToEmit: []stream.StreamEvent{stream.TokenEvent(stream.AgentConfucius, "should not run")}}
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
	assert.Nil(t, env.stub.gotMessages, "orchestrator must NOT be called when session not found")
	assert.Zero(t, env.redis.dualCount(), "a 404 request must not write any message row — not even the send-time user row")
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

// TestGetSession_ReturnsDetailFields verifies the single-session read returns
// the list-entry superset — session_id, title, create_time, update_time
// (RFC3339 UTC) — and, with no run store wired, generating=false and no
// run_id key.
func TestGetSession_ReturnsDetailFields(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	ts := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	env.mysql.getSessionByIDFound = &model.Session{
		SessionID:  "sess-1",
		UserID:     "user-1",
		CreateTime: ts,
		UpdateTime: ts,
	}
	env.mysql.getTitleFound = &model.Title{SessionID: "sess-1", Title: "调研纪要"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		SessionID  string `json:"session_id"`
		Title      string `json:"title"`
		CreateTime string `json:"create_time"`
		UpdateTime string `json:"update_time"`
		Generating bool   `json:"generating"`
		RunID      string `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "sess-1", resp.SessionID)
	assert.Equal(t, "调研纪要", resp.Title)
	assert.Equal(t, "2026-08-21T12:00:00Z", resp.CreateTime)
	assert.Equal(t, "2026-08-21T12:00:00Z", resp.UpdateTime)
	assert.False(t, resp.Generating)
	assert.Empty(t, resp.RunID, "run_id must be omitted when not generating")
	assert.NotContains(t, w.Body.String(), `"run_id"`, "run_id key must be absent entirely (omitempty)")
}

// TestGetSession_ActiveRunMarkedGenerating verifies a session with an active
// run claim reports generating=true and carries the run id (the reload
// discovery path), mirroring the list entry.
func TestGetSession_ActiveRunMarkedGenerating(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)

	store := run.NewMemStore()
	_, ok, err := store.ClaimSession(context.Background(), "sess-1", "run-9")
	require.NoError(t, err)
	require.True(t, ok, "claim must succeed on an idle session")

	h := NewSessionHandler(
		newSessionSvc(sessionDeps(env.mysql, env.redis, env.fs)),
		service.NewTitleService(nil, env.mysql, config.OpenAIConfig{TitleModel: "title-model"}),
		store,
	)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.GET("/api/v1/sessions/:session_id", h.GetSession)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Generating bool   `json:"generating"`
		RunID      string `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Generating)
	assert.Equal(t, "run-9", resp.RunID)
}

// TestGetSession_WrongOwner_404 verifies a session owned by another user is a
// 404 (existence never disclosed), matching the other session endpoints.
func TestGetSession_WrongOwner_404(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "sess-1", UserID: "someone-else"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestGetSession_Missing_404 verifies an unknown session_id is a 404.
func TestGetSession_Missing_404(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionByIDFound = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/nope", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestGetSession_TitleError_DegradesToEmptyTitle verifies a title lookup
// failure degrades to an empty title instead of failing the detail read.
func TestGetSession_TitleError_DegradesToEmptyTitle(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getTitleErr = errors.New("titles table unavailable")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Title string `json:"title"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Title)
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

// TestSendMessage_FirstTurnFiresTitle verifies that the first user message
// (n=1) fires title generation at SEND time — in parallel with the turn, not
// after it. The env's default TitleService has a nil LLM, so we configure one
// with a fake LLM and assert the title is upserted.
func TestSendMessage_FirstTurnFiresTitle(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	// Wire a real TitleService backed by a fake LLM and the env's MySQL fake.
	deps := sessionDeps(env.mysql, env.redis, env.fs)
	env.stream.titleSvc = newTitleSvcWithFake(t, deps, "Generated Title")

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

// TestSendMessage_TitleCadence verifies the user-message-ordinal throttle
// (title-generation-cadence): n = persisted prior user rows + 1, generation
// fires iff n % 3 == 1 — the 1st message always, then the 4th, 7th, ... All
// other ordinals (2, 3, 5, ...) never fire.
func TestSendMessage_TitleCadence(t *testing.T) {
	cases := []struct {
		name       string
		priorUsers int // seeded persisted user rows before the request
		fires      bool
	}{
		{"n=1 first message fires", 0, true},
		{"n=2 does not fire", 1, false},
		{"n=3 does not fire", 2, false},
		{"n=4 fires", 3, true},
		{"n=5 does not fire", 4, false},
		{"n=7 fires", 6, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSessionHandlerEnv(t, nil)
			// Seed the Redis read tier so RecoverMessages returns the prior
			// user rows the ordinal count is derived from.
			raws := make([][]byte, 0, tc.priorUsers)
			for i := 0; i < tc.priorUsers; i++ {
				raw, err := json.Marshal(model.Message{
					SessionID: "sess-1",
					Agent:     model.AgentUser,
					MsgIndex:  0,
					Role:      model.RoleUser,
					EventType: model.EventTypeMessage,
					Content:   fmt.Sprintf("prior user message %d", i+1),
				})
				require.NoError(t, err)
				raws = append(raws, raw)
			}
			env.redis.getResult = raws

			env.stream.titleSvc = newTitleSvcWithFake(t, sessionDeps(env.mysql, env.redis, env.fs), "should not be used")

			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/sessions/sess-1/messages",
				strings.NewReader(`{"content":"current message"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

			if tc.fires {
				require.Eventually(t, func() bool {
					env.mysql.mu.Lock()
					defer env.mysql.mu.Unlock()
					return env.mysql.upsertTitleCalls == 1
				}, time.Second, 10*time.Millisecond, "expected one title upsert at this ordinal")
			} else {
				// Give the would-be fire-and-forget goroutine plenty of time
				// it would NOT be scheduled; assert no upsert ever happened.
				time.Sleep(50 * time.Millisecond)
				env.mysql.mu.Lock()
				defer env.mysql.mu.Unlock()
				assert.Equal(t, 0, env.mysql.upsertTitleCalls, "title generation must NOT fire at this ordinal")
			}
		})
	}
}

// TestSendMessage_TitleInputIsFirstPlusCurrentMessage verifies the generation
// input shape: the session's FIRST user message plus the message triggering
// the generation (n=4 here), never assistant content.
func TestSendMessage_TitleInputIsFirstPlusCurrentMessage(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	// Three prior user rows → the request is n=4, which fires; the first
	// anchor must come from the FIRST prior row, not the latest one.
	raws := make([][]byte, 0, 3)
	for i := 0; i < 3; i++ {
		raw, err := json.Marshal(model.Message{
			SessionID: "sess-1",
			Agent:     model.AgentUser,
			MsgIndex:  0,
			Role:      model.RoleUser,
			EventType: model.EventTypeMessage,
			Content:   fmt.Sprintf("prior question %d", i+1),
		})
		require.NoError(t, err)
		raws = append(raws, raw)
	}
	env.redis.getResult = raws

	llm := &fakeTitleLLM{resp: agent.LLMResponse{Content: "Refreshed Title"}}
	env.stream.titleSvc = service.NewTitleService(llm, env.mysql, config.OpenAIConfig{TitleModel: "title-model"})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"the fourth question"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		env.mysql.mu.Lock()
		defer env.mysql.mu.Unlock()
		return env.mysql.upsertTitleCalls == 1
	}, time.Second, 10*time.Millisecond, "expected the n=4 title upsert")

	content := llm.lastUserContent()
	assert.Contains(t, content, "prior question 1", "the FIRST user message must be an anchor")
	assert.Contains(t, content, "the fourth question", "the triggering message must be an anchor")
	assert.NotContains(t, content, "Assistant", "assistant content must not be part of the title input")
}

// TestSendMessage_SessionBusy_DoesNotFireTitle verifies that a request
// rejected with 409 SESSION_BUSY never triggers title generation (the trigger
// sits after the claim, and a busy session is not worth a wasted LLM call).
func TestSendMessage_SessionBusy_DoesNotFireTitle(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	deps := sessionDeps(env.mysql, env.redis, env.fs)

	// A run store with the session's slot already claimed by another run.
	store := run.NewMemStore()
	_, ok, err := store.ClaimSession(context.Background(), "sess-1", "run-holder")
	require.NoError(t, err)
	require.True(t, ok)

	stream := NewMessageStreamHandler(
		newSessionSvc(deps), newMessageSvc(deps),
		newTitleSvcWithFake(t, deps, "Busy Title"),
		nil, nil, env.stub, "/tmp/blowball-test-data",
		run.NewManager(store, run.NewRegistry()), testSelectionConfig(), 0)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	r.POST("/api/v1/sessions/:session_id/messages", stream.SendMessage)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"no title for you"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())

	time.Sleep(50 * time.Millisecond)
	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 0, env.mysql.upsertTitleCalls, "a 409-rejected request must not trigger title generation")
}

// TestSendMessage_SessionBusy_NoPersistence verifies the zero-write invariant
// of the send-time user-row persistence (send-time-user-message-persistence,
// "Rejected requests write nothing" scenario): the write sits AFTER the
// session-run claim, so a request rejected with 409 SESSION_BUSY — every
// 400/404 rejection likewise returns before it — persists nothing at all.
func TestSendMessage_SessionBusy_NoPersistence(t *testing.T) {
	env := newSessionHandlerEnv(t, &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{stream.TokenEvent(stream.AgentConfucius, "must not run")},
	})

	// The session's run slot is already claimed by another run.
	store := run.NewMemStore()
	_, ok, err := store.ClaimSession(context.Background(), "sess-1", "run-holder")
	require.NoError(t, err)
	require.True(t, ok)
	env.stream.runs = run.NewManager(store, run.NewRegistry())

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-1/messages",
		strings.NewReader(`{"content":"busy"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())

	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, env.redis.dualCount(), "a 409-rejected request must not write any message row — not even the send-time user row")
	env.mysql.mu.Lock()
	assert.Zero(t, env.mysql.appendMessagesCalls, "no fallback direct write either")
	env.mysql.mu.Unlock()
	env.stub.mu.Lock()
	assert.Nil(t, env.stub.gotMessages, "orchestrator must not run")
	env.stub.mu.Unlock()
}

// TestSendMessage_ProducesDeterministicSSESequence asserts the exact SSE byte
// sequence a client sees for a direct-answer turn. Phase 12 integration tests
// can copy this assertion verbatim.
func TestSendMessage_ProducesDeterministicSSESequence(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "Hello"),
			stream.AgentEndEvent(stream.AgentConfucius),
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
	// Since turn-detach-resume every frame also carries an id line (the run
	// event log's stream entry id, echoed back by clients as Last-Event-ID).
	blocks := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n\n")
	require.Len(t, blocks, 4, "expected exactly 4 SSE events")

	types := make([]string, 0, 4)
	prevID := 0
	for _, b := range blocks {
		lines := strings.Split(b, "\n")
		require.Len(t, lines, 3, "expected id + event + data line per block; got %q", b)
		require.True(t, strings.HasPrefix(lines[0], "id: "), "block %q", b)
		require.True(t, strings.HasPrefix(lines[1], "event: "), "block %q", b)
		require.True(t, strings.HasPrefix(lines[2], "data: "), "block %q", b)
		// Stream entry ids are strictly increasing cursors ("<seq>-1" here).
		id := strings.TrimSuffix(strings.TrimPrefix(lines[0], "id: "), "-1")
		seq := 0
		_, err := fmt.Sscanf(id, "%d", &seq)
		require.NoError(t, err, "block %q", b)
		require.Greater(t, seq, prevID, "ids must strictly increase: %q", b)
		prevID = seq
		types = append(types, strings.TrimPrefix(lines[1], "event: "))
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
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "Thinking"),
			stream.ToolCallEvent(stream.AgentConfucius, "tc-1", "invoke_chongzhi", map[string]any{"task": "compute"}),
			stream.AgentStartEvent(stream.AgentChongzhi),
			stream.TokenEvent(stream.AgentChongzhi, "42"),
			stream.AgentEndEvent(stream.AgentChongzhi),
			stream.AgentEndEvent(stream.AgentConfucius),
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
		return env.redis.dualCount() == 2
	}, time.Second, 10*time.Millisecond, "expected send-time batch + turn-end batch")

	rows := env.redis.dualSnapshot()

	// Concatenated batches: 1 user message + 7 assistant events, done excluded.
	require.Len(t, rows, 8)

	// User message comes first (from the send-time batch).
	userMsg := rows[0]
	assert.Equal(t, model.AgentUser, userMsg.Agent)
	assert.Equal(t, model.EventTypeMessage, userMsg.EventType)
	assert.Equal(t, model.RoleUser, userMsg.Role)
	assert.Equal(t, 0, userMsg.MsgIndex)
	assert.Equal(t, "do it", userMsg.Content)

	// Assistant event stream: 7 events, done excluded.
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
		assert.Equal(t, want, rows[i+1].EventType, "event %d", i)
	}

	// Token/tool_call rows carry the assistant role; markers have empty role.
	for _, m := range rows[1:] {
		switch m.EventType {
		case model.EventTypeToken, model.EventTypeToolCall:
			assert.Equal(t, model.RoleAssistant, m.Role, "event_type=%s", m.EventType)
		default:
			assert.Empty(t, m.Role, "event_type=%s", m.EventType)
		}
	}

	// Agent column is preserved from the event.
	assert.Equal(t, stream.AgentConfucius, rows[1].Agent)
	assert.Equal(t, stream.AgentChongzhi, rows[5].Agent)

	// Tool_call content is JSON with name and args.
	toolMsg := rows[3]
	require.Equal(t, model.EventTypeToolCall, toolMsg.EventType)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(toolMsg.Content), &payload))
	assert.Equal(t, "invoke_chongzhi", payload["name"])
}

// TestSendMessage_OrchestratorFailure_PersistsPartialTurn verifies that when
// the orchestrator returns a non-cancellation error (e.g. a model-provider
// 429/5xx) after streaming some events, the user message and the partial
// assistant events ARE persisted in a single batch so the session history
// matches what the client saw.
func TestSendMessage_OrchestratorFailure_PersistsPartialTurn(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "oops"),
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

	// The send-time batch plus the partial turn-end batch must land despite
	// the failure.
	require.Eventually(t, func() bool {
		return env.redis.dualCount() == 2
	}, time.Second, 10*time.Millisecond, "expected send-time + turn-end batches for failed turn")

	rows := env.redis.dualSnapshot()

	// 1 user message + 2 merged assistant events (agent_start, token).
	require.Len(t, rows, 3, "batches must contain user + partial assistant events")
	userMsg := rows[0]
	assert.Equal(t, model.AgentUser, userMsg.Agent)
	assert.Equal(t, model.EventTypeMessage, userMsg.EventType)
	assert.Equal(t, model.RoleUser, userMsg.Role)
	assert.Equal(t, "hi", userMsg.Content)

	wantTypes := []string{
		model.EventTypeAgentStart,
		model.EventTypeToken,
	}
	for i, want := range wantTypes {
		assert.Equal(t, want, rows[i+1].EventType, "assistant event %d", i)
	}
	assert.Equal(t, "oops", rows[2].Content, "partial token should be persisted")
}

// TestSendMessage_ContextCanceled_PersistsUserAndPartialEvents verifies that a
// client-initiated cancellation (context.Canceled) does not discard the turn.
// The user message and any assistant events emitted before cancellation are
// persisted in a single batch.
func TestSendMessage_ContextCanceled_PersistsUserAndPartialEvents(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "partial "),
			stream.TokenEvent(stream.AgentConfucius, "reply"),
			stream.AgentEndEvent(stream.AgentConfucius),
		},
		returnErr: context.Canceled,
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-cancel/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// The send-time batch plus the partial turn-end batch must land despite
	// the cancellation.
	require.Eventually(t, func() bool {
		return env.redis.dualCount() == 2
	}, time.Second, 10*time.Millisecond, "expected send-time + turn-end batches for interrupted turn")

	rows := env.redis.dualSnapshot()

	// 1 user message + 3 merged assistant events (agent_start, token, agent_end).
	require.Len(t, rows, 4, "batches must contain user + partial assistant events")
	userMsg := rows[0]
	assert.Equal(t, model.AgentUser, userMsg.Agent)
	assert.Equal(t, model.EventTypeMessage, userMsg.EventType)
	assert.Equal(t, model.RoleUser, userMsg.Role)
	assert.Equal(t, "hi", userMsg.Content)

	wantTypes := []string{
		model.EventTypeAgentStart,
		model.EventTypeToken,
		model.EventTypeAgentEnd,
	}
	for i, want := range wantTypes {
		assert.Equal(t, want, rows[i+1].EventType, "assistant event %d", i)
	}
	assert.Equal(t, "partial reply", rows[2].Content, "partial tokens should be merged")
}

// TestSendMessage_ContextCanceled_NoAssistantEvents_PersistsOnlyUser verifies
// that when cancellation happens before the assistant emits any event, only the
// user message is persisted — here as the send-time batch ALONE: the turn-end
// render produces an empty suffix (user row gated out, no events), so no
// second dual write ever happens.
func TestSendMessage_ContextCanceled_NoAssistantEvents_PersistsOnlyUser(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{},
		returnErr:    context.Canceled,
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-cancel-empty/messages",
		strings.NewReader(`{"content":"hello?"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		return env.redis.dualCount() == 1
	}, time.Second, 10*time.Millisecond, "expected the send-time batch for the user-only interrupted turn")

	// The async turn-end save runs after the response; give it the time it
	// would use to (wrongly) re-emit the user row, then assert it never did.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 1, env.redis.dualCount(), "the empty turn-end suffix must not produce another dual write")

	rows := env.redis.dualSnapshot()
	require.Len(t, rows, 1, "only the user message should be persisted")
	assert.Equal(t, model.RoleUser, rows[0].Role)
	assert.Equal(t, "hello?", rows[0].Content)
}

// TestSendMessage_ContextCanceled_FirstTurnGeneratesTitle verifies that a
// cancelled first turn still ends up with a title: generation fired at SEND
// time (before the turn started), so the interruption path neither needs nor
// performs any extra triggering, and the partial assistant content never
// enters the title input (title-generation-cadence).
func TestSendMessage_ContextCanceled_FirstTurnGeneratesTitle(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "partial title content"),
			stream.AgentEndEvent(stream.AgentConfucius),
		},
		returnErr: context.Canceled,
	}
	env.stream.orch = stub
	// Env constructor already built the engine with the original stub; rebuild
	// the route so the new orchestrator is wired in.
	env.engine = gin.New()
	env.engine.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-1")
		c.Next()
	})
	env.engine.POST("/api/v1/sessions/:session_id/messages", env.stream.SendMessage)

	deps := sessionDeps(env.mysql, env.redis, env.fs)
	env.stream.titleSvc = newTitleSvcWithFake(t, deps, "Canceled Title")

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-cancel-title/messages",
		strings.NewReader(`{"content":"first interrupted"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		env.mysql.mu.Lock()
		defer env.mysql.mu.Unlock()
		return env.mysql.upsertTitleCalls == 1
	}, time.Second, 10*time.Millisecond, "expected title generation on interrupted first turn")

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, "sess-cancel-title", env.mysql.upsertTitleArg.SessionID)
	assert.Equal(t, "Canceled Title", env.mysql.upsertTitleArg.Title)
}

// TestSendMessage_OrchestratorFailure_NonCancellation_PersistsUserAndPartialEvents
// verifies that a non-cancellation orchestrator error (e.g. a model-provider
// 429) returned after several events have been streamed results in the
// send-time user batch plus a turn-end batch carrying the merged partial
// assistant events.
func TestSendMessage_OrchestratorFailure_NonCancellation_PersistsUserAndPartialEvents(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "partial "),
			stream.TokenEvent(stream.AgentConfucius, "reply"),
			stream.AgentEndEvent(stream.AgentConfucius),
		},
		returnErr: errors.New("confucius: stream chat: 429 Too Many Requests"),
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-fail-partial/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		return env.redis.dualCount() == 2
	}, time.Second, 10*time.Millisecond, "expected send-time + turn-end batches for failed turn")

	rows := env.redis.dualSnapshot()

	// 1 user message + 3 merged assistant events (agent_start, token, agent_end).
	require.Len(t, rows, 4, "batches must contain user + partial assistant events")
	assert.Equal(t, model.RoleUser, rows[0].Role)
	assert.Equal(t, "hi", rows[0].Content)

	wantTypes := []string{
		model.EventTypeAgentStart,
		model.EventTypeToken,
		model.EventTypeAgentEnd,
	}
	for i, want := range wantTypes {
		assert.Equal(t, want, rows[i+1].EventType, "assistant event %d", i)
	}
	assert.Equal(t, "partial reply", rows[2].Content, "partial tokens should be merged")
}

// TestSendMessage_OrchestratorFailure_NoAssistantEvents_PersistsOnlyUser
// verifies that a non-cancellation orchestrator error returned before any
// assistant event still persists the user message (never silently lost) — as
// the send-time batch alone, with the turn-end suffix empty.
func TestSendMessage_OrchestratorFailure_NoAssistantEvents_PersistsOnlyUser(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{},
		returnErr:    errors.New("confucius: stream chat: 500 Internal Server Error"),
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-fail-empty/messages",
		strings.NewReader(`{"content":"hello?"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		return env.redis.dualCount() == 1
	}, time.Second, 10*time.Millisecond, "expected the send-time batch for the user-only failed turn")

	// The async turn-end save runs after the response; give it the time it
	// would use to (wrongly) re-emit the user row, then assert it never did.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 1, env.redis.dualCount(), "the empty turn-end suffix must not produce another dual write")

	rows := env.redis.dualSnapshot()
	require.Len(t, rows, 1, "only the user message should be persisted")
	assert.Equal(t, model.RoleUser, rows[0].Role)
	assert.Equal(t, "hello?", rows[0].Content)
}

// TestSendMessage_OrchestratorFailure_FirstTurnGeneratesTitle verifies that a
// non-cancellation orchestrator error on a first turn still ends up with a
// title: generation fired at send time, so the failure path performs no extra
// triggering and the title is unaffected by the turn's outcome.
func TestSendMessage_OrchestratorFailure_FirstTurnGeneratesTitle(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "partial title content"),
			stream.AgentEndEvent(stream.AgentConfucius),
		},
		returnErr: errors.New("confucius: stream chat: 429 Too Many Requests"),
	}
	env := newSessionHandlerEnv(t, stub)
	deps := sessionDeps(env.mysql, env.redis, env.fs)
	env.stream.titleSvc = newTitleSvcWithFake(t, deps, "Failure Title")

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-fail-title/messages",
		strings.NewReader(`{"content":"first failed"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		env.mysql.mu.Lock()
		defer env.mysql.mu.Unlock()
		return env.mysql.upsertTitleCalls == 1
	}, time.Second, 10*time.Millisecond, "expected title generation on failed first turn")

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, "sess-fail-title", env.mysql.upsertTitleArg.SessionID)
	assert.Equal(t, "Failure Title", env.mysql.upsertTitleArg.Title)
}

// TestSendMessage_OrchestratorFailure_RecordsTurnUsage verifies that a failed
// turn whose done event carries usage still records its token cost in
// turn_usage (the orchestrator emits done with usage on the error path too).
func TestSendMessage_OrchestratorFailure_RecordsTurnUsage(t *testing.T) {
	stub := &stubOrchestrator{
		eventsToEmit: []stream.StreamEvent{
			stream.AgentStartEvent(stream.AgentConfucius),
			stream.TokenEvent(stream.AgentConfucius, "partial"),
			stream.AgentEndEvent(stream.AgentConfucius),
			stream.DoneEvent(map[string]any{
				"total":    map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
				"by_agent": map[string]any{"Confucius": map[string]any{"total_tokens": 120}},
				"meta":     map[string]any{"sub_agent_invocations": []string{}, "parallel": false},
				"error":    "confucius: stream chat: 429 Too Many Requests",
			}),
		},
		returnErr: errors.New("confucius: stream chat: 429 Too Many Requests"),
	}
	env := newSessionHandlerEnv(t, stub)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/sess-fail-usage/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		env.mysql.mu.Lock()
		defer env.mysql.mu.Unlock()
		return env.mysql.saveTurnUsageCalls == 1
	}, time.Second, 10*time.Millisecond, "expected turn_usage recorded for failed turn")

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, "sess-fail-usage", env.mysql.saveTurnUsageArg.SessionID)
	assert.Equal(t, 120, env.mysql.saveTurnUsageArg.TotalTokens, "total_tokens should be copied from usage.total")
	// The error field rides along inside the serialized usage JSON.
	assert.Contains(t, env.mysql.saveTurnUsageArg.UsageJSON, "429 Too Many Requests")
}

// TestDeleteSession_Success_204 verifies the owner deleting their own session
// returns 204 and drives the service purge exactly once.
func TestDeleteSession_Success_204(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 1, env.mysql.deleteSessionCalls, "MySQL purge must run once")
	assert.Equal(t, "sess-1", env.mysql.deleteSessionArg)
}

// TestDeleteSession_SessionMissing_404 verifies a non-existent session returns
// 404 and never reaches the purge.
func TestDeleteSession_SessionMissing_404(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionByIDFound = nil

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-1", nil)
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

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 0, env.mysql.deleteSessionCalls)
}

// TestDeleteSession_WrongOwner_404 verifies deleting another user's session is
// reported as not-found (no existence leak) and is not purged.
func TestDeleteSession_WrongOwner_404(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "sess-1", UserID: "other-user"}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 0, env.mysql.deleteSessionCalls)
}

// TestDeleteSession_LookupError_500 verifies a store error during the ownership
// lookup surfaces as 500 (not 404).
func TestDeleteSession_LookupError_500(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionIDErr = errors.New("db down")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 0, env.mysql.deleteSessionCalls)
}

// TestUpdateTitle_Success verifies the owner can set a manual title and the
// response echoes the sanitized title plus the refreshed update_time.
func TestUpdateTitle_Success(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/sess-1",
		strings.NewReader(`{"title":"  New Title  "}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp updateTitleResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "sess-1", resp.SessionID)
	assert.Equal(t, "New Title", resp.Title)
	_, err := time.Parse(time.RFC3339, resp.UpdateTime)
	assert.NoError(t, err)

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 1, env.mysql.upsertTitleCalls)
	assert.Equal(t, "New Title", env.mysql.upsertTitleArg.Title)
	assert.True(t, env.mysql.upsertTitleArg.IsManual)
}

// TestUpdateTitle_TruncatesLongTitle verifies titles longer than 20 runes are
// stored truncated and the truncated value is returned.
func TestUpdateTitle_TruncatesLongTitle(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/sess-1",
		strings.NewReader(`{"title":"`+strings.Repeat("x", 50)+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp updateTitleResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, strings.Repeat("x", 20), resp.Title)
}

// TestUpdateTitle_EmptyTitle_400 verifies whitespace-only titles are rejected.
func TestUpdateTitle_EmptyTitle_400(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/sess-1",
		strings.NewReader(`{"title":"   "}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "BAD_REQUEST", body.Error.Code)
}

// TestUpdateTitle_SessionNotFound_404 verifies updating a missing or unowned
// session returns 404 without leaking existence.
func TestUpdateTitle_SessionNotFound_404(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionByIDFound = nil

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/sess-missing",
		strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "NOT_FOUND", body.Error.Code)

	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 0, env.mysql.upsertTitleCalls)
}

// TestUpdateTitle_WrongOwner_404 verifies a user cannot update another user's
// session title.
func TestUpdateTitle_WrongOwner_404(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionByIDFound = &model.Session{SessionID: "sess-1", UserID: "other-user"}

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/sess-1",
		strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 0, env.mysql.upsertTitleCalls)
}

// TestUpdateTitle_LookupError_500 verifies a store error during ownership
// lookup surfaces as 500 (not 404).
func TestUpdateTitle_LookupError_500(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.getSessionIDErr = errors.New("db down")

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/sessions/sess-1",
		strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
}

// TestDeleteSession_PurgeError_500 verifies a purge failure surfaces as 500.
func TestDeleteSession_PurgeError_500(t *testing.T) {
	env := newSessionHandlerEnv(t, nil)
	env.mysql.deleteSessionErr = errors.New("archive blew up")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-1", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	env.mysql.mu.Lock()
	defer env.mysql.mu.Unlock()
	assert.Equal(t, 1, env.mysql.deleteSessionCalls, "purge must be attempted before failing")
}
