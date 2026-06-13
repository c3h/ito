package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/c3h/ito/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// Surface frame width: capped at a readable measure so it never sprawls on a
// wide terminal, but shrinks to fit a narrow one — see model.viewWidth. All
// three surfaces (digest, board, issue) share this so switching views never
// reflows the frame.
const (
	surfaceMaxWidth = 88
	surfaceMinWidth = 32
)

const (
	boardColumnGap      = 3
	boardMinColumnWidth = 24
	// boardComfortableColumnWidth is the per-column width the Board grows toward
	// before it stops widening — past it the columns would only sprawl.
	boardComfortableColumnWidth = 27
	// boardAffordanceWidth is the two columns the ‹ › slide indicators occupy on
	// each side of the track.
	boardAffordanceWidth = 4
)

// Digest rows View() draws besides Issues: digestChromeLines (header, divider,
// blank, bottom rule, shortcut bar) and sectionChromeLines per section
// (heading + blank).
const (
	digestChromeLines  = 5
	sectionChromeLines = 2
)

// detailBodyWidth keeps the Issue body at a readable prose measure, narrower
// than the full surface frame. On a narrow terminal the body shrinks below
// this to fit (see issueDetailView).
const detailBodyWidth = 80

// linkIDWidth is the column the linked Issue id occupies before its title in
// the detail view, so the titles line up across link rows.
const linkIDWidth = 8

type viewMode string

const (
	viewDigest   viewMode = "digest"
	viewBoard    viewMode = "board"
	viewBatches  viewMode = "batches"
	viewIssue    viewMode = "issue"
	viewLabels   viewMode = "labels"
	viewProjects viewMode = "projects"
)

// priorityCycle steps a priority upward (low → urgent, wrapping), so it walks
// store.Priorities — ordered by descending precedence — in reverse.
var priorityCycle = func() []string {
	cycle := slices.Clone(store.Priorities)
	slices.Reverse(cycle)
	return cycle
}()

var commandActions = []commandAction{
	{Shortcut: "s", Name: "status"},
	{Shortcut: "p", Name: "priority"},
	{Shortcut: "l", Name: "labels"},
	{Name: "board"},
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
	batchSections []batchSection
	batchFocus    int
	focusIndex    int
	mode          viewMode
	returnMode    viewMode
	detailIssue   store.Issue
	detailScroll  int
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
			case viewBoard:
				m.mode = viewDigest
			}
		case "1":
			if m.isSurfaceMode() {
				m.mode = viewDigest
			}
		case "2":
			if m.isSurfaceMode() && m.mode != viewBatches {
				m.mode = viewBatches
				m.reloadBatches()
			}
		case "/":
			if m.isSurfaceMode() {
				m.filterOpen = true
			}
		case ":":
			if m.mode != viewLabels {
				m.commandOpen = true
			}
		case "enter":
			switch m.mode {
			case viewDigest, viewBoard, viewBatches:
				m.openSelectedIssue()
			case viewLabels:
				m.toggleFocusedLabel()
			}
		case "tab":
			if m.isSurfaceMode() {
				m.cursor().moveFocus(1)
			}
		case "h":
			switch m.mode {
			case viewDigest:
				m.toggleFocusedSection()
			case viewBatches:
				m.toggleFocusedBatch()
			}
		case "s":
			if m.mode != viewLabels {
				m.moveSelectedIssueStatus()
			}
		case "r":
			switch m.mode {
			case viewLabels:
			case viewBatches:
				m.reloadBatches()
			default:
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
			if m.isSurfaceMode() {
				m.cursor().moveFocus(-1)
			}
		case "up":
			switch m.mode {
			case viewIssue:
				m.moveDetailIssue(-1)
			case viewLabels:
				m.moveLabelCursor(-1)
			default:
				m.cursor().moveSelection(-1)
			}
		case "down":
			switch m.mode {
			case viewIssue:
				m.moveDetailIssue(1)
			case viewLabels:
				m.moveLabelCursor(1)
			default:
				m.cursor().moveSelection(1)
			}
		case "pgdown":
			if m.mode == viewIssue {
				m.scrollDetailBody(1, true)
			}
		case "pgup":
			if m.mode == viewIssue {
				m.scrollDetailBody(-1, true)
			}
		case "j":
			if m.mode == viewIssue {
				m.scrollDetailBody(1, false)
			}
		case "k":
			if m.mode == viewIssue {
				m.scrollDetailBody(-1, false)
			}
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}
	return m, nil
}

// isSurfaceMode reports whether one of the header-tab surfaces is active — the
// only modes the 1/2/3 surface keys switch between.
func (m model) isSurfaceMode() bool {
	return m.mode == viewDigest || m.mode == viewBoard || m.mode == viewBatches
}

// editInlineInput applies a key to an open inline input — the / filter or the :
// command bar. Ctrl-C quits, Esc closes and clears, Backspace, Space and runes
// edit the query in place; any other key is ignored.
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
	case tea.KeySpace:
		*query += " "
	case tea.KeyRunes:
		*query += string(msg.Runes)
	}
	return nil
}

