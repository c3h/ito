package tui

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/c3h/ito/internal/store"
)

// batchSection is one Batch rendered as a Digest-style section. Its row groups
// come from the shared core derivation (store.ShowBatch) — the TUI never
// recomputes them — and a cyclic Batch carries the Issues in the cycle instead.
// selected indexes the flattened listed rows (batchRows), so the cursor
// crosses sub-headings transparently.
type batchSection struct {
	batch     store.Batch
	groups    []batchGroup
	cycle     []string
	collapsed bool
	selected  int
	// top is the scroll offset into the listed rows, kept so the cursor walks to
	// an edge before the body slides, like the Digest.
	top int
}

// batchGroup is one labeled run of rows inside a Batch: a derived Wave, or the
// waiting members no Wave can place. Flattening both into one list is what lets
// every height, window and render walk a single sequence of headed groups.
type batchGroup struct {
	label string
	state string
	// ready renders state in the READY ink instead of dimmed.
	ready bool
	// waiting marks the one group that is not a Wave, so it stays out of the
	// heading's wave count.
	waiting bool
	issues  []store.Issue
}

type batchReload struct {
	batches []store.Batch
	plans   []batchPlanResult
}

type batchPlanResult struct {
	plan store.BatchPlan
	err  error
}

// batchGroups turns a derived plan into the groups the surface draws: one per
// Wave, then the members gated outside the Batch when the derivation left any.
func batchGroups(plan store.BatchPlan) []batchGroup {
	groups := make([]batchGroup, 0, len(plan.Waves)+1)
	for _, wave := range plan.Waves {
		state := "WAITING"
		if wave.Ready {
			state = "READY"
		}
		groups = append(groups, batchGroup{
			label:  fmt.Sprintf("WAVE %d", wave.Wave),
			state:  state,
			ready:  wave.Ready,
			issues: wave.Issues,
		})
	}
	if len(plan.Waiting) > 0 {
		groups = append(groups, batchGroup{
			label:   "WAITING",
			state:   "BLOCKED OUTSIDE THE BATCH",
			waiting: true,
			issues:  plan.Waiting,
		})
	}
	return groups
}

// loadBatches snapshots the Project's Batches, deriving each open plan through
// the core concurrently. Fully done Batches are skipped because the surface
// only reads their summary.
func (m *model) loadBatches() (batchReload, error) {
	batches, err := m.store.ListBatches(m.project)
	if err != nil {
		return batchReload{}, err
	}

	plans := make([]batchPlanResult, len(batches))
	var loads sync.WaitGroup
	for i, batch := range batches {
		if batchDone(batch) {
			continue
		}
		loads.Add(1)
		go func() {
			defer loads.Done()
			plans[i].plan, plans[i].err = m.store.ShowBatch(m.project, batch.Name)
		}()
	}
	loads.Wait()
	return batchReload{batches: batches, plans: plans}, nil
}

// reloadBatches applies a loaded snapshot. Batches with open work come first,
// newest-first among themselves, and the fully done ones follow — the surface
// rolls those into one section, so their order only decides how the rollup
// lists them. A blocked_by cycle is captured per Batch so one bad graph never
// blanks the surface. Across reloads the focus follows its Batch by name
// (landing on the rollup when that Batch just completed), a manual collapse
// toggle survives, and each selection keeps its Issue by ID, falling back to
// clamping.
func (m *model) reloadBatches(reload batchReload) error {
	previous := make(map[string]batchSection, len(m.batchSections))
	for _, section := range m.batchSections {
		previous[section.batch.Name] = section
	}
	focusedName := ""
	focused, focusedCompleted := m.focusedBatch()
	if focused != nil {
		focusedName = focused.batch.Name
	}

	// Partitioned as it is built: ListBatches is newest-first, so appending to
	// one of the two halves keeps that order inside each.
	open := make([]batchSection, 0, len(reload.batches))
	var completed []batchSection
	for i, b := range reload.batches {
		section := batchSection{batch: b}
		done := batchDone(b)
		// A fully done Batch lists no open member, so it only ever reaches the
		// completed rollup — deriving its plan would be work nothing reads.
		if !done {
			plan, err := reload.plans[i].plan, reload.plans[i].err
			var cycleErr *store.BatchCycleError
			switch {
			case errors.As(err, &cycleErr):
				section.cycle = cycleErr.Issues
			case err != nil:
				return err
			default:
				section.groups = batchGroups(plan)
			}
		}
		if prev, ok := previous[b.Name]; ok {
			section.collapsed = prev.collapsed
			section.selected = restoredBatchSelection(prev, section)
		}
		if done {
			completed = append(completed, section)
			continue
		}
		open = append(open, section)
	}

	m.batchSections = append(open, completed...)
	m.batchFocus = 0
	switch {
	case focusedCompleted && len(completed) > 0:
		m.batchFocus = len(open)
	case focusedName != "":
		for i, section := range m.batchSections {
			if section.batch.Name == focusedName {
				// A Batch that just completed hands the focus to the rollup.
				m.batchFocus = min(i, len(open))
				break
			}
		}
	}
	return nil
}

