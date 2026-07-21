package logger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lush/blowball/internal/config"

	"go.uber.org/zap"
)

func TestInit_LevelsReturnNonNilLogger(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", ""} {
		t.Run(lvl, func(t *testing.T) {
			l, err := Init(config.LoggingConfig{Level: lvl}, t.TempDir())
			if err != nil {
				t.Fatalf("Init(%q) returned error: %v", lvl, err)
			}
			if l == nil {
				t.Fatalf("Init(%q) returned nil logger", lvl)
			}
			// Sanity: logger should be usable without panicking.
			l.Info("smoke test", zap.String("level", lvl))
			SetDefault(zap.NewNop())
		})
	}
}

func TestInit_InvalidLevelReturnsError(t *testing.T) {
	if _, err := Init(config.LoggingConfig{Level: "verbose"}, t.TempDir()); err == nil {
		t.Fatal("Init(invalid level) expected error, got nil")
	}
}

func TestL_ReturnsLoggerAfterInit(t *testing.T) {
	// Ensure default L() is non-nil and safe even before Init.
	if L() == nil {
		t.Fatal("L() returned nil before Init")
	}

	l, err := Init(config.LoggingConfig{Level: "debug"}, t.TempDir())
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if L() == nil {
		t.Fatal("L() returned nil after Init")
	}

	// Restore to a fresh nop logger to avoid leaking state into other tests.
	SetDefault(zap.NewNop())
	_ = l
}

func TestNewEncoder_FormatSelection(t *testing.T) {
	cases := []struct {
		format string
		wantOK bool
	}{
		{"json", true},
		{"console", true},
		{"", true}, // defaults to json
		{"xml", false},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			_, err := newEncoder(tc.format, encoderConfig())
			if tc.wantOK && err != nil {
				t.Fatalf("newEncoder(%q) unexpected error: %v", tc.format, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("newEncoder(%q) expected error, got nil", tc.format)
			}
		})
	}
}

func TestResolveSinks_DefaultsAndSelection(t *testing.T) {
	// Empty output → stderr + file defaults.
	console, file, ws, err := resolveSinks(nil)
	if err != nil {
		t.Fatalf("resolveSinks(nil): %v", err)
	}
	if !console || !file || ws == nil {
		t.Fatalf("default sinks: console=%v file=%v ws=%v", console, file, ws)
	}

	// Console-only → no file sink.
	console, file, _, err = resolveSinks([]string{"stderr"})
	if err != nil || !console || file {
		t.Fatalf("stderr only: console=%v file=%v err=%v", console, file, err)
	}

	// File-only → no console sink.
	console, file, ws, err = resolveSinks([]string{"file"})
	if err != nil || console || !file || ws != nil {
		t.Fatalf("file only: console=%v file=%v ws=%v err=%v", console, file, ws, err)
	}

	// Unknown sink → error.
	if _, _, _, err := resolveSinks([]string{"syslog"}); err == nil {
		t.Fatal("resolveSinks(syslog) expected error")
	}
}

func TestInit_ConsoleOnlyCreatesNoFile(t *testing.T) {
	dir := t.TempDir()
	l, err := Init(config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: []string{"stderr"},
	}, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	l.Info("hello console")
	_ = l.Sync()
	SetDefault(zap.NewNop())

	if _, err := os.Stat(filepath.Join(dir, LogFileName)); !os.IsNotExist(err) {
		t.Fatalf("expected no log file under console-only output, got err=%v", err)
	}
}

func TestInit_FileOnlyWritesJSON(t *testing.T) {
	dir := t.TempDir()
	l, err := Init(config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: []string{"file"},
	}, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	l.Info("file only message", zap.String("k", "v"))
	_ = l.Sync()
	SetDefault(zap.NewNop())

	data, err := os.ReadFile(filepath.Join(dir, LogFileName))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "file only message") {
		t.Errorf("log file missing message; content=%q", data)
	}
	var entry map[string]any
	if err := json.Unmarshal(jsonLinesFirst(t, data), &entry); err != nil {
		t.Errorf("log line is not valid JSON: %v (content=%q)", err, data)
	}
}

func TestInit_ConsoleEncodingShape(t *testing.T) {
	dir := t.TempDir()
	l, err := Init(config.LoggingConfig{
		Level:  "info",
		Format: "console",
		Output: []string{"file"},
	}, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	l.Info("console encoded")
	_ = l.Sync()
	SetDefault(zap.NewNop())

	data, err := os.ReadFile(filepath.Join(dir, LogFileName))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	// Console encoder is human-readable and not a JSON object: it should not
	// start with '{' and should contain the level + message.
	if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		t.Errorf("expected console encoding, got JSON-shaped output: %q", data)
	}
	if !strings.Contains(string(data), "console encoded") {
		t.Errorf("log file missing message; content=%q", data)
	}
}

