package interact

import (
	"context"
	"sync/atomic"
	"time"

	bubbles "github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

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

// Spinner displays an animated spinner with a title in the terminal.
type Spinner struct {
	title *atomic.Value
	prog  *tea.Program
	done  chan struct{}
}

// NewSpinner creates a new spinner. Call Start to display it.
func NewSpinner(title string) *Spinner {
	t := &atomic.Value{}
	t.Store(title)
	return &Spinner{title: t, done: make(chan struct{})}
}

// SetTitle updates the spinner title while it is running.
func (s *Spinner) SetTitle(title string) {
	s.title.Store(title)
}

// Start begins displaying the spinner. It blocks until Stop is called.
// Typically run in a goroutine.
func (s *Spinner) Start(ctx context.Context) error {
	theme := NewTheme()
	model := &spinnerModel{
		spinner:    bubbles.New(bubbles.WithSpinner(bridgeFrames), bubbles.WithStyle(theme.Spinner)),
		title:      s.title,
		titleStyle: theme.Muted,
	}

	s.prog = tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil))

	_, err := s.prog.Run()
	close(s.done)
	return err
}

// Stop stops the spinner and waits for it to fully exit.
func (s *Spinner) Stop() {
	if s.prog != nil {
		s.prog.Send(stopMsg{})
		<-s.done
	}
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
