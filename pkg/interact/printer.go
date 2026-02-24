package interact

import (
	"fmt"
	"io"
)

// Printer writes themed, styled messages to a writer.
type Printer struct {
	w     io.Writer
	theme *Theme
}

// NewPrinter returns a Printer that writes styled output to w.
func NewPrinter(w io.Writer) *Printer {
	return &Printer{w: w, theme: NewTheme()}
}

// Success prints a green check-mark message.
func (p *Printer) Success(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Success.Render("✓"), p.theme.Bold.Render(msg))
}

// Warn prints a yellow warning message.
func (p *Printer) Warn(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Warning.Render("!"), p.theme.Warning.Render(msg))
}

// Info prints a blue info message.
func (p *Printer) Info(msg string) {
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Info.Render("→"), msg)
}

// Errorf prints a red error message with formatting.
func (p *Printer) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintf(p.w, "%s %s\n", p.theme.Error.Render("✗"), p.theme.Error.Render(msg))
}

// Header prints a bold, underlined header.
func (p *Printer) Header(msg string) {
	fmt.Fprintf(p.w, "%s\n", p.theme.Header.Render(msg))
}

// KeyValue prints a dimmed key with a bold value.
func (p *Printer) KeyValue(key, value string) {
	fmt.Fprintf(p.w, "  %s %s\n", p.theme.Key.Render(key+":"), p.theme.Value.Render(value))
}

// Muted prints a dimmed message.
func (p *Printer) Muted(msg string) {
	fmt.Fprintf(p.w, "%s\n", p.theme.Muted.Render(msg))
}

// Newline prints a blank line.
func (p *Printer) Newline() {
	fmt.Fprintln(p.w)
}
