package tui

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/c3h/ito/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// Surface frame width: capped at a readable measure so it never sprawls on a
// wide terminal, but shrinks to fit a narrow one — see model.viewWidth. All
// three surfaces (digest, board, issue) share this so switching views never
// reflows the frame.
const (
	surfaceMaxWidth = 100
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
// bottom rule, shortcut bar) and sectionChromeLines per section (heading +
// blank).
const (
	digestChromeLines  = 4
	sectionChromeLines = 2
)

// detailBodyWidth keeps the Issue body at a readable prose measure, narrower
// than the full surface frame. On a narrow terminal the body shrinks below
// this to fit (see issueDetailView).
const detailBodyWidth = 80

// linkIDWidth is the column the linked Issue id occupies before its title in
// the detail view, so the titles line up across link rows.
const linkIDWidth = 8

// detailLabelWidth is the gutter the detail view's field labels occupy, so
// branch, created and updated share one column whichever of them renders.
const detailLabelWidth = 13

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
	// completedShown reveals the Batches surface's completed rollup, the one
	// section standing in for every fully done Batch.
	completedShown bool
	focusIndex     int
	mode           viewMode
	returnMode     viewMode
	detailIssue    store.Issue
	detailScroll   int
	linkTitles     map[string]string
	labelCursor    int
	projects       []store.Project
	projectCursor  int
	filterOpen     bool
	filterQuery    string
	commandOpen    bool
	commandQuery   string
	// sync runs a Ledger sync, nil when no Ledger is connected; syncing is true
	// while one is in flight and the header shows the indicator.
	sync    SyncFunc
	syncing bool
	// note is a one-line notice the bottom bar shows until the next key press.
	note    string
	loadErr error
	width   int
	height  int
}

type digestSection struct {
	Label    string
	Issues   []store.Issue
	selected int
	// top is the first visible row — the scroll offset the cursor walks within
	// before the list slides, persisted so scrolling sticks to an edge rather
	// than riding the centre.
	top    int
	hidden bool
}

// statusLabel renders a store status as its Digest section heading
// (in_progress → "IN PROGRESS"), keeping the flow order owned by store.Statuses.
func statusLabel(status string) string {
	return strings.ToUpper(strings.ReplaceAll(status, "_", " "))
}

func Run(st *store.Store, project store.Project, opts Options) error {
	if st == nil {
		return fmt.Errorf("store is required")
	}
	_, err := tea.NewProgram(newModel(st, project, opts), tea.WithAltScreen()).Run()
	return err
}

func newModel(st *store.Store, project store.Project, opts Options) model {
	m := model{
		store:      st,
		project:    project,
		sync:       opts.Sync,
		mode:       viewDigest,
		linkTitles: map[string]string{},
	}
	if project.ID == 0 {
		m.openProjectPicker()
	} else {
		m.reload()
	}
	// Init runs the sync on this model by value, so the in-flight mark is set
	// here, where it sticks — by the one rule startSync also asks.
	m.syncing = m.shouldSync()
	return m
}

