package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// blockIDs flattens blocks into their item ids, each block as its head's id
// followed by its results', so a test can say which item went where.
func blockIDs(bs []block) [][]int64 {
	out := make([][]int64, len(bs))
	for i, b := range bs {
		out[i] = []int64{b.head.id}
		for _, r := range b.results {
			out[i] = append(out[i], r.id)
		}
	}
	return out
}

// Which result joins which call, on rows built by hand: the join looks past
// other items, takes the nearest earlier call with the result's id, and leaves
// anything it cannot place as a block of its own.
func TestBlocksJoin(t *testing.T) {
	step := func(id int64, calls ...string) itemRow {
		it := itemRow{id: id, typ: "message", data: itemData{Role: "assistant"}}
		for _, c := range calls {
			it.data.ToolCalls = append(it.data.ToolCalls, toolCall{ID: flexString(c)})
		}
		return it
	}
	result := func(id int64, call string) itemRow {
		return itemRow{id: id, typ: "message", data: itemData{Role: "tool", ToolCallID: flexString(call)}}
	}
	marker := func(id int64) itemRow { return itemRow{id: id, typ: "subsession"} }
	unreadable := step(1, "c1")
	unreadable.unknown = "message (unreadable)"

	for _, c := range []struct {
		name  string
		items []itemRow
		want  [][]int64
	}{
		{"past another item", []itemRow{step(1, "c1", "c2"), marker(2), result(3, "c2"), result(4, "c1")},
			[][]int64{{1, 3, 4}, {2}}},
		{"an id used twice", []itemRow{step(1, "c1"), result(2, "c1"), step(3, "c1"), result(4, "c1")},
			[][]int64{{1, 2}, {3, 4}}},
		{"an empty id", []itemRow{step(1, ""), result(2, "")},
			[][]int64{{1}, {2}}},
		{"a result before its call", []itemRow{result(1, "c1"), step(2, "c1")},
			[][]int64{{1}, {2}}},
		{"an unreadable step", []itemRow{unreadable, result(2, "c1")},
			[][]int64{{1}, {2}}},
	} {
		got := blockIDs(blocks(c.items))
		if !slices.EqualFunc(got, c.want, slices.Equal) {
			t.Errorf("%s: blocks = %v, want %v", c.name, got, c.want)
		}
		// Every item exactly once, whatever the grouping.
		var flat, ids []int64
		for _, b := range got {
			flat = append(flat, b...)
		}
		for _, it := range c.items {
			ids = append(ids, it.id)
		}
		slices.Sort(flat)
		if !slices.Equal(flat, ids) {
			t.Errorf("%s: items %v, blocks hold %v", c.name, ids, flat)
		}
	}
}

// The blocks of the zoo, session by session: every item in exactly one block,
// and every result with the step that made its call, however far apart the two
// are.
func TestBlocksZoo(t *testing.T) {
	st := zooDB(t).open(t)
	ctx := context.Background()
	rows, err := st.sessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.items(ctx, tagSessions(rows))
	if err != nil {
		t.Fatal(err)
	}
	runs := map[string][]itemRow{}
	for _, it := range items {
		runs[tagFor(it.sessionID)] = append(runs[tagFor(it.sessionID)], it)
	}
	if len(runs) != 4 {
		t.Fatalf("sessions = %d, want 4", len(runs))
	}
	var all []block
	byTag := map[string][]block{}
	for tag, run := range runs {
		byTag[tag] = blocks(run)
		all = append(all, byTag[tag]...)
	}

	var got, want []int64
	for _, b := range blockIDs(all) {
		got = append(got, b...)
	}
	for _, it := range items {
		want = append(want, it.id)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("blocks hold items %v, want each of %v once", got, want)
	}

	// The step each block heads, by message_id, and the calls its results
	// answer. Every block not named here has no results.
	joins := map[string][]string{
		"m1": {"call_1", "call_2", "call_3", "call_4"},
		"m2": {"call_5"},
		"m3": {"call_6"},
		"m4": {"call_7"},
		"m5": {"call_8"},
		"m7": nil, // its call never got a result
		"h1": {"call_h1"},
		// In the order the calls finished, not the order they were made.
		"v1": {"call_v1", "call_v2", "call_v3", "call_v4", "call_v6", "call_v7", "call_v8", "call_v9",
			"call_v10", "call_v11", "call_v12", "call_v14", "call_v13", "call_v5"},
	}
	found := map[string]bool{}
	for _, b := range all {
		var answered []string
		for _, r := range b.results {
			answered = append(answered, string(r.data.ToolCallID))
		}
		key := string(b.head.data.MessageID)
		want, named := joins[key]
		if b.head.data.Role != "assistant" || !named {
			want = nil
		} else {
			found[key] = true
		}
		if !slices.Equal(answered, want) {
			t.Errorf("block at %s position %d answers %v, want %v",
				tagFor(b.head.sessionID), b.head.position, answered, want)
		}
	}
	for key := range joins {
		if !found[key] {
			t.Errorf("no block headed by step %s", key)
		}
	}

	// A result no call answers heads a block of its own.
	orphan := false
	for _, b := range byTag["zoo001"] {
		orphan = orphan || b.head.data.ToolCallID == "call_orphan"
	}
	if !orphan {
		t.Error("the result no call answers heads no block")
	}

	// The fixture has the shape the joins above are meant to cover: the
	// transfer_task's result arrives after the subagent's last item and after
	// its subsession row, and the background agent's subsession row lands
	// before the step that waits for it.
	var subs []int64 // zoo001's subsession rows: zoo003's, then zoo004's
	steps := map[string]block{}
	for _, b := range byTag["zoo001"] {
		if b.head.typ == "subsession" {
			subs = append(subs, b.head.position)
		}
		if id := string(b.head.data.MessageID); id != "" {
			steps[id] = b
		}
	}
	if len(subs) != 2 {
		t.Fatalf("zoo001 has %d subsession rows, want 2", len(subs))
	}
	call5 := steps["m2"].results[0]
	if subs[0] <= steps["m2"].head.position || call5.position <= subs[0] {
		t.Errorf("call_5's result at position %d, want it after the subsession row at %d", call5.position, subs[0])
	}
	for _, it := range runs["zoo003"] {
		if it.timeCreated >= call5.timeCreated {
			t.Errorf("zoo003 item at position %d is no earlier than call_5's result", it.position)
		}
	}
	if m4, m5 := steps["m4"].head.position, steps["m5"].head.position; subs[1] <= m4 || m5 <= subs[1] {
		t.Errorf("zoo004's subsession row at %d, want it between m4 at %d and m5 at %d", subs[1], m4, m5)
	}
}