// focusedBatch resolves the current focus to the Batch it rests on, or reports
// that it rests on the completed rollup instead — the stop just past the
// Batches with open work.
func (m *model) focusedBatch() (*batchSection, bool) {
	open := openBatchCount(m.batchSections)
	switch {
	case m.batchFocus >= 0 && m.batchFocus < open:
		return &m.batchSections[m.batchFocus], false
	case m.batchFocus == open && open < len(m.batchSections):
		return nil, true
	}
	return nil, false
}

// restoredBatchSelection keeps a Batch's selection on the same Issue across a
// reload when it still renders, otherwise clamps the old index to the new rows.
func restoredBatchSelection(prev, next batchSection) int {
	issues := batchIssues(next)
	if id := batchSelectedID(prev); id != "" {
		for j, issue := range issues {
			if issue.ID == id {
				return j
			}
		}
	}
	return min(max(prev.selected, 0), max(0, len(issues)-1))
}

func batchDone(b store.Batch) bool {
	return b.Total > 0 && b.Done == b.Total
}

// openBatchCount is where the completed Batches begin: reloadBatches keeps the
// ones with open work first, so everything from this index on is fully done.
func openBatchCount(sections []batchSection) int {
	for i, section := range sections {
		if batchDone(section.batch) {
			return i
		}
	}
	return len(sections)
}

// batchRow is one listed member paired with the index of the group it sits
// under, so the line budget can count the headings the visible rows bring
// along with them.
type batchRow struct {
	issue store.Issue
	group int
}

// batchRows flattens the rows a Batch lists, in group order — the list the
// selection cursor walks.
func batchRows(section batchSection) []batchRow {
	rows := make([]batchRow, 0, batchRowCount(section))
	for i, group := range section.groups {
		for _, issue := range group.issues {
			rows = append(rows, batchRow{issue: issue, group: i})
		}
	}
	return rows
}

// batchIssues is the same list as batchRows without the group indices, for the
// callers that only walk Issues.
func batchIssues(section batchSection) []store.Issue {
	issues := make([]store.Issue, 0, batchRowCount(section))
	for _, group := range section.groups {
		issues = append(issues, group.issues...)
	}
	return issues
}

// batchRowCount is how many rows a Batch lists, without building them.
func batchRowCount(section batchSection) int {
	total := 0
	for _, group := range section.groups {
		total += len(group.issues)
	}
	return total
}

// batchSelectedID resolves a section's selection to its Issue ID, "" when the
// section lists no rows.
func batchSelectedID(section batchSection) string {
	issues := batchIssues(section)
	if section.selected >= 0 && section.selected < len(issues) {
		return issues[section.selected].ID
	}
	return ""
}

// batchIssueCount totals the listed rows — open members across every Batch —
// feeding the filter bar's matched/total counts.
func batchIssueCount(sections []batchSection) int {
	total := 0
	for _, section := range sections {
		total += batchRowCount(section)
	}
	return total
}

// toggleFocusedBatch folds the focused Batch, or the completed rollup when the
// focus rests on it.
func (m *model) toggleFocusedBatch() {
	section, completed := m.focusedBatch()
	switch {
	case section != nil:
		section.collapsed = !section.collapsed
	case completed:
		m.completedShown = !m.completedShown
	}
}

// focusBatchIssue points the Batches cursor at an Issue when it still renders.
func (m *model) focusBatchIssue(id string) {
	m.batchCursor().focusRow(id)
}