// Init starts the background sync newModel marked: the first View already
// shows the local data, and the sync lands whenever the Ledger answers.
func (m model) Init() tea.Cmd {
	if !m.syncing {
		return nil
	}
	return m.syncCmd()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case prSyncMsg:
		m.applyPRSync(msg)
	case syncMsg:
		m.applySync(msg)
	case tea.KeyMsg:
		m.note = ""
		if m.filterOpen {
			return m, editInlineInput(&m.filterOpen, &m.filterQuery, msg)
		}
		if m.commandOpen {
			if msg.Type == tea.KeyEnter {
				cmd = m.runSelectedCommandAction()
				break
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
			if m.isSurfaceMode() {
				m.mode = viewBatches
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
			if m.mode != viewLabels {
				cmd = m.refresh()
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
	m.syncScroll()
	return m, cmd
}

// syncScroll advances each section's scroll offset to the window the View is
// about to render, so the offset persists across moves. The window math keeps
// the selected row visible on its own — persisting the offset is what makes the
// cursor walk to an edge and stick there instead of snapping back to centre.
// While a filter narrows the list the rendered copies derive their own offset
// (reset to the top), so there is nothing to persist.
func (m *model) syncScroll() {
	if m.filtering() {
		return
	}
	switch m.mode {
	case viewDigest:
		caps := m.digestSectionCapacities(m.sections)
		for i := range m.sections {
			m.sections[i].top = visibleIssueWindow(len(m.sections[i].Issues), m.sections[i].selected, m.sections[i].top, caps[i]).start
		}
	case viewBoard:
		budget := m.boardIssueLineBudget()
		for i := range m.sections {
			m.sections[i].top = visibleIssueWindow(len(m.sections[i].Issues), m.sections[i].selected, m.sections[i].top, budget).start
		}
	case viewBatches:
		for _, block := range m.batchBlocks(m.batchSections) {
			if block.completed != nil {
				continue // the rollup lists no rows, so it has nothing to scroll
			}
			window, _ := batchBodyWindow(m.batchSections[block.index], block.rows, block.budget)
			m.batchSections[block.index].top = window.start
		}
	}
}

// filtering reports whether the live / filter is narrowing the current surface.
func (m model) filtering() bool {
	return strings.TrimSpace(m.filterQuery) != ""
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
	var body []string
	windows := m.digestWindows(sections)
	filtering := m.filtering()
	for i, section := range sections {
		if filtering && len(section.Issues) == 0 {
			continue // while filtering, only sections with matches are shown
		}
		focused := i == m.focusIndex
		if section.hidden {
			body = append(body, sectionHeading(section.Label, len(section.Issues), focused, true, width))
			body = append(body, "")
			continue
		}
		body = append(body, sectionHeading(section.Label, len(section.Issues), focused, false, width))
		window := windows[i]
		rows := make([]string, 0, window.end-window.start)
		for j, issue := range section.Issues[window.start:window.end] {
			issueIndex := window.start + j
			// Two leading spaces put the cursor under the heading's ▾ and the
			// priority mark under the first letter of the status label.
			prefix := "    "
			if focused && issueIndex == section.selected {
				prefix = "  " + styleActive.Render("▸") + " "
			}
			rows = append(rows, prefix+renderIssueRow(issue, width-4))
		}
		body = append(body, withOverflow(window, len(section.Issues), rows)...)
		body = append(body, "")
	}
	return surfaceFrame(header(m.project.Name, activeIssueCount(sections), width, viewDigest, m.syncing),
		body, m.digestBottomBar(issueCount(sections), issueCount(m.sections)), width)
}

// surfaceFrame wraps a tab surface's body between its header and bottom bar: the
// header line and a full rule above, a full rule and the bottom bar below. It
// owns the frame's spacing — no blank padding hugs either rule, and any trailing
// blanks the sections left are trimmed, so the bottom rule never floats a line
// off the last row. digestChromeLines counts these four fixed lines.
func surfaceFrame(headerLine string, body []string, bottomBar string, width int) string {
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}
	lines := make([]string, 0, len(body)+4)
	lines = append(lines, headerLine, fullRule(width))
	lines = append(lines, body...)
	lines = append(lines, fullRule(width), bottomBar)
	return strings.Join(lines, "\n")
}

func (m model) boardView() string {
	sections := m.boardSections()
	width := m.boardViewWidth(len(sections))
	visible := m.visibleBoardSections(sections, width)
	var body []string

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
		body = append(body, line)
	}

	return surfaceFrame(boardHeader(m.project.Name, activeIssueCount(sections), width, m.syncing),
		body, m.boardBottomBar(issueCount(sections), issueCount(m.sections)), width)
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
	window := visibleIssueWindow(len(section.Issues), section.selected, section.top, m.boardIssueLineBudget())
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
	if m.note != "" {
		return m.noteBar()
	}
	return statusBar(keys...)
}

// noteBar renders the pending notice in the bottom bar, cut to the frame so a
// long Ledger error never wraps the line.
func (m model) noteBar() string {
	return " " + styleDim.Render(truncate(m.note, m.viewWidth()-1))
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

func (m *model) runSelectedCommandAction() tea.Cmd {
	actions := m.filteredCommandActions()
	if len(actions) == 0 {
		return nil
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
		return m.refresh()
	case "quit":
		return tea.Quit
	}
	return nil
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
		return m, m.switchToSelectedProject()
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

// switchToSelectedProject lands on the chosen Project and starts its sync;
// a sync still running for the previous Project is disowned, its result
// dropped on arrival.
func (m *model) switchToSelectedProject() tea.Cmd {
	if len(m.projects) == 0 || m.projectCursor < 0 || m.projectCursor >= len(m.projects) {
		return nil
	}
	m.project = m.projects[m.projectCursor]
	m.sections = nil
	m.batchSections = nil
	m.batchFocus = 0
	m.completedShown = false
	m.focusIndex = 0
	m.detailIssue = store.Issue{}
	m.linkTitles = map[string]string{}
	m.filterOpen = false
	m.filterQuery = ""
	m.commandOpen = false
	m.commandQuery = ""
	m.mode = viewDigest
	m.syncing = false
	m.reload()
	return m.startSync()
}

// reload re-reads every surface from the store in one pass, loading the Digest
// and Batches concurrently before applying either result. A refresh from any
// view leaves the Digest, the Board and the Batches updated together —
// switching tabs never lands on a surface that was left behind. Both loads
// always run; the first failure is the one reported, and a failing load leaves
// its previous rows on screen. It returns the Digest snapshot it read so a
// caller that needs the flat Issue list does not rebuild it from the sections.
func (m *model) reload() []store.Issue {
	var (
		digestIssues []store.Issue
		digestErr    error
		batches      []batchLoad
		batchErr     error
	)
	var loads sync.WaitGroup
	loads.Add(2)
	go func() {
		defer loads.Done()
		digestIssues, digestErr = m.loadDigest()
	}()
	go func() {
		defer loads.Done()
		batches, batchErr = m.loadBatches()
	}()
	loads.Wait()

	if digestErr == nil {
		digestErr = m.reloadDigest(digestIssues)
	}
	if batchErr == nil {
		batchErr = m.reloadBatches(batches)
	}
	m.loadErr = cmp.Or(digestErr, batchErr)
	return digestIssues
}

// refresh re-reads every surface and schedules the PR sync against the Issues
// that reload just returned, plus a Ledger sync when one is connected.
func (m *model) refresh() tea.Cmd {
	return tea.Batch(syncPRsCmd(m.project, m.reload()), m.startSync())
}

// applyPRSync writes the moves gh reported and notes how many landed. A message
// for a Project the user has since left is dropped before any write.
func (m *model) applyPRSync(msg prSyncMsg) {
	if msg.Project.ID != m.project.ID {
		return
	}
	updated := 0
	for _, move := range msg.Moves {
		result, err := m.store.Move(msg.Project, move.ID, move.Status)
		if err != nil {
			m.loadErr = err
			continue
		}
		if result.Changed {
			updated++
		}
	}
	if updated > 0 {
		m.reload()
		m.note = fmt.Sprintf("PR sync: %d updated", updated)
	}
}

func (m *model) loadDigest() ([]store.Issue, error) {
	// One query for every status: a single snapshot, so a concurrent write can
	// never show an Issue in two sections (or in none) within the same reload.
	return m.store.ListIssues(store.ListOptions{
		ProjectID:   m.project.ID,
		IncludeDone: true,
	})
}

func (m *model) reloadDigest(all []store.Issue) error {
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
			titles, err := m.loadLinkTitles(refreshed)
			m.linkTitles = titles
			m.focusIssue(detailID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type issueWindow struct {
	start     int
	end       int
	showAbove bool
	showBelow bool
}

// withOverflow wraps rendered rows in the dim "↑/↓ N more" markers, so a body
// the line budget cut never looks complete.
func withOverflow(window issueWindow, total int, rows []string) []string {
	lines := make([]string, 0, len(rows)+2)
	if window.showAbove {
		lines = append(lines, styleDim.Render(fmt.Sprintf("    ↑ %d more", window.start)))
	}
	lines = append(lines, rows...)
	if window.showBelow {
		lines = append(lines, styleDim.Render(fmt.Sprintf("    ↓ %d more", total-window.end)))
	}
	return lines
}

func (m model) digestWindows(sections []digestSection) []issueWindow {
	windows := make([]issueWindow, len(sections))
	capacities := m.digestSectionCapacities(sections)
	for i, section := range sections {
		if section.hidden {
			continue
		}
		windows[i] = visibleIssueWindow(len(section.Issues), section.selected, section.top, capacities[i])
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
	// Every Digest section spends sectionChromeLines on its heading and trailing
	// blank, including empty and hidden ones, so hiding preserves the surface's
	// visual rhythm — it has five fixed sections and can afford it.
	return allocateLineBudgets(counts, m.height, digestChromeLines+len(sections)*sectionChromeLines)
}

// allocateLineBudgets splits the terminal height left over after chrome across
// sections wanting counts[i] lines each — shared by the Digest sections and the
// Batch bodies, which each account for their own chrome. When everything fits
// each section gets its full count; otherwise non-empty sections start at one
// line and grow round-robin.
func allocateLineBudgets(counts []int, height, chrome int) []int {
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

	available := height - chrome
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

// visibleIssueWindow places a capacity-sized window over total rows at the
// stored scroll offset top, shrinking the row capacity to make room for the
// "↑/↓ N more" overflow indicators when they apply. selected is kept inside
// the window so the surfaces never lose the cursor.
func visibleIssueWindow(total, selected, top, lineBudget int) issueWindow {
	if total <= 0 || lineBudget <= 0 {
		return issueWindow{}
	}
	if lineBudget >= total {
		return issueWindow{end: total}
	}
	selected = min(max(selected, 0), total-1)

	issueCapacity := min(lineBudget, total)
	for {
		start := scrollTop(top, selected, total, issueCapacity)
		window := issueWindow{start: start, end: start + issueCapacity}
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

// scrollTop resolves a window's first visible row: it keeps the stored offset
// put while the cursor stays inside the window — so the cursor walks all the
// way to an edge before anything scrolls — and shifts the offset by just enough
// to reveal the cursor when it would fall off either edge, sticking there.
func scrollTop(top, selected, total, capacity int) int {
	if capacity >= total {
		return 0
	}
	top = min(max(top, 0), total-capacity)
	switch {
	case selected < top:
		return selected
	case selected >= top+capacity:
		return selected - capacity + 1
	default:
		return top
	}
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
		filtered.top = 0
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

// batchCursor builds the cursor over the Batches surface's stops: the Batches
// with open work, then the completed rollup. A collapsed Batch folds away and
// the surface flows across Batch boundaries like the Digest. The rollup is a
// stop so tab reaches it and h reveals it, but it lists no rows — a fully done
// Batch has no open member to select — so its selection is a scratch cell the
// cursor writes to and nothing reads.
func (m *model) batchCursor() rowCursor {
	open := openBatchCount(m.batchSections)
	stops := make([]rowStop, 0, open+1)
	for i := range m.batchSections[:open] {
		stops = append(stops, rowStop{
			rows:      batchIssues(m.batchSections[i]),
			collapsed: m.batchSections[i].collapsed,
			selected:  &m.batchSections[i].selected,
		})
	}
	if open < len(m.batchSections) {
		stops = append(stops, rowStop{collapsed: true, selected: new(int)})
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
	m.setLinkTitles(issue)
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

// reloadAfterEdit refreshes every surface from the store and keeps the edited
// Issue focused on each of them, re-rendering whichever detail surface is open
// so the change shows immediately.
func (m *model) reloadAfterEdit(edited store.Issue) {
	m.reload()
	m.focusIssue(edited.ID)
	m.focusBatchIssue(edited.ID)
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
	m.setLinkTitles(issue)
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
// since the last reload. A missing target renders blank; a real store error is
// returned rather than written to loadErr, because the reload path owns that
// field — a failure raised inside a reload has to travel back up to it.
func (m *model) loadLinkTitles(issue store.Issue) (map[string]string, error) {
	titles := map[string]string{}
	var failure error
	resolve := func(ids []string) {
		for _, id := range ids {
			if linked, ok := m.issueInSections(id); ok {
				titles[id] = linked.Title
				continue
			}
			linked, err := m.store.FindIssue(m.project, id)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					failure = cmp.Or(failure, err)
				}
				continue
			}
			titles[id] = linked.Title
		}
	}
	resolve(issue.BlockedBy)
	resolve(issue.RelatesTo)
	resolve(issue.ConflictsWith)
	return titles, failure
}

// setLinkTitles is the non-reload path into loadLinkTitles: opening a detail
// surface reports its own failure straight away.
func (m *model) setLinkTitles(issue store.Issue) {
	titles, err := m.loadLinkTitles(issue)
	m.linkTitles = titles
	if err != nil {
		m.loadErr = err
	}
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
		issueHeader(m.project.Name, issue, width, m.syncing),
		fullRule(width),
		"",
		meta,
		"",
	}
	if issue.Branch != "" {
		top = append(top, metaLine("branch", issue.Branch), "")
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
		metaLine("created", issue.Created),
		metaLine("updated", issue.Updated),
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
	} else if m.note != "" {
		bottom = append(bottom, m.noteBar())
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
		issueHeader(m.project.Name, issue, width, m.syncing),
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

func header(projectName string, count, width int, active viewMode, syncing bool) string {
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
	badge := syncBadge(syncing)
	right := fmt.Sprintf("%d %s   %s ", count, noun, projectName)
	gap := width - runeLen(left) - runeLen(badge) - runeLen(right)
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
	return styledLeft + strings.Repeat(" ", gap) + headerRight(badge, right)
}

// headerRight styles a header's right segment: the dim sync badge, when one
// shows, ahead of the count and Project name in the default ink.
func headerRight(badge, right string) string {
	if badge == "" {
		return styleText.Render(right)
	}
	return styleDim.Render(badge) + styleText.Render(right)
}

// boardHeader is the Board's crumb header. The Board lives behind the :
// command line, not the header tabs, so its name sits where the tab set
// would — in the accent, like any active view name.
func boardHeader(projectName string, count, width int, syncing bool) string {
	left := " ito · board"
	badge := syncBadge(syncing)
	right := fmt.Sprintf("%d issues   %s ", count, projectName)
	gap := width - runeLen(left) - runeLen(badge) - runeLen(right)
	if gap < 1 {
		gap = 1
	}
	return styleText.Render(" ito · ") + styleActive.Render("board") +
		strings.Repeat(" ", gap) + headerRight(badge, right)
}

func issueHeader(projectName string, issue store.Issue, width int, syncing bool) string {
	// Same inset shape as header(): a leading space on the crumb and a trailing
	// space on the project name so the line aligns with the content below. The
	// Issue id is cyan, like every other id in the surfaces.
	prefix, sep := " ito · ", " · "
	badge := syncBadge(syncing)
	name := projectName + " "
	// right is the plain right segment — badge included — for the width
	// maths and the truncating fallbacks; the styled form splits it again.
	right := badge + name
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
	return styled + strings.Repeat(" ", gap) + headerRight(badge, name)
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

// metaLine renders one of the Issue detail's scalar fields, every label padded
// to the same gutter so the values line up whichever fields the Issue carries.
func metaLine(label, value string) string {
	pad := strings.Repeat(" ", max(1, detailLabelWidth-runeLen(label)))
	return " " + styleDim.Render(label) + pad + styleText.Render(value)
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

// maxRowLinkIDs caps how many linked ids a Digest/Board row lists inline before
// the rest collapse into a dim "…(N)" count, so a heavily-linked issue can't blow
// out the row width.
const maxRowLinkIDs = 3

// rowLinkIDs renders up to maxRowLinkIDs ids joined by commas, then a dim "…(N)"
// summary for the remainder. Returns the plain and styled forms; their widths
// match so callers can lay out with runeLen on the plain form.
func rowLinkIDs(ids []string) (string, string) {
	if len(ids) <= maxRowLinkIDs {
		joined := strings.Join(ids, ",")
		return joined, styleID.Render(joined)
	}
	shown := strings.Join(ids[:maxRowLinkIDs], ",")
	rest := fmt.Sprintf(" …(%d)", len(ids)-maxRowLinkIDs)
	return shown + rest, styleID.Render(shown) + styleDim.Render(rest)
}

func issueRowRight(issue store.Issue, includeLabels bool) (string, string) {
	plainRight, styledRight := "", ""
	if len(issue.BlockedBy) > 0 {
		blockers, styledBlockers := rowLinkIDs(issue.BlockedBy)
		plainRight += "⊘ " + blockers + "   "
		styledRight += styleBlock.Render("⊘ ") + styledBlockers + "   "
	}
	if len(issue.ConflictsWith) > 0 {
		conflicts, styledConflicts := rowLinkIDs(issue.ConflictsWith)
		plainRight += "⊘ " + conflicts + "   "
		styledRight += styleConflict.Render("⊘ ") + styledConflicts + "   "
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
