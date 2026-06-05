package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gimigliano/ito/internal/store"
)

const digestWidth = 88

type model struct {
	store      *store.Store
	project    store.Project
	sections   []digestSection
	focusIndex int
	loadErr    error
}

type digestSection struct {
	Label    string
	Issues   []store.Issue
	selected int
}

var statusFlow = []struct {
	status string
	label  string
}{
	{"backlog", "BACKLOG"},
	{"todo", "TODO"},
	{"in_progress", "IN PROGRESS"},
	{"in_review", "IN REVIEW"},
	{"done", "DONE"},
}

func Run(st *store.Store, project store.Project) error {
	if st == nil {
		return fmt.Errorf("store is required")
	}
	_, err := tea.NewProgram(newModel(st, project), tea.WithAltScreen()).Run()
	return err
}

func newModel(st *store.Store, project store.Project) model {
	m := model{
		store:   st,
		project: project,
	}
	m.reloadDigest()
	return m
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
		case "tab":
			m.moveFocus(1)
		case "shift+tab":
			m.moveFocus(-1)
		case "up":
			m.moveSelection(-1)
		case "down":
			m.moveSelection(1)
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.loadErr != nil {
		return fmt.Sprintf("ito · [1] digest · [2] board\n\ncould not load Issues: %v\n\nq quit", m.loadErr)
	}

	lines := []string{
		header(m.project.Name, m.issueCount()),
		strings.Repeat("─", digestWidth),
		"",
	}
	for i, section := range m.sections {
		focused := i == m.focusIndex
		marker := " "
		if focused {
			marker = "▌"
		}
		lines = append(lines, fmt.Sprintf(" %s▾ %s  (%d)", marker, section.Label, len(section.Issues)))
		if len(section.Issues) == 0 {
			lines = append(lines, "    no Issues")
		}
		for j, issue := range section.Issues {
			prefix := "   "
			if focused && j == section.selected {
				prefix = " ▸ "
			}
			lines = append(lines, prefix+renderIssueRow(issue))
		}
		lines = append(lines, "")
	}
	lines = append(lines, " tab focus   ↑↓ select   ⏎ open   s status   h hide   / filter   : cmd   q quit")
	return strings.Join(lines, "\n")
}

func (m *model) reloadDigest() {
	m.sections = make([]digestSection, 0, len(statusFlow))
	m.loadErr = nil
	for _, entry := range statusFlow {
		issues, err := m.store.ListIssues(store.ListOptions{
			ProjectID: m.project.ID,
			Status:    entry.status,
		})
		if err != nil {
			m.loadErr = err
			return
		}
		m.sections = append(m.sections, digestSection{
			Label:  entry.label,
			Issues: issues,
		})
	}
}

func (m model) issueCount() int {
	total := 0
	for _, section := range m.sections {
		total += len(section.Issues)
	}
	return total
}

func (m *model) moveFocus(delta int) {
	if len(m.sections) == 0 {
		return
	}
	m.focusIndex = (m.focusIndex + delta + len(m.sections)) % len(m.sections)
}

func (m *model) moveSelection(delta int) {
	if len(m.sections) == 0 {
		return
	}
	section := &m.sections[m.focusIndex]
	if len(section.Issues) == 0 {
		return
	}
	section.selected = min(max(section.selected+delta, 0), len(section.Issues)-1)
}

func header(projectName string, issueCount int) string {
	left := "ito · [1] digest · [2] board"
	right := fmt.Sprintf("%d issues   %s", issueCount, projectName)
	width := digestWidth
	if len(left)+len(right)+1 >= width {
		return left + " " + right
	}
	return left + strings.Repeat(" ", width-len(left)-len(right)) + right
}

func renderIssueRow(issue store.Issue) string {
	left := fmt.Sprintf("%s %s %s", priorityMark(issue.Priority), issue.ID, truncate(issue.Title, 42))
	parts := []string{left}
	if len(issue.BlockedBy) > 0 {
		parts = append(parts, "⊘ "+strings.Join(issue.BlockedBy, ","))
	}
	if len(issue.Labels) > 0 {
		parts = append(parts, strings.Join(issue.Labels, " "))
	}
	return strings.Join(parts, "   ")
}

func priorityMark(priority string) string {
	switch priority {
	case "urgent":
		return "●"
	case "high":
		return "▲"
	case "medium":
		return "◆"
	default:
		return "·"
	}
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	if max <= 1 {
		return "…"
	}
	return value[:max-1] + "…"
}