// The merge takes the lane whose next block is earliest, then one whose time
// is borrowed, then the lower id, and never reorders a lane: A's 20 after its
// 30 stays there, where a sort would have put it before B's 25.
func TestMerge(t *testing.T) {
	lane := func(sid string, heads ...itemRow) []block {
		var out []block
		for i := range heads {
			heads[i].sessionID = sid
			out = append(out, block{head: &heads[i]})
		}
		return out
	}
	at := func(ms, id int64) itemRow { return itemRow{id: id, timeCreated: ms} }
	borrowed := at(40, 9)
	borrowed.borrowed = true
	merged := merge([][]block{lane("a", at(10, 1), at(30, 4), at(20, 5)), lane("b", at(25, 3), at(40, 8)),
		lane("c", at(40, 6)), lane("d", borrowed)})
	var got []string
	for _, b := range merged {
		got = append(got, fmt.Sprint(b.head.sessionID, b.head.timeCreated))
	}
	if want := "a10 b25 a30 a20 d40 c40 b40"; strings.Join(got, " ") != want {
		t.Errorf("merged = %s, want %s", strings.Join(got, " "), want)
	}
}

// TestMergeSameSecond: a subagent run whole inside its parent's seconds — its
// session started in the second of the parent's, and sorting first by id. Each
// item lands where it was written: the subagent below the prompt and the step
// that called it, its marker ahead of it rather than inside its run, which
// borrows the step's second, and its last step above the parent's next one.
func TestMergeSameSecond(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("zz_par", "", "P", "/p", clock(0))
	d.userText("zz_par", clock(0), "the-prompt")
	d.step("zz_par", clock(0), "", callJSON("c1", "transfer_task", `{"agent":"helper","task":"look it up"}`))
	d.whole("zz_par", "aa_kid", clock(0), userJSON(clock(0), "kid-task"), stepJSON(clock(0), "kid-first"),
		stepJSON(clock(5), "kid-last"))
	d.retitle("aa_kid", "Transferred task")
	d.toolResult("zz_par", "c1", clock(5), "found it")
	d.step("zz_par", clock(5), "parent-next")

	out := strings.Join(onePass(t, d, roundOpts(t, d)), "\n")
	last := -1
	for _, want := range []string{
		"the-prompt", "transfer_task", "session " + tagOpen + "aa_kid↓" + tagClose,
		newFlag + " " + tagOpen + "aa_kid↓" + tagClose, "kid-task", "kid-first", "kid-last", "parent-next",
	} {
		i := strings.Index(out, want)
		if i < 0 || i < last {
			t.Errorf("%q missing or out of order (at %d, after %d):\n%s", want, i, last, out)
		}
		last = i
	}
	// Two sessions, three runs: the parent, the subagent whole, the parent.
	if n := strings.Count(out, tagOpen+"aa_kid↓"+tagClose+" Transferred task"); n != 1 {
		t.Errorf("subagent banner drawn %d times, want once:\n%s", n, out)
	}
}
