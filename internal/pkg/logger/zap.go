// Package logger initializes the application-wide zap logger.
//
// Init builds a zap.Logger that tees output to one or more sinks selected by
// the logging config: a console sink (stderr or stdout) and/or a rotated file
// sink rooted at a caller-supplied log directory. The file sink writes through
// gopkg.in/natefinch/lumberjack.v2 so disk usage stays bounded via size/age/
// backup rotation.
//
// A package-level default logger is exposed via L() so callers that do not yet
// hold a *zap.Logger can still emit logs; Init installs the built logger as that
// default.
//
// Per-request fields such as trace_id are intended to be attached through
// logger.With(zap.String("trace_id", id)) when a context-aware logger is
// threaded through the call chain; the global L() remains context-free.
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/lush/blowball/internal/config"
)

// LogFileName is the active log file written inside the log directory.
const LogFileName = "blowball.log"

var (
	mu            sync.RWMutex
	defaultLogger = zap.NewNop()
)

// Init builds a zap.Logger from the logging config, teeing together every
// requested sink. cfg.Output selects the sinks (stderr, stdout, file); when it
// is empty it defaults to stderr + file (see config.DefaultLoggingOutput).
// cfg.Format selects the encoder (json default | console) for every sink.
//
// When a file sink is enabled, logDir is created (0o755) if missing and the
// rotated file {logDir}/blowball.log is opened through lumberjack. If that
// fails Init returns an error so startup aborts rather than silently dropping
// the configured persistent log (D8: fail fast).
//
// The built logger is also installed as the package default returned by L().
func Init(cfg config.LoggingConfig, logDir string) (*zap.Logger, error) {
	lvl, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	encCfg := encoderConfig()
	encoder, err := newEncoder(cfg.Format, encCfg)
	if err != nil {
		return nil, err
	}

	hasConsole, hasFile, consoleWS, err := resolveSinks(cfg.Output)
	if err != nil {
		return nil, err
	}

	var cores []zapcore.Core

	if hasConsole {
		cores = append(cores, zapcore.NewCore(encoder, zapcore.AddSync(consoleWS), lvl))
	}

	if hasFile {
		writeSyncer, err := fileSink(logDir, cfg.File)
		if err != nil {
			return nil, err
		}
		cores = append(cores, zapcore.NewCore(encoder, writeSyncer, lvl))
	}

	if len(cores) == 0 {
		// Output parsed but selected no recognizable sink (e.g. an empty list
		// slipped past validate). Fall back to stderr so the process never runs
		// silently.
		cores = append(cores, zapcore.NewCore(encoder, zapcore.Lock(os.Stderr), lvl))
	}

	logger := zap.New(zapcore.NewTee(cores...))

	mu.Lock()
	defaultLogger = logger
	mu.Unlock()

	return logger, nil
}

// encoderConfig returns the encoder config shared by every core: ISO8601
// timestamps under the "timestamp" key plus the standard zap field set.
func encoderConfig() zapcore.EncoderConfig {
	ec := zap.NewProductionEncoderConfig()
	ec.TimeKey = "timestamp"
	ec.EncodeTime = zapcore.ISO8601TimeEncoder
	return ec
}

// newEncoder builds the encoder for the requested format. An empty format
// defaults to json (the loader normalizes it, but be defensive here too).
func newEncoder(format string, ec zapcore.EncoderConfig) (zapcore.Encoder, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		return zapcore.NewJSONEncoder(ec), nil
	case "console":
		return zapcore.NewConsoleEncoder(ec), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q (want json|console)", format)
	}
}

// resolveSinks parses the output list, reporting whether a console sink and/or
// a file sink are requested and which console stream to use. An empty list
// defaults to stderr + file.
func resolveSinks(output []string) (console, file bool, consoleWS zapcore.WriteSyncer, err error) {
	if len(output) == 0 {
		output = config.DefaultLoggingOutput
	}
	for _, sink := range output {
		switch sink {
		case "stderr":
			console = true
			consoleWS = os.Stderr
		case "stdout":
			console = true
			consoleWS = os.Stdout
		case "file":
			file = true
		default:
			return false, false, nil, fmt.Errorf("unsupported log output sink %q (want stderr|stdout|file)", sink)
		}
	}
	// If the config named both stderr and stdout, the last one wins (consoleWS
	// reflects the final assignment) and hasConsole stays true.
	return console, file, consoleWS, nil
}

// fileSink builds the lumberjack-backed WriteSyncer for the file core. The log
// directory is created if missing; failure to create it or open the file is
// surfaced as an error so startup fails fast (D8).
func fileSink(logDir string, file config.LogFileConfig) (zapcore.WriteSyncer, error) {
	if strings.TrimSpace(logDir) == "" {
		return nil, fmt.Errorf("file logging enabled but log directory is empty")
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory %q: %w", logDir, err)
	}
	lj := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, LogFileName),
		MaxSize:    file.MaxSizeMB,
		MaxBackups: file.MaxBackups,
		MaxAge:     file.MaxAgeDays,
		Compress:   file.Compress,
	}
	// lumberjack opens lazily on first Write; force an open so a bad path or
	// permission fails at startup, not on the first log line post-landlock.
	if _, err := lj.Write([]byte{}); err != nil {
		return nil, fmt.Errorf("open log file %q: %w", lj.Filename, err)
	}
	return zapcore.AddSync(lj), nil
}

// L returns the package-level default logger. Before Init is called it returns
// a no-op logger so callers can call L() safely at any time.
func L() *zap.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return defaultLogger
}

// SetDefault replaces the package-level default logger. Intended for tests and
// bootstrap paths that construct a logger directly.
func SetDefault(l *zap.Logger) {
	mu.Lock()
	defaultLogger = l
	mu.Unlock()
}

// parseLevel maps a human-readable level string to zapcore.Level.
func parseLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return zapcore.InfoLevel, nil
	case "debug":
		return zapcore.DebugLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q (want debug|info|warn|error)", s)
	}
}
