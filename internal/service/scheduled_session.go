package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/store/mysql"
	"go.uber.org/zap"
)

// ErrScheduledSessionNotFound collapses a missing, cross-user, or
// wrong-source scheduled session into one API-safe 404.
var ErrScheduledSessionNotFound = errors.New("scheduled session not found")

// ErrScheduledSessionActive reports that a scheduled session may still be
// executing (running or uncertain), so it must not be deleted before a stop
// proof exists.
var ErrScheduledSessionActive = errors.New("scheduled session execution may still be running")

// ScheduledSessionStore is the narrow, source-bound persistence surface for
// task conversations. It is satisfied only by a SessionStore bound to the
// scheduled table group — never by the raw interactive store.
type ScheduledSessionStore interface {
	// Source pins the store's table group; the constructor rejects anything
	// other than SessionSourceScheduled.
	Source() model.SessionSource
	CreateScheduledSession(ctx context.Context, sess model.Session) (*model.Session, error)
	GetScheduledSessionByID(ctx context.Context, sessionID string) (*model.Session, error)
	ListScheduledSessions(ctx context.Context, userID, taskID string, offset, limit int) ([]mysql.SessionWithTitle, error)
	DeleteSession(ctx context.Context, sessionID string) error
	ListMessagesPaged(ctx context.Context, sessionID, cursorStr string, pageSize int, order, subagentContent string) ([]model.Message, string, error)
	ListToolCalls(ctx context.Context, sessionID string) ([]model.ToolCall, error)
	GetTitle(ctx context.Context, sessionID string) (*model.Title, error)
}

// ScheduledSessionSummary is the list DTO for a task execution session.
type ScheduledSessionSummary struct {
	SessionID        string              `json:"session_id"`
	Title            string              `json:"title"`
	TaskID           string              `json:"task_id"`
	OccurrenceID     string              `json:"occurrence_id"`
	RunID            string              `json:"run_id"`
	ContextCompacted bool                `json:"context_compacted"`
	Source           model.SessionSource `json:"session_source"`
	UpdateTime       time.Time           `json:"update_time"`
	CreateTime       time.Time           `json:"create_time"`
}

// ScheduledSessionDetail is the single-session read model.
type ScheduledSessionDetail struct {
	Session model.Session `json:"session"`
	Title   string        `json:"title"`
}

// ScheduledSessions owns the only public entry point to the scheduled table
// group. Every read validates owner and source before touching cache or rows,
// and no method falls back to the ordinary session tables.
type ScheduledSessions struct {
	store ScheduledSessionStore
	redis RedisStore
	// deletionGuard reports the authoritative reason a session may still be
	// executing. Nil (unwired) leaves deletion guarded only by row state.
	deletionGuard func(ctx context.Context, sessionID string) (string, error)
	// onDeleted clears source-specific cache keys (e.g. runtime status) after
	// the database delete.
	onDeleted func(ctx context.Context, sessionID string)
}

// NewScheduledSessions wires the task-session read/write service. store must be
// a session store bound to the scheduled table group; wiring the interactive
// store here is a programming error and panics at startup rather than risking
// a cross-table read.
func NewScheduledSessions(store ScheduledSessionStore, redis RedisStore) *ScheduledSessions {
	if store.Source() != model.SessionSourceScheduled {
		panic(fmt.Sprintf("service.NewScheduledSessions: store source %q is not scheduled", store.Source()))
	}
	return &ScheduledSessions{store: store, redis: redis}
}

// WithDeletionGuard registers the execution-fact check used by Delete.
func (s *ScheduledSessions) WithDeletionGuard(fn func(ctx context.Context, sessionID string) (string, error)) *ScheduledSessions {
	s.deletionGuard = fn
	return s
}

// WithDeletedHook registers post-delete cache cleanup (runtime status log).
func (s *ScheduledSessions) WithDeletedHook(fn func(ctx context.Context, sessionID string)) *ScheduledSessions {
	s.onDeleted = fn
	return s
}

// Create idempotently creates the occurrence's session.
func (s *ScheduledSessions) Create(ctx context.Context, sess model.Session) (*model.Session, error) {
	return s.store.CreateScheduledSession(ctx, sess)
}

