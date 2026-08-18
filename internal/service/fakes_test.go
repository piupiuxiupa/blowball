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

	deleteSessionCalls int
	deleteSessionArg   string
	deleteSessionErr   error

	listSessionsWithTitleRows []mysqlstore.SessionWithTitle
	listSessionsWithTitleErr  error

	upsertTitleCalls int
	upsertTitleArg   model.Title
	upsertTitleErr   error

	getTitleCalls int
	getTitleFound *model.Title
	getTitleErr   error

	appendMessagesCalls int
	appendMessagesArg   []model.Message
	appendMessagesIDs   []int64
	appendMessagesErr   error

	saveTurnUsageCalls int
	saveTurnUsageArg   model.TurnUsage
	saveTurnUsageErr   error

	// Compaction-storage recording (context-compaction capability).
	compactions                 []model.ContextCompaction
	insertCompactionCalls       int
	insertCompactionErr         error
	latestCompactionErr         error
	updateSessionCompactedCalls int
	updateSessionCompactedErr   error
	latestContextTokens         int

	updateSessionTimeCalls int
	updateSessionTimeArg   string
	updateSessionTimeErr   error

	// order (when non-nil) records the call sequence of delete-path
	// operations so tests can assert the drain → purge → cache-clear
	// ordering across fakes sharing one log.
	order *[]string

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

func (f *fakeMySQLStore) DeleteSession(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteSessionCalls++
	f.deleteSessionArg = sessionID
	if f.order != nil {
		*f.order = append(*f.order, "mysql.delete")
	}
	return f.deleteSessionErr
}

func (f *fakeMySQLStore) UpdateSessionTime(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateSessionTimeCalls++
	f.updateSessionTimeArg = sessionID
	return f.updateSessionTimeErr
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

func (f *fakeMySQLStore) UpsertTitleManual(_ context.Context, t model.Title) error {
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

func (f *fakeMySQLStore) AppendMessages(_ context.Context, msgs []model.Message) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendMessagesCalls++
	f.appendMessagesArg = msgs
	if f.order != nil {
		*f.order = append(*f.order, "mysql.append")
	}
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

// SaveTurnUsage records the turn_usage row handed to the store. It mirrors the
// real store's write-only path (no not-found handling).
func (f *fakeMySQLStore) SaveTurnUsage(_ context.Context, tu model.TurnUsage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveTurnUsageCalls++
	f.saveTurnUsageArg = tu
	return f.saveTurnUsageErr
}

// Compaction-storage members (context-compaction capability). compactions is
// the append-only record list; the error knobs simulate per-step failures so
// tests can assert the write-order degradation policy.
func (f *fakeMySQLStore) InsertCompaction(_ context.Context, rec model.ContextCompaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.insertCompactionCalls++
	if f.insertCompactionErr == nil {
		f.compactions = append(f.compactions, rec)
	}
	return f.insertCompactionErr
}

func (f *fakeMySQLStore) LatestCompaction(_ context.Context, _ string) (*model.ContextCompaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latestCompactionErr != nil {
		return nil, f.latestCompactionErr
	}
	if len(f.compactions) == 0 {
		return nil, nil
	}
	cp := f.compactions[len(f.compactions)-1]
	return &cp, nil
}

func (f *fakeMySQLStore) UpdateSessionCompacted(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateSessionCompactedCalls++
	return f.updateSessionCompactedErr
}

func (f *fakeMySQLStore) LatestContextTokens(_ context.Context, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latestContextTokens, nil
}

// fakeRedisStore records AppendMessagesDual/GetMessages/SetMessages/
// ClearMessages/DelSessionCache calls. Order entries feed the delete-path
// ordering assertions.
type fakeRedisStore struct {
	mu sync.Mutex

	dualCalls int
	dualSID   string
	dualArgs  [][]byte
	dualErr   error

	getCalls  int
	getResult [][]byte
	getErr    error

	setCalls int
	setArgs  [][]byte
	setErr   error

	clearCalls   int
	clearErr     error
	delSessCalls int
	delSessErr   error

	// Compaction-cache recording (context-compaction capability).
	compactionCache    []byte
	setCompactionCalls int
	setCompactionErr   error
	getCompactionCalls int
	getCompactionErr   error

	// order (when non-nil) records the delete-path call sequence; it may be
	// shared with the other fakes and the drain hook.
	order *[]string
}

func (f *fakeRedisStore) AppendMessagesDual(_ context.Context, sessionID string, raws [][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dualCalls++
	f.dualSID = sessionID
	f.dualArgs = make([][]byte, len(raws))
	for i, b := range raws {
		f.dualArgs[i] = append([]byte(nil), b...)
	}
	return f.dualErr
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

func (f *fakeRedisStore) ClearMessages(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearCalls++
	if f.order != nil {
		*f.order = append(*f.order, "redis.clear_msgs")
	}
	return f.clearErr
}

func (f *fakeRedisStore) DelSessionCache(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delSessCalls++
	if f.order != nil {
		*f.order = append(*f.order, "redis.del_session")
	}
	return f.delSessErr
}

// Compaction-cache members (context-compaction capability): a single-slot
// cache mirroring the whole-key overwrite semantics of the real store. A nil
// compactionCache is a miss.
func (f *fakeRedisStore) SetCompactionCache(_ context.Context, _ string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCompactionCalls++
	if f.setCompactionErr == nil {
		f.compactionCache = append([]byte(nil), data...)
	}
	return f.setCompactionErr
}

func (f *fakeRedisStore) GetCompactionCache(_ context.Context, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCompactionCalls++
	if f.getCompactionErr != nil {
		return nil, f.getCompactionErr
	}
	if f.compactionCache == nil {
		return nil, nil
	}
	return append([]byte(nil), f.compactionCache...), nil
}

func (f *fakeRedisStore) DelCompactionCache(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compactionCache = nil
	return nil
}

// fakeFSStore records EnsureUserDirs (the FS store's only remaining duty
// after the warm-tier removal).
type fakeFSStore struct {
	mu sync.Mutex

	ensureCalls int
	ensureErr   error
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

// newDeps builds a SessionDeps with the supplied fakes and no drain hook.
func newDeps(m *fakeMySQLStore, r *fakeRedisStore, f *fakeFSStore) SessionDeps {
	return SessionDeps{MySQL: m, Redis: r, FS: f}
}

// fakeDrain records write-behind drain invocations (the msgflush.Drain stand-in).
type fakeDrain struct {
	mu    sync.Mutex
	calls int
	err   error
	order *[]string // optional shared order log (mirrors the store fakes)
}

func (d *fakeDrain) drain(ctx context.Context) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	if d.order != nil {
		*d.order = append(*d.order, "drain")
	}
	return d.err
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
