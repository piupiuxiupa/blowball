// Package mysql provides the sqlx-backed implementation of the blowball
// persistent store. It owns CRUD operations for users, sessions, titles and
// messages.
//
// Every method accepts a context.Context and logs through logger.L() with the
// trace_id recovered via trace.FromContext so request flows stay correlated
// across the handler → service → store boundary. Read paths return
// (nil, nil) for not-found rows so callers can branch on the value rather than
// having to unwrap sql.ErrNoRows.
package mysql

import (
	"context"
	"fmt"

	_ "github.com/go-sql-driver/mysql" // register the mysql driver
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// Store wraps a *sqlx.DB connection. Construct one with New.
type Store struct {
	db *sqlx.DB
}

// New opens the MySQL connection described by dsn using sqlx.Connect, pings
// the server, and returns a ready Store. The returned Store must be closed
// with Close when no longer needed.
func New(dsn string) (*Store, error) {
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql connect: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database connection pool.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the wrapped *sqlx.DB so higher layers (e.g. migrations or debug
// tooling) can run ad-hoc queries. Service code should prefer the typed
// methods on Store.
func (s *Store) DB() *sqlx.DB {
	return s.db
}

// traceIDFromCtx pulls the trace_id out of ctx, returning an empty string when
// none is set. It is a thin shim over trace.FromContext kept here so the rest
// of this package does not need to import trace directly in every file.
func traceIDFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	return trace.FromContext(ctx)
}

// logQuery emits a debug log describing the about-to-be-run query with the
// current trace_id (when present) attached. It is a no-op equivalent at the
// call site — every public method calls it before issuing its query.
func logQuery(ctx context.Context, op, query string, args ...any) {
	fields := []zap.Field{
		zap.String("op", op),
		zap.String("query", query),
	}
	if tid := traceIDFromCtx(ctx); tid != "" {
		fields = append(fields, zap.String("trace_id", tid))
	}
	if len(args) > 0 {
		fields = append(fields, zap.String("args", fmt.Sprint(args...)))
	}
	logger.L().Debug("mysql query", fields...)
}
