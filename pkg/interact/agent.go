package interact

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

type spinnerKey struct{}

// WithSpinner returns a new context carrying the given Spinner.
func WithSpinner(ctx context.Context, sp Spinner) context.Context {
	return context.WithValue(ctx, spinnerKey{}, sp)
}

// GetSpinner retrieves the Spinner from the context, or nil if none is set.
func GetSpinner(ctx context.Context) Spinner {
	sp, _ := ctx.Value(spinnerKey{}).(Spinner)
	return sp
}

// IsAgent returns true when the command is being driven by an automated agent
// rather than a human. It checks whether CI=true or stdin is not a terminal.
var IsAgent = sync.OnceValue(func() bool {
	if strings.EqualFold(os.Getenv("CI"), "true") {
		return true
	}
	return !term.IsTerminal(int(os.Stdin.Fd()))
})

// NewPrinter returns a pretty printer for humans or a plain printer for agents.
func NewPrinter(w io.Writer) Printer {
	if IsAgent() {
		return NewPlainPrinter(w)
	}
	return NewPrettyPrinter(w)
}

// NewSpinner returns an animated spinner for humans or a plain spinner for agents.
func NewSpinner(w io.Writer, title string) Spinner {
	if IsAgent() {
		return NewPlainSpinner(w, title)
	}
	return NewPrettySpinner(title)
}
