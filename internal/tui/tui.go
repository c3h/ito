package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gimigliano/ito/internal/store"
)

const digestWidth = 88

// Digest rows View() draws besides Issues: digestChromeLines (header, divider,
// blank, shortcut bar) and sectionChromeLines per section (heading + blank).
const (
	digestChromeLines  = 4
	sectionChromeLines = 2
)

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
	viewLabels viewMode = "labels"
)

var priorityCycle = []string{"low", "medium", "high", "urgent"}

type model struct {
	store       *store.Store
	project     store.Project
	sections    []digestSection
	focusIndex  int
	mode        viewMode
	detailIssue store.Issue
	linkTitles  map[string]string
	labelCursor int
	filterOpen  bool
	filterQuery string
	loadErr     error
	height      int
}

type digestSection struct {
	Label    string
	Issues   []store.Issue
	selected int
	hidden   bool
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
		if m.filterOpen {
			switch msg.Type {
			case tea.KeyCtrlC:
				return m, tea.Quit
			case tea.KeyEsc:
				m.filterOpen = false
				m.filterQuery = ""
			case tea.KeyBackspace:
				if m.filterQuery != "" {
					runes := []rune(m.filterQuery)
					m.filterQuery = string(runes[:len(runes)-1])
				}
			case tea.KeyRunes:
				m.filterQuery += string(msg.Runes)
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			switch m.mode {
			case viewLabels:
				m.mode = viewIssue
			case viewIssue:
				m.mode = viewDigest
			}
		case "/":
			if m.mode == viewDigest {
				m.filterOpen = true
			}
		case "enter":
			switch m.mode {
			case viewDigest:
				m.openSelectedIssue()
			case viewLabels:
				m.toggleFocusedLabel()
			}
		case "tab":
			if m.mode == viewDigest {
				m.moveFocus(1)
			}
		case "h":
			if m.mode == viewDigest {
				m.toggleFocusedSection()
			}
		case "s":
			if m.mode != viewLabels {
				m.moveSelectedIssueStatus()
			}
		case "p":
			if m.mode == viewIssue {
				m.cycleDetailIssuePriority()
			}
		case "l":
			if m.mode == viewIssue {
				m.openLabelPicker()
			}
		case "shift+tab":
			if m.mode == viewDigest {
				m.moveFocus(-1)
			}
		case "up":
			switch m.mode {
			case viewIssue:
				m.moveDetailIssue(-1)
			case viewLabels:
				m.moveLabelCursor(-1)
			default:
				m.moveSelection(-1)
			}
		case "down":
			switch m.mode {
			case viewIssue:
				m.moveDetailIssue(1)
			case viewLabels:
				m.moveLabelCursor(1)
			default:
				m.moveSelection(1)
			}
		}
	case tea.WindowSizeMsg:
		m.height = msg.Height
	}
	return m, nil
}

func (m model) View() string {
	if m.loadErr != nil {
		return fmt.Sprintf("ito · [1] digest · [2] board\n\ncould not load Issues: %v\n\nq quit", m.loadErr)
	}
	if m.mode == viewLabels {
		return m.labelPickerView()
	}
	if m.mode == viewIssue {
		return m.issueDetailView()
	}

	sections := m.digestSections()
	lines := []string{
		header(m.project.Name, issueCount(sections)),
		strings.Repeat("─", digestWidth),
		"",
	}
	windows := m.digestWindows(sections)
	filtering := strings.TrimSpace(m.filterQuery) != ""
	for i, section := range sections {
		if filtering && len(section.Issues) == 0 {
			continue // while filtering, only sections with matches are shown
		}
		focused := i == m.focusIndex
		marker := " "
		if focused {
			marker = "▌"
		}
		if section.hidden {
			lines = append(lines, fmt.Sprintf(" %s▸ %s (%d) · h to show", marker, section.Label, len(section.Issues)))
			continue
		}
		lines = append(lines, fmt.Sprintf(" %s▾ %s  (%d)", marker, section.Label, len(section.Issues)))
		if len(section.Issues) == 0 {
			lines = append(lines, "    no Issues")
		}
		window := windows[i]
		if window.showAbove {
			lines = append(lines, fmt.Sprintf("   ↑ %d more", window.start))
		}
		for j, issue := range section.Issues[window.start:window.end] {
			issueIndex := window.start + j
			prefix := "   "
			if focused && issueIndex == section.selected {
				prefix = " ▸ "
			}
			lines = append(lines, prefix+renderIssueRow(issue))
		}
		if window.showBelow {
			lines = append(lines, fmt.Sprintf("   ↓ %d more", len(section.Issues)-window.end))
		}
		lines = append(lines, "")
	}
	lines = append(lines, m.digestBottomBar(issueCount(sections), issueCount(m.sections)))
	return strings.Join(lines, "\n")
}