// Get returns the session when it exists and belongs to userID; otherwise
// (nil, nil). No existence or ownership detail is disclosed.
func (s *ScheduledSessions) Get(ctx context.Context, userID, sessionID string) (*model.Session, error) {
	sess, err := s.store.GetScheduledSessionByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil || sess.UserID != userID {
		return nil, nil
	}
	return sess, nil
}

// GetDetail returns the session plus its title.
func (s *ScheduledSessions) GetDetail(ctx context.Context, userID, sessionID string) (*ScheduledSessionDetail, error) {
	sess, err := s.Get(ctx, userID, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	detail := &ScheduledSessionDetail{Session: *sess}
	title, terr := s.store.GetTitle(ctx, sessionID)
	if terr != nil {
		logger.FromContext(ctx).Warn("scheduled session title read failed; returning empty title",
			zap.String("op", "scheduled_session.get_detail"),
			zap.String("session_id", sessionID),
			zap.Error(terr))
	} else if title != nil {
		detail.Title = title.Title
	}
	return detail, nil
}

// List returns one owner-scoped page, optionally filtered by task.
func (s *ScheduledSessions) List(ctx context.Context, userID, taskID string, offset, limit int) ([]ScheduledSessionSummary, error) {
	rows, err := s.store.ListScheduledSessions(ctx, userID, taskID, offset, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduledSessionSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, ScheduledSessionSummary{
			SessionID:        r.SessionID,
			Title:            r.Title,
			TaskID:           r.TaskID,
			OccurrenceID:     r.OccurrenceID,
			RunID:            r.RunID,
			ContextCompacted: r.ContextCompacted,
			Source:           model.SessionSourceScheduled,
			UpdateTime:       r.UpdateTime,
			CreateTime:       r.CreateTime,
		})
	}
	return out, nil
}

// Messages returns one cursor page of a scheduled session's history after an
// owner/source check. A foreign or ordinary session ID reads as not found.
func (s *ScheduledSessions) Messages(ctx context.Context, userID, sessionID, cursorStr string, pageSize int, order, subagentContent string) ([]model.Message, string, error) {
	sess, err := s.Get(ctx, userID, sessionID)
	if err != nil {
		return nil, "", err
	}
	if sess == nil {
		return nil, "", ErrScheduledSessionNotFound
	}
	return s.store.ListMessagesPaged(ctx, sessionID, cursorStr, pageSize, order, subagentContent)
}

// ToolCalls returns a scheduled session's tool interactions after the same
// owner/source check.
func (s *ScheduledSessions) ToolCalls(ctx context.Context, userID, sessionID string) ([]model.ToolCall, error) {
	sess, err := s.Get(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, ErrScheduledSessionNotFound
	}
	return s.store.ListToolCalls(ctx, sessionID)
}

// Delete archives and purges one owner's stopped scheduled session. A session
// that may still be executing is rejected with ErrScheduledSessionActive.
func (s *ScheduledSessions) Delete(ctx context.Context, userID, sessionID string) error {
	sess, err := s.Get(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return ErrScheduledSessionNotFound
	}
	if s.deletionGuard != nil {
		reason, gerr := s.deletionGuard(ctx, sessionID)
		if gerr != nil {
			return fmt.Errorf("scheduled session delete: execution fact read: %w", gerr)
		}
		if reason != "" {
			return fmt.Errorf("%w: %s", ErrScheduledSessionActive, reason)
		}
	}
	if err := s.store.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("scheduled session delete: %w", err)
	}
	if s.redis != nil {
		if err := s.redis.ClearMessages(ctx, sessionID); err != nil {
			logger.FromContext(ctx).Warn("scheduled msgs cache clear failed; key expires on TTL",
				zap.String("op", "scheduled_session.delete"),
				zap.String("session_id", sessionID),
				zap.Error(err))
		}
		if err := s.redis.DelCompactionCache(ctx, sessionID); err != nil {
			logger.FromContext(ctx).Warn("scheduled compaction cache clear failed; key expires on TTL",
				zap.String("op", "scheduled_session.delete"),
				zap.String("session_id", sessionID),
				zap.Error(err))
		}
	}
	if s.onDeleted != nil {
		s.onDeleted(ctx, sessionID)
	}
	return nil
}
