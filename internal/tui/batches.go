package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/c3h/ito/internal/store"
)

// batchSection is one Batch rendered as a Digest-style section. The waves come
// from the shared core derivation (store.ShowBatch) — the TUI never recomputes
// them — and a cyclic Batch carries the Issues in the cycle instead of waves.
type batchSection struct {
	batch     store.Batch
	waves     []store.BatchWave
	cycle     []string
	collapsed bool
}

// reloadBatches snapshots the Project's Batches newest-first, deriving each
// plan through the core. A fully-done Batch starts collapsed; a blocked_by
// cycle is captured per Batch so one bad graph never blanks the surface.
func (m *model) reloadBatches() {
	m.loadErr = nil
	batches, err := m.store.ListBatches(m.project)
	if err != nil {
		m.loadErr = err
		return
	}
	sections := make([]batchSection, 0, len(batches))
	for _, b := range batches {
		section := batchSection{batch: b, collapsed: b.Total > 0 && b.Done == b.Total}
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
		sections = append(sections, section)
	}
	m.batchSections = sections
}

func (m model) batchesView() string {
	width := m.viewWidth()
	lines := []string{
		header(m.project.Name, len(m.batchSections), width, viewBatches),
		fullRule(width),
		"",
	}
	if len(m.batchSections) == 0 {
		lines = append(lines,
			" no Batches yet",
			"",
			" run ito batch new <name> to plan one",
			"",
			fullRule(width),
			batchesBottomBar(),
		)
		return strings.Join(lines, "\n")
	}

	bodies := make([][]string, len(m.batchSections))
	counts := make([]int, len(m.batchSections))
	for i, section := range m.batchSections {
		bodies[i] = batchBody(section, width)
		counts[i] = len(bodies[i])
	}
	budgets := allocateLineBudgets(counts, m.height)
	for i, section := range m.batchSections {
		// Focus interactions arrive in the next slice; until then the first
		// (newest) Batch wears the focus bar, like the Digest's initial focus.
		lines = append(lines, batchHeading(section, i == 0, width))
		window := visibleIssueWindow(len(bodies[i]), 0, budgets[i])
		lines = append(lines, bodies[i][window.start:window.end]...)
		if window.showBelow {
			lines = append(lines, styleDim.Render(fmt.Sprintf("    ↓ %d more", len(bodies[i])-window.end)))
		}
		lines = append(lines, "")
	}
	lines = append(lines, fullRule(width), batchesBottomBar())
	return strings.Join(lines, "\n")
}

// batchBody renders a Batch's open members under their Wave sub-headings —
// done members live only in the heading count, a collapsed Batch has no body,
// and a cyclic Batch shows the cycle line instead of waves.
func batchBody(section batchSection, width int) []string {
	if section.collapsed {
		return nil
	}
	if len(section.cycle) > 0 {
		return []string{"    " + styleBlock.Render("⊘ ") +
			styleText.Render("blocked_by cycle among ") + styleID.Render(strings.Join(section.cycle, ", "))}
	}
	var lines []string
	for _, wave := range section.waves {
		lines = append(lines, waveHeading(wave))
		for _, issue := range wave.Issues {
			// Rows sit two columns right of Digest rows, under their Wave heading.
			lines = append(lines, "      "+renderIssueRow(issue, width-6))
		}
	}
	return lines
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
	date := batchCreatedDate(section.batch.Created)

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
	if b.Total > 0 && b.Done == b.Total {
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

// batchCreatedDate shows the Batch's created timestamp as its calendar date —
// chronology, never identity — matching the CLI's batch list rendering.
func batchCreatedDate(created string) string {
	parsed, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return created
	}
	return parsed.UTC().Format("2006-01-02")
}

// batchesBottomBar lists only the keys this read-only slice honours — the
// focus/selection/edit keys join when the interactions land.
func batchesBottomBar() string {
	return statusBar(
		[2]string{"1", "digest"}, [2]string{"2", "board"},
		[2]string{"r", "refresh"}, [2]string{"q", "quit"},
	)
}
