package tui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/c3h/ito/internal/store"
)

// batchSection is one Batch rendered as a Digest-style section. The waves come
// from the shared core derivation (store.ShowBatch) — the TUI never recomputes
// them — and a cyclic Batch carries the Issues in the cycle instead of waves.
// selected indexes the flattened listed rows (batchIssues), so the cursor
// crosses Wave sub-headings transparently.
type batchSection struct {
	batch     store.Batch
	waves     []store.BatchWave
	cycle     []string
	collapsed bool
	selected  int
}

// reloadBatches snapshots the Project's Batches newest-first, deriving each
// plan through the core. A fully-done Batch starts collapsed; a blocked_by
// cycle is captured per Batch so one bad graph never blanks the surface.
// Across reloads the focus follows its Batch by name, a manual collapse toggle
// survives (except that a Batch completing right now collapses on the spot),
// and each selection keeps its Issue by ID, falling back to clamping.
func (m *model) reloadBatches() {
	previous := make(map[string]batchSection, len(m.batchSections))
	for _, section := range m.batchSections {
		previous[section.batch.Name] = section
	}
	focusedName := ""
	if m.batchFocus >= 0 && m.batchFocus < len(m.batchSections) {
		focusedName = m.batchSections[m.batchFocus].batch.Name
	}

	m.loadErr = nil
	batches, err := m.store.ListBatches(m.project)
	if err != nil {
		m.loadErr = err
		return
	}
	sections := make([]batchSection, 0, len(batches))
	focusIndex := 0
	for i, b := range batches {
		section := batchSection{batch: b, collapsed: batchDone(b)}
		plan, err := m.store.ShowBatch(m.project, b.Name)
		var cycleErr *store.BatchCycleError
		switch {
		case errors.As(err, &cycleErr):
			section.cycle = cycleErr.Issues
		case err != nil:
			m.loadErr = err
			return
		default:
			section.waves = plan.Waves
		}
		if prev, ok := previous[b.Name]; ok {
			// A manual toggle survives the reload, but a Batch that just went
			// fully done collapses on the spot.
			if !batchDone(b) || batchDone(prev.batch) {
				section.collapsed = prev.collapsed
			}
			section.selected = restoredBatchSelection(prev, section)
		}
		if b.Name == focusedName {
			focusIndex = i
		}
		sections = append(sections, section)
	}
	m.batchSections = sections
	m.batchFocus = focusIndex
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

// batchIssues flattens the rows a Batch lists — its open members in wave
// order — the list the selection cursor walks.
func batchIssues(section batchSection) []store.Issue {
	var issues []store.Issue
	for _, wave := range section.waves {
		issues = append(issues, wave.Issues...)
	}
	return issues
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
		total += len(batchIssues(section))
	}
	return total
}

func (m *model) moveBatchFocus(delta int) {
	if len(m.batchSections) == 0 {
		return
	}
	m.batchFocus = (m.batchFocus + delta + len(m.batchSections)) % len(m.batchSections)
}

