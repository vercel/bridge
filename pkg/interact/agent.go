package interact

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/term"
)

// OutputFormat controls how the CLI presents output.
const (
	OutputPretty = "pretty"
	OutputJSON   = "json"
)

// outputFormat holds the global output format set by --output.
var outputFormat atomic.Pointer[string]

// SetOutputFormat sets the global output format. Typically called from the
// root Before hook after parsing --output.
func SetOutputFormat(format string) {
	outputFormat.Store(&format)
}

// GetOutputFormat returns the current output format. If SetOutputFormat has
// not been called, it defaults to "json" for agents and "pretty" for humans.
func GetOutputFormat() string {
	if v := outputFormat.Load(); v != nil && *v != "" {
		return *v
	}
	if isAgent() {
		return OutputJSON
	}
	return OutputPretty
}

// IsJSON returns true when the output format is JSON.
func IsJSON() bool {
	return GetOutputFormat() == OutputJSON
}

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

// isAgent returns true when the command is being driven by an automated agent
// rather than a human. It checks whether CI=true or stdin is not a terminal.
// Used only to derive the default output format when --output is not set.
var isAgent = sync.OnceValue(func() bool {
	if strings.EqualFold(os.Getenv("CI"), "true") {
		return true
	}
	return !term.IsTerminal(int(os.Stdin.Fd()))
})

// NewPrinter returns a printer that always logs via slog. In pretty mode
// it also writes styled output to w.
func NewPrinter(w io.Writer) Printer {
	return &printer{w: w, theme: NewTheme(), pretty: !IsJSON()}
}

// NewSpinner returns a spinner appropriate for the current output format.
// In JSON mode it logs via slog; otherwise it shows an animation.
func NewSpinner(_ io.Writer, title string) Spinner {
	if IsJSON() {
		return NewSlogSpinner(title)
	}
	return NewPrettySpinner(title)
}

// NewViewport returns the appropriate Viewport for the current environment.
func NewViewport(w io.Writer, opts ViewportOpts) Viewport {
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = defaultViewportLines
	}
	if IsJSON() {
		return &plainViewport{w: io.Discard, title: opts.Title}
	}
	return &prettyViewport{w: w, title: opts.Title, maxLines: maxLines}
}
