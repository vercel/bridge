package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	maxLogFileSize = 10 * 1024 * 1024 // 10 MB
	logDir         = ".bridge/logs"
	logFileName    = "bridge.log"
	logFileBackup  = "bridge.log.1"
)

// multiHandler fans out slog records to multiple handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}

// Setup configures logging output. By default, logs go only to the rolling
// log file at ~/.bridge/logs/bridge.log (at Debug level). Additional
// destinations can be specified via logPaths: each entry may be "stdout",
// "stderr", or a file path.
//
// Returns a cleanup function to close any opened files. The caller should defer it.
func Setup(level slog.Level, logPaths []string) (cleanup func(), err error) {
	var handlers []slog.Handler
	var closers []io.Closer

	// Always include the default rolling log file.
	logFile, fileErr := openLogFile()
	if fileErr == nil {
		handlers = append(handlers, newJSONHandler(logFile, slog.LevelDebug))
		closers = append(closers, logFile)
	}

	// Add any extra destinations from --log-path.
	for _, p := range logPaths {
		switch p {
		case "stdout":
			handlers = append(handlers, newJSONHandler(os.Stdout, level))
		case "stderr":
			handlers = append(handlers, newJSONHandler(os.Stderr, level))
		default:
			f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				// Skip paths we can't open; don't fail the whole CLI.
				continue
			}
			handlers = append(handlers, newJSONHandler(f, level))
			closers = append(closers, f)
		}
	}

	cleanupFn := func() {
		for _, c := range closers {
			c.Close()
		}
	}

	if len(handlers) == 0 {
		// Nothing worked at all — set a silent discard handler.
		slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
		return cleanupFn, fileErr
	}

	if len(handlers) == 1 {
		slog.SetDefault(slog.New(handlers[0]))
	} else {
		slog.SetDefault(slog.New(&multiHandler{handlers: handlers}))
	}

	return cleanupFn, fileErr
}

// newJSONHandler returns a slog JSON handler with source shortening.
func newJSONHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.SourceKey {
				if src, ok := a.Value.Any().(*slog.Source); ok {
					dir := filepath.Base(filepath.Dir(src.File))
					file := filepath.Base(src.File)
					a.Value = slog.StringValue(fmt.Sprintf("%s/%s:%d", dir, file, src.Line))
				}
			}
			return a
		},
	})
}

// openLogFile opens the log file at ~/.bridge/logs/bridge.log, rotating the
// existing file to bridge.log.1 if it exceeds maxLogFileSize.
func openLogFile() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}

	dir := filepath.Join(home, logDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create log directory: %w", err)
	}

	logPath := filepath.Join(dir, logFileName)

	// Simple rotation: if the file exceeds the size limit, rename to .1.
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogFileSize {
		backupPath := filepath.Join(dir, logFileBackup)
		_ = os.Remove(backupPath)
		_ = os.Rename(logPath, backupPath)
	}

	return os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
}
