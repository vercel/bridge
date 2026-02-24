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

// RunWithSpinner runs fn while displaying a themed spinner with the given title.
// The spinner is guaranteed to display for at least minSpinnerDuration to avoid flashing.
func RunWithSpinner(ctx context.Context, title string, fn func(ctx context.Context) error) error {
	return RunSteps(ctx, []Step{{Title: title, Run: fn}})
}

// Step is a named unit of work for a StepSpinner.
type Step struct {
	Title string
	Run   func(ctx context.Context) error
}

// RunSteps runs multiple steps sequentially under a single spinner, updating
// the title between steps to avoid terminal jitter from re-creating the spinner.
func RunSteps(ctx context.Context, steps []Step) error {
	if len(steps) == 0 {
		return nil
	}

	theme := NewTheme()
	title := &atomic.Value{}
	title.Store(steps[0].Title)

	model := &stepModel{
		spinner:    bubbles.New(bubbles.WithSpinner(bridgeFrames), bubbles.WithStyle(theme.Spinner)),
		title:      title,
		titleStyle: theme.Muted,
		ctx:        ctx,
	}

	model.action = func(actionCtx context.Context) error {
		for _, s := range steps {
			title.Store(s.Title)
			if err := s.Run(actionCtx); err != nil {
				return err
			}
		}
		return nil
	}

	p := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil))
	if _, err := p.Run(); err != nil {
		return err
	}
	if model.doneErr != nil {
		return *model.doneErr
	}
	return nil
}

// stepModel is a bubbletea model that reads the title from an atomic value,
// allowing the action goroutine to update the title mid-run.
type stepModel struct {
	spinner        bubbles.Model
	title          *atomic.Value
	titleStyle     lipgloss.Style
	action         func(context.Context) error
	ctx            context.Context
	doneErr        *error // non-nil once the action has finished
	minTimeElapsed bool   // true once minSpinnerDuration has passed
}

type stepDoneMsg struct{ err error }
type minTimeMsg struct{}

const minSpinnerDuration = 500 * time.Millisecond

func (m *stepModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			if m.action != nil {
				err := m.action(m.ctx)
				return stepDoneMsg{err}
			}
			return nil
		},
		tea.Tick(minSpinnerDuration, func(time.Time) tea.Msg {
			return minTimeMsg{}
		}),
	)
}

func (m *stepModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case stepDoneMsg:
		m.doneErr = &msg.err
		if m.minTimeElapsed {
			return m, tea.Quit
		}
		return m, nil
	case minTimeMsg:
		m.minTimeElapsed = true
		if m.doneErr != nil {
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Interrupt
		}
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m *stepModel) View() string {
	t, _ := m.title.Load().(string)
	return m.spinner.View() + m.titleStyle.Render(t)
}