func (m model) digestBottomBar(matched, total int) string {
	if m.filterOpen {
		hint := fmt.Sprintf("%d of %d issues · esc to clear", matched, total)
		return " / " + m.filterQuery + "▏   " + hint
	}
	return " tab focus   ↑↓ select   ⏎ open   s status   h hide   / filter   : cmd   q quit"
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
			hidden: status == "done",
		})
	}
}

type issueWindow struct {
	start     int
	end       int
	showAbove bool
	showBelow bool
}

func (m model) digestWindows(sections []digestSection) []issueWindow {
	windows := make([]issueWindow, len(sections))
	capacities := m.digestSectionCapacities(sections)
	for i, section := range sections {
		if section.hidden {
			continue
		}
		windows[i] = visibleIssueWindow(len(section.Issues), section.selected, capacities[i])
	}
	return windows
}

func (m model) digestSectionCapacities(sections []digestSection) []int {
	capacities := make([]int, len(sections))
	showAll := func() []int {
		for i, section := range sections {
			if section.hidden {
				continue
			}
			capacities[i] = len(section.Issues)
		}
		return capacities
	}
	if m.height <= 0 {
		return showAll()
	}

	emptySections := 0
	nonEmptySections := 0
	hiddenSections := 0
	totalIssues := 0
	for _, section := range sections {
		if section.hidden {
			hiddenSections++
			continue
		}
		if len(section.Issues) == 0 {
			emptySections++
			continue
		}
		nonEmptySections++
		totalIssues += len(section.Issues)
	}
	if nonEmptySections == 0 {
		return capacities
	}

	// Each visible section spends sectionChromeLines on its heading and footer;
	// collapsed and empty sections each render as a single line instead.
	visibleSections := len(sections) - hiddenSections
	fixedLines := digestChromeLines + visibleSections*sectionChromeLines + hiddenSections + emptySections
	available := m.height - fixedLines
	if available >= totalIssues {
		return showAll()
	}
	if available < nonEmptySections {
		available = nonEmptySections
	}

	for i, section := range sections {
		if section.hidden {
			continue
		}
		if len(section.Issues) > 0 {
			capacities[i] = 1
		}
	}
	remaining := available - nonEmptySections
	for remaining > 0 {
		grew := false
		for i, section := range sections {
			if remaining == 0 {
				break
			}
			if section.hidden || capacities[i] == 0 || capacities[i] >= len(section.Issues) {
				continue
			}
			capacities[i]++
			remaining--
			grew = true
		}
		if !grew {
			break
		}
	}
	return capacities
}

func visibleIssueWindow(total, selected, lineBudget int) issueWindow {
	if total <= 0 || lineBudget <= 0 {
		return issueWindow{}
	}
	if lineBudget >= total {
		return issueWindow{end: total}
	}
	selected = min(max(selected, 0), total-1)

	issueCapacity := min(lineBudget, total)
	for {
		window := issueRange(total, selected, issueCapacity)
		indicatorLines := 0
		if window.start > 0 {
			indicatorLines++
		}
		if window.end < total {
			indicatorLines++
		}
		nextIssueCapacity := lineBudget - indicatorLines
		if nextIssueCapacity < 1 {
			nextIssueCapacity = 1
		}
		if nextIssueCapacity == issueCapacity {
			window.showAbove = window.start > 0 && issueCapacity+indicatorLines <= lineBudget
			window.showBelow = window.end < total && issueCapacity+indicatorLines <= lineBudget
			return window
		}
		issueCapacity = nextIssueCapacity
	}
}

func issueRange(total, selected, capacity int) issueWindow {
	start := min(max(selected-capacity/2, 0), total-capacity)
	return issueWindow{start: start, end: start + capacity}
}

func issueCount(sections []digestSection) int {
	total := 0
	for _, section := range sections {
		total += len(section.Issues)
	}
	return total
}

func (m model) digestSections() []digestSection {
	query := strings.TrimSpace(strings.ToLower(m.filterQuery))
	if query == "" {
		return m.sections
	}

	sections := make([]digestSection, 0, len(m.sections))
	for _, section := range m.sections {
		selectedID := ""
		if section.selected < len(section.Issues) {
			selectedID = section.Issues[section.selected].ID
		}
		filtered := section
		filtered.Issues = nil
		filtered.selected = 0
		for _, issue := range section.Issues {
			if !issueMatchesFilter(issue, query) {
				continue
			}
			if issue.ID == selectedID {
				filtered.selected = len(filtered.Issues)
			}
			filtered.Issues = append(filtered.Issues, issue)
		}
		if len(filtered.Issues) > 0 {
			filtered.hidden = false
		}
		sections = append(sections, filtered)
	}
	return sections
}

