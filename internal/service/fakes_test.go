package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	cursorpkg "github.com/lush/blowball/internal/pkg/cursor"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
)

// fakeMySQLStore is an in-memory MySQLStore for service tests. Each op records
// its invocation count and arguments so tests can assert the three-layer write
// behaviour and the fallback chain.
type fakeMySQLStore struct {
	mu sync.Mutex

	createSessionCalls   int
	createSessionSession model.Session
	createSessionErr     error

	getSessionByIDCalls int
	getSessionByIDFound *model.Session
	getSessionIDErr     error

	listSessionsWithTitleRows []mysqlstore.SessionWithTitle
	listSessionsWithTitleErr  error

	upsertTitleCalls int
	upsertTitleArg   model.Title
	upsertTitleErr   error

	getTitleCalls int
	getTitleFound *model.Title
	getTitleErr   error

	appendMessageCalls int
	appendMessageArg   model.Message
	appendMessageID    int64
	appendMessageErr   error

	appendMessagesCalls int
	appendMessagesArg   []model.Message
	appendMessagesIDs   []int64
	appendMessagesErr   error

	listMessagesRows []model.Message
	listMessagesErr  error
}

func (f *fakeMySQLStore) CreateSession(_ context.Context, sess model.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createSessionCalls++
	f.createSessionSession = sess
	return f.createSessionErr
}

func (f *fakeMySQLStore) GetSessionByID(_ context.Context, sessionID string) (*model.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getSessionByIDCalls++
	if f.getSessionIDErr != nil {
		return nil, f.getSessionIDErr
	}
	if f.getSessionByIDFound == nil {
		return nil, nil
	}
	cp := *f.getSessionByIDFound
	return &cp, nil
}

func (f *fakeMySQLStore) ListSessionsWithTitle(_ context.Context, userID string) ([]mysqlstore.SessionWithTitle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listSessionsWithTitleErr != nil {
		return nil, f.listSessionsWithTitleErr
	}
	out := make([]mysqlstore.SessionWithTitle, len(f.listSessionsWithTitleRows))
	copy(out, f.listSessionsWithTitleRows)
	return out, nil
}

func (f *fakeMySQLStore) UpsertTitle(_ context.Context, t model.Title) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upsertTitleCalls++
	f.upsertTitleArg = t
	return f.upsertTitleErr
}

func (f *fakeMySQLStore) GetTitle(_ context.Context, sessionID string) (*model.Title, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getTitleCalls++
	if f.getTitleErr != nil {
		return nil, f.getTitleErr
	}
	if f.getTitleFound == nil {
		return nil, nil
	}
	cp := *f.getTitleFound
	return &cp, nil
}

func (f *fakeMySQLStore) AppendMessage(_ context.Context, m model.Message) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendMessageCalls++
	f.appendMessageArg = m
	if f.appendMessageErr != nil {
		return 0, f.appendMessageErr
	}
	return f.appendMessageID, nil
}

func (f *fakeMySQLStore) AppendMessages(_ context.Context, msgs []model.Message) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendMessagesCalls++
	f.appendMessagesArg = msgs
	if f.appendMessagesErr != nil {
		return nil, f.appendMessagesErr
	}
	return f.appendMessagesIDs, nil
}

func (f *fakeMySQLStore) ListMessages(_ context.Context, sessionID string) ([]model.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listMessagesErr != nil {
		return nil, f.listMessagesErr
	}
	out := make([]model.Message, len(f.listMessagesRows))
	copy(out, f.listMessagesRows)
	return out, nil
}

