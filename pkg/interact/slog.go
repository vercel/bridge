package interact

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// slogSpinner logs spinner state transitions via slog instead of displaying
// an animated or plain-text spinner.
type slogSpinner struct {
	title atomic.Pointer[string]
}

// NewSlogSpinner creates a spinner that logs via slog.
func NewSlogSpinner(title string) Spinner {
	s := &slogSpinner{}
	s.title.Store(&title)
	return s
}

func (s *slogSpinner) SetTitle(title string) {
	s.title.Store(&title)
	slog.Info("spinner: set title", "title", title)
}

func (s *slogSpinner) Start(_ context.Context) {
	slog.Info("spinner: start", "title", *s.title.Load())
}

func (s *slogSpinner) Stop() {
	slog.Info("spinner: stop", "title", *s.title.Load())
}