func issueMatchesFilter(issue store.Issue, query string) bool {
	if strings.Contains(strings.ToLower(issue.ID), query) || strings.Contains(strings.ToLower(issue.Title), query) {
		return true
	}
	for _, label := range issue.Labels {
		if strings.Contains(strings.ToLower(label), query) {
			return true
		}
	}
	return false
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
	if section.hidden || len(section.Issues) == 0 {
		return
	}
	section.selected = min(max(section.selected+delta, 0), len(section.Issues)-1)
}

func (m *model) toggleFocusedSection() {
	if len(m.sections) == 0 || m.focusIndex < 0 || m.focusIndex >= len(m.sections) {
		return
	}
	m.sections[m.focusIndex].hidden = !m.sections[m.focusIndex].hidden
}

func (m *model) moveSelectedIssueStatus() {
	issue, ok := m.currentIssue()
	if !ok {
		return
	}
	moved, err := m.store.Move(m.project, issue.ID, nextValue(store.Statuses, issue.Status))
	if err != nil {
		m.loadErr = err
		return
	}
	m.reloadAfterEdit(moved.Issue)
}

func (m *model) cycleDetailIssuePriority() {
	issue, ok := m.currentIssue()
	if !ok {
		return
	}
	edited, err := m.store.Edit(m.project, issue.ID, store.EditIssueOptions{
		PrioritySet: true,
		Priority:    nextValue(priorityCycle, issue.Priority),
	})
	if err != nil {
		m.loadErr = err
		return
	}
	m.reloadAfterEdit(edited.Issue)
}

func (m *model) openLabelPicker() {
	if len(store.Labels) == 0 {
		return
	}
	if _, ok := m.currentIssue(); !ok {
		return
	}
	m.labelCursor = 0
	m.mode = viewLabels
}

func (m *model) moveLabelCursor(delta int) {
	if len(store.Labels) == 0 {
		return
	}
	m.labelCursor = min(max(m.labelCursor+delta, 0), len(store.Labels)-1)
}

func (m *model) toggleFocusedLabel() {
	if m.labelCursor < 0 || m.labelCursor >= len(store.Labels) {
		return
	}
	issue, ok := m.currentIssue()
	if !ok {
		return
	}
	label := store.Labels[m.labelCursor]
	action := "add"
	if slices.Contains(issue.Labels, label) {
		action = "remove"
	}
	edited, err := m.store.Edit(m.project, issue.ID, store.EditIssueOptions{
		LabelOps: []store.LabelEditOp{{Kind: action, Label: label}},
	})
	if err != nil {
		m.loadErr = err
		return
	}
	m.reloadAfterEdit(edited.Issue)
}

// reloadAfterEdit refreshes the Digest from the store and keeps the edited
// Issue focused, re-rendering whichever detail surface is open so the change
// shows immediately.
func (m *model) reloadAfterEdit(edited store.Issue) {
	m.reloadDigest()
	m.focusIssue(edited.ID)
	switch m.mode {
	case viewIssue:
		m.showIssue(edited)
	case viewLabels:
		m.detailIssue = edited
	}
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
	if section.hidden || len(section.Issues) == 0 || section.selected < 0 || section.selected >= len(section.Issues) {
		return store.Issue{}, false
	}
	return section.Issues[section.selected], true
}

func (m model) currentIssue() (store.Issue, bool) {
	if m.mode == viewIssue && m.detailIssue.ID != "" {
		return m.detailIssue, true
	}
	return m.selectedIssue()
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
		if section.hidden {
			continue
		}
		issues = append(issues, section.Issues...)
	}
	return issues
}

func (m *model) focusIssue(id string) {
	for i := range m.sections {
		if m.sections[i].hidden {
			continue
		}
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
	lines = append(lines, "", " esc back   ↑↓ prev/next   s status   p priority   l labels   r refresh   : cmd   q quit")
	return strings.Join(lines, "\n")
}

func (m model) labelPickerView() string {
	issue := m.detailIssue
	lines := []string{
		issueHeader(m.project.Name, issue),
		strings.Repeat("─", digestWidth),
		"",
	}
	for i, label := range store.Labels {
		mark := "[ ]"
		if slices.Contains(issue.Labels, label) {
			mark = "[x]"
		}
		prefix := "   "
		if i == m.labelCursor {
			prefix = " ▸ "
		}
		lines = append(lines, prefix+mark+" "+label)
	}
	lines = append(lines,
		"",
		strings.Repeat("─", digestWidth),
		"",
		" ↑↓ move   ⏎ toggle   esc done   q quit",
	)
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

func nextValue(values []string, current string) string {
	for i, value := range values {
		if value == current {
			return values[(i+1)%len(values)]
		}
	}
	if len(values) == 0 {
		return current
	}
	return values[0]
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