// displayBatchSections applies the live / filter to the Batch rows with the
// Digest's matching rules: only matching members stay, empty groups drop, the
// selection follows its Issue into the filtered rows, and a collapsed Batch
// with matches is revealed — mirroring digestSections. The partition order
// survives, so openBatchCount still splits the result.
func (m model) displayBatchSections() []batchSection {
	query := strings.TrimSpace(strings.ToLower(m.filterQuery))
	if query == "" {
		return m.batchSections
	}
	sections := make([]batchSection, 0, len(m.batchSections))
	for _, section := range m.batchSections {
		selectedID := batchSelectedID(section)
		filtered := section
		filtered.groups = nil
		filtered.selected = 0
		filtered.top = 0
		row := 0
		for _, group := range section.groups {
			match := group
			match.issues = nil
			for _, issue := range group.issues {
				if !issueMatchesFilter(issue, query) {
					continue
				}
				if issue.ID == selectedID {
					filtered.selected = row
				}
				match.issues = append(match.issues, issue)
				row++
			}
			if len(match.issues) > 0 {
				filtered.groups = append(filtered.groups, match)
			}
		}
		if row > 0 {
			filtered.collapsed = false
		}
		sections = append(sections, filtered)
	}
	return sections
}

// batchBlock is one section the Batches surface draws: a Batch with open work,
// or the single rollup standing in for every completed one. index is the cursor
// stop it answers to — stable across the filter, which only drops blocks.
type batchBlock struct {
	index   int
	section batchSection
	// rows is the section's listed members, flattened once here so the height,
	// the window and the render all read the same list.
	rows      []batchRow
	completed []batchSection
	// budget is the body height the block gets, out of the height it wants.
	budget int
}

// batchBlocks lays the surface out: the Batches with open work that survive the
// filter, then the completed rollup, each with its share of the terminal
// height. The View and syncScroll both go through here, so the offsets the
// cursor persists are the ones the frame is drawn with.
func (m model) batchBlocks(sections []batchSection) []batchBlock {
	filtering := m.filtering()
	open := openBatchCount(sections)
	blocks := make([]batchBlock, 0, open+1)
	for i, section := range sections[:open] {
		if filtering && batchRowCount(section) == 0 {
			continue // while filtering, only Batches with matches are shown
		}
		blocks = append(blocks, batchBlock{index: i, section: section, rows: batchRows(section)})
	}
	// The filter hides the rollup — it matches Issues, and a completed Batch
	// lists none.
	if !filtering && open < len(sections) {
		blocks = append(blocks, batchBlock{index: open, completed: sections[open:]})
	}

	// Outside the bodies the frame spends its own chrome, one line per heading,
	// and the blank line closing an expanded section. A collapsed section spends
	// none — unlike the Digest, whose five fixed sections can afford the rhythm,
	// the Batches surface grows a section per Batch and every line it saves goes
	// back to the open work.
	chrome := digestChromeLines + len(blocks)
	counts := make([]int, len(blocks))
	for i, block := range blocks {
		if m.blockCollapsed(block) {
			continue
		}
		chrome++
		counts[i] = m.blockHeight(block)
	}
	budgets := allocateLineBudgets(counts, m.height, chrome)
	for i := range blocks {
		blocks[i].budget = budgets[i]
	}
	return blocks
}

// blockCollapsed reports whether a block is folded away: a Batch carries its own
// toggle, the rollup follows the surface-wide reveal.
func (m model) blockCollapsed(block batchBlock) bool {
	if block.completed != nil {
		return !m.completedShown
	}
	return block.section.collapsed
}

// blockHeight is the body height an expanded block wants: one line per Batch
// inside the rollup, and for a Batch every listed row plus a line per group
// heading — or the single line a cyclic Batch shows instead.
func (m model) blockHeight(block batchBlock) int {
	switch {
	case block.completed != nil:
		return len(block.completed)
	case len(block.section.cycle) > 0:
		return 1
	}
	return len(block.rows) + len(block.section.groups)
}

func (m model) batchesView() string {
	width := m.viewWidth()
	if len(m.batchSections) == 0 {
		// The surface keys all act on Batch rows, so the empty state trims the
		// bottom bar to the keys that still do something.
		bar := m.batchesBottomBar(0, 0)
		if !m.filterOpen && !m.commandOpen {
			bar = statusBar([2]string{"r", "refresh"}, [2]string{":", "cmd"}, [2]string{"q", "quit"})
		}
		return surfaceFrame(header(m.project.Name, 0, width, viewBatches),
			emptyState("no Batches yet", "ito batch new <name>", "to plan one"), bar, width)
	}

	sections := m.displayBatchSections()
	var body []string
	for _, block := range m.batchBlocks(sections) {
		focused := block.index == m.batchFocus
		collapsed := m.blockCollapsed(block)
		if block.completed != nil {
			body = append(body, m.completedBlock(block, collapsed, focused, width)...)
			continue
		}
		body = append(body, batchHeading(block.section, focused, width))
		if collapsed {
			continue
		}
		body = append(body, batchBodyLines(block, focused, width)...)
		body = append(body, "")
	}
	// displayBatchSections hands back m.batchSections untouched when no filter is
	// active, so the two counts are the same walk.
	matched := batchIssueCount(sections)
	total := matched
	if m.filtering() {
		total = batchIssueCount(m.batchSections)
	}
	return surfaceFrame(header(m.project.Name, len(m.batchSections), width, viewBatches),
		body, m.batchesBottomBar(matched, total), width)
}