// moveBatchSelection moves the Batches cursor, flowing across Batch
// boundaries the way the Digest cursor flows across sections: past a Batch's
// last row it steps to the adjacent Batch, stopping at the surface's ends.
// Every Batch is a stop — a collapsed or rowless one takes the focus on its
// heading with no row selected, so h reveals a collapsed one from here.
func (m *model) moveBatchSelection(delta int) {
	if m.batchFocus < 0 || m.batchFocus >= len(m.batchSections) {
		return
	}
	section := &m.batchSections[m.batchFocus]
	issues := batchIssues(*section)
	next := section.selected + delta
	if !section.collapsed && next >= 0 && next < len(issues) {
		section.selected = next
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	i := m.batchFocus + step
	if i < 0 || i >= len(m.batchSections) {
		return // at the surface's ends
	}
	m.batchFocus = i
	// Seed the edge row so revealing a collapsed Batch lands the cursor where
	// it arrived; a rowless Batch has none and shows no cursor.
	target := &m.batchSections[i]
	if step > 0 {
		target.selected = 0
	} else {
		target.selected = max(0, len(batchIssues(*target))-1)
	}
}

func (m *model) toggleFocusedBatch() {
	if m.batchFocus < 0 || m.batchFocus >= len(m.batchSections) {
		return
	}
	m.batchSections[m.batchFocus].collapsed = !m.batchSections[m.batchFocus].collapsed
}

// selectedBatchIssue is the member the surface's actions apply to: the focused
// Batch's selected row. A collapsed Batch exposes no rows, so it never yields
// a selection — mirroring a hidden Digest section.
func (m model) selectedBatchIssue() (store.Issue, bool) {
	if m.batchFocus < 0 || m.batchFocus >= len(m.batchSections) {
		return store.Issue{}, false
	}
	section := m.batchSections[m.batchFocus]
	if section.collapsed {
		return store.Issue{}, false
	}
	issues := batchIssues(section)
	if section.selected < 0 || section.selected >= len(issues) {
		return store.Issue{}, false
	}
	return issues[section.selected], true
}

// focusBatchIssue points the Batch focus and selection at an Issue when it
// still renders on the surface — collapsed Batches are skipped, like hidden
// Digest sections in focusIssue.
func (m *model) focusBatchIssue(id string) {
	for i := range m.batchSections {
		if m.batchSections[i].collapsed {
			continue
		}
		for j, issue := range batchIssues(m.batchSections[i]) {
			if issue.ID == id {
				m.batchFocus = i
				m.batchSections[i].selected = j
				return
			}
		}
	}
}

// displayBatchSections applies the live / filter to the Batch rows with the
// Digest's matching rules: only matching members stay, empty Waves drop, the
// selection follows its Issue into the filtered rows, and a collapsed Batch
// with matches is revealed — mirroring digestSections.
func (m model) displayBatchSections() []batchSection {
	query := strings.TrimSpace(strings.ToLower(m.filterQuery))
	if query == "" {
		return m.batchSections
	}
	sections := make([]batchSection, 0, len(m.batchSections))
	for _, section := range m.batchSections {
		selectedID := batchSelectedID(section)
		filtered := section
		filtered.waves = nil
		filtered.selected = 0
		row := 0
		for _, wave := range section.waves {
			match := wave
			match.Issues = nil
			for _, issue := range wave.Issues {
				if !issueMatchesFilter(issue, query) {
					continue
				}
				if issue.ID == selectedID {
					filtered.selected = row
				}
				match.Issues = append(match.Issues, issue)
				row++
			}
			if len(match.Issues) > 0 {
				filtered.waves = append(filtered.waves, match)
			}
		}
		if row > 0 {
			filtered.collapsed = false
		}
		sections = append(sections, filtered)
	}
	return sections
}

func (m model) batchesView() string {
	width := m.viewWidth()
	lines := []string{
		header(m.project.Name, len(m.batchSections), width, viewBatches),
		fullRule(width),
		"",
	}
	if len(m.batchSections) == 0 {
		// The surface keys all act on Batch rows, so the empty state trims the
		// bottom bar to the keys that still do something.
		bar := m.batchesBottomBar(0, 0)
		if !m.filterOpen && !m.commandOpen {
			bar = statusBar([2]string{"r", "refresh"}, [2]string{":", "cmd"}, [2]string{"q", "quit"})
		}
		lines = append(lines, emptyState("no Batches yet", "ito batch new <name>", "to plan one")...)
		lines = append(lines, "", fullRule(width), bar)
		return strings.Join(lines, "\n")
	}

	sections := m.displayBatchSections()
	filtering := strings.TrimSpace(m.filterQuery) != ""
	bodies := make([][]string, len(sections))
	selectedLines := make([]int, len(sections))
	counts := make([]int, len(sections))
	for i, section := range sections {
		if filtering && len(batchIssues(section)) == 0 {
			continue // while filtering, only Batches with matches are shown
		}
		bodies[i], selectedLines[i] = batchBody(section, i == m.batchFocus, width)
		counts[i] = len(bodies[i])
	}
	budgets := allocateLineBudgets(counts, m.height)
	for i, section := range sections {
		if filtering && len(batchIssues(section)) == 0 {
			continue
		}
		lines = append(lines, batchHeading(section, i == m.batchFocus, width))
		window := visibleIssueWindow(len(bodies[i]), selectedLines[i], budgets[i])
		if window.showAbove {
			lines = append(lines, styleDim.Render(fmt.Sprintf("    ↑ %d more", window.start)))
		}
		lines = append(lines, bodies[i][window.start:window.end]...)
		if window.showBelow {
			lines = append(lines, styleDim.Render(fmt.Sprintf("    ↓ %d more", len(bodies[i])-window.end)))
		}
		lines = append(lines, "")
	}
	lines = append(lines, fullRule(width),
		m.batchesBottomBar(batchIssueCount(sections), batchIssueCount(m.batchSections)))
	return strings.Join(lines, "\n")
}

// batchBody renders a Batch's open members under their Wave sub-headings —
// done members live only in the heading count, a collapsed Batch has no body,
// and a cyclic Batch shows the cycle line instead of waves. It also reports
// the line the selection cursor sits on, so windowing keeps it visible.
func batchBody(section batchSection, focused bool, width int) ([]string, int) {
	if section.collapsed {
		return nil, 0
	}
	if len(section.cycle) > 0 {
		return []string{"    " + styleBlock.Render("⊘ ") +
			styleText.Render("blocked_by cycle among ") + styleID.Render(strings.Join(section.cycle, ", "))}, 0
	}
	var lines []string
	selectedLine := 0
	row := 0
	for _, wave := range section.waves {
		lines = append(lines, waveHeading(wave))
		for _, issue := range wave.Issues {
			// Rows sit two columns right of Digest rows, under their Wave heading.
			prefix := "      "
			if focused && row == section.selected {
				prefix = "    " + styleActive.Render("▸") + " "
				selectedLine = len(lines)
			}
			lines = append(lines, prefix+renderIssueRow(issue, width-6))
			row++
		}
	}
	return lines, selectedLine
}

// batchHeading is the Digest sectionHeading shape with the Batch's derived
// meta after the count and the created date dim at the rule's right end.
func batchHeading(section batchSection, focused bool, width int) string {
	bar, styledBar := " ", " "
	if focused {
		bar, styledBar = "▌", styleActive.Render("▌")
	}
	triangle := "▾"
	if section.collapsed {
		triangle = "▸"
	}
	count := fmt.Sprintf("  (%d)", section.batch.Total)
	meta := " · " + batchMeta(section)
	if section.collapsed {
		meta += " · h to show"
	}
	date := section.batch.Date()

	plain := " " + bar + triangle + " " + section.batch.Name + count + meta + "  " + "  " + date + " "
	ruleLen := max(0, width-runeLen(plain))
	return " " + styledBar + styleActive.Render(triangle) + " " + styleStatus.Render(section.batch.Name) +
		styleText.Render(count) + styleDim.Render(meta) + "  " +
		styleLine.Render(strings.Repeat("─", ruleLen)) + "  " + styleDim.Render(date) + " "
}

// batchMeta derives the dim heading meta: "done" for a complete Batch,
// otherwise members done over total plus the wave count when the plan derived
// waves (a cyclic or empty Batch has none).
func batchMeta(section batchSection) string {
	b := section.batch
	if batchDone(b) {
		return "done"
	}
	if len(section.waves) == 0 {
		return fmt.Sprintf("%d/%d done", b.Done, b.Total)
	}
	return fmt.Sprintf("%d/%d done · wave 1/%d", b.Done, b.Total, len(section.waves))
}

// waveHeading renders the quiet Wave sub-heading: the wave name in the label
// ink, READY in the id colour on Wave 1, WAITING dimmed on the rest.
func waveHeading(wave store.BatchWave) string {
	state := styleDim.Render("WAITING")
	if wave.Ready {
		state = styleStatus.Render("READY")
	}
	return "    " + styleLabel.Render(fmt.Sprintf("WAVE %d", wave.Wave)) +
		styleDim.Render(" · ") + state + styleDim.Render(fmt.Sprintf("  (%d)", len(wave.Issues)))
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
