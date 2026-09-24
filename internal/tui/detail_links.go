package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/c3h/ito/internal/store"
)

// linkIDWidth is the column the linked Issue id occupies before its title in
// the detail view, so the titles line up across link rows.
const linkIDWidth = 8

// detailLinkLines caps the detail view's Links block, overflow indicators
// included, so a heavily linked Issue still leaves the body room to read.
const detailLinkLines = 4

// detailLink is one row of the detail's Links block: the kind's label and the
// linked Issue's id.
type detailLink struct {
	kind string
	id   string
}

// detailStop is where a followed Link left from: esc walks back to that Issue
// with its Links block focused on the same Link. The Link is kept by id, not
// row, so a refresh that reorders the block still lands on it.
type detailStop struct {
	id   string
	link string
	top  int
}

// detailLinks lists an Issue's Links strongest kind first — blocked by, then
// conflicts with, then relates to — naming each linked Issue once, under the
// strongest kind that holds it.
func detailLinks(issue store.Issue) []detailLink {
	var links []detailLink
	seen := map[string]bool{}
	add := func(kind string, ids []string) {
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			links = append(links, detailLink{kind: kind, id: id})
		}
	}
	add("blocked by", issue.BlockedBy)
	add("conflicts with", issue.ConflictsWith)
	add("relates to", issue.RelatesTo)
	return links
}

// linkBlock renders the Links block windowed to detailLinkLines, with the same
// "↑/↓ N more" affordance the Digest sections use. A kind's label heads its
// group — repeated on the window's first row, so a scrolled group never loses
// it — and the cursor marks the selected Link only while the block has focus.
func (m model) linkBlock(links []detailLink, width int) []string {
	selected := m.selectedLink(links)
	window := visibleIssueWindow(len(links), selected, m.linkTop, detailLinkLines)
	lines := make([]string, 0, detailLinkLines)
	if window.showAbove {
		lines = append(lines, linkOverflowLine("↑", window.start))
	}
	for i := window.start; i < window.end; i++ {
		label := ""
		if i == window.start || links[i].kind != links[i-1].kind {
			label = links[i].kind
		}
		lines = append(lines, m.linkLine(label, links[i].id, m.linksFocused && i == selected, width))
	}
	if window.showBelow {
		lines = append(lines, linkOverflowLine("↓", len(links)-window.end))
	}
	return lines
}

// linkLine renders a Link row: the dim kind label (blank past its group's first
// row), the cursor slot, the linked id in cyan, then the linked title aligned
// past linkIDWidth and cut to the frame's one-column right inset.
func (m model) linkLine(label, id string, selected bool, width int) string {
	labelPad := strings.Repeat(" ", max(1, detailLabelWidth-2-runeLen(label)))
	cursor, styledCursor := "  ", "  "
	if selected {
		cursor, styledCursor = "▸ ", styleActive.Render("▸")+" "
	}
	idPad := strings.Repeat(" ", max(1, linkIDWidth-runeLen(id)))
	lead := " " + label + labelPad + cursor + id + idPad
	title := truncate(m.linkTitles[id], max(1, width-1-runeLen(lead)))
	return " " + styleDim.Render(label) + labelPad + styledCursor + styleID.Render(id) + idPad + styleText.Render(title)
}

// linkOverflowLine renders a Links block overflow indicator, dim and aligned
// under the linked ids.
func linkOverflowLine(arrow string, n int) string {
	return strings.Repeat(" ", 1+detailLabelWidth) + styleDim.Render(fmt.Sprintf("%s %d more", arrow, n))
}

// detailKeys is the detail's shortcut bar. With the Links block focused, ↑↓ and
// enter act on it and tab hands focus back to the body; otherwise ↑↓ step
// through the surface's list, and tab reaches the Links when the Issue has any.
func (m model) detailKeys(hasLinks bool) [][2]string {
	shared := [][2]string{{"s", "status"}, {"p", "priority"}, {"l", "labels"}, {"r", "refresh"}, {":", "cmd"}, {"q", "quit"}}
	if hasLinks && m.linksFocused {
		return append([][2]string{{"tab", "body"}, {"↑↓", "select"}, {"⏎", "open"}}, shared...)
	}
	keys := [][2]string{{"esc", "back"}}
	if hasLinks {
		keys = append(keys, [2]string{"tab", "links"})
	}
	return append(append(keys, [2]string{"↑↓", "prev/next"}), shared...)
}

