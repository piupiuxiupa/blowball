// Package memory implements the cross-session long-term memory capability
// (cross-session-memory) on top of an external OpenViking server. v1 is
// auto-only: Recall runs at turn start (the result is injected into the
// turn's LLM context by the handler) and CaptureTurn at turn end (the server
// asynchronously extracts durable memories from the submitted exchange).
// There are no agent tools and no system-prompt threading — the agent layer
// is unaware memory exists. Every failure is best-effort by contract: memory
// must never block or fail a chat turn.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	openviking "github.com/volcengine/OpenViking/sdk/go"

	"github.com/lush/blowball/internal/config"
)

// Service is the process-wide memory facade, constructed once per agent-role
// process. A nil *Service means the capability is disabled; Enabled is
// nil-safe so handlers keep a single call shape for both states (the
// compSvc.Enabled precedent).
type Service struct {
	cfg config.MemoryConfig
	// http is the ONE *http.Client behind every per-user OpenViking client:
	// connection pooling and concurrency safety live here. Its Timeout is
	// deliberately 0 — deadlines are enforced per call with
	// context.WithTimeout because recall (turn-start path) and capture
	// (detached goroutine) carry very different budgets, and a shared client
	// timeout would force one budget onto both.
	http *http.Client

	mu sync.Mutex
	// clients caches one OpenViking client per OV user id. The client value
	// is a cheap header-bearing struct (identity is fixed at construction),
	// so the cache is unbounded by design: cardinality = user count, a few
	// strings per entry, no eviction (documented trade-off).
	clients map[string]*openviking.Client
	// opClient is the operator-scoped client (no X-OpenViking-User header)
	// used only by the startup Health probe.
	opClient *openviking.Client
}

// NewService builds the process-wide memory facade. It returns nil for a
// disabled config so callers hold the zero-wiring shape unchanged.
func NewService(cfg config.MemoryConfig) *Service {
	if !cfg.Enabled {
		return nil
	}
	return &Service{
		cfg:     cfg,
		http:    &http.Client{},
		clients: make(map[string]*openviking.Client),
	}
}

// Enabled reports whether the capability is wired in. Nil-safe.
func (s *Service) Enabled() bool { return s != nil }

// client returns (creating and caching) the per-user OpenViking client. The
// X-OpenViking-User header stamped at construction is the tenant identity
// the server scopes EVERY request — Find, session writes, commits — to, so
// per-user memory isolation holds by construction on every call that flows
// through here (the trusted-gateway model: one operator key + account for
// the whole deployment, identity per request).
func (s *Service) client(ovUser string) (*openviking.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[ovUser]; ok {
		return c, nil
	}
	c, err := openviking.NewClient(openviking.Config{
		BaseURL:    strings.TrimSpace(s.cfg.BaseURL),
		APIKey:     s.cfg.APIKey, // empty → no header: local no-auth server
		Account:    s.cfg.Account,
		User:       ovUser,
		HTTPClient: s.http,
	})
	if err != nil {
		return nil, fmt.Errorf("openviking client for user %q: %w", ovUser, err)
	}
	s.clients[ovUser] = c
	return c, nil
}

// ovSafeID is the OpenViking-safe identifier charset. Blowball mints UUID v7
// user and session ids, which pass through verbatim.
var ovSafeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ovUserID maps a blowball user_id onto the X-OpenViking-User tenant value.
// A non-conforming id (hand-minted row, import artifact) is mapped to a
// SHA-256 digest — deterministic and injective. Character REPLACEMENT would
// be wrong here: it can alias two distinct users onto one OpenViking tenant,
// which is a memory-isolation breach, not a cosmetic mismatch.
func ovUserID(userID string) string {
	if ovSafeID.MatchString(userID) {
		return userID
	}
	sum := sha256.Sum256([]byte("blowball-user:" + userID))
	return "u-" + hex.EncodeToString(sum[:16])
}

// ovSessionID derives the persistent OpenViking session id for a blowball
// session: "bb-" + session id, mirroring OpenViking's own harness prefixes
// (cc-<id> for Claude Code, cx-<id> for Codex). An OpenViking session
// survives commits (commit archives pending messages and triggers memory
// extraction; the session itself stays), so one blowball session maps to one
// OpenViking session for its whole life. Turn ordering inside one session is
// serialized upstream by the Redis session-run claim, so batch/commit pairs
// on the same OV session id never race.
func ovSessionID(sessionID string) string {
	sid := sessionID
	if !ovSafeID.MatchString(sid) {
		sum := sha256.Sum256([]byte("blowball-session:" + sid))
		sid = hex.EncodeToString(sum[:16])
	}
	return "bb-" + sid
}

