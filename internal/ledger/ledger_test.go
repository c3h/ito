package ledger

import (
	"encoding/json"
	"testing"
)

func TestMemoryAppendAssignsPositionsAndDeduplicatesResends(t *testing.T) {
	l := NewMemory()
	first := []Change{
		{Sequence: 1, Device: "mac", Kind: KindIssue, Project: "ito", Key: "ITO-1", Updated: "2026-08-24T10:00:00Z", State: json.RawMessage(`{"title":"a"}`)},
		{Sequence: 2, Device: "mac", Kind: KindIssue, Project: "ito", Key: "ITO-2", Updated: "2026-08-24T10:00:01Z", State: json.RawMessage(`{"title":"b"}`)},
	}
	positions, err := l.Append(first)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(positions) != 2 || positions[0] != 1 || positions[1] != 2 {
		t.Fatalf("positions = %v, want [1 2]", positions)
	}

	// A resend of an already appended Change (same Device and Sequence) keeps
	// its original position and does not grow the Ledger.
	positions, err = l.Append([]Change{first[1], {Sequence: 1, Device: "vps", Kind: KindIssue, Project: "ito", Key: "ITO-3", Updated: "2026-08-24T10:00:02Z"}})
	if err != nil {
		t.Fatalf("append resend: %v", err)
	}
	if len(positions) != 2 || positions[0] != 2 || positions[1] != 3 {
		t.Fatalf("positions after resend = %v, want [2 3]", positions)
	}

	entries, err := l.ReadAfter(0, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	entries, err = l.ReadAfter(2, 10)
	if err != nil {
		t.Fatalf("read after 2: %v", err)
	}
	if len(entries) != 1 || entries[0].Position != 3 || entries[0].Device != "vps" {
		t.Fatalf("entries after 2 = %#v", entries)
	}
	entries, err = l.ReadAfter(0, 2)
	if err != nil {
		t.Fatalf("read limited: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("limit must cap the page, got %d entries", len(entries))
	}
}

func TestMemoryReserveIssueNumberIsSequentialPerProject(t *testing.T) {
	l := NewMemory()
	for want := int64(1); want <= 3; want++ {
		got, err := l.ReserveIssueNumber("ito")
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if got != want {
			t.Fatalf("reserved %d, want %d", got, want)
		}
	}
	got, err := l.ReserveIssueNumber("other")
	if err != nil {
		t.Fatalf("reserve other: %v", err)
	}
	if got != 1 {
		t.Fatalf("other project must count from 1, got %d", got)
	}
}
