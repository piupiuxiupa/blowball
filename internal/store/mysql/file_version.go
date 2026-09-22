package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// file_versions columns in canonical order (turn-artifacts capability).
const fileVersionCols = `user_id, path, version_id, size, mime, sha256, create_time`

const insertFileVersionSQL = `
INSERT INTO file_versions (user_id, path, version_id, size, mime, sha256)
VALUES (:user_id, :path, :version_id, :size, :mime, :sha256)
`

const latestFileVersionSQL = `
SELECT ` + fileVersionCols + `
FROM file_versions
WHERE user_id = ? AND path = ?
ORDER BY create_time DESC, id DESC
LIMIT 1
`

const fileVersionAsOfSQL = `
SELECT ` + fileVersionCols + `
FROM file_versions
WHERE user_id = ? AND path = ? AND create_time <= ?
ORDER BY create_time DESC, id DESC
LIMIT 1
`

const fileVersionByIDSQL = `
SELECT ` + fileVersionCols + `
FROM file_versions
WHERE version_id = ?
LIMIT 1
`

// scanFileVersion runs q and returns the row, mapping sql.ErrNoRows to
// (nil, nil) — "no version" is a normal outcome for every lookup here.
func scanFileVersion(row func(dest ...any) error) (*model.FileVersion, error) {
	var rec model.FileVersion
	err := row(&rec.UserID, &rec.Path, &rec.VersionID, &rec.Size, &rec.Mime, &rec.SHA256, &rec.CreateTime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// InsertVersion records one artifact version snapshot (artifact.Index).
func (s *Store) InsertVersion(ctx context.Context, rec model.FileVersion) error {
	logQuery(ctx, "file_version.insert", insertFileVersionSQL, rec.UserID, rec.Path, rec.VersionID)
	_, err := sqlx.NamedExecContext(ctx, s.db, insertFileVersionSQL, rec)
	return err
}

// LatestVersion returns the newest version record for (userID, path), or
// (nil, nil) when the path was never versioned (artifact.Index).
func (s *Store) LatestVersion(ctx context.Context, userID, path string) (*model.FileVersion, error) {
	logQuery(ctx, "file_version.latest", latestFileVersionSQL, userID, path)
	return scanFileVersion(s.db.QueryRowxContext(ctx, latestFileVersionSQL, userID, path).Scan)
}

// VersionAsOf returns the newest version record for (userID, path) created at
// or before before, or (nil, nil) when none exists (artifact.Index).
func (s *Store) VersionAsOf(ctx context.Context, userID, path string, before time.Time) (*model.FileVersion, error) {
	logQuery(ctx, "file_version.as_of", fileVersionAsOfSQL, userID, path, before)
	return scanFileVersion(s.db.QueryRowxContext(ctx, fileVersionAsOfSQL, userID, path, before).Scan)
}

// VersionByID returns the record for versionID, or (nil, nil) when it does
// not exist (artifact.Index). Ownership is the caller's check.
func (s *Store) VersionByID(ctx context.Context, versionID string) (*model.FileVersion, error) {
	logQuery(ctx, "file_version.by_id", fileVersionByIDSQL, versionID)
	return scanFileVersion(s.db.QueryRowxContext(ctx, fileVersionByIDSQL, versionID).Scan)
}
