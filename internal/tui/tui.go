package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gimigliano/ito/internal/store"
)

const digestWidth = 88

const (
	boardColumnGap      = 3
	boardMinColumnWidth = 24
)

// Digest rows View() draws besides Issues: digestChromeLines (header, divider,
// blank, bottom rule, shortcut bar) and sectionChromeLines per section
// (heading + blank).
const (
	digestChromeLines  = 5
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
	viewDigest   viewMode = "digest"
	viewBoard    viewMode = "board"
	viewIssue    viewMode = "issue"
	viewLabels   viewMode = "labels"
	viewProjects viewMode = "projects"
)

var priorityCycle = []string{"low", "medium", "high", "urgent"}

var commandActions = []commandAction{
	{Shortcut: "s", Name: "status"},
	{Shortcut: "p", Name: "priority"},
	{Shortcut: "l", Name: "labels"},
	{Name: "switch project"},
	{Shortcut: "r", Name: "refresh"},
	{Shortcut: "q", Name: "quit"},
}

type commandAction struct {
	Shortcut string
	Name     string
}

type model struct {
	store         *store.Store
	project       store.Project
	sections      []digestSection
	focusIndex    int
	mode          viewMode
	returnMode    viewMode
	detailIssue   store.Issue
	linkTitles    map[string]string
	labelCursor   int
	projects      []store.Project
	projectCursor int
	filterOpen    bool
	filterQuery   string
	commandOpen   bool
	commandQuery  string
	loadErr       error
	width         int
	height        int
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
	if project.ID == 0 {
		m.openProjectPicker()
	} else {
		m.reloadDigest()
	}
	return m
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.filterOpen {
			return m, editInlineInput(&m.filterOpen, &m.filterQuery, msg)
		}
		if m.commandOpen {
			if msg.Type == tea.KeyEnter {
				return m.runSelectedCommandAction()
			}
			return m, editInlineInput(&m.commandOpen, &m.commandQuery, msg)
		}
		if m.mode == viewProjects {
			return m.updateProjectPicker(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			switch m.mode {
			case viewLabels:
				m.mode = viewIssue
			case viewIssue:
				m.mode = m.detailReturnMode()
			}
		case "1":
			if m.mode == viewDigest || m.mode == viewBoard {
				m.mode = viewDigest
			}
		case "2":
			if m.mode == viewDigest || m.mode == viewBoard {
				m.mode = viewBoard
			}
		case "/":
			if m.mode == viewDigest || m.mode == viewBoard {
				m.filterOpen = true
			}
		case ":":
			if m.mode != viewLabels {
				m.commandOpen = true
			}
		case "enter":
			switch m.mode {
			case viewDigest, viewBoard:
				m.openSelectedIssue()
			case viewLabels:
				m.toggleFocusedLabel()
			}
		case "tab":
			if m.mode == viewDigest || m.mode == viewBoard {
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
		case "r":
			if m.mode != viewLabels {
				m.reloadDigest()
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
			if m.mode == viewDigest || m.mode == viewBoard {
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
		m.width = msg.Width
		m.height = msg.Height
	}
	return m, nil
}

// editInlineInput applies a key to an open inline input — the / filter or the :
// command bar. Ctrl-C quits, Esc closes and clears, Backspace and runes edit
// the query in place; any other key is ignored.
func editInlineInput(open *bool, query *string, msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyCtrlC:
		return tea.Quit
	case tea.KeyEsc:
		*open = false
		*query = ""
	case tea.KeyBackspace:
		if *query != "" {
			runes := []rune(*query)
			*query = string(runes[:len(runes)-1])
		}
	case tea.KeyRunes:
		*query += string(msg.Runes)
	}
	return nil
}

func (m model) View() string {
	if m.loadErr != nil {
		return fmt.Sprintf("ito · [1] digest · [2] board\n\ncould not load Issues: %v\n\nq quit", m.loadErr)
	}
	if m.mode == viewLabels {
		return m.labelPickerView()
	}
	if m.mode == viewProjects {
		return m.projectPickerView()
	}
	if m.mode == viewIssue {
		return m.issueDetailView()
	}
	if m.mode == viewBoard {
		return m.boardView()
	}

	sections := m.digestSections()
	lines := []string{
		header(m.project.Name, issueCount(sections), digestWidth, viewDigest),
		fullRule(digestWidth),
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
	lines = append(lines, fullRule(digestWidth), m.digestBottomBar(issueCount(sections), issueCount(m.sections)))
	return strings.Join(lines, "\n")
}

func (m model) boardView() string {
	sections := m.boardSections()
	width := m.boardWidth()
	visible := m.visibleBoardSections(sections, width)
	lines := []string{
		header(m.project.Name, issueCount(sections), width, viewBoard),
		fullRule(width),
		"",
	}

	columns := make([][]string, 0, len(visible.sections))
	for i, section := range visible.sections {
		sectionIndex := visible.start + i
		columns = append(columns, m.boardColumn(section, sectionIndex, visible.columnWidth))
	}
	maxRows := 0
	for _, column := range columns {
		maxRows = max(maxRows, len(column))
	}
	for i := range columns {
		for len(columns[i]) < maxRows {
			columns[i] = append(columns[i], strings.Repeat(" ", visible.columnWidth))
		}
	}
	for row := 0; row < maxRows; row++ {
		lead := "  "
		if row == 0 && visible.hasLeft {
			lead = "‹ "
		}
		parts := make([]string, 0, len(columns))
		for _, column := range columns {
			parts = append(parts, column[row])
		}
		line := lead + strings.Join(parts, strings.Repeat(" ", boardColumnGap))
		if row == 0 && visible.hasRight {
			line += " ›"
		}
		lines = append(lines, line)
	}

	lines = append(lines, "", fullRule(width), m.boardBottomBar(issueCount(sections), issueCount(m.sections)))
	return strings.Join(lines, "\n")
}

type visibleBoardSections struct {
	sections    []digestSection
	start       int
	columnWidth int
	hasLeft     bool
	hasRight    bool
}

func (m model) visibleBoardSections(sections []digestSection, width int) visibleBoardSections {
	count := len(sections)
	if count == 0 {
		return visibleBoardSections{}
	}
	visibleCount := min(count, max(1, (width+boardColumnGap)/(boardMinColumnWidth+boardColumnGap)))
	focusIndex := m.boardWindowFocusIndex(sections)
	start := min(max(focusIndex-visibleCount/2, 0), count-visibleCount)
	trackWidth := width - 4 // left/right affordance columns
	columnWidth := (trackWidth - boardColumnGap*(visibleCount-1)) / visibleCount
	if columnWidth < 1 {
		columnWidth = 1
	}
	return visibleBoardSections{
		sections:    sections[start : start+visibleCount],
		start:       start,
		columnWidth: columnWidth,
		hasLeft:     start > 0,
		hasRight:    start+visibleCount < count,
	}
}

func (m model) boardWindowFocusIndex(sections []digestSection) int {
	if len(sections) == 0 {
		return 0
	}
	if strings.TrimSpace(m.filterQuery) == "" {
		return min(max(m.focusIndex, 0), len(sections)-1)
	}
	if m.focusIndex >= 0 && m.focusIndex < len(sections) && len(sections[m.focusIndex].Issues) > 0 {
		return m.focusIndex
	}
	for i, section := range sections {
		if len(section.Issues) > 0 {
			return i
		}
	}
	return min(max(m.focusIndex, 0), len(sections)-1)
}

func (m model) boardColumn(section digestSection, sectionIndex int, width int) []string {
	focused := sectionIndex == m.focusIndex
	marker := " "
	if focused {
		marker = "▌"
	}
	lines := []string{truncate(marker+section.Label+"  ("+fmt.Sprint(len(section.Issues))+")", width)}
	window := visibleIssueWindow(len(section.Issues), section.selected, m.boardIssueLineBudget())
	if window.showAbove {
		lines = append(lines, truncate(fmt.Sprintf("    ↑ %d more", window.start), width))
	}
	for j, issue := range section.Issues[window.start:window.end] {
		issueIndex := window.start + j
		selected := focused && issueIndex == section.selected
		lines = append(lines, renderBoardIssue(issue, selected, width))
	}
	if window.showBelow {
		lines = append(lines, truncate(fmt.Sprintf("    ↓ %d more", len(section.Issues)-window.end), width))
	}
	for i := range lines {
		lines[i] = padRight(truncate(lines[i], width), width)
	}
	return lines
}

func (m model) boardIssueLineBudget() int {
	if m.height <= 0 {
		return 1 << 20
	}
	return max(1, m.height-6)
}

func (m model) boardWidth() int {
	if m.width > 0 {
		return max(m.width, 32)
	}
	return digestWidth
}

func (m model) boardBottomBar(matched, total int) string {
	if m.filterOpen {
		hint := fmt.Sprintf("%d of %d issues · esc to clear", matched, total)
		return inputBar("/", m.filterQuery, hint)
	}
	if m.commandOpen {
		return m.commandBottomBar()
	}
	return statusBar(
		[2]string{"tab", "focus"}, [2]string{"↑↓", "select"}, [2]string{"⏎", "open"},
		[2]string{"s", "status"}, [2]string{"/", "filter"}, [2]string{":", "cmd"}, [2]string{"q", "quit"},
	)
}

func (m model) digestBottomBar(matched, total int) string {
	if m.filterOpen {
		hint := fmt.Sprintf("%d of %d issues · esc to clear", matched, total)
		return inputBar("/", m.filterQuery, hint)
	}
	if m.commandOpen {
		return m.commandBottomBar()
	}
	return statusBar(
		[2]string{"tab", "focus"}, [2]string{"↑↓", "select"}, [2]string{"⏎", "open"},
		[2]string{"s", "status"}, [2]string{"h", "hide"}, [2]string{"/", "filter"},
		[2]string{":", "cmd"}, [2]string{"q", "quit"},
	)
}

func (m model) commandBottomBar() string {
	lines := []string{styleLine.Render(divider("─ actions ", m.surfaceWidth()))}
	for _, action := range m.filteredCommandActions() {
		lines = append(lines, " "+renderCommandAction(action))
	}
	lines = append(lines, inputBar(":", m.commandQuery, "esc cancel"))
	return strings.Join(lines, "\n")
}

func divider(head string, width int) string {
	return head + strings.Repeat("─", width-len(head))
}

// fullRule is the full-width horizontal separator the surfaces draw under the
// header (and around the detail view), in the dim separator colour — matching
// the prototype, where every ─ renders in --line, not the default foreground.
func fullRule(width int) string {
	return styleLine.Render(strings.Repeat("─", width))
}

// statusBar renders the always-visible shortcut bar: each key in the accent
// colour, its label dimmed, three spaces between pairs.
func statusBar(pairs ...[2]string) string {
	parts := make([]string, len(pairs))
	for i, pair := range pairs {
		parts[i] = styleKey.Render(pair[0]) + " " + styleDim.Render(pair[1])
	}
	return " " + strings.Join(parts, "   ")
}

// inputBar renders the inline / filter or : command field: the prefix as a key,
// the typed query in ink, the caret in the accent colour, the hint dimmed.
func inputBar(prefix, query, hint string) string {
	return " " + styleKey.Render(prefix) + " " + styleText.Render(query) +
		styleActive.Render("▏") + "   " + styleDim.Render(hint)
}

func (m model) filteredCommandActions() []commandAction {
	query := strings.TrimSpace(strings.ToLower(m.commandQuery))
	if query == "" {
		return commandActions
	}
	var filtered []commandAction
	for _, action := range commandActions {
		if strings.Contains(strings.ToLower(action.Name), query) || strings.Contains(strings.ToLower(action.Shortcut), query) {
			filtered = append(filtered, action)
		}
	}
	return filtered
}

func renderCommandAction(action commandAction) string {
	if action.Shortcut == "" {
		return "   " + styleText.Render(action.Name)
	}
	return styleKey.Render(action.Shortcut) + "  " + styleText.Render(action.Name)
}

func (m model) runSelectedCommandAction() (tea.Model, tea.Cmd) {
	actions := m.filteredCommandActions()
	if len(actions) == 0 {
		return m, nil
	}
	m.commandOpen = false
	m.commandQuery = ""
	// The : command line never opens in viewLabels, so these actions always run
	// against the Digest or the Issue detail.
	switch actions[0].Name {
	case "status":
		m.moveSelectedIssueStatus()
	case "priority":
		m.cycleDetailIssuePriority()
	case "labels":
		m.openLabelPicker()
	case "switch project":
		m.openProjectPicker()
	case "refresh":
		m.reloadDigest()
	case "quit":
		return m, tea.Quit
	}
	return m, nil
}

func (m model) updateProjectPicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.project.ID != 0 {
			m.mode = viewDigest
		}
	case "up":
		m.moveProjectCursor(-1)
	case "down":
		m.moveProjectCursor(1)
	case "enter":
		m.switchToSelectedProject()
	}
	return m, nil
}

func (m *model) openProjectPicker() {
	projects, err := m.store.ListProjects()
	if err != nil {
		m.loadErr = err
		return
	}
	m.projects = projects
	m.projectCursor = 0
	for i, project := range projects {
		if project.ID == m.project.ID {
			m.projectCursor = i
			break
		}
	}
	m.mode = viewProjects
}

func (m *model) moveProjectCursor(delta int) {
	if len(m.projects) == 0 {
		return
	}
	m.projectCursor = min(max(m.projectCursor+delta, 0), len(m.projects)-1)
}

func (m *model) switchToSelectedProject() {
	if len(m.projects) == 0 || m.projectCursor < 0 || m.projectCursor >= len(m.projects) {
		return
	}
	m.project = m.projects[m.projectCursor]
	m.sections = nil
	m.focusIndex = 0
	m.detailIssue = store.Issue{}
	m.linkTitles = map[string]string{}
	m.filterOpen = false
	m.filterQuery = ""
	m.commandOpen = false
	m.commandQuery = ""
	m.mode = viewDigest
	m.reloadDigest()
}

func (m *model) reloadDigest() {
	focusedLabel := ""
	if m.focusIndex >= 0 && m.focusIndex < len(m.sections) {
		focusedLabel = m.sections[m.focusIndex].Label
	}
	hiddenByLabel := map[string]bool{}
	selectedByLabel := map[string]string{}
	for _, section := range m.sections {
		hiddenByLabel[section.Label] = section.hidden
		if section.selected >= 0 && section.selected < len(section.Issues) {
			selectedByLabel[section.Label] = section.Issues[section.selected].ID
		}
	}
	detailID := m.detailIssue.ID

	m.sections = make([]digestSection, 0, len(store.Statuses))
	m.loadErr = nil
	for i, status := range store.Statuses {
		label := statusLabel(status)
		issues, err := m.store.ListIssues(store.ListOptions{
			ProjectID: m.project.ID,
			Status:    status,
		})
		if err != nil {
			m.loadErr = err
			return
		}
		section := digestSection{
			Label:  label,
			Issues: issues,
			hidden: status == "done",
		}
		if hidden, ok := hiddenByLabel[label]; ok {
			section.hidden = hidden
		}
		if selectedID := selectedByLabel[label]; selectedID != "" {
			for j := range issues {
				if issues[j].ID == selectedID {
					section.selected = j
					break
				}
			}
		}
		if label == focusedLabel {
			m.focusIndex = i
		}
		m.sections = append(m.sections, section)
	}
	if m.focusIndex < 0 || m.focusIndex >= len(m.sections) {
		m.focusIndex = 0
	}
	if detailID != "" {
		if refreshed, ok := m.issueInSections(detailID); ok {
			m.detailIssue = refreshed
			m.linkTitles = m.loadLinkTitles(refreshed)
			m.focusIssue(detailID)
		}
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

	nonEmptySections := 0
	hiddenSections := 0
	totalIssues := 0
	for _, section := range sections {
		if section.hidden {
			hiddenSections++
			continue
		}
		if len(section.Issues) == 0 {
			continue
		}
		nonEmptySections++
		totalIssues += len(section.Issues)
	}
	if nonEmptySections == 0 {
		return capacities
	}

	// Each visible section spends sectionChromeLines on its heading and trailing
	// blank — empty sections included; collapsed sections render as a single line.
	visibleSections := len(sections) - hiddenSections
	fixedLines := digestChromeLines + visibleSections*sectionChromeLines + hiddenSections
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

func (m model) boardSections() []digestSection {
	sections := m.digestSections()
	for i := range sections {
		sections[i].hidden = false
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
	if (section.hidden && m.mode != viewBoard) || len(section.Issues) == 0 {
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
	issue, ok := m.currentIssue()
	if !ok {
		return
	}
	if m.mode == viewDigest || m.mode == viewBoard {
		m.returnMode = m.mode
	}
	m.detailIssue = issue
	m.linkTitles = m.loadLinkTitles(issue)
	m.focusIssue(issue.ID)
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
	if m.mode == viewDigest || m.mode == viewBoard {
		m.returnMode = m.mode
	}
	m.detailIssue = issue
	m.linkTitles = m.loadLinkTitles(issue)
	m.focusIssue(issue.ID)
	m.mode = viewIssue
}

func (m model) detailReturnMode() viewMode {
	if m.returnMode == viewBoard {
		return viewBoard
	}
	return viewDigest
}

func (m model) selectedIssue() (store.Issue, bool) {
	if len(m.sections) == 0 || m.focusIndex < 0 || m.focusIndex >= len(m.sections) {
		return store.Issue{}, false
	}
	section := m.sections[m.focusIndex]
	if (section.hidden && m.mode != viewBoard) || len(section.Issues) == 0 || section.selected < 0 || section.selected >= len(section.Issues) {
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
		if m.sections[i].hidden && m.mode != viewBoard {
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

func (m model) issueInSections(id string) (store.Issue, bool) {
	for _, section := range m.sections {
		for _, issue := range section.Issues {
			if issue.ID == id {
				return issue, true
			}
		}
	}
	return store.Issue{}, false
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
		fullRule(digestWidth),
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
		fullRule(digestWidth),
		"",
	)
	for _, line := range strings.Split(issue.Body, "\n") {
		for _, wrapped := range wrapLine(line, detailBodyWidth) {
			lines = append(lines, " "+wrapped)
		}
	}
	lines = append(lines, "", fullRule(digestWidth))
	if m.commandOpen {
		lines = append(lines, m.commandBottomBar())
	} else {
		lines = append(lines, statusBar(
			[2]string{"esc", "back"}, [2]string{"↑↓", "prev/next"}, [2]string{"s", "status"},
			[2]string{"p", "priority"}, [2]string{"l", "labels"}, [2]string{"r", "refresh"},
			[2]string{":", "cmd"}, [2]string{"q", "quit"},
		))
	}
	return strings.Join(lines, "\n")
}

func (m model) labelPickerView() string {
	issue := m.detailIssue
	lines := []string{
		issueHeader(m.project.Name, issue),
		fullRule(digestWidth),
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
		fullRule(digestWidth),
		statusBar([2]string{"↑↓", "move"}, [2]string{"⏎", "toggle"}, [2]string{"esc", "done"}, [2]string{"q", "quit"}),
	)
	return strings.Join(lines, "\n")
}

func (m model) projectPickerView() string {
	lines := []string{
		padBetween("ito · switch project", m.project.Name, digestWidth),
		fullRule(digestWidth),
		"",
	}
	if len(m.projects) == 0 {
		lines = append(lines, " no Projects yet", "", " run ito init to get started", "", statusBar([2]string{"q", "quit"}))
		return strings.Join(lines, "\n")
	}

	nameWidth := 0
	for _, project := range m.projects {
		nameWidth = max(nameWidth, len(project.Name))
	}
	for i, project := range m.projects {
		prefix := "   "
		if i == m.projectCursor {
			prefix = " ▸ "
		}
		lines = append(lines, prefix+padRight(project.Name, nameWidth)+"   "+project.Prefix)
	}
	lines = append(lines,
		"",
		fullRule(digestWidth),
		statusBar([2]string{"↑↓", "move"}, [2]string{"⏎", "switch"}, [2]string{"esc", "cancel"}, [2]string{"q", "quit"}),
	)
	return strings.Join(lines, "\n")
}

func header(projectName string, issueCount, width int, active viewMode) string {
	// Measure the plain text so the gap counts visible runes only, then style
	// each segment — colouring adds no visible width. The numbers stay in the
	// default ink; only the active view name takes the accent colour.
	left := "ito · [1] digest · [2] board"
	right := fmt.Sprintf("%d issues   %s", issueCount, projectName)
	gap := width - runeLen(left) - runeLen(right)
	if gap < 1 {
		gap = 1
	}

	digestStyle, boardStyle := styleText, styleText
	if active == viewBoard {
		boardStyle = styleActive
	} else {
		digestStyle = styleActive
	}
	styledLeft := styleText.Render("ito · [1] ") + digestStyle.Render("digest") +
		styleText.Render(" · [2] ") + boardStyle.Render("board")
	return styledLeft + strings.Repeat(" ", gap) + styleText.Render(right)
}

func (m model) surfaceWidth() int {
	if m.mode == viewBoard {
		return m.boardWidth()
	}
	return digestWidth
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
	gap := width - runeLen(left) - runeLen(right)
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

func renderBoardIssue(issue store.Issue, selected bool, width int) string {
	pointer := " "
	if selected {
		pointer = "▸"
	}
	prefix := fmt.Sprintf("%s %s %s ", pointer, priorityMark(issue.Priority), issue.ID)
	labels := ""
	if len(issue.Labels) > 0 {
		labels = "   " + strings.Join(issue.Labels, " ")
	}
	titleWidth := width - runeLen(prefix) - runeLen(labels)
	if titleWidth < 1 {
		return truncate(prefix+issue.Title+labels, width)
	}
	return prefix + truncate(issue.Title, titleWidth) + labels
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
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max <= 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}

func padRight(value string, width int) string {
	length := runeLen(value)
	if length >= width {
		return value
	}
	return value + strings.Repeat(" ", width-length)
}

func runeLen(value string) int {
	return len([]rune(value))
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