func (f *fakeMySQLStore) ListMessagesPaged(_ context.Context, sessionID, cursor string, pageSize int, order string) ([]model.Message, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listMessagesErr != nil {
		return nil, "", f.listMessagesErr
	}
	rows := make([]model.Message, len(f.listMessagesRows))
	copy(rows, f.listMessagesRows)

	// Default stable ascending sort matching MySQL.
	if order == "desc" {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	} else {
		less := func(i, j int) bool {
			if rows[i].MsgTime.Equal(rows[j].MsgTime) {
				if rows[i].MsgIndex == rows[j].MsgIndex {
					return rows[i].ID < rows[j].ID
				}
				return rows[i].MsgIndex < rows[j].MsgIndex
			}
			return rows[i].MsgTime.Before(rows[j].MsgTime)
		}
		// Bubble sort is fine for test data.
		for i := 0; i < len(rows); i++ {
			for j := i + 1; j < len(rows); j++ {
				if !less(i, j) {
					rows[i], rows[j] = rows[j], rows[i]
				}
			}
		}
	}

	start := 0
	if cursor != "" {
		// Find position immediately after the cursor row (matched by id).
		for i, m := range rows {
			enc, err := cursorpkg.Encode(cursorpkg.Cursor{MsgTime: m.MsgTime, MsgIndex: m.MsgIndex, ID: m.ID})
			if err != nil {
				return nil, "", err
			}
			if enc == cursor {
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

// fakeRedisStore records AppendMessage/GetMessages/SetMessages calls.
type fakeRedisStore struct {
	mu sync.Mutex

	appendCalls int
	appendArg   []byte
	appendErr   error

	appendMessagesCalls int
	appendMessagesArgs  [][]byte
	appendMessagesErr   error

	getCalls  int
	getResult [][]byte
	getErr    error

	setCalls int
	setArgs  [][]byte
	setErr   error
}

func (f *fakeRedisStore) AppendMessage(_ context.Context, sessionID string, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCalls++
	f.appendArg = append([]byte(nil), raw...)
	return f.appendErr
}

func (f *fakeRedisStore) AppendMessages(_ context.Context, sessionID string, raws [][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendMessagesCalls++
	f.appendMessagesArgs = make([][]byte, len(raws))
	for i, b := range raws {
		f.appendMessagesArgs[i] = append([]byte(nil), b...)
	}
	return f.appendMessagesErr
}

func (f *fakeRedisStore) GetMessages(_ context.Context, sessionID string) ([][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := make([][]byte, len(f.getResult))
	for i, b := range f.getResult {
		out[i] = append([]byte(nil), b...)
	}
	return out, nil
}

func (f *fakeRedisStore) SetMessages(_ context.Context, sessionID string, raws [][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.setArgs = make([][]byte, len(raws))
	for i, b := range raws {
		f.setArgs[i] = append([]byte(nil), b...)
	}
	return f.setErr
}

// fakeFSStore records WriteSession/ReadSession/DeleteSession/EnsureUserDirs.
type fakeFSStore struct {
	mu sync.Mutex

	writeCalls int
	writeData  []byte
	writeErr   error

	readResult []byte
	readErr    error

	deleteCalls int
	deleteErr   error

	ensureCalls int
	ensureErr   error
}

func (f *fakeFSStore) WriteSession(_ context.Context, userID, sessionID string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeCalls++
	f.writeData = append([]byte(nil), data...)
	return f.writeErr
}

func (f *fakeFSStore) ReadSession(_ context.Context, userID, sessionID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.readResult == nil {
		return nil, nil
	}
	return append([]byte(nil), f.readResult...), nil
}

func (f *fakeFSStore) DeleteSession(_ context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	return f.deleteErr
}

func (f *fakeFSStore) EnsureUserDirs(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	return f.ensureErr
}

// fakeLLMClient is an agent.LLMClient for service tests. It returns a
// preconfigured response (and optional error) and remembers the last request.
type fakeLLMClient struct {
	mu       sync.Mutex
	lastReq  agent.LLMRequest
	gotCall  bool
	resp     agent.LLMResponse
	err      error
	onTokens []string
}

func (c *fakeLLMClient) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error, onReasoning func(string) error) (agent.LLMResponse, error) {
	c.mu.Lock()
	c.lastReq = req
	c.gotCall = true
	c.mu.Unlock()
	if c.err != nil {
		return agent.LLMResponse{}, c.err
	}
	if onToken != nil {
		for _, t := range c.onTokens {
			_ = onToken(t)
		}
	}
	return c.resp, nil
}

// newDeps builds a SessionDeps with the supplied fakes.
func newDeps(m *fakeMySQLStore, r *fakeRedisStore, f *fakeFSStore) SessionDeps {
	return SessionDeps{MySQL: m, Redis: r, FS: f}
}

// sampleMessage is a stable message used across tests.
func sampleMessage(sessionID, content string) model.Message {
	return model.Message{
		ID:        1,
		SessionID: sessionID,
		MsgTime:   time.Unix(1_700_000_000, 0).UTC(),
		Agent:     model.AgentUser,
		MsgIndex:  0,
		Role:      model.RoleUser,
		EventType: model.EventTypeMessage,
		Content:   content,
		TraceID:   "trace-1",
	}
}

var errFake = fmt.Errorf("fake store error")
