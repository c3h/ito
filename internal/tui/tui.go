package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gimigliano/ito/internal/store"
)

const digestWidth = 88

// detailBodyWidth keeps the Issue body at a readable prose measure, narrower
// than the full digestWidth frame (see docs/prototypes/NOTES.md).
const detailBodyWidth = 80

// linkIDWidth is the column the linked Issue id occupies before its title in
// the detail view, so the titles line up across blocked_by / relates_to rows.
const linkIDWidth = 8

type viewMode string

const (
	viewDigest viewMode = "digest"
	viewIssue  viewMode = "issue"
)

type model struct {
	store       *store.Store
	project     store.Project
	sections    []digestSection
	focusIndex  int
	mode        viewMode
	detailIssue store.Issue
	linkTitles  map[string]string
	loadErr     error
}

type digestSection struct {
	Label    string
	Issues   []store.Issue
	selected int
}

// statusLabel renders a store status as its Digest section heading
// (in_progress → "IN PROGRESS"), keeping the flow order owned by store.Statuses.
func statusLabel(status string) string {
	return strings.ToUpper(strings.ReplaceAll(status, "_", " "))
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
		store:      st,
		project:    project,
		mode:       viewDigest,
		linkTitles: map[string]string{},
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
		case "esc":
			if m.mode == viewIssue {
				m.mode = viewDigest
			}
		case "enter":
			if m.mode == viewDigest {
				m.openSelectedIssue()
			}
		case "tab":
			if m.mode == viewDigest {
				m.moveFocus(1)
			}
		case "shift+tab":
			if m.mode == viewDigest {
				m.moveFocus(-1)
			}
		case "up":
			if m.mode == viewIssue {
				m.moveDetailIssue(-1)
			} else {
				m.moveSelection(-1)
			}
		case "down":
			if m.mode == viewIssue {
				m.moveDetailIssue(1)
			} else {
				m.moveSelection(1)
			}
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.loadErr != nil {
		return fmt.Sprintf("ito · [1] digest · [2] board\n\ncould not load Issues: %v\n\nq quit", m.loadErr)
	}
	if m.mode == viewIssue {
		return m.issueDetailView()
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
	m.sections = make([]digestSection, 0, len(store.Statuses))
	m.loadErr = nil
	for _, status := range store.Statuses {
		issues, err := m.store.ListIssues(store.ListOptions{
			ProjectID: m.project.ID,
			Status:    status,
		})
		if err != nil {
			m.loadErr = err
			return
		}
		m.sections = append(m.sections, digestSection{
			Label:  statusLabel(status),
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

func (m *model) openSelectedIssue() {
	issue, ok := m.selectedIssue()
	if !ok {
		return
	}
	m.showIssue(issue)
}

// showIssue opens the read-only detail for an Issue already loaded in the
// Digest sections, so opening and prev/next navigation never re-read the store
// for data the sections already hold.
func (m *model) showIssue(issue store.Issue) {
	m.detailIssue = issue
	m.linkTitles = m.loadLinkTitles(issue)
	m.focusIssue(issue.ID)
	m.mode = viewIssue
}

func (m model) selectedIssue() (store.Issue, bool) {
	if len(m.sections) == 0 || m.focusIndex < 0 || m.focusIndex >= len(m.sections) {
		return store.Issue{}, false
	}
	section := m.sections[m.focusIndex]
	if len(section.Issues) == 0 || section.selected < 0 || section.selected >= len(section.Issues) {
		return store.Issue{}, false
	}
	return section.Issues[section.selected], true
}

func (m *model) moveDetailIssue(delta int) {
	issues := m.allIssues()
	if len(issues) == 0 {
		return
	}
	idx := 0
	for i, issue := range issues {
		if issue.ID == m.detailIssue.ID {
			idx = i
			break
		}
	}
	next := min(max(idx+delta, 0), len(issues)-1)
	if issues[next].ID != m.detailIssue.ID {
		m.showIssue(issues[next])
	}
}

func (m model) allIssues() []store.Issue {
	var issues []store.Issue
	for _, section := range m.sections {
		issues = append(issues, section.Issues...)
	}
	return issues
}

func (m *model) focusIssue(id string) {
	for i := range m.sections {
		for j := range m.sections[i].Issues {
			if m.sections[i].Issues[j].ID == id {
				m.focusIndex = i
				m.sections[i].selected = j
				return
			}
		}
	}
}

func (m model) loadLinkTitles(issue store.Issue) map[string]string {
	titles := map[string]string{}
	resolve := func(ids []string) {
		for _, id := range ids {
			if linked, err := m.store.FindIssue(m.project, id); err == nil {
				titles[id] = linked.Title
			}
		}
	}
	resolve(issue.BlockedBy)
	resolve(issue.RelatesTo)
	return titles
}

func (m model) issueDetailView() string {
	issue := m.detailIssue
	lines := []string{
		issueHeader(m.project.Name, issue),
		strings.Repeat("─", digestWidth),
		"",
		fmt.Sprintf(" %s   ·   %s   ·   %s", issue.Status, issue.Priority, strings.Join(issue.Labels, "  ")),
		"",
	}
	for _, id := range issue.BlockedBy {
		lines = append(lines, " blocked by   "+padRight(id, linkIDWidth)+m.linkTitles[id])
	}
	for _, id := range issue.RelatesTo {
		lines = append(lines, " relates to   "+padRight(id, linkIDWidth)+m.linkTitles[id])
	}
	lines = append(lines,
		"",
		" created      "+issue.Created,
		" updated      "+issue.Updated,
		"",
		strings.Repeat("─", digestWidth),
		"",
	)
	for _, line := range strings.Split(issue.Body, "\n") {
		for _, wrapped := range wrapLine(line, detailBodyWidth) {
			lines = append(lines, " "+wrapped)
		}
	}
	lines = append(lines, "", " esc back   ↑↓ prev/next   r refresh   : cmd   q quit")
	return strings.Join(lines, "\n")
}

func header(projectName string, issueCount int) string {
	left := "ito · [1] digest · [2] board"
	right := fmt.Sprintf("%d issues   %s", issueCount, projectName)
	return padBetween(left, right, digestWidth)
}

func issueHeader(projectName string, issue store.Issue) string {
	left := fmt.Sprintf("ito · %s · %s", issue.ID, issue.Title)
	maxLeft := digestWidth - len(projectName) - 1
	if maxLeft < 1 {
		return truncate(left+" "+projectName, digestWidth)
	}
	return padBetween(truncate(left, maxLeft), projectName, digestWidth)
}

// padBetween left-aligns left and right-aligns right across width, keeping at
// least one space between them when the two would otherwise meet or overflow.
func padBetween(left, right string, width int) string {
	gap := width - len(left) - len(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
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
	ellipsis := "…"
	if max <= len(ellipsis) {
		return "…"
	}
	return value[:max-len(ellipsis)] + ellipsis
}

func padRight(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}

func wrapLine(value string, width int) []string {
	if value == "" {
		return []string{""}
	}
	words := strings.Fields(value)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	var current string
	for _, word := range words {
		next := word
		if current != "" {
			next = current + " " + word
		}
		if len(next) <= width {
			current = next
			continue
		}
		if current != "" {
			lines = append(lines, current)
		}
		current = word
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}
