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

// prettyPrinter provides styled terminal output with colors and icons.
type prettyPrinter struct {
	w     io.Writer
	theme *Theme
}

// NewPrettyPrinter returns a Printer that writes styled output to w.
func NewPrettyPrinter(w io.Writer) Printer {
	return &prettyPrinter{w: w, theme: NewTheme()}
}

func (p *prettyPrinter) Success(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Success.Render("✓"), p.theme.Bold.Render(msg))
}

func (p *prettyPrinter) Warn(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Warning.Render("!"), p.theme.Warning.Render(msg))
}

func (p *prettyPrinter) Info(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Info.Render("→"), msg)
}

// Errorf prints a red error message with formatting. If the message contains
// newlines, only the first line is styled to avoid lipgloss mangling
// multi-line output (e.g. devcontainer build logs).
func (p *prettyPrinter) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	first, rest, _ := strings.Cut(msg, "\n")
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Error.Render("✗"), p.theme.Error.Render(first))
	if rest != "" {
		fmt.Fprintln(p.w, rest)
	}
}

func (p *prettyPrinter) Header(msg string) {
	fmt.Fprintf(p.w, "%s\n", p.theme.Header.Render(msg))
}

func (p *prettyPrinter) KeyValue(key, value string) {
	fmt.Fprintf(p.w, "  %s %s\n", p.theme.Key.Render(key+":"), p.theme.Value.Render(value))
}

func (p *prettyPrinter) Muted(msg string) {
	fmt.Fprintf(p.w, "%s\n", p.theme.Muted.Render(msg))
}

func (p *prettyPrinter) Newline() {
	fmt.Fprintln(p.w)
}

func (p *prettyPrinter) Prompt(msg string) {
	fmt.Fprint(p.w, msg)
}

func (p *prettyPrinter) Println(msg string) {
	fmt.Fprintln(p.w, msg)
}

// plainPrinter provides unstyled output intended for automated agents.
type plainPrinter struct {
	w io.Writer
}

// NewPlainPrinter returns a Printer that writes plain, unstyled output to w.
func NewPlainPrinter(w io.Writer) Printer {
	return &plainPrinter{w: w}
}

func (p *plainPrinter) Success(msg string) {
	fmt.Fprintln(p.w, msg)
}

func (p *plainPrinter) Warn(msg string) {
	fmt.Fprintf(p.w, "warning: %s\n", msg)
}

func (p *plainPrinter) Info(msg string) {
	fmt.Fprintln(p.w, msg)
}

func (p *plainPrinter) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintf(p.w, "error: %s\n", msg)
}

func (p *plainPrinter) Header(msg string) {
	fmt.Fprintln(p.w, msg)
}

func (p *plainPrinter) KeyValue(key, value string) {
	fmt.Fprintf(p.w, "%s: %s\n", key, value)
}

func (p *plainPrinter) Muted(msg string) {
	fmt.Fprintln(p.w, msg)
}

func (p *plainPrinter) Newline() {
	fmt.Fprintln(p.w)
}

func (p *plainPrinter) Prompt(msg string) {
	fmt.Fprint(p.w, msg)
}

func (p *plainPrinter) Println(msg string) {
	fmt.Fprintln(p.w, msg)
}
