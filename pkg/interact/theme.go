package interact

import "github.com/charmbracelet/lipgloss"

// Adaptive colors that auto-detect dark/light terminal background.
var (
	colorGreen  = lipgloss.AdaptiveColor{Light: "#00873E", Dark: "#3BDB80"}
	colorYellow = lipgloss.AdaptiveColor{Light: "#B8860B", Dark: "#FFD700"}
	colorRed    = lipgloss.AdaptiveColor{Light: "#CC0000", Dark: "#FF5555"}
	colorBlue   = lipgloss.AdaptiveColor{Light: "#0057B8", Dark: "#66B2FF"}
	colorMuted  = lipgloss.AdaptiveColor{Light: "#666666", Dark: "#999999"}
	colorBold   = lipgloss.AdaptiveColor{Light: "#1A1A1A", Dark: "#F0F0F0"}
)

// Theme holds lipgloss styles for consistent terminal output.
type Theme struct {
	Success lipgloss.Style
	Warning lipgloss.Style
	Error   lipgloss.Style
	Info    lipgloss.Style
	Muted   lipgloss.Style
	Header  lipgloss.Style
	Key     lipgloss.Style
	Value   lipgloss.Style
	Bold    lipgloss.Style
	Spinner lipgloss.Style
}

// NewTheme returns a Theme that adapts to the terminal's color scheme.
func NewTheme() *Theme {
	return &Theme{
		Success: lipgloss.NewStyle().Foreground(colorGreen),
		Warning: lipgloss.NewStyle().Foreground(colorYellow),
		Error:   lipgloss.NewStyle().Foreground(colorRed),
		Info:    lipgloss.NewStyle().Foreground(colorBlue),
		Muted:   lipgloss.NewStyle().Foreground(colorMuted),
		Header:  lipgloss.NewStyle().Foreground(colorBold).Bold(true).Underline(true),
		Key:     lipgloss.NewStyle().Foreground(colorMuted),
		Value:   lipgloss.NewStyle().Foreground(colorBold).Bold(true),
		Bold:    lipgloss.NewStyle().Foreground(colorBold).Bold(true),
		Spinner: lipgloss.NewStyle().Foreground(colorBlue),
	}
}