// batchBodyLines renders an expanded Batch's body with its overflow indicators.
func batchBodyLines(block batchBlock, focused bool, width int) []string {
	if len(block.section.cycle) > 0 {
		if block.budget <= 0 {
			return nil
		}
		return []string{"    " + styleBlock.Render("⊘ ") +
			styleText.Render("blocked_by cycle among ") + styleID.Render(strings.Join(block.section.cycle, ", "))}
	}
	window, showHeadings := batchBodyWindow(block.section, block.rows, block.budget)
	return withOverflow(window, len(block.rows), batchBody(block, focused, window, showHeadings, width))
}

// completedBlock renders the rollup: its heading, and — when revealed — one
// quiet line per completed Batch, windowed like any other body.
func (m model) completedBlock(block batchBlock, collapsed, focused bool, width int) []string {
	lines := []string{completedHeading(len(block.completed), m.completedShown, focused, width)}
	if collapsed {
		return lines
	}
	window := visibleIssueWindow(len(block.completed), 0, 0, block.budget)
	rows := make([]string, 0, window.end-window.start)
	for _, section := range block.completed[window.start:window.end] {
		rows = append(rows, completedRow(section.batch, width))
	}
	lines = append(lines, withOverflow(window, len(block.completed), rows)...)
	return append(lines, "")
}

// batchBodyWindow spends an expanded Batch's line budget on Issue rows first:
// the group headings are chrome riding along with the rows they cover, and a
// budget too tight for them drops the headings rather than the work — a lone
// heading over no rows says nothing. The overflow indicators are part of the
// budget too, so rows only vanish silently when not even one row plus a marker
// fits, which is where the Digest gives up as well.
func batchBodyWindow(section batchSection, rows []batchRow, lineBudget int) (issueWindow, bool) {
	total := len(rows)
	if total == 0 || lineBudget <= 0 {
		return issueWindow{}, false
	}
	for _, showHeadings := range [2]bool{true, false} {
		for capacity := min(lineBudget, total); capacity >= 1; capacity-- {
			window := batchRowWindow(section, total, capacity)
			if batchBodyWindowLines(rows, window, showHeadings) <= lineBudget {
				return window, showHeadings
			}
		}
	}
	window := batchRowWindow(section, total, min(lineBudget, total))
	window.showAbove, window.showBelow = false, false
	return window, false
}

func batchRowWindow(section batchSection, total, capacity int) issueWindow {
	selected := min(max(section.selected, 0), total-1)
	start := scrollTop(section.top, selected, total, capacity)
	return issueWindow{
		start:     start,
		end:       start + capacity,
		showAbove: start > 0,
		showBelow: start+capacity < total,
	}
}

// batchBodyWindowLines is the screen height a window costs: its rows, the group
// headings they bring along, and the overflow markers.
func batchBodyWindowLines(rows []batchRow, window issueWindow, showHeadings bool) int {
	lines := window.end - window.start
	if window.showAbove {
		lines++
	}
	if window.showBelow {
		lines++
	}
	if showHeadings {
		previous := -1
		for _, row := range rows[window.start:window.end] {
			if row.group != previous {
				lines++
			}
			previous = row.group
		}
	}
	return lines
}

// batchBody renders the Batch's rows inside window, each group heading above the
// first of its rows that survives the window. Done members live only in the
// heading's progress count.
func batchBody(block batchBlock, focused bool, window issueWindow, showHeadings bool, width int) []string {
	lines := make([]string, 0, window.end-window.start)
	previous := -1
	for i := window.start; i < window.end; i++ {
		row := block.rows[i]
		if showHeadings && row.group != previous {
			lines = append(lines, batchGroupHeading(block.section.groups[row.group]))
		}
		previous = row.group
		// Rows sit two columns right of Digest rows, under their group heading.
		prefix := "      "
		if focused && i == block.section.selected {
			prefix = "    " + styleActive.Render("▸") + " "
		}
		lines = append(lines, prefix+renderIssueRow(row.issue, width-6))
	}
	return lines
}

