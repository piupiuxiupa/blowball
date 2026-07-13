package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
)

// newSeedCmd builds the `seed` cobra subcommand, a one-shot tool for inserting a user row with a properly bcrypt-hashed password. This is the supported path for "manual 入库" since the API deliberately exposes no user-creation endpoint. The config path comes from the persistent -f flag; the password is read from a hidden terminal prompt when --password is omitted so it does not end up in shell history.
func newSeedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Create a user with a bcrypt-hashed password",
		Long: "Create a user row in MySQL with a bcrypt-hashed password.\n\n" +
			"Examples:\n" +
			"  blowball seed --username alice                      # prompt for password\n" +
			"  blowball seed --username alice --password 's3cret'   # non-interactive\n" +
			"  blowball seed --username alice --dry-run            # preview hash only",
		SilenceUsage: true,
	}

	cmd.Flags().String("username", "", "username to create (required)")
	cmd.Flags().String("password", "", "password (omit to be prompted securely)")
	cmd.Flags().String("status", model.UserStatusActive, "user status: active|disabled")
	cmd.Flags().Int("cost", bcrypt.DefaultCost, "bcrypt cost factor")
	cmd.Flags().Bool("dry-run", false, "print the bcrypt hash without writing to MySQL")

	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return seedRun(c)
	}
	return cmd
}

// seedRun ports the legacy cmd/seed logic: validate flags, hash the password, and (unless --dry-run) load config from -f and insert the user.
func seedRun(cmd *cobra.Command) error {
	username, _ := cmd.Flags().GetString("username")
	password, _ := cmd.Flags().GetString("password")
	status, _ := cmd.Flags().GetString("status")
	cost, _ := cmd.Flags().GetInt("cost")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	if strings.TrimSpace(username) == "" {
		return errors.New("--username is required")
	}
	if status != model.UserStatusActive && status != model.UserStatusDisabled {
		return fmt.Errorf("invalid --status %q (want %q or %q)", status, model.UserStatusActive, model.UserStatusDisabled)
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return fmt.Errorf("invalid --cost %d (want %d..%d)", cost, bcrypt.MinCost, bcrypt.MaxCost)
	}

	pw, err := readPassword(password)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), cost)
	if err != nil {
		return fmt.Errorf("bcrypt hashing: %w", err)
	}
	userID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generating user_id: %w", err)
	}
	userIDStr := userID.String()

	fmt.Fprintf(os.Stderr, "username : %s\n", username)
	fmt.Fprintf(os.Stderr, "user_id  : %s\n", userIDStr)
	fmt.Fprintf(os.Stderr, "status   : %s\n", status)
	fmt.Fprintf(os.Stderr, "bcrypt   : cost=%d len=%d\n", cost, len(hash))

	if dryRun {
		fmt.Fprintf(os.Stderr, "(dry-run; not persisted)\n")
		return nil
	}

	configPath, _, err := persistentFlags(cmd)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
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

	existing, err := store.GetUserByUsername(ctx, username)
	if err != nil {
		return fmt.Errorf("checking existing user: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("username %q already exists (user_id=%s)", username, existing.UserID)
	}

	u := model.User{
		UserID:   userIDStr,
		Username: username,
		Password: string(hash),
		Status:   status,
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