// selectedLink is the Links cursor clamped to the block, which a refresh may
// have shortened under it.
func (m model) selectedLink(links []detailLink) int {
	return min(max(m.linkSelected, 0), len(links)-1)
}

// linksActive reports whether the detail's ↑↓ and enter act on its Links block:
// it has focus and the Issue still has Links (a refresh may have removed them).
func (m model) linksActive() bool {
	return m.linksFocused && len(detailLinks(m.detailIssue)) > 0
}

// toggleLinksFocus moves the detail's focus between the body and the Links
// block — the detail's two focus stops, so tab and shift+tab both toggle.
func (m *model) toggleLinksFocus() {
	if m.linksFocused {
		m.linksFocused = false
		return
	}
	m.linksFocused = len(detailLinks(m.detailIssue)) > 0
}

func (m *model) resetLinkCursor() {
	m.linksFocused = false
	m.linkSelected, m.linkTop = 0, 0
}

// moveLinkCursor steps the Links cursor, clamped to the block, and slides the
// window only once the cursor would leave it — the Digest's sticky scroll.
func (m *model) moveLinkCursor(delta int) {
	links := detailLinks(m.detailIssue)
	if len(links) == 0 {
		return
	}
	m.linkSelected = min(max(m.selectedLink(links)+delta, 0), len(links)-1)
	m.linkTop = visibleIssueWindow(len(links), m.linkSelected, m.linkTop, detailLinkLines).start
}

// followLink opens the selected Link's Issue, leaving a stop on the trail so
// esc walks back here with the same Link selected.
func (m *model) followLink() {
	links := detailLinks(m.detailIssue)
	if len(links) == 0 {
		return
	}
	link := links[m.selectedLink(links)]
	target, ok := m.linkedIssue(link.id)
	if !ok {
		return
	}
	m.detailTrail = append(m.detailTrail, detailStop{id: m.detailIssue.ID, link: link.id, top: m.linkTop})
	m.showIssue(target)
}

// escapeDetail handles esc on the Issue detail, one step at a time: from the
// Links block back to the body, then back along the followed Links, and only
// then out to the surface the detail was opened from. A focus a refresh left
// on a now empty block is no stop — the bar already offers esc as back.
func (m *model) escapeDetail() {
	switch {
	case m.linksActive():
		m.linksFocused = false
	case len(m.detailTrail) > 0:
		m.walkBack()
	default:
		m.mode = m.detailReturnMode()
	}
}

// walkBack returns from a followed Link to the Issue it left, its Links block
// focused on the Link that was followed — or on the first, if that Link is
// gone. A stop whose Issue no longer exists is dropped with a note, so the next
// esc keeps walking.
func (m *model) walkBack() {
	stop := m.detailTrail[len(m.detailTrail)-1]
	m.detailTrail = m.detailTrail[:len(m.detailTrail)-1]
	origin, ok := m.linkedIssue(stop.id)
	if !ok {
		return
	}
	m.showIssue(origin)
	links := detailLinks(origin)
	m.linksFocused = len(links) > 0
	m.linkSelected = max(0, slices.IndexFunc(links, func(link detailLink) bool { return link.id == stop.link }))
	m.linkTop = visibleIssueWindow(len(links), m.linkSelected, stop.top, detailLinkLines).start
}

// linkedIssue resolves an Issue the detail navigates to by id, the way
// loadLinkTitles does: from the loaded sections, falling back to the store for
// Issues created since the last reload. A missing Issue becomes a note; a real
// store error goes to loadErr.
func (m *model) linkedIssue(id string) (store.Issue, bool) {
	if issue, ok := m.issueInSections(id); ok {
		return issue, true
	}
	issue, err := m.store.FindIssue(m.project, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			m.note = id + " no longer exists"
		} else {
			m.loadErr = err
		}
		return store.Issue{}, false
	}
	return issue, true
}