// batchHeading is the Digest sectionHeading shape with the Batch's derived
// meta after the count and the created date dim at the rule's right end.
func batchHeading(section batchSection, focused bool, width int) string {
	return batchSectionHeading(section.batch.Name, section.batch.Total, batchMeta(section),
		section.batch.Date(), section.collapsed, focused, width)
}

// completedHeading renders the rollup that stands in for every fully done
// Batch — the Digest's single hidden `done` section, in Batch clothing. It ends
// the rule with no date: the Batches under it span dates, so none of them
// belongs at the right end.
func completedHeading(count int, shown, focused bool, width int) string {
	return batchSectionHeading("completed", count, "done", "", !shown, focused, width)
}

// batchSectionHeading is the Digest sectionHeading shape the Batches surface
// draws its sections with: focus bar, disclosure triangle, name, member count,
// dim meta, then the rule — ending in the dim trailing text when there is one.
func batchSectionHeading(name string, count int, meta, trailing string, collapsed, focused bool, width int) string {
	bar, styledBar := " ", " "
	if focused {
		bar, styledBar = "▌", styleActive.Render("▌")
	}
	triangle := "▾"
	if collapsed {
		triangle = "▸"
		meta += " · h to show"
	}
	label := fmt.Sprintf("  (%d)", count)
	meta = " · " + meta
	plainTail, styledTail := " ", " "
	if trailing != "" {
		plainTail, styledTail = "  "+trailing+" ", "  "+styleDim.Render(trailing)+" "
	}

	plain := " " + bar + triangle + " " + name + label + meta + "  " + plainTail
	ruleLen := max(0, width-runeLen(plain))
	return " " + styledBar + styleActive.Render(triangle) + " " + styleStatus.Render(name) +
		styleText.Render(label) + styleDim.Render(meta) + "  " +
		styleLine.Render(strings.Repeat("─", ruleLen)) + styledTail
}

// completedRow is one Batch inside the rollup: name and member count left, the
// date it was created dim at the right. Nothing here is navigable — a fully
// done Batch lists no open members, so there is nothing under it to reach.
func completedRow(b store.Batch, width int) string {
	count := fmt.Sprintf("  (%d)", b.Total)
	date := b.Date()
	gap := max(1, width-6-runeLen(b.Name+count)-runeLen(date))
	return "      " + styleText.Render(b.Name) + styleDim.Render(count) +
		strings.Repeat(" ", gap) + styleDim.Render(date)
}

// batchMeta derives the dim heading meta: members done over total plus the wave
// count when the plan derived waves (a cyclic Batch, or one whose every open
// member waits on work outside it, has none). A fully done Batch never reaches
// here — it lives in the completed rollup.
func batchMeta(section batchSection) string {
	b := section.batch
	waves := 0
	for _, group := range section.groups {
		if !group.waiting {
			waves++
		}
	}
	if waves == 0 {
		return fmt.Sprintf("%d/%d done", b.Done, b.Total)
	}
	return fmt.Sprintf("%d/%d done · wave 1/%d", b.Done, b.Total, waves)
}

// batchGroupHeading renders the quiet sub-heading over a group's rows: the
// label in the label ink, the group's state next to it — READY in the id colour
// on the current Wave, dimmed otherwise — then the rows it covers.
func batchGroupHeading(group batchGroup) string {
	state := styleDim.Render(group.state)
	if group.ready {
		state = styleStatus.Render(group.state)
	}
	return "    " + styleLabel.Render(group.label) +
		styleDim.Render(" · ") + state + styleDim.Render(fmt.Sprintf("  (%d)", len(group.issues)))
}

// batchesBottomBar shows the same footer and key set as the Digest — the
// Batches surface shares the Digest's row interactions.
func (m model) batchesBottomBar(matched, total int) string {
	return m.surfaceBottomBar(matched, total,
		[2]string{"tab", "focus"}, [2]string{"↑↓", "select"}, [2]string{"⏎", "open"},
		[2]string{"s", "status"}, [2]string{"h", "hide"}, [2]string{"/", "filter"},
		[2]string{":", "cmd"}, [2]string{"q", "quit"},
	)
}
