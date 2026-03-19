package interact

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const defaultViewportLines = 12

// Viewport displays streaming output from a reader. For interactive terminals
// it shows a scrolling window; for agents it streams lines directly.
type Viewport interface {
	// Run reads from r and displays output until EOF or context cancellation.
	Run(ctx context.Context, r io.Reader) error
	// Clear removes the viewport output from the terminal. Call after Run
	// on success; skip on failure so the user can see the output.
	Clear()
}

// ViewportOpts configures a Viewport.
type ViewportOpts struct {
	// Title is displayed above the viewport. Optional.
	Title string
	// MaxLines is the number of visible lines. Defaults to 12.
	MaxLines int
}

// --- pretty (interactive viewport) ---

type prettyViewport struct {
	w              io.Writer
	title          string
	maxLines       int
	renderedHeight int
}

type viewportLineMsg string
type viewportDoneMsg struct{}

func (v *prettyViewport) Run(ctx context.Context, r io.Reader) error {
	model := newViewportModel(v.title, v.maxLines)

	prog := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil))

	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			prog.Send(viewportLineMsg(scanner.Text()))
		}
		prog.Send(viewportDoneMsg{})
	}()

	_, err := prog.Run()

	// Calculate how many lines the viewport rendered so Clear() can erase them.
	// border top + maxLines + border bottom + title line (if present).
	v.renderedHeight = v.maxLines + 2
	if v.title != "" {
		v.renderedHeight++
	}

	return err
}

func (v *prettyViewport) Clear() {
	if v.renderedHeight <= 0 {
		return
	}
	// Move cursor up and clear each line.
	for i := 0; i < v.renderedHeight; i++ {
		fmt.Fprint(v.w, "\033[A\033[2K")
	}
}

// --- bubbletea model ---

type viewportModel struct {
	title          string
	viewport       viewport.Model
	lines          []string
	maxLines       int
	width          int
	theme          *Theme
	borderStyle    lipgloss.Style
	done           bool
	minTimeElapsed bool
}

func newViewportModel(title string, maxLines int) *viewportModel {
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("241")).
		Padding(0, 1)

	// Initial viewport sized for default width; resized on WindowSizeMsg.
	vp := viewport.New(76, maxLines) // 80 - border/padding
	vp.Style = lipgloss.NewStyle()

	return &viewportModel{
		title:       title,
		viewport:    vp,
		maxLines:    maxLines,
		width:       80,
		theme:       NewTheme(),
		borderStyle: border,
	}
}

func (m *viewportModel) Init() tea.Cmd {
	return tea.Tick(minSpinnerDuration, func(time.Time) tea.Msg {
		return minTimeMsg{}
	})
}

func (m *viewportModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.viewport.Width = msg.Width - m.borderStyle.GetHorizontalFrameSize()
		m.viewport.Height = m.maxLines
	case viewportLineMsg:
		m.lines = append(m.lines, string(msg))
		if len(m.lines) > 500 {
			m.lines = m.lines[len(m.lines)-500:]
		}
		m.viewport.SetContent(strings.Join(m.lines, "\n"))
		m.viewport.GotoBottom()
	case viewportDoneMsg:
		m.done = true
		if m.minTimeElapsed {
			return m, tea.Quit
		}
	case minTimeMsg:
		m.minTimeElapsed = true
		if m.done {
			return m, tea.Quit
		}
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Interrupt
		}
	}

	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m *viewportModel) View() string {
	box := m.borderStyle.Width(m.width - m.borderStyle.GetHorizontalFrameSize())
	content := box.Render(m.viewport.View())

	if m.title != "" {
		header := m.theme.Muted.Render(m.title)
		return header + "\n" + content
	}
	return content
}

// --- plain (agent streaming) ---

type plainViewport struct {
	w     io.Writer
	title string
}

func (v *plainViewport) Run(_ context.Context, r io.Reader) error {
	if v.title != "" {
		fmt.Fprintf(v.w, "%s\n", v.title)
	}
	_, err := io.Copy(v.w, r)
	return err
}

func (v *plainViewport) Clear() {}