// Recall queries the user's OpenViking memory store with the new turn's text
// and returns the rendered injection block ("" when nothing relevant was
// found — the caller skips injection entirely rather than injecting an empty
// frame). A cfg.RecallTimeout deadline is applied on top of the caller's
// context because recall sits on the turn-start path. Errors are returned
// for the caller to WARN; recall never blocks or fails a turn by contract.
func (s *Service) Recall(ctx context.Context, userID, query string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RecallTimeout)
	defer cancel()
	c, err := s.client(ovUserID(userID))
	if err != nil {
		return "", err
	}
	// TargetURI already scopes the search to the authenticated user's
	// memory namespace; context_type memory is belt-and-braces against the
	// namespace ever carrying mixed content.
	res, err := c.Find(ctx, query, &openviking.FindOptions{
		TargetURI:   "viking://user/memories",
		ContextType: []string{"memory"},
		Limit:       s.cfg.RecallLimit,
	})
	if err != nil {
		return "", err
	}
	return renderRecallBlock(res.Memories, s.cfg.RecallTokenBudget), nil
}

// TurnCapture is one turn's memory-capture payload.
type TurnCapture struct {
	UserID        string
	SessionID     string
	UserContent   string    // the turn's user message
	AssistantText string    // merged top-level assistant tokens; "" when the turn produced none
	UserAt        time.Time // request arrival (the persisted user row's msg_time)
	TurnEndAt     time.Time // terminal-path time (the assistant side's timestamp)
}

// CaptureTurn submits the turn to OpenViking: get-or-create the session,
// batch-append the user message (always — a question the model failed to
// answer is still durable memory signal) plus the assistant text (skipped
// when the turn produced none), then commit. Commit archives the pending
// messages and kicks off the server-side asynchronous memory extraction; the
// returned task is deliberately NOT polled — extraction latency is the
// server's business and nothing downstream waits on it.
func (s *Service) CaptureTurn(ctx context.Context, tc TurnCapture) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.CaptureTimeout)
	defer cancel()
	c, err := s.client(ovUserID(tc.UserID))
	if err != nil {
		return err
	}
	sid := ovSessionID(tc.SessionID)
	if _, err := c.GetSession(ctx, sid, &openviking.GetSessionOptions{AutoCreate: true}); err != nil {
		return fmt.Errorf("get-or-create session %s: %w", sid, err)
	}
	msgs := []openviking.Message{{
		Role:      "user",
		Content:   openviking.String(truncateCapture(tc.UserContent, s.cfg.MaxCaptureBytes)),
		CreatedAt: tc.UserAt.UTC().Format(time.RFC3339),
	}}
	if strings.TrimSpace(tc.AssistantText) != "" {
		msgs = append(msgs, openviking.Message{
			Role:      "assistant",
			Content:   openviking.String(truncateCapture(tc.AssistantText, s.cfg.MaxCaptureBytes)),
			CreatedAt: tc.TurnEndAt.UTC().Format(time.RFC3339),
		})
	}
	if _, err := c.BatchAddMessages(ctx, sid, msgs, nil); err != nil {
		return fmt.Errorf("batch add messages to %s: %w", sid, err)
	}
	// keep_recent_count 0: blowball owns conversation continuity (full
	// history is recovered from its own store every turn), so the OV session
	// needs no live tail — commit archives everything pending for
	// extraction. Commit is idempotent (a no-op with nothing pending), so a
	// per-turn cadence is safe.
	if _, err := c.CommitSession(ctx, sid, &openviking.CommitSessionOptions{KeepRecentCount: 0}); err != nil {
		return fmt.Errorf("commit session %s: %w", sid, err)
	}
	return nil
}

// truncateCapture caps a captured message at max bytes, rune-safely, and
// marks the cut so the server's extractor sees an honest truncation rather
// than a suspiciously neat ending. max <= 0 disables the cap (defensive —
// config validation rejects negatives when enabled).
func truncateCapture(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…[blowball: truncated, %d more bytes]", len(s)-cut)
}

// Health probes the server through an operator-scoped client. Startup-only;
// the caller treats a failure as WARN, never fatal — memory degrades per
// turn, it does not gate boot.
//
// The probe client carries a synthetic user identity: in trusted mode the
// server resolves EVERY request against X-OpenViking-User (registration not
// required), so a user-less probe would log "Failed to resolve identity"
// warnings server-side on every startup — the request still succeeds, but
// the identity keeps the OV log clean.
const probeUser = "blowball-health"

func (s *Service) Health(ctx context.Context) (bool, error) {
	s.mu.Lock()
	if s.opClient == nil {
		c, err := openviking.NewClient(openviking.Config{
			BaseURL:    strings.TrimSpace(s.cfg.BaseURL),
			APIKey:     s.cfg.APIKey,
			Account:    s.cfg.Account,
			User:       probeUser,
			HTTPClient: s.http,
		})
		if err != nil {
			s.mu.Unlock()
			return false, err
		}
		s.opClient = c
	}
	c := s.opClient
	s.mu.Unlock()
	return c.Health(ctx)
}

// Close releases the shared transport's idle keep-alive connections. It does
// NOT abort in-flight calls (the http.Client stays valid until process
// exit); a capture racing shutdown is at-least-once — the OV session keeps
// whatever landed and the session's next turn commits it.
func (s *Service) Close() {
	if s == nil || s.http == nil {
		return
	}
	s.http.CloseIdleConnections()
}
