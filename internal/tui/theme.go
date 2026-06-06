package tui

import "github.com/charmbracelet/lipgloss"

// Theme tokens for the TUI surfaces, mirroring the v2 prototype
// (docs/prototypes/v2-tui.prototype.html). Chromatic accents are fixed hex —
// they read on both light and dark terminals — while the neutrals (text,
// dimmed text, separators) adapt to the terminal background.
var (
	// Neutrals — adapt to the terminal's light/dark background.
	colorInk  = lipgloss.AdaptiveColor{Light: "#3a3f5a", Dark: "#c0caf5"}
	colorDim  = lipgloss.AdaptiveColor{Light: "#8089a8", Dark: "#565f89"}
	colorLine = lipgloss.AdaptiveColor{Light: "#c4c8da", Dark: "#2c3047"}

	// Chromatic accents — fixed, straight from the prototype palette.
	colorID     = lipgloss.Color("#7dcfff")
	colorSel    = lipgloss.Color("#bb9af7")
	colorUrgent = lipgloss.Color("#f7768e")
	colorHigh   = lipgloss.Color("#ff9e64")
	colorMedium = lipgloss.Color("#7aa2f7")
	colorLow    = lipgloss.Color("#6b7394")
	colorBlock  = lipgloss.Color("#f7768e")
)

var (
	styleText   = lipgloss.NewStyle().Foreground(colorInk)
	styleDim    = lipgloss.NewStyle().Foreground(colorDim)
	styleLine   = lipgloss.NewStyle().Foreground(colorLine)
	styleActive = lipgloss.NewStyle().Foreground(colorSel)
	styleKey    = lipgloss.NewStyle().Foreground(colorSel).Bold(true)
)
