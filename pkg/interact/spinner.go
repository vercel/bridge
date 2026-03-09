package interact

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	bubbles "github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Spinner displays a progress indicator with a title.
type Spinner interface {
	SetTitle(title string)
	Start(ctx context.Context)
	Stop()
}

// bridgeFrames animates a deck being laid between two ornate pillars.
var bridgeFrames = bubbles.Spinner{
	Frames: []string{
		" ╥           ╥ ",
		" ╥           ╥ ",
		" ╥═          ╥ ",
		" ╥══         ╥ ",
		" ╥═══        ╥ ",
		" ╥════       ╥ ",
		" ╥═════      ╥ ",
		" ╥══════     ╥ ",
		" ╥═══════    ╥ ",
		" ╥════════   ╥ ",
		" ╥═════════  ╥ ",
		" ╥══════════ ╥ ",
		" ╥═══════════╥ ",
		" ╥═══════════╥ ",
		" ╥═══════════╥ ",
		" ╥═══════════╥ ",
		" ╥══════════ ╥ ",
		" ╥═════════  ╥ ",
		" ╥════════   ╥ ",
		" ╥═══════    ╥ ",
		" ╥══════     ╥ ",
		" ╥═════      ╥ ",
		" ╥════       ╥ ",
		" ╥═══        ╥ ",
		" ╥══         ╥ ",
		" ╥═          ╥ ",
	},
	FPS: time.Second / 8,
}

// prettySpinner displays an animated spinner with a title in the terminal.
type prettySpinner struct {
	title *atomic.Value
	prog  *tea.Program
	done  chan struct{}
}

// NewPrettySpinner creates a new animated spinner. Call Start to display it.
func NewPrettySpinner(title string) Spinner {
	t := &atomic.Value{}
	t.Store(title)
	return &prettySpinner{title: t}
}

func (s *prettySpinner) SetTitle(title string) {
	s.title.Store(title)
}

func (s *prettySpinner) Start(ctx context.Context) {
	s.done = make(chan struct{})

	theme := NewTheme()
	model := &spinnerModel{
		spinner:    bubbles.New(bubbles.WithSpinner(bridgeFrames), bubbles.WithStyle(theme.Spinner)),
		title:      s.title,
		titleStyle: theme.Muted,
	}

	s.prog = tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil))

	go func() {
		s.prog.Run()
		close(s.done)
	}()
}

func (s *prettySpinner) Stop() {
	if s.prog != nil {
		s.prog.Send(stopMsg{})
		<-s.done
		s.prog = nil
	}
}

// plainSpinner prints the title with "..." and does not animate.
type plainSpinner struct {
	w     io.Writer
	title *atomic.Value
	done  chan struct{}
}

// NewPlainSpinner creates a non-animated spinner for agent output.
func NewPlainSpinner(w io.Writer, title string) Spinner {
	t := &atomic.Value{}
	t.Store(title)
	return &plainSpinner{w: w, title: t}
}

func (s *plainSpinner) SetTitle(title string) {
	s.title.Store(title)
	fmt.Fprintf(s.w, "%s...\n", title)
}

func (s *plainSpinner) Start(_ context.Context) {
	s.done = make(chan struct{})
	t, _ := s.title.Load().(string)
	fmt.Fprintf(s.w, "%s...\n", t)
}

func (s *plainSpinner) Stop() {
	if s.done == nil {
		return
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.done = nil
}

type stopMsg struct{}

const minSpinnerDuration = 500 * time.Millisecond

type minTimeMsg struct{}

type spinnerModel struct {
	spinner        bubbles.Model
	title          *atomic.Value
	titleStyle     lipgloss.Style
	stopped        bool
	minTimeElapsed bool
}

func (m *spinnerModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		tea.Tick(minSpinnerDuration, func(time.Time) tea.Msg {
			return minTimeMsg{}
		}),
	)
}

func (m *spinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case stopMsg:
		m.stopped = true
		if m.minTimeElapsed {
			return m, tea.Quit
		}
		return m, nil
	case minTimeMsg:
		m.minTimeElapsed = true
		if m.stopped {
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyMsg:
		if msg.(tea.KeyMsg).String() == "ctrl+c" {
			return m, tea.Interrupt
		}
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m *spinnerModel) View() string {
	t, _ := m.title.Load().(string)
	return m.spinner.View() + m.titleStyle.Render(t)
}
