package interact

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Printer provides styled terminal output.
type Printer interface {
	Success(msg string)
	Successf(format string, a ...any)
	Warn(msg string)
	Warnf(format string, a ...any)
	Info(msg string)
	Infof(format string, a ...any)
	Errorf(format string, a ...any)
	Header(msg string)
	Headerf(format string, a ...any)
	KeyValue(key, value string)
	Muted(msg string)
	Mutedf(format string, a ...any)
	Newline()
	// Prompt prints a message without a trailing newline, for user input.
	Prompt(msg string)
	// Println writes an unstyled message with a trailing newline.
	Println(msg string)
	Printlnf(format string, a ...any)
}

// printer logs every call via slog. When pretty is true it also writes
// styled output to w.
type printer struct {
	w      io.Writer
	theme  *Theme
	pretty bool
}

func (p *printer) Success(msg string) {
	slog.Info(msg, "level", "success")
	if p.pretty {
		fmt.Fprintf(p.w, "%s %s\n", p.theme.Success.Render("✓"), p.theme.Bold.Render(msg))
	}
}

func (p *printer) Successf(format string, a ...any) { p.Success(fmt.Sprintf(format, a...)) }

func (p *printer) Warn(msg string) {
	slog.Warn(msg)
	if p.pretty {
		fmt.Fprintf(p.w, "%s %s\n", p.theme.Warning.Render("!"), p.theme.Warning.Render(msg))
	}
}

func (p *printer) Warnf(format string, a ...any) { p.Warn(fmt.Sprintf(format, a...)) }

func (p *printer) Info(msg string) {
	slog.Info(msg)
	if p.pretty {
		fmt.Fprintf(p.w, "%s %s\n", p.theme.Info.Render("→"), msg)
	}
}

func (p *printer) Infof(format string, a ...any) { p.Info(fmt.Sprintf(format, a...)) }

// Errorf prints a red error message with formatting. If the message contains
// newlines, only the first line is styled to avoid lipgloss mangling
// multi-line output (e.g. devcontainer build logs).
func (p *printer) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	slog.Error(msg)
	if p.pretty {
		first, rest, _ := strings.Cut(msg, "\n")
		fmt.Fprintf(p.w, "%s %s\n", p.theme.Error.Render("✗"), p.theme.Error.Render(first))
		if rest != "" {
			fmt.Fprintln(p.w, rest)
		}
	}
}

func (p *printer) Header(msg string) {
	slog.Info(msg, "level", "header")
	if p.pretty {
		fmt.Fprintf(p.w, "%s\n", p.theme.Header.Render(msg))
	}
}

func (p *printer) Headerf(format string, a ...any) { p.Header(fmt.Sprintf(format, a...)) }

func (p *printer) KeyValue(key, value string) {
	slog.Info(key, "value", value)
	if p.pretty {
		fmt.Fprintf(p.w, "  %s %s\n", p.theme.Key.Render(key+":"), p.theme.Value.Render(value))
	}
}

func (p *printer) Muted(msg string) {
	slog.Debug(msg)
	if p.pretty {
		fmt.Fprintf(p.w, "%s\n", p.theme.Muted.Render(msg))
	}
}

func (p *printer) Mutedf(format string, a ...any) { p.Muted(fmt.Sprintf(format, a...)) }

func (p *printer) Newline() {
	if p.pretty {
		fmt.Fprintln(p.w)
	}
}

func (p *printer) Prompt(msg string) {
	slog.Info(msg, "level", "prompt")
	if p.pretty {
		fmt.Fprint(p.w, msg)
	}
}

func (p *printer) Println(msg string) {
	slog.Info(msg)
	if p.pretty {
		fmt.Fprintln(p.w, msg)
	}
}

func (p *printer) Printlnf(format string, a ...any) { p.Println(fmt.Sprintf(format, a...)) }
