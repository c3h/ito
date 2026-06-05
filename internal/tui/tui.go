package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gimigliano/ito/internal/store"
)

type model struct {
	store *store.Store
}

func Run(st *store.Store) error {
	if st == nil {
		return fmt.Errorf("store is required")
	}
	_, err := tea.NewProgram(newModel(st), tea.WithAltScreen()).Run()
	return err
}

func newModel(st *store.Store) model {
	return model{store: st}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() string {
	return strings.Join([]string{
		"ito Digest",
		"",
		"No issues loaded yet.",
		"",
		"q quits",
	}, "\n")
}
