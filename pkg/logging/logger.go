package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	logDir      = ".bridge/logs"
	logFileName = "bridge.log"
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
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		dir := filepath.Join(home, logDir)
		if err := os.MkdirAll(dir, 0755); err == nil {
			w := newRollingWriter(filepath.Join(dir, logFileName))
			handlers = append(handlers, newJSONHandler(w, slog.LevelDebug))
			closers = append(closers, w)
		}
	}

	// Add any extra destinations from --log-paths.
	for _, p := range logPaths {
		switch p {
		case "stdout":
			handlers = append(handlers, newJSONHandler(os.Stdout, level))
		case "stderr":
			handlers = append(handlers, newJSONHandler(os.Stderr, level))
		default:
			w := newRollingWriter(p)
			handlers = append(handlers, newJSONHandler(w, level))
			closers = append(closers, w)
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
		return cleanupFn, homeErr
	}

	if len(handlers) == 1 {
		slog.SetDefault(slog.New(handlers[0]))
	} else {
		slog.SetDefault(slog.New(&multiHandler{handlers: handlers}))
	}

	return cleanupFn, homeErr
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

// newRollingWriter returns a lumberjack rolling writer for the given path.
// It rotates when the file exceeds 10 MB, keeps 3 old files, and discards logs
// older than 7 days.
func newRollingWriter(filename string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   filename,
		MaxSize:    10, // MB
		MaxBackups: 3,
		MaxAge:     7, // days
		Compress:   true,
	}
}