// TestInit_TeeWritesToBothSinks redirects os.Stderr to a temp file, inits with
// stderr+file, writes one line, and asserts the line lands in both sinks.
func TestInit_TeeWritesToBothSinks(t *testing.T) {
	dir := t.TempDir()

	// Redirect os.Stderr into a capture file so the console core (which binds
	// os.Stderr at Init time) writes there.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = origStderr
		_ = w.Close()
		_ = r.Close()
	})

	l, err := Init(config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: []string{"stderr", "file"},
	}, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	const marker = "tee-marker-12345"
	l.Info(marker)
	_ = l.Sync()
	SetDefault(zap.NewNop())

	// Flush the stderr pipe into a buffer.
	_ = w.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, rerr := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			break
		}
	}

	if !strings.Contains(string(buf), marker) {
		t.Errorf("console sink (stderr) missing message; got %q", buf)
	}
	fileData, err := os.ReadFile(filepath.Join(dir, LogFileName))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(fileData), marker) {
		t.Errorf("file sink missing message; got %q", fileData)
	}
}

func TestInit_RotationTriggersOnSize(t *testing.T) {
	dir := t.TempDir()
	l, err := Init(config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: []string{"file"},
		File: config.LogFileConfig{
			MaxSizeMB:  1,
			MaxBackups: 3,
		},
	}, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// First write establishes a non-zero file size; subsequent writes that
	// cross 1 MiB force lumberjack to rotate.
	blob := strings.Repeat("a", 600*1024) // 600 KiB per line
	for i := 0; i < 4; i++ {              // 4 * 600 KiB > 1 MiB
		l.Info("fill", zap.String("blob", blob))
	}
	_ = l.Sync()
	SetDefault(zap.NewNop())

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	// Expect at least the active blowball.log plus one rotated backup. lumberjack
	// names backups by inserting a timestamp before the extension:
	// blowball-<timestamp>.log.
	var rotated bool
	for _, e := range entries {
		if e.Name() != LogFileName && strings.HasPrefix(e.Name(), "blowball-") && strings.HasSuffix(e.Name(), ".log") {
			rotated = true
		}
	}
	if !rotated {
		t.Fatalf("expected a rotated backup log file; entries=%v", entryNames(entries))
	}
}

// TestLogFileNameFor_RoleMapping verifies the per-role lumberjack filename so
// two role processes on the same host do not contend on one file.
func TestLogFileNameFor_RoleMapping(t *testing.T) {
	cases := []struct {
		role string
		want string
	}{
		{"all", LogFileName},
		{"", LogFileName}, // empty / unrecognized collapses to the all default
		{"api", "blowball-api.log"},
		{"agent", "blowball-agent.log"},
		{"bogus", LogFileName},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			if got := LogFileNameFor(tc.role); got != tc.want {
				t.Errorf("LogFileNameFor(%q) = %q, want %q", tc.role, got, tc.want)
			}
		})
	}
}

// TestInitForRole_WritesRoleScopedFilename verifies each role actually opens
// the role-scoped file under the log directory.
func TestInitForRole_WritesRoleScopedFilename(t *testing.T) {
	cases := []struct {
		role string
		want string
	}{
		{"all", LogFileName},
		{"api", "blowball-api.log"},
		{"agent", "blowball-agent.log"},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			dir := t.TempDir()
			l, err := InitForRole(config.LoggingConfig{
				Level:  "info",
				Format: "json",
				Output: []string{"file"},
			}, dir, tc.role)
			if err != nil {
				t.Fatalf("InitForRole(%q): %v", tc.role, err)
			}
			l.Info("role marker", zap.String("role", tc.role))
			_ = l.Sync()
			SetDefault(zap.NewNop())

			data, err := os.ReadFile(filepath.Join(dir, tc.want))
			if err != nil {
				t.Fatalf("read %s: %v (dir entries: %v)", tc.want, err, entryNames(mustReadDir(t, dir)))
			}
			if !strings.Contains(string(data), "role marker") {
				t.Errorf("%s missing message; content=%q", tc.want, data)
			}

			// No other role's file should have been created.
			for _, other := range []string{LogFileName, "blowball-api.log", "blowball-agent.log"} {
				if other == tc.want {
					continue
				}
				if _, err := os.Stat(filepath.Join(dir, other)); !os.IsNotExist(err) {
					t.Errorf("role %q should not create %s, got err=%v", tc.role, other, err)
				}
			}
		})
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	return entries
}

func TestInit_FileSinkFailsFastOnUnwritableDir(t *testing.T) {
	// Point logDir at a path whose parent is a regular file, so MkdirAll fails.
	regular := filepath.Join(t.TempDir(), "i-am-a-file")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	logDir := filepath.Join(regular, "nested")

	if _, err := Init(config.LoggingConfig{
		Level:  "info",
		Output: []string{"file"},
	}, logDir); err == nil {
		t.Fatal("expected Init to fail fast when the log dir cannot be created")
	}
}

// jsonLinesFirst returns the first non-empty line of data as []byte, for JSON
// decoding of a possibly multi-line log file.
func jsonLinesFirst(t *testing.T, data []byte) []byte {
	t.Helper()
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			return []byte(line)
		}
	}
	t.Fatalf("no log lines in data: %q", data)
	return nil
}

func entryNames(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
