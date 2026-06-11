// Package main implements the blowball seed CLI: a one-shot tool for
// inserting a user row with a properly bcrypt-hashed password. This is the
// supported path for "manual 入库" since the API deliberately exposes no
// user-creation endpoint.
//
// Usage:
//
//	go run ./cmd/seed -username alice
//	go run ./cmd/seed -username alice -password 's3cret' -dry-run
//	bin/seed -username alice -status active -cost 12
//
// The password is read from a hidden terminal prompt when -password is
// omitted, so it does not end up in shell history.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/trace"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
)

const defaultConfigPath = "config.yaml"

type flags struct {
	config   string
	username string
	password string
	status   string
	cost     int
	dryRun   bool
}

func parseFlags(args []string) (*flags, error) {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	f := &flags{}
	fs.StringVar(&f.config, "config", defaultConfigPath, "path to config.yaml")
	fs.StringVar(&f.username, "username", "", "username to create (required)")
	fs.StringVar(&f.password, "password", "", "password (omit to be prompted securely)")
	fs.StringVar(&f.status, "status", model.UserStatusActive, "user status: active|disabled")
	fs.IntVar(&f.cost, "cost", bcrypt.DefaultCost, "bcrypt cost factor")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the bcrypt hash without writing to MySQL")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(f.username) == "" {
		return nil, errors.New("-username is required")
	}
	if f.status != model.UserStatusActive && f.status != model.UserStatusDisabled {
		return nil, fmt.Errorf("invalid -status %q (want %q or %q)", f.status, model.UserStatusActive, model.UserStatusDisabled)
	}
	if f.cost < bcrypt.MinCost || f.cost > bcrypt.MaxCost {
		return nil, fmt.Errorf("invalid -cost %d (want %d..%d)", f.cost, bcrypt.MinCost, bcrypt.MaxCost)
	}
	return f, nil
}

// readPassword returns the password from -password, or prompts the terminal
// for one when stdin is a TTY (with a confirmation prompt to catch typos).
func readPassword(f *flags) (string, error) {
	if f.password != "" {
		return f.password, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("-password is required when stdin is not a terminal")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	first, err := term.ReadPassword(fd)
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, "Confirm:  ")
	second, err := term.ReadPassword(fd)
	if err != nil {
		return "", fmt.Errorf("reading password confirmation: %w", err)
	}
	fmt.Fprintln(os.Stderr)
	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	if len(first) == 0 {
		return "", errors.New("password is empty")
	}
	return string(first), nil
}

func run(ctx context.Context, f *flags) error {
	pw, err := readPassword(f)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), f.cost)
	if err != nil {
		return fmt.Errorf("bcrypt hashing: %w", err)
	}
	userID := uuid.NewString()
	traceID := trace.New()

	fmt.Fprintf(os.Stderr, "username : %s\n", f.username)
	fmt.Fprintf(os.Stderr, "user_id  : %s\n", userID)
	fmt.Fprintf(os.Stderr, "trace_id : %s\n", traceID)
	fmt.Fprintf(os.Stderr, "status   : %s\n", f.status)
	fmt.Fprintf(os.Stderr, "bcrypt   : cost=%d len=%d\n", f.cost, len(hash))

	if f.dryRun {
		fmt.Fprintf(os.Stderr, "(dry-run; not persisted)\n")
		return nil
	}

	cfg, err := config.Load(f.config)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	store, err := mysqlstore.New(cfg.MySQL.DSN)
	if err != nil {
		return fmt.Errorf("connecting to MySQL: %w", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: closing mysql: %v\n", cerr)
		}
	}()

	existing, err := store.GetUserByUsername(ctx, f.username)
	if err != nil {
		return fmt.Errorf("checking existing user: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("username %q already exists (user_id=%s)", f.username, existing.UserID)
	}

	u := model.User{
		UserID:   userID,
		Username: f.username,
		Password: string(hash),
		Status:   f.status,
		TraceID:  traceID,
	}
	if err := store.CreateUser(ctx, u); err != nil {
		return fmt.Errorf("inserting user: %w", err)
	}
	fmt.Fprintf(os.Stderr, "created   : ok\n")
	fmt.Println(userID)
	return nil
}

func main() {
	f, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Usage: seed -username <name> [-password <pw>] [-status active|disabled] [-cost N] [-config path] [-dry-run]")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, f); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
