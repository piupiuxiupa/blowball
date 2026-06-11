// Package model defines the blowball domain structs that mirror the MySQL
// schema in migrations/. Every struct carries `db` tags for sqlx and `json`
// tags for HTTP serialization.
package model

import "time"

// User status values. These match the `status` column on the users table.
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

// User mirrors the `users` table (migration 001_users.sql).
type User struct {
	UserID     string    `db:"user_id"     json:"user_id"`
	Username   string    `db:"username"    json:"username"`
	Password   string    `db:"password"    json:"-"` // bcrypt hash, never serialized
	Status     string    `db:"status"      json:"status"`
	UpdateTime time.Time `db:"update_time" json:"update_time"`
	CreateTime time.Time `db:"create_time" json:"create_time"`
}