func (m model) View() string {
	if m.loadErr != nil {
		return fmt.Sprintf("ito · [1] digest · [2] batches\n\ncould not load Issues: %v\n\nq quit", m.loadErr)
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
	if m.mode == viewBatches {
		return m.batchesView()
	}

	width := m.viewWidth()
	sections := m.digestSections()
	lines := []string{
		header(m.project.Name, activeIssueCount(sections), width, viewDigest),
		fullRule(width),
		"",
	}
	windows := m.digestWindows(sections)
	filtering := strings.TrimSpace(m.filterQuery) != ""
	for i, section := range sections {
		if filtering && len(section.Issues) == 0 {
			continue // while filtering, only sections with matches are shown
		}
		focused := i == m.focusIndex
		if section.hidden {
			lines = append(lines, sectionHeading(section.Label, len(section.Issues), focused, true, width))
			lines = append(lines, "")
			continue
		}
		lines = append(lines, sectionHeading(section.Label, len(section.Issues), focused, false, width))
		window := windows[i]
		if window.showAbove {
			lines = append(lines, styleDim.Render(fmt.Sprintf("    ↑ %d more", window.start)))
		}
		for j, issue := range section.Issues[window.start:window.end] {
			issueIndex := window.start + j
			// Two leading spaces put the cursor under the heading's ▾ and the
			// priority mark under the first letter of the status label.
			prefix := "    "
			if focused && issueIndex == section.selected {
				prefix = "  " + styleActive.Render("▸") + " "
			}
			lines = append(lines, prefix+renderIssueRow(issue, width-4))
		}
		if window.showBelow {
			lines = append(lines, styleDim.Render(fmt.Sprintf("    ↓ %d more", len(section.Issues)-window.end)))
		}
		lines = append(lines, "")
	}
	lines = append(lines, fullRule(width), m.digestBottomBar(issueCount(sections), issueCount(m.sections)))
	return strings.Join(lines, "\n")
}

func (m model) boardView() string {
	sections := m.boardSections()
	width := m.boardViewWidth(len(sections))
	visible := m.visibleBoardSections(sections, width)
	lines := []string{
		boardHeader(m.project.Name, activeIssueCount(sections), width),
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
	trackWidth := width - boardAffordanceWidth // left/right affordance columns
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
	// Each cell builder pads itself to exactly width in plain runes and styles
	// the content inline — so column alignment never miscounts ANSI escapes.
	lines := []string{boardColumnHeading(section.Label, len(section.Issues), focused, width)}
	window := visibleIssueWindow(len(section.Issues), section.selected, m.boardIssueLineBudget())
	if window.showAbove {
		lines = append(lines, boardMoreLine("↑", window.start, width))
	}
	for j, issue := range section.Issues[window.start:window.end] {
		issueIndex := window.start + j
		selected := focused && issueIndex == section.selected
		lines = append(lines, renderBoardIssue(issue, selected, width))
	}
	if window.showBelow {
		lines = append(lines, boardMoreLine("↓", len(section.Issues)-window.end, width))
	}
	return lines
}

// padStyled pads a styled cell with trailing spaces so its visible width reaches
// width — measured against the plain text, since styling adds no visible runes.
func padStyled(styled, plain string, width int) string {
	if pad := width - runeLen(plain); pad > 0 {
		styled += strings.Repeat(" ", pad)
	}
	return styled
}

// boardColumnHeading renders a Board column header — focus bar in the accent
// colour, status label in cyan, count in ink — padded to the column width.
func boardColumnHeading(label string, count int, focused bool, width int) string {
	bar, styledBar := " ", styleText.Render(" ")
	if focused {
		bar, styledBar = "▌", styleActive.Render("▌")
	}
	suffix := fmt.Sprintf("  (%d)", count)
	plain := bar + label + suffix
	if runeLen(plain) >= width {
		return truncate(plain, width)
	}
	styled := styledBar + styleStatus.Render(label) + styleText.Render(suffix)
	return padStyled(styled, plain, width)
}

// boardMoreLine renders a Board overflow indicator, dimmed and padded to width.
func boardMoreLine(arrow string, n, width int) string {
	plain := truncate(fmt.Sprintf("    %s %d more", arrow, n), width)
	return padStyled(styleDim.Render(plain), plain, width)
}

func (m model) boardIssueLineBudget() int {
	if m.height <= 0 {
		return 1 << 20
	}
	// Chrome around the columns spends 6 lines (header, rule, blank, blank, rule,
	// shortcut bar) and each column spends one more on its own heading — so the
	// per-column issue budget is height minus 7, not 6.
	return max(1, m.height-7)
}

// viewWidth caps the surface at a readable measure but shrinks to fit a narrow
// terminal: fixed on wide screens, responsive on small ones. Before the first
// WindowSizeMsg (width 0) it falls back to the cap.
func (m model) viewWidth() int {
	if m.width <= 0 {
		return surfaceMaxWidth
	}
	return max(surfaceMinWidth, min(m.width, surfaceMaxWidth))
}

// boardViewWidth is the Board's take on viewWidth. The Board's content is
// columns, not rows — so it grows to fit every column at a comfortable width
// and then stops (rather than capping at the row-oriented surfaceMaxWidth),
// and shrinks below that, sliding the column track when the columns don't fit.
func (m model) boardViewWidth(columns int) int {
	maxWidth := columns*boardComfortableColumnWidth + (columns-1)*boardColumnGap + boardAffordanceWidth
	if m.width <= 0 {
		return maxWidth
	}
	return max(surfaceMinWidth, min(m.width, maxWidth))
}

// surfaceBottomBar is the row surfaces' shared footer: the inline filter input
// with its matched/total hint while a filter is open, the : command line while
// a command is open, otherwise the surface's own key set.
func (m model) surfaceBottomBar(matched, total int, keys ...[2]string) string {
	if m.filterOpen {
		hint := fmt.Sprintf("%d of %d issues · esc to clear", matched, total)
		return inputBar("/", m.filterQuery, hint)
	}
	if m.commandOpen {
		return m.commandBottomBar()
	}
	return statusBar(keys...)
}

func (m model) boardBottomBar(matched, total int) string {
	return m.surfaceBottomBar(matched, total,
		[2]string{"esc", "back"}, [2]string{"tab", "focus"}, [2]string{"↑↓", "select"}, [2]string{"⏎", "open"},
		[2]string{"s", "status"}, [2]string{"/", "filter"}, [2]string{":", "cmd"}, [2]string{"q", "quit"},
	)
}

func (m model) digestBottomBar(matched, total int) string {
	return m.surfaceBottomBar(matched, total,
		[2]string{"tab", "focus"}, [2]string{"↑↓", "select"}, [2]string{"⏎", "open"},
		[2]string{"s", "status"}, [2]string{"h", "hide"}, [2]string{"/", "filter"},
		[2]string{":", "cmd"}, [2]string{"q", "quit"},
	)
}

func (m model) commandBottomBar() string {
	lines := []string{styleLine.Render(divider("─ actions ", m.viewWidth()))}
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
// header (and around the detail view). Every ─ renders in the dim separator
// colour, never the default foreground, so rules read as chrome, not content.
func fullRule(width int) string {
	return styleLine.Render(strings.Repeat("─", width))
}

// sectionHeading renders a Digest section heading: the focus bar and disclosure
// triangle in the accent colour, the status label in cyan, the count in ink,
// then a dim rule filling the rest of the width.
func sectionHeading(label string, count int, focused, collapsed bool, width int) string {
	bar, styledBar := " ", " "
	if focused {
		bar, styledBar = "▌", styleActive.Render("▌")
	}
	triangle, suffix := "▾", fmt.Sprintf("  (%d)", count)
	if collapsed {
		triangle, suffix = "▸", fmt.Sprintf("  (%d) · h to show", count)
	}
	styledBody := styleActive.Render(triangle) + " " + styleStatus.Render(label) + styleText.Render(suffix)

	plain := " " + bar + triangle + " " + label + suffix + "  "
	ruleLen := max(0, width-runeLen(plain))
	return " " + styledBar + styledBody + styleText.Render("  ") + styleLine.Render(strings.Repeat("─", ruleLen))
}

// emptyState renders a surface's empty placeholder: the fact in the ink, then
// a dim hint with the actionable command back in the ink — quiet chrome with
// the next step as the only thing that pops.
func emptyState(fact, command, rest string) []string {
	return []string{
		" " + styleText.Render(fact),
		"",
		" " + styleDim.Render("run ") + styleText.Render(command) + styleDim.Render(" "+rest),
	}
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
	// against a surface selection (Digest, Board or Batches) or the Issue detail.
	switch actions[0].Name {
	case "status":
		m.moveSelectedIssueStatus()
	case "priority":
		m.cycleDetailIssuePriority()
	case "labels":
		m.openLabelPicker()
	case "board":
		m.mode = viewBoard
	case "switch project":
		m.openProjectPicker()
	case "refresh":
		if m.mode == viewBatches {
			m.reloadBatches()
		} else {
			m.reloadDigest()
		}
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
	m.batchSections = nil
	m.batchFocus = 0
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

	m.loadErr = nil
	// One query for every status: a single snapshot, so a concurrent write can
	// never show an Issue in two sections (or in none) within the same reload.
	all, err := m.store.ListIssues(store.ListOptions{
		ProjectID:   m.project.ID,
		IncludeDone: true,
	})
	if err != nil {
		m.loadErr = err
		return
	}
	byStatus := make(map[string][]store.Issue, len(store.Statuses))
	for _, issue := range all {
		byStatus[issue.Status] = append(byStatus[issue.Status], issue)
	}

	m.sections = make([]digestSection, 0, len(store.Statuses))
	for i, status := range store.Statuses {
		label := statusLabel(status)
		issues := byStatus[status]
		if issues == nil {
			issues = []store.Issue{}
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
	counts := make([]int, len(sections))
	for i, section := range sections {
		if !section.hidden {
			counts[i] = len(section.Issues)
		}
	}
	return allocateLineBudgets(counts, m.height)
}

// allocateLineBudgets splits the terminal height across sections wanting
// counts[i] lines each — shared by the Digest sections and the Batch bodies.
// Every section spends sectionChromeLines on its heading and trailing blank,
// including empty and collapsed ones (count 0), so hiding preserves the
// surface's visual rhythm. When everything fits each section gets its full
// count; otherwise non-empty sections start at one line and grow round-robin.
func allocateLineBudgets(counts []int, height int) []int {
	budgets := make([]int, len(counts))
	showAll := func() []int {
		copy(budgets, counts)
		return budgets
	}
	if height <= 0 {
		return showAll()
	}

	nonEmptySections := 0
	totalLines := 0
	for _, count := range counts {
		if count == 0 {
			continue
		}
		nonEmptySections++
		totalLines += count
	}
	if nonEmptySections == 0 {
		return budgets
	}

	fixedLines := digestChromeLines + len(counts)*sectionChromeLines
	available := height - fixedLines
	if available >= totalLines {
		return showAll()
	}
	if available < nonEmptySections {
		available = nonEmptySections
	}

	for i, count := range counts {
		if count > 0 {
			budgets[i] = 1
		}
	}
	remaining := available - nonEmptySections
	for remaining > 0 {
		grew := false
		for i, count := range counts {
			if remaining == 0 {
				break
			}
			if budgets[i] == 0 || budgets[i] >= count {
				continue
			}
			budgets[i]++
			remaining--
			grew = true
		}
		if !grew {
			break
		}
	}
	return budgets
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

func activeIssueCount(sections []digestSection) int {
	total := 0
	for _, section := range sections {
		if section.Label == statusLabel("done") {
			continue
		}
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
	sections := slices.Clone(m.digestSections())
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

// rowStop is one section a cursor can rest on: its listed rows, whether it is
// folded away (a hidden Digest section or a collapsed Batch lists no rows), and
// a pointer to the section's own selection field so the cursor writes the
// selection back through it.
type rowStop struct {
	rows      []store.Issue
	collapsed bool
	selected  *int
}

// rowCursor is the selection state machine the Digest, Board, and Batches
// surfaces all drive. The stops are the surface's sections in order, focus
// points at the focused section's index, and flows says whether the selection
// crosses section boundaries: the Digest and Batches flow — past a section's
// last row the cursor steps to the next section — while the Board clamps per
// column, since its sections sit side by side and vertical flow would jump
// columns. A folded section is still a stop: the focus rests on its heading
// with no row selected, so h reveals it from there.
type rowCursor struct {
	stops []rowStop
	focus *int
	flows bool
}

func (c rowCursor) moveFocus(delta int) {
	if len(c.stops) == 0 {
		return
	}
	*c.focus = (*c.focus + delta + len(c.stops)) % len(c.stops)
}

// moveSelection moves the cursor one row. A flowing cursor steps to the
// adjacent section past a section's ends — stopping at the surface's ends —
// and seeds the edge row so revealing a folded section lands the cursor where
// it arrived; an empty section has none and shows no cursor until it fills. A
// clamped cursor keeps the selection inside the focused section.
func (c rowCursor) moveSelection(delta int) {
	if *c.focus < 0 || *c.focus >= len(c.stops) {
		return
	}
	stop := c.stops[*c.focus]
	if !c.flows {
		if len(stop.rows) == 0 {
			return
		}
		*stop.selected = min(max(*stop.selected+delta, 0), len(stop.rows)-1)
		return
	}
	next := *stop.selected + delta
	if !stop.collapsed && next >= 0 && next < len(stop.rows) {
		*stop.selected = next
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	i := *c.focus + step
	if i < 0 || i >= len(c.stops) {
		return // at the surface's ends
	}
	*c.focus = i
	target := c.stops[i]
	if step > 0 {
		*target.selected = 0
	} else {
		*target.selected = max(0, len(target.rows)-1)
	}
}

// selected is the row the surface's actions apply to: the focused section's
// selected row, or none when that section is folded or lists no rows.
func (c rowCursor) selected() (store.Issue, bool) {
	if *c.focus < 0 || *c.focus >= len(c.stops) {
		return store.Issue{}, false
	}
	stop := c.stops[*c.focus]
	if stop.collapsed || *stop.selected < 0 || *stop.selected >= len(stop.rows) {
		return store.Issue{}, false
	}
	return stop.rows[*stop.selected], true
}

// focusRow points the focus and selection at an Issue when it still lists on an
// unfolded section, leaving the cursor untouched when the Issue does not render.
func (c rowCursor) focusRow(id string) {
	for i := range c.stops {
		if c.stops[i].collapsed {
			continue
		}
		for j, issue := range c.stops[i].rows {
			if issue.ID == id {
				*c.focus = i
				*c.stops[i].selected = j
				return
			}
		}
	}
}

// digestCursor builds the cursor over the Digest sections. A hidden section
// folds away — except on the Board, which shows every column — and only the
// Digest flows; the Board clamps.
func (m *model) digestCursor() rowCursor {
	stops := make([]rowStop, len(m.sections))
	for i := range m.sections {
		stops[i] = rowStop{
			rows:      m.sections[i].Issues,
			collapsed: m.sections[i].hidden && m.mode != viewBoard,
			selected:  &m.sections[i].selected,
		}
	}
	return rowCursor{stops: stops, focus: &m.focusIndex, flows: m.mode != viewBoard}
}

// batchCursor builds the cursor over the Batch sections: a collapsed Batch
// folds away and the surface flows across Batch boundaries like the Digest.
func (m *model) batchCursor() rowCursor {
	stops := make([]rowStop, len(m.batchSections))
	for i := range m.batchSections {
		stops[i] = rowStop{
			rows:      batchIssues(m.batchSections[i]),
			collapsed: m.batchSections[i].collapsed,
			selected:  &m.batchSections[i].selected,
		}
	}
	return rowCursor{stops: stops, focus: &m.batchFocus, flows: true}
}

// cursor returns the active surface's cursor — the Batches surface drives its
// own sections, every other surface drives the Digest's.
func (m *model) cursor() rowCursor {
	if m.mode == viewBatches {
		return m.batchCursor()
	}
	return m.digestCursor()
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
	if m.isSurfaceMode() {
		m.returnMode = m.mode
	}
	if m.detailIssue.ID != issue.ID {
		// Esc from the picker lands on this Issue's detail — don't inherit the
		// previous Issue's scroll offset.
		m.detailScroll = 0
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
// shows immediately. When the Batches surface is active (or the open detail
// returns to it), the waves re-derive too — a member moved to done leaves its
// rows on the spot.
func (m *model) reloadAfterEdit(edited store.Issue) {
	m.reloadDigest()
	m.focusIssue(edited.ID)
	if m.mode == viewBatches || m.returnMode == viewBatches {
		m.reloadBatches()
		m.focusBatchIssue(edited.ID)
	}
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
// originating surface, so opening and prev/next navigation never re-read the
// store for data the sections already hold.
func (m *model) showIssue(issue store.Issue) {
	if m.isSurfaceMode() {
		m.returnMode = m.mode
	}
	m.detailIssue = issue
	m.detailScroll = 0
	m.linkTitles = m.loadLinkTitles(issue)
	m.focusIssue(issue.ID)
	if m.returnMode == viewBatches {
		m.focusBatchIssue(issue.ID)
	}
	m.mode = viewIssue
}

func (m model) detailReturnMode() viewMode {
	if m.returnMode == viewBoard || m.returnMode == viewBatches {
		return m.returnMode
	}
	return viewDigest
}

func (m model) selectedIssue() (store.Issue, bool) {
	return m.cursor().selected()
}

// currentIssue resolves the Issue an action applies to: the detail Issue while
// the detail or the label picker is open (the picker always displays
// m.detailIssue, so acting on the digest selection instead could mutate a
// different Issue than the one on screen), otherwise the digest selection.
func (m model) currentIssue() (store.Issue, bool) {
	if (m.mode == viewIssue || m.mode == viewLabels) && m.detailIssue.ID != "" {
		return m.detailIssue, true
	}
	return m.selectedIssue()
}

func (m *model) moveDetailIssue(delta int) {
	issues := m.allIssues()
	idx := -1
	for i, issue := range issues {
		if issue.ID == m.detailIssue.ID {
			idx = i
			break
		}
	}
	if idx == -1 {
		// The open Issue is not in the navigable list (e.g. it just moved into a
		// hidden section) — jumping from index 0 would land on an unrelated Issue.
		return
	}
	next := min(max(idx+delta, 0), len(issues)-1)
	if issues[next].ID != m.detailIssue.ID {
		m.showIssue(issues[next])
	}
}

// allIssues lists the Issues prev/next navigation walks: the sections as the
// originating view shows them — the Board displays hidden sections, so a detail
// opened from it navigates across them too, and a detail opened from the
// Batches surface walks that surface's listed rows (collapsed Batches skipped).
func (m model) allIssues() []store.Issue {
	if m.detailReturnMode() == viewBatches {
		var issues []store.Issue
		for _, section := range m.batchSections {
			if section.collapsed {
				continue
			}
			issues = append(issues, batchIssues(section)...)
		}
		return issues
	}
	includeHidden := m.detailReturnMode() == viewBoard
	var issues []store.Issue
	for _, section := range m.sections {
		if section.hidden && !includeHidden {
			continue
		}
		issues = append(issues, section.Issues...)
	}
	return issues
}

// focusIssue points the Digest cursor at an Issue when it still renders.
func (m *model) focusIssue(id string) {
	m.digestCursor().focusRow(id)
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

// loadLinkTitles resolves linked Issue titles from the already-loaded sections
// (which hold every status), falling back to the store only for Issues created
// since the last reload. A missing target renders blank; a real store error
// surfaces through loadErr instead of being silently swallowed.
func (m *model) loadLinkTitles(issue store.Issue) map[string]string {
	titles := map[string]string{}
	resolve := func(ids []string) {
		for _, id := range ids {
			if linked, ok := m.issueInSections(id); ok {
				titles[id] = linked.Title
				continue
			}
			linked, err := m.store.FindIssue(m.project, id)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					m.loadErr = err
				}
				continue
			}
			titles[id] = linked.Title
		}
	}
	resolve(issue.BlockedBy)
	resolve(issue.RelatesTo)
	resolve(issue.ConflictsWith)
	return titles
}

func (m model) issueDetailView() string {
	top, body, bottom, _ := m.detailLayout()
	body = windowDetailBody(body, m.detailScroll, m.detailBodyBudget(len(top)+len(bottom)))
	lines := append(append(top, body...), bottom...)
	return strings.Join(lines, "\n")
}

// detailLayout builds the Issue detail in three blocks: a fixed top (header,
// meta, links, dates), the scrollable body, and a fixed bottom (rule + shortcut
// bar, or the inline command line). The View windows only the body so the header
// stays put while a long body scrolls (PgUp/PgDn, j/k).
func (m model) detailLayout() (top, body, bottom []string, width int) {
	issue := m.detailIssue
	width = m.viewWidth()
	bodyWidth := min(detailBodyWidth, width-2)

	dot := styleDim.Render("   ·   ")
	meta := " " + styleStatus.Render(issue.Status) + dot + styledPriorityWord(issue.Priority)
	if len(issue.Labels) > 0 {
		meta += dot + labelChips(issue.Labels, "  ")
	}
	top = []string{
		issueHeader(m.project.Name, issue, width),
		fullRule(width),
		"",
		meta,
		"",
	}

	var links []string
	for _, id := range issue.BlockedBy {
		links = append(links, m.linkLine("blocked by", id))
	}
	for _, id := range issue.RelatesTo {
		links = append(links, m.linkLine("relates to", id))
	}
	for _, id := range issue.ConflictsWith {
		links = append(links, m.linkLine("conflicts with", id))
	}
	if len(links) > 0 {
		top = append(top, links...)
		top = append(top, "")
	}

	top = append(top,
		" "+styleDim.Render("created")+"      "+styleText.Render(issue.Created),
		" "+styleDim.Render("updated")+"      "+styleText.Render(issue.Updated),
		"",
		fullRule(width),
		"",
	)

	for _, line := range strings.Split(issue.Body, "\n") {
		for _, wrapped := range wrapLine(line, bodyWidth) {
			body = append(body, " "+wrapped)
		}
	}

	bottom = []string{"", fullRule(width)}
	if m.commandOpen {
		bottom = append(bottom, m.commandBottomBar())
	} else {
		bottom = append(bottom, statusBar(
			[2]string{"esc", "back"}, [2]string{"↑↓", "prev/next"}, [2]string{"s", "status"},
			[2]string{"p", "priority"}, [2]string{"l", "labels"}, [2]string{"r", "refresh"},
			[2]string{":", "cmd"}, [2]string{"q", "quit"},
		))
	}
	return top, body, bottom, width
}

// detailBodyBudget is how many lines the Issue body region may occupy, given the
// fixed chrome around it. Before the first WindowSizeMsg (height 0) the body is
// shown whole.
func (m model) detailBodyBudget(chrome int) int {
	if m.height <= 0 {
		return 1 << 20
	}
	return max(1, m.height-chrome)
}

// scrollDetailBody moves the Issue body viewport by a page (PgUp/PgDn) or a
// single line (j/k), clamped so it never scrolls past the last line.
func (m *model) scrollDetailBody(direction int, byPage bool) {
	top, body, bottom, _ := m.detailLayout()
	budget := m.detailBodyBudget(len(top) + len(bottom))
	step := direction
	if byPage {
		step = direction * max(1, budget-1)
	}
	m.detailScroll = min(maxDetailScroll(len(body), budget), max(0, m.detailScroll+step))
}

func maxDetailScroll(total, budget int) int {
	if total <= budget {
		return 0
	}
	// At the bottom the top "↑ N more" indicator costs one line, so the last
	// window starts here — matching scrollWindow's bottom case.
	return max(0, total-(budget-1))
}

// windowDetailBody slices the body to the lines visible at the given scroll
// offset, prefixing/suffixing dim "↑ N more" / "↓ N more" indicators (each
// costing a line from the budget) — the same overflow affordance the Digest and
// Board use.
func windowDetailBody(body []string, scroll, budget int) []string {
	start, end, above, below := scrollWindow(len(body), scroll, budget)
	out := make([]string, 0, budget)
	if above {
		out = append(out, styleDim.Render(fmt.Sprintf("   ↑ %d more", start)))
	}
	out = append(out, body[start:end]...)
	if below {
		out = append(out, styleDim.Render(fmt.Sprintf("   ↓ %d more", len(body)-end)))
	}
	return out
}

// scrollWindow returns the [start,end) slice of total lines to show at a scroll
// offset within a line budget, and whether overflow indicators are needed above
// and below. Each indicator costs one line, so the visible total never exceeds
// the budget.
func scrollWindow(total, scroll, budget int) (start, end int, above, below bool) {
	if budget < 1 {
		budget = 1
	}
	if total <= budget {
		return 0, total, false, false
	}
	if scroll < 0 {
		scroll = 0
	}
	bottomStart := total - (budget - 1) // last window: one line spent on the ↑ indicator
	if scroll >= bottomStart {
		return bottomStart, total, bottomStart > 0, false
	}
	if scroll == 0 {
		return 0, budget - 1, false, true // first window: one line spent on the ↓ indicator
	}
	// Mid window: both indicators. On a degenerate 1-line budget the indicators
	// take it all — clamp so the slice never inverts (body[start:end], end < start).
	return scroll, max(scroll, scroll+budget-2), true, true
}

func (m model) labelPickerView() string {
	issue := m.detailIssue
	width := m.viewWidth()
	lines := []string{
		issueHeader(m.project.Name, issue, width),
		fullRule(width),
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
		fullRule(width),
		statusBar([2]string{"↑↓", "move"}, [2]string{"⏎", "toggle"}, [2]string{"esc", "done"}, [2]string{"q", "quit"}),
	)
	return strings.Join(lines, "\n")
}

func (m model) projectPickerView() string {
	width := m.viewWidth()
	lines := []string{
		padBetween(" ito · switch project", m.project.Name+" ", width),
		fullRule(width),
		"",
	}
	if len(m.projects) == 0 {
		lines = append(lines, emptyState("no Projects yet", "ito init", "to get started")...)
		lines = append(lines, "", statusBar([2]string{"q", "quit"}))
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
		fullRule(width),
		statusBar([2]string{"↑↓", "move"}, [2]string{"⏎", "switch"}, [2]string{"esc", "cancel"}, [2]string{"q", "quit"}),
	)
	return strings.Join(lines, "\n")
}

func header(projectName string, count, width int, active viewMode) string {
	// Measure the plain text so the gap counts visible runes only, then style
	// each segment — colouring adds no visible width. The numbers stay in the
	// default ink; only the active view name takes the accent colour.
	// Inset by one space on each side so the header aligns with the content rows
	// below (which all start at column 1) while the rule spans edge to edge.
	left := " ito · [1] digest · [2] batches"
	noun := "issues"
	if active == viewBatches {
		noun = "batches"
	}
	right := fmt.Sprintf("%d %s   %s ", count, noun, projectName)
	gap := width - runeLen(left) - runeLen(right)
	if gap < 1 {
		gap = 1
	}

	tab := func(name string, mode viewMode) string {
		if mode == active {
			return styleActive.Render(name)
		}
		return styleText.Render(name)
	}
	styledLeft := styleText.Render(" ito · [1] ") + tab("digest", viewDigest) +
		styleText.Render(" · [2] ") + tab("batches", viewBatches)
	return styledLeft + strings.Repeat(" ", gap) + styleText.Render(right)
}

// boardHeader is the Board's crumb header. The Board lives behind the :
// command line, not the header tabs, so its name sits where the tab set
// would — in the accent, like any active view name.
func boardHeader(projectName string, count, width int) string {
	left := " ito · board"
	right := fmt.Sprintf("%d issues   %s ", count, projectName)
	gap := width - runeLen(left) - runeLen(right)
	if gap < 1 {
		gap = 1
	}
	return styleText.Render(" ito · ") + styleActive.Render("board") +
		strings.Repeat(" ", gap) + styleText.Render(right)
}

func issueHeader(projectName string, issue store.Issue, width int) string {
	// Same inset shape as header(): a leading space on the crumb and a trailing
	// space on the project name so the line aligns with the content below. The
	// Issue id is cyan, like every other id in the surfaces.
	prefix, sep := " ito · ", " · "
	right := projectName + " "
	maxLeft := width - runeLen(right) - 1
	plainLeft := prefix + issue.ID + sep + issue.Title
	if maxLeft < 1 {
		return truncate(plainLeft+" "+right, width)
	}
	titleBudget := maxLeft - runeLen(prefix) - runeLen(issue.ID) - runeLen(sep)
	if titleBudget < 1 {
		// The id and chrome already fill the line — fall back to plain truncation.
		return padBetween(truncate(plainLeft, maxLeft), right, width)
	}
	title := truncate(issue.Title, titleBudget)
	plain := prefix + issue.ID + sep + title
	styled := styleText.Render(prefix) + styleID.Render(issue.ID) + styleText.Render(sep+title)
	gap := width - runeLen(plain) - runeLen(right)
	if gap < 1 {
		gap = 1
	}
	return styled + strings.Repeat(" ", gap) + styleText.Render(right)
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

// labelChips renders each label as a filled chip joined by sep — the styling
// applies per word only, so the plain width matches strings.Join(labels, sep).
// The Issue meta line joins with two spaces, the Digest rows with one.
func labelChips(labels []string, sep string) string {
	chips := make([]string, len(labels))
	for i, label := range labels {
		chips[i] = styleLabel.Render(label)
	}
	return strings.Join(chips, sep)
}

// styledPriorityWord colours the spelled-out priority on the Issue meta line —
// the same hues as the marks, urgent/high bold, low in its own muted colour.
func styledPriorityWord(priority string) string {
	switch priority {
	case "urgent":
		return stylePriorityWordUrgent.Render(priority)
	case "high":
		return stylePriorityWordHigh.Render(priority)
	case "medium":
		return stylePriorityWordMedium.Render(priority)
	default:
		return stylePriorityWordLow.Render(priority)
	}
}

// linkLine renders a link row: a dim label, the linked id in cyan, then the
// linked title aligned past linkIDWidth.
func (m model) linkLine(label, id string) string {
	pad := strings.Repeat(" ", max(1, linkIDWidth-runeLen(id)))
	return " " + styleDim.Render(label) + "   " + styleID.Render(id) + pad + styleText.Render(m.linkTitles[id])
}

// renderIssueRow draws a Digest row across width: priority mark, id and title on
// the left, the link markers and labels right-aligned to the edge — the title
// absorbs the slack and truncates when the two groups would collide.
func renderIssueRow(issue store.Issue, width int) string {
	plainRight, styledRight := issueRowRight(issue, true)
	mark, id := priorityMark(issue.Priority), issue.ID
	fixed := runeLen(mark) + 1 + runeLen(id) + 1 // "mark id " before the title
	title := truncate(issue.Title, max(0, width-runeLen(plainRight)-fixed))
	styledLeft := styledPriorityMark(issue.Priority) + " " + styleID.Render(id) + " " + styleText.Render(title)

	if plainRight == "" {
		return styledLeft
	}
	gap := max(0, width-runeLen(plainRight)-fixed-runeLen(title))
	return styledLeft + strings.Repeat(" ", gap) + styledRight
}

func issueRowRight(issue store.Issue, includeLabels bool) (string, string) {
	plainRight, styledRight := "", ""
	if len(issue.BlockedBy) > 0 {
		blockers := strings.Join(issue.BlockedBy, ",")
		plainRight += "⊘ " + blockers + "   "
		styledRight += styleBlock.Render("⊘ ") + styleID.Render(blockers) + "   "
	}
	if len(issue.ConflictsWith) > 0 {
		conflicts := strings.Join(issue.ConflictsWith, ",")
		plainRight += "⊘ " + conflicts + "   "
		styledRight += styleConflict.Render("⊘ ") + styleID.Render(conflicts) + "   "
	}
	if includeLabels && len(issue.Labels) > 0 {
		plainRight += strings.Join(issue.Labels, " ") + " "
		styledRight += labelChips(issue.Labels, " ") + " "
	}
	return plainRight, styledRight
}

func renderBoardIssue(issue store.Issue, selected bool, width int) string {
	pointer, styledPointer := " ", styleText.Render(" ")
	if selected {
		pointer, styledPointer = "▸", styleActive.Render("▸")
	}
	// Board rows omit labels in narrow columns, but keep link markers so the
	// traceability signal matches Digest.
	plainRight, styledRight := issueRowRight(issue, false)
	plainPrefix := fmt.Sprintf("%s %s %s ", pointer, priorityMark(issue.Priority), issue.ID)
	styledPrefix := styledPointer + " " + styledPriorityMark(issue.Priority) + " " + styleID.Render(issue.ID) + " "

	titleWidth := width - runeLen(plainPrefix) - runeLen(plainRight)
	if titleWidth < 1 {
		return truncate(plainPrefix+issue.Title, width) // too narrow to style cleanly
	}
	title := truncate(issue.Title, titleWidth)
	if plainRight == "" {
		return padStyled(styledPrefix+styleText.Render(title), plainPrefix+title, width)
	}
	gap := max(0, width-runeLen(plainPrefix)-runeLen(title)-runeLen(plainRight))
	return styledPrefix + styleText.Render(title) + strings.Repeat(" ", gap) + styledRight
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

// styledPriorityMark colours the priority mark: urgent red, high orange, medium
// blue, low left in the default ink.
func styledPriorityMark(priority string) string {
	switch priority {
	case "urgent":
		return stylePriorityUrgent.Render("●")
	case "high":
		return stylePriorityHigh.Render("▲")
	case "medium":
		return stylePriorityMedium.Render("◆")
	default:
		return styleText.Render("·")
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
		if runeLen(next) <= width {
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
