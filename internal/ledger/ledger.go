// Package ledger defines the shared, append-only history of Changes that
// Devices exchange during Sync (decision 0005). The store appends to and reads
// from a Ledger through the interface below; the transport behind it is a
// detail, so an in-memory implementation serves tests.
package ledger

import (
	"encoding/json"
	"sync"
)

// Row kinds a Change can carry. Issue, Project and Batch Changes hold the
// row's full state; Link and Label rows are set-like, so their Change is an
// insert or a delete keyed by the row itself.
const (
	KindIssue   = "issue"
	KindProject = "project"
	KindBatch   = "batch"
	KindLink    = "link"
	KindLabel   = "label"
)

// Change records one mutation to one row: the row's full new state, or a
// tombstone when Deleted is set. Device and Sequence together identify the
// Change wherever it travels, so a resend never appends twice.
type Change struct {
	// Sequence is the Change's position in the originating Device's local log.
	Sequence int64 `json:"sequence"`
	// Device identifies the machine that made the Change.
	Device string `json:"device"`
	// Kind names the row kind; Project and Key form the row's natural key.
	Kind    string `json:"kind"`
	Project string `json:"project"`
	Key     string `json:"key"`
	Deleted bool   `json:"deleted"`
	// Updated is the row's own updated stamp — the last-writer-wins key.
	Updated string `json:"updated"`
	// State is the row's full new state, absent on a tombstone.
	State json.RawMessage `json:"state,omitempty"`
}

// Entry is a Change as stored in the Ledger, at its assigned position.
type Entry struct {
	Position int64
	Change
}

// Ledger is what a Device talks to during Sync.
type Ledger interface {
	// EnsureSchema prepares the Ledger for use; a Device calls it once, when
	// it connects.
	EnsureSchema() error
	// Append stores the Changes in order and returns their positions. A Change
	// the Ledger already holds (same Device and Sequence) keeps its position.
	Append(changes []Change) ([]int64, error)
	// ReadAfter returns up to limit Entries whose position is greater than
	// position, in Ledger order.
	ReadAfter(position int64, limit int) ([]Entry, error)
	// ReserveIssueNumber atomically hands out the next Issue number for the
	// Project, keyed by its Prefix — the identity that survives renames. The
	// number is always above floor, the highest number the caller has seen,
	// so Issues numbered before the Ledger counted them are never reissued.
	ReserveIssueNumber(prefix string, floor int64) (int64, error)
}

type deviceSequence struct {
	device   string
	sequence int64
}

// Memory is an in-process Ledger for tests.
type Memory struct {
	mu        sync.Mutex
	entries   []Entry
	positions map[deviceSequence]int64
	counters  map[string]int64
}

func NewMemory() *Memory {
	return &Memory{positions: make(map[deviceSequence]int64), counters: make(map[string]int64)}
}

func (m *Memory) EnsureSchema() error { return nil }

func (m *Memory) Append(changes []Change) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	positions := make([]int64, 0, len(changes))
	for _, change := range changes {
		key := deviceSequence{change.Device, change.Sequence}
		if position, ok := m.positions[key]; ok {
			positions = append(positions, position)
			continue
		}
		position := int64(len(m.entries) + 1)
		m.entries = append(m.entries, Entry{Position: position, Change: change})
		m.positions[key] = position
		positions = append(positions, position)
	}
	return positions, nil
}

func (m *Memory) ReadAfter(position int64, limit int) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if position < 0 {
		position = 0
	}
	if position >= int64(len(m.entries)) {
		return []Entry{}, nil
	}
	page := m.entries[position:]
	if limit > 0 && len(page) > limit {
		page = page[:limit]
	}
	return append([]Entry{}, page...), nil
}

func (m *Memory) ReserveIssueNumber(prefix string, floor int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[prefix] = max(m.counters[prefix], floor) + 1
	return m.counters[prefix], nil
}
