package interact

import (
	"fmt"
	"io"
	"strings"
)

// Printer provides styled terminal output.
type Printer interface {
	Success(msg string)
	Warn(msg string)
	Info(msg string)
	Errorf(format string, a ...any)
	Header(msg string)
	KeyValue(key, value string)
	Muted(msg string)
	Newline()
	// Prompt prints a message without a trailing newline, for user input.
	Prompt(msg string)
	// Println writes an unstyled message with a trailing newline.
	Println(msg string)
}

type printer struct {
	w     io.Writer
	theme *Theme
}

// NewPrinter returns a Printer that writes styled output to w.
func NewPrinter(w io.Writer) Printer {
	return &printer{w: w, theme: NewTheme()}
}

func (p *printer) Success(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Success.Render("✓"), p.theme.Bold.Render(msg))
}

func (p *printer) Warn(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Warning.Render("!"), p.theme.Warning.Render(msg))
}

func (p *printer) Info(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Info.Render("→"), msg)
}

// Errorf prints a red error message with formatting. If the message contains
// newlines, only the first line is styled to avoid lipgloss mangling
// multi-line output (e.g. devcontainer build logs).
func (p *printer) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	first, rest, _ := strings.Cut(msg, "\n")
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Error.Render("✗"), p.theme.Error.Render(first))
	if rest != "" {
		fmt.Fprintln(p.w, rest)
	}
}

func (p *printer) Header(msg string) {
	fmt.Fprintf(p.w, "%s\n", p.theme.Header.Render(msg))
}

func (p *printer) KeyValue(key, value string) {
	fmt.Fprintf(p.w, "  %s %s\n", p.theme.Key.Render(key+":"), p.theme.Value.Render(value))
}

func (p *printer) Muted(msg string) {
	fmt.Fprintf(p.w, "%s\n", p.theme.Muted.Render(msg))
}

func (p *printer) Newline() {
	fmt.Fprintln(p.w)
}

func (p *printer) Prompt(msg string) {
	fmt.Fprint(p.w, msg)
}

func (p *printer) Println(msg string) {
	fmt.Fprintln(p.w, msg)
}
