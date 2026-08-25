package tui

import (
	"github.com/c3h/ito/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// SyncFunc exchanges Changes with the connected Ledger. The TUI never dials
// the Ledger itself: the caller hands it this, or nil when no Ledger is
// connected, and nothing runs.
type SyncFunc func() (store.SyncResult, error)

// Options carries what the TUI needs beyond the store and the Project.
type Options struct {
	Sync SyncFunc
}

// syncMsg is the outcome of one background sync, stamped with the Project it
// ran under so a result that outlives a Project switch is dropped.
type syncMsg struct {
	Project store.Project
	Result  store.SyncResult
	Err     error
}

// shouldSync reports whether a background sync may start: a Ledger is
// connected, none is already running, and a Project is open.
func (m *model) shouldSync() bool {
	return m.sync != nil && !m.syncing && m.project.ID != 0
}

// startSync schedules a sync when shouldSync allows it, marking the model so
// the header shows it.
func (m *model) startSync() tea.Cmd {
	if !m.shouldSync() {
		return nil
	}
	m.syncing = true
	return m.syncCmd()
}

// syncCmd runs the sync on the command's goroutine; the model keeps serving
// input from local data until the message lands.
func (m model) syncCmd() tea.Cmd {
	project, sync := m.project, m.sync
	return func() tea.Msg {
		result, err := sync()
		return syncMsg{Project: project, Result: result, Err: err}
	}
}

// applySync ends the running sync. Pulled Changes are already in the store, so
// a reload picks them up in place; a sync that pulled nothing leaves the
// screen untouched. A failure is one dismissible notice — the local data on
// screen stays valid whatever happened to the Ledger. A result for a Project
// the user has since left is dropped whole: the switch already started the
// current Project's own sync, and only that one's result may end it.
func (m *model) applySync(msg syncMsg) {
	if msg.Project.ID != m.project.ID {
		return
	}
	m.syncing = false
	if msg.Err != nil {
		m.note = "sync failed: " + msg.Err.Error()
		return
	}
	if msg.Result.Pulled > 0 {
		m.reload()
	}
}

// syncBadge is the header's sync indicator, empty when nothing runs.
func syncBadge(syncing bool) string {
	if syncing {
		return "⟳ syncing   "
	}
	return ""
}
