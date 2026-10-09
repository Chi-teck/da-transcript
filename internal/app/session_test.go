package app

import (
	"slices"
	"testing"
)

func TestTagSessionsMarksSubagents(t *testing.T) {
	rows := []sessionRow{
		{id: "3f2c9e1a-7b4d-4c8e-9a1f-5d6e7faaaaaa", timeCreated: 1},
		{id: "8d1e4b7c-2a9f-4e3d-8c6b-1f0a2bbbbbbb", parentID: "3f2c9e1a-7b4d-4c8e-9a1f-5d6e7faaaaaa", timeCreated: 2}, // child
		{id: "c6a9f3e2-5d8b-4a1c-b7e4-9e2d3ccccccc", parentID: "8d1e4b7c-2a9f-4e3d-8c6b-1f0a2bbbbbbb", timeCreated: 3}, // grandchild — three-level chain
		{id: "1b7e5a3d-9c2f-4d6a-8e1b-7a4c5ddddddd", timeCreated: 4},
		{id: "e4d2c8b1-6a3e-4f9b-a5d7-2c8b9eeeeeee", parentID: "5a8c1d4f-3e7b-4b2a-9d6c-8f1e2aoffscr", timeCreated: 5}, // parent out of scope
	}
	sessions := tagSessions(rows)

	byID := map[string]*session{}
	for _, s := range sessions {
		byID[s.id] = s
	}
	for _, id := range []string{"8d1e4b7c-2a9f-4e3d-8c6b-1f0a2bbbbbbb", "c6a9f3e2-5d8b-4a1c-b7e4-9e2d3ccccccc"} {
		if !byID[id].sub {
			t.Errorf("%s: sub=false, want a subagent down the parent chain", id)
		}
	}
	for _, id := range []string{"3f2c9e1a-7b4d-4c8e-9a1f-5d6e7faaaaaa", "1b7e5a3d-9c2f-4d6a-8e1b-7a4c5ddddddd", "e4d2c8b1-6a3e-4f9b-a5d7-2c8b9eeeeeee"} {
		if byID[id].sub {
			t.Errorf("%s: sub=true, want a top-level session", id)
		}
	}

	if got := byID["3f2c9e1a-7b4d-4c8e-9a1f-5d6e7faaaaaa"].tag; got != "aaaaaa" {
		t.Errorf("tag = %q", got)
	}
	if got := byID["8d1e4b7c-2a9f-4e3d-8c6b-1f0a2bbbbbbb"].tag; got != "bbbbbb↓" {
		t.Errorf("subagent tag = %q", got)
	}

	// The input order is the output order: a caller reading the list back gets
	// the sessions as they started.
	got := make([]string, len(sessions))
	for i, s := range sessions {
		got[i] = s.id
	}
	want := []string{"3f2c9e1a-7b4d-4c8e-9a1f-5d6e7faaaaaa", "8d1e4b7c-2a9f-4e3d-8c6b-1f0a2bbbbbbb", "c6a9f3e2-5d8b-4a1c-b7e4-9e2d3ccccccc", "1b7e5a3d-9c2f-4d6a-8e1b-7a4c5ddddddd", "e4d2c8b1-6a3e-4f9b-a5d7-2c8b9eeeeeee"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// An id shorter than the tag is its own tag, rather than being padded or
// panicking on the slice.
func TestTagForShortID(t *testing.T) {
	if got := tagFor("abc"); got != "abc" {
		t.Errorf("tagFor(short) = %q", got)
	}
}
