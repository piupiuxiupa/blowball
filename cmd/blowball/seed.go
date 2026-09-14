package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"flag"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
)

// seedCmd is the `seed` subcommand, a one-shot tool for inserting a user row with a properly bcrypt-hashed password. This is the supported path for "manual 入库" since the API deliberately exposes no user-creation endpoint. The config path comes from the shared -f flag; the password is read from a hidden terminal prompt when --password is omitted so it does not end up in shell history.
func seedCmd(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Create a user row in MySQL with a bcrypt-hashed password.

Examples:
  blowball seed --username alice                     # prompt for password
  blowball seed --username alice --password 's3cret' # non-interactive
  blowball seed --username alice --dry-run           # preview hash only

Flags:
`)
		fs.PrintDefaults()
	}
	username := fs.String("username", "", "username to create (required)")
	password := fs.String("password", "", "password (omit to be prompted securely)")
	status := fs.String("status", model.UserStatusActive, "user status: active|disabled")
	cost := fs.Int("cost", bcrypt.DefaultCost, "bcrypt cost factor")
	dryRun := fs.Bool("dry-run", false, "print the bcrypt hash without writing to MySQL")
	configPath, _ := sharedFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*username) == "" {
		return errors.New("--username is required")
	}
	if *status != model.UserStatusActive && *status != model.UserStatusDisabled {
		return fmt.Errorf("invalid --status %q (want %q or %q)", *status, model.UserStatusActive, model.UserStatusDisabled)
	}
	if *cost < bcrypt.MinCost || *cost > bcrypt.MaxCost {
		return fmt.Errorf("invalid --cost %d (want %d..%d)", *cost, bcrypt.MinCost, bcrypt.MaxCost)
	}

	pw, err := readPassword(*password)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), *cost)
	if err != nil {
		return fmt.Errorf("bcrypt hashing: %w", err)
	}
	userID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generating user_id: %w", err)
	}
	userIDStr := userID.String()

	fmt.Fprintf(os.Stderr, "username : %s\n", *username)
	fmt.Fprintf(os.Stderr, "user_id  : %s\n", userIDStr)
	fmt.Fprintf(os.Stderr, "status   : %s\n", *status)
	fmt.Fprintf(os.Stderr, "bcrypt   : cost=%d len=%d\n", *cost, len(hash))

	if *dryRun {
		fmt.Fprintf(os.Stderr, "(dry-run; not persisted)\n")
		return nil
	}

	cfg, err := config.Load(*configPath)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	existing, err := store.GetUserByUsername(ctx, *username)
	if err != nil {
		return fmt.Errorf("checking existing user: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("username %q already exists (user_id=%s)", *username, existing.UserID)
	}

	u := model.User{
		UserID:   userIDStr,
		Username: *username,
		Password: string(hash),
		Status:   *status,
	}
	if err := store.CreateUser(ctx, u); err != nil {
		return fmt.Errorf("inserting user: %w", err)
	}
	fmt.Fprintf(os.Stderr, "created   : ok\n")
	fmt.Println(userIDStr)
	return nil
}

// readPassword returns the password from the explicit value, or prompts the terminal for one when stdin is a TTY (with a confirmation prompt to catch typos).
func readPassword(password string) (string, error) {
	if password != "" {
		return password, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("--password is required when stdin is not a terminal")
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
