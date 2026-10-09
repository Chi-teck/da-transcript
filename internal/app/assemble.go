package app

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
)

// transcript is what one build() produced: the rendered lines, the sessions
// that contributed and — when there are no lines — why.
type transcript struct {
	lines []string
	used  []*session
	empty string
}

// block is one thing the transcript draws: an item, plus — when the item is an
// assistant step — the tool results that answer its calls, in position order.
type block struct {
	head    *itemRow
	results []*itemRow
}

// scope works out which sessions are in play: everything under the root,
// tagged, then narrowed by --session.
func scope(ctx context.Context, st *store, opts *options) (all, wanted []*session, empty string, err error) {
	rows, err := st.sessions(ctx, opts.root)
	if err != nil {
		return nil, nil, "", err
	}
	if len(rows) == 0 {
		if opts.root == "" {
			return nil, nil, "This database has no sessions at all.", nil
		}
		return nil, nil, fmt.Sprintf("No docker-agent session has ever run under %s — try --root or --everywhere.", opts.root), nil
	}
	all = tagSessions(rows)

	wanted = all
	if opts.sessions != nil {
		wanted = slices.DeleteFunc(slices.Clone(wanted), func(s *session) bool {
			for _, frag := range opts.sessions {
				if strings.Contains(s.id, frag) {
					return false
				}
			}
			return true
		})
	}
	if len(wanted) == 0 {
		empty = fmt.Sprintf("%d session(s) in scope, none matching --session.", len(all))
	}
	return all, wanted, empty, nil
}

// blocks groups one session's items, in position order, into the blocks the
// transcript draws.
//
// docker-agent stores a tool's result as a row of its own, with no tool name,
// after the step that made the call, so the two are joined through
// tool_call_id. The join looks past whatever lands between them: a
// transfer_task's result comes back only after the subagent's whole run, with
// its subsession row in between, and a background agent's subsession row lands
// wherever the parent was when it finished. A result joins the nearest earlier
// call with its id. One that no call answers — no id, or an id no step before
// it made, or a step this version could not read — is a block of its own, so
// nothing is dropped and nothing is drawn twice.
func blocks(items []itemRow) []block {
	var out []block
	calls := map[string]int{} // call id → the index in out of the step that made the call
	for i := range items {
		it := &items[i]
		if it.unknown == "" && it.typ == "message" && it.data.Role == "tool" {
			if at, ok := calls[string(it.data.ToolCallID)]; ok {
				out[at].results = append(out[at].results, it)
				continue
			}
		}
		out = append(out, block{head: it})
		if it.unknown == "" && it.typ == "message" && it.data.Role == "assistant" {
			for _, c := range it.data.ToolCalls {
				if c.ID != "" {
					calls[string(c.ID)] = len(out) - 1
				}
			}
		}
	}
	return out
}

// merge interleaves the sessions' lanes, each one session's blocks in position
// order, into one stream: at each step, the lane whose next block sorts first
// by the block's time, then its session's start, then the session id. Position
// never decides, since a lane offers one block at a time.
//
// A merge and not a sort, because a session's order is its positions and has
// to survive whatever its stamps say. They rarely go backwards, but they can:
// an evaluation's sub-second stamp followed in the same second by a
// whole-second one, or a clock set back. A sort would reorder those rows; the
// merge never does.
//
// The next lane is found by a linear scan, O(blocks × sessions in the window):
// milliseconds on a store of a hundred sessions, and a hundred times that on
// one ten times the size. container/heap is the fix if that ever shows.
func merge(lanes [][]block, byID map[string]*session) []block {
	lanes = slices.Clone(lanes)
	total := 0
	for _, l := range lanes {
		total += len(l)
	}
	first := func(a, b *itemRow) bool {
		sa, sb := byID[a.sessionID], byID[b.sessionID]
		return cmp.Or(
			cmp.Compare(a.timeCreated, b.timeCreated),
			cmp.Compare(sa.timeCreated, sb.timeCreated),
			strings.Compare(sa.id, sb.id),
		) < 0
	}
	out := make([]block, 0, total)
	for len(out) < total {
		next := -1
		for i, l := range lanes {
			if len(l) > 0 && (next < 0 || first(l[0].head, lanes[next][0].head)) {
				next = i
			}
		}
		out = append(out, lanes[next][0])
		lanes[next] = lanes[next][1:]
	}
	return out
}

// build renders the transcript, plus a line saying why it is empty when it is.
//
// Coming back with nothing has three unrelated causes — no session ever ran
// under the root, the filters excluded the ones that did, or the window holds
// nothing to draw — and a reader told the wrong one goes looking in the wrong
// place. Whenever no lines come back, `empty` is set.
//
// Every item of the sessions in scope is read, tallied into their --list
// figures and grouped into blocks, all of it over the whole session: a step
// outside the --since/--until window still claims its results, and a result
// outside it still joins a step inside. The window then keeps the blocks whose
// head falls in it, an item without a timestamp counting at the time of the one
// before it, and a session contributes when a block of its is drawn.
func build(ctx context.Context, st *store, opts *options) (*transcript, error) {
	_, wanted, empty, err := scope(ctx, st, opts)
	if err != nil {
		return nil, err
	}
	t := &transcript{}
	if empty != "" {
		t.empty = empty
		return t, nil
	}
	byID := make(map[string]*session, len(wanted))
	for _, s := range wanted {
		byID[s.id] = s
	}
	items, err := st.items(ctx, wanted)
	if err != nil {
		return nil, err
	}
	outside := func(b block) bool {
		at := b.head.timeCreated
		return (opts.since != nil && at < *opts.since) || (opts.until != nil && at >= *opts.until)
	}
	// items() hands each session over as one run of its items.
	var lanes [][]block
	for len(items) > 0 {
		n := 1
		for n < len(items) && items[n].sessionID == items[0].sessionID {
			n++
		}
		run := items[:n]
		items = items[n:]
		for i := range run {
			tally(byID[run[i].sessionID], &run[i])
		}
		if lane := slices.DeleteFunc(blocks(run), outside); len(lane) > 0 {
			lanes = append(lanes, lane)
		}
	}

	gap := func(rows int) {
		for range rows {
			t.lines = append(t.lines, "")
		}
	}
	usedSet := map[string]bool{}
	seen := map[string]bool{}
	prevSess := ""
	for _, b := range merge(lanes, byID) {
		lines := renderBlock(b, opts)
		if len(lines) == 0 {
			continue
		}
		s := byID[b.head.sessionID]
		// One warning a run, at the first item drawn that this version could
		// not read; the placeholders say the rest.
		if b.head.unknown != "" && !opts.warnedUnknown {
			opts.warnf("some items are unknown to this version (first: %s in %s)", neutralise(b.head.unknown), s.tag)
			opts.warnedUnknown = true
		}
		if !usedSet[s.id] {
			usedSet[s.id] = true
			t.used = append(t.used, s)
		}
		if s.id != prevSess {
			flag := bannerFlag(s, opts.since, seen[s.id])
			gap(sessionGap)
			t.lines = append(t.lines, sessionBanner(s, flag, opts.paint, opts.width)...)
			gap(bannerGap)
			prevSess = s.id
			seen[s.id] = true
		} else {
			gap(turnGap)
		}
		t.lines = append(t.lines, lines...)
	}
	if len(t.lines) > 0 {
		gap(endGap)
	}
	if len(t.lines) == 0 {
		hint := " — try --all"
		if opts.all {
			hint = ""
		}
		t.empty = fmt.Sprintf("Nothing from %d session(s) in this window%s.", len(wanted), hint)
	}
	return t, nil
}
