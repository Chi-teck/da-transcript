package app

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// transcript is what one build() produced: the rendered lines, the sessions
// that contributed, where the tail should resume, and — when there are no
// lines — why.
type transcript struct {
	lines []string
	used  []*session
	cur   *cursor // nil when nothing was read: the next round starts over
	seam  seam
	empty string
	held  []heldStep
}

// heldStep is a step a --follow round refused to print: the rest of its
// session waits with it, and the session's lane did not move past it.
type heldStep struct {
	tag, what  string // the session's short handle, and what the step waits on, for the notice
	itemID, at int64  // the step's row, which keys the notice, and its time
}

// seam is the banner state one --follow round hands the next: which session
// the stream is in the middle of, so a banner is not repeated, and which
// sessions have had a banner at all, so one the stream comes back to after a
// poll is flagged as a return rather than as a beginning — the seam is not a
// place the reader can see, so nothing about a banner may depend on which side
// of it the banner falls. high is the latest head time drawn so far: a block
// before it arrived late.
type seam struct {
	prevSess string
	seen     map[string]bool
	high     int64
}

// block is one thing the transcript draws: an item, plus — when the item is an
// assistant step — the tool results that answer its calls, in position order.
type block struct {
	head    *itemRow
	results []*itemRow
}

// answers pairs a step's calls with its results, in the order the model made
// the calls: each call gets the first result for its id that no call before
// it took, or nil when none is left. rest is what no call is left to claim —
// an id answered twice. The drawing and the --follow hold both go by it, so a
// step is held for exactly as long as it would draw a call without a result.
func (b block) answers() (paired, rest []*itemRow) {
	calls := b.head.data.ToolCalls
	paired = make([]*itemRow, len(calls))
	taken := make([]bool, len(b.results))
	for i, call := range calls {
		for j, r := range b.results {
			if !taken[j] && r.data.ToolCallID == call.ID {
				taken[j], paired[i] = true, r
				break
			}
		}
	}
	for j, r := range b.results {
		if !taken[j] {
			rest = append(rest, r)
		}
	}
	return paired, rest
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
		return nil, nil, fmt.Sprintf("No docker-agent session has ever run under %s — try --root or --everywhere.", neutraliseLine(opts.root)), nil
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
// by the block's time, then by which was written first. Position never
// decides, since a lane offers one block at a time.
//
// Stamps are whole seconds, so ties between sessions are common, and the id —
// global insertion order — is what breaks them: a subagent's first item can
// share the second of its parent's prompt, and its last item the second of
// the parent's next step, and the id puts each where it was written. Ordering
// by the sessions instead would put one session first in both, with the
// subagent above the prompt or the parent's next step inside the subagent's
// run. --follow agrees: a round reads what was written after what earlier
// rounds printed, and in a tie the id puts it after them too.
//
// A block whose time is borrowed from the item before it is the exception: it
// goes first in its second, right behind that item. Its id is no guide — a
// subagent's marker is written after the subagent's whole run, and by id would
// cut the run in two at the second its time came from. The price: one read a
// round after another session printed in its second comes out behind that,
// where one pass puts it ahead.
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
func merge(lanes [][]block) []block {
	lanes = slices.Clone(lanes)
	total := 0
	for _, l := range lanes {
		total += len(l)
	}
	first := func(a, b *itemRow) bool {
		switch {
		case a.timeCreated != b.timeCreated:
			return a.timeCreated < b.timeCreated
		case a.borrowed != b.borrowed:
			return a.borrowed
		}
		return a.id < b.id // unique, so no two blocks tie
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
// With no cursor it is a whole transcript: the opening one of a --follow, or a
// run without it. Every item of the sessions in scope is read, tallied into
// their --list figures and grouped into blocks, all of it over the whole
// session: a step outside the --since/--until window still claims its results,
// and a result outside it still joins a step inside. The window then keeps the
// blocks whose head falls in it, an item without a timestamp counting at the
// time of the one before it, and a session contributes when a block of its is
// drawn.
//
// With a cursor it is a --follow round, which reads only what it has not
// printed: the tail of each session with a new row or a held step, from where
// its printed part ends, and the whole of a session new to scope. A held step
// and every result answering it are in its session's tail, so the blocks a
// tail makes are the ones the whole session would make there. carry passes
// the banner state across rounds so a tail neither repeats a banner nor
// forgets a session it has already announced.
func build(ctx context.Context, st *store, opts *options, prev *cursor, carry seam) (*transcript, error) {
	_, wanted, empty, err := scope(ctx, st, opts)
	if err != nil {
		return nil, err
	}
	// The round's own copy: what it hands on is what it was given plus what it
	// announced, and the caller's map is not written through.
	t := &transcript{seam: seam{prevSess: carry.prevSess, seen: maps.Clone(carry.seen), high: carry.high}}
	if t.seam.seen == nil {
		t.seam.seen = map[string]bool{}
	}
	if empty != "" {
		t.empty = empty
		return t, nil
	}
	byID := make(map[string]*session, len(wanted))
	for _, s := range wanted {
		byID[s.id] = s
	}
	before := func(b block) bool {
		return opts.since != nil && b.head.timeCreated < *opts.since
	}
	outside := func(b block) bool {
		return before(b) || (opts.until != nil && b.head.timeCreated >= *opts.until)
	}

	// take groups one session's run of items, read from its lane's next, into
	// the blocks to draw, and moves the lane past what it took, noting whether
	// any of it was skipped as older than --since. Under --follow it stops at a
	// step not yet complete (settledPrefix), and notes the subagents whose
	// marker it left behind the step. A subagent is never held: it is written
	// whole when it ends and never changes.
	var lanes [][]block
	behind := map[string]bool{} // by subagent id: its subsession row is past a held step
	take := func(s *session, run []itemRow) {
		bs := blocks(run)
		cut := len(bs)
		if opts.follow && s.parentID == "" {
			cut = settledPrefix(bs, outside)
		}
		l := t.cur.lanes[s.id]
		l.held = cut < len(bs)
		switch {
		case l.held:
			// Results follow their step, so the held step's are all past it.
			// A step before it answered past it too waits with it: drawn now,
			// its result would be read again with the tail and drawn a second
			// time, as one no call answers.
			head := bs[cut].head
			t.held = append(t.held, heldStep{tag: s.tag, what: waitingOn(bs[cut]), itemID: head.id, at: head.timeCreated})
			if i := slices.IndexFunc(bs[:cut], func(b block) bool {
				return len(b.results) > 0 && b.results[len(b.results)-1].position > head.position
			}); i >= 0 {
				cut = i
			}
			l.next = bs[cut].head.position
		case len(run) > 0:
			l.next = run[len(run)-1].position + 1
		}
		for i := range run {
			if run[i].position < l.next {
				l.at = run[i].timeCreated
			} else if run[i].subsession != "" {
				behind[run[i].subsession] = true
			}
		}
		l.above = l.above || slices.ContainsFunc(bs[:cut], before)
		t.cur.lanes[s.id] = l
		if kept := slices.DeleteFunc(bs[:cut], outside); len(kept) > 0 {
			lanes = append(lanes, kept)
		}
	}
	// A subagent new to the round is taken after every other session, when
	// behind is complete (below).
	type newSub struct {
		s   *session
		run []itemRow
	}
	var subs []newSub

	if prev == nil {
		// Taken before the items are read: a row committed in between is then
		// looked at twice, which the lanes make harmless, rather than never.
		last, err := st.lastID(ctx)
		if err != nil {
			return nil, err
		}
		items, err := st.items(ctx, wanted)
		if err != nil {
			return nil, err
		}
		t.cur = &cursor{lastID: last, lanes: make(map[string]lane, len(wanted))}
		for _, s := range wanted {
			t.cur.lanes[s.id] = lane{at: s.timeCreated}
		}
		// items() hands each session over as one run of its items.
		for len(items) > 0 {
			n := 1
			for n < len(items) && items[n].sessionID == items[0].sessionID {
				n++
			}
			run := items[:n]
			items = items[n:]
			s := byID[run[0].sessionID]
			for i := range run {
				tally(s, &run[i])
			}
			if s.parentID != "" {
				subs = append(subs, newSub{s, run})
				continue
			}
			take(s, run)
		}
	} else {
		touched, last, err := st.changed(ctx, prev.lastID)
		if err != nil {
			return nil, err
		}
		t.cur = &cursor{lastID: last, lanes: maps.Clone(prev.lanes)}
		for _, s := range wanted {
			// A session with no lane is new to scope and read whole — also one
			// whose rows an earlier round's id already passed, because they
			// were committed after that round read the scope.
			l, ok := t.cur.lanes[s.id]
			if !ok {
				l = lane{at: s.timeCreated}
				t.cur.lanes[s.id] = l
			} else if !touched[s.id] && !l.held {
				continue
			}
			run, err := st.tail(ctx, s.id, l.next, l.at)
			if err != nil {
				return nil, err
			}
			if !ok && s.parentID != "" {
				subs = append(subs, newSub{s, run})
				continue
			}
			take(s, run)
		}
	}
	// A transfer_task's subagent, or a forked skill's, is written by the call
	// itself, a moment before the call's result — seconds, with docker-agent's
	// hooks — so a marker of one past a held step is that step's own: the
	// subagent gets no lane, is read whole again next round, and comes out
	// after the step, as one pass draws it. A background agent's lands
	// whenever it ends and does not wait. docker-agent tells the kinds apart
	// only by title.
	for _, sub := range subs {
		if behind[sub.s.id] && (sub.s.title == "Transferred task" || strings.HasPrefix(sub.s.title, "Skill: ")) {
			delete(t.cur.lanes, sub.s.id)
			continue
		}
		take(sub.s, sub.run)
	}

	// standing is what the stream already ends in, and the round's first gap is
	// the only one that has to care. A --follow round is appended to a
	// transcript that ended in endGap rows: those are out there and nothing can
	// take them back, so the first break lays down the difference and no more.
	// After that the round is writing on ground it owns.
	standing := 0
	if carry.prevSess != "" {
		standing = endGap
	}
	gap := func(rows int) {
		for range max(0, rows-standing) {
			t.lines = append(t.lines, "")
		}
		standing = 0
	}
	usedSet := map[string]bool{}
	for _, b := range merge(lanes) {
		lines := renderBlock(b, opts)
		if len(lines) == 0 {
			continue
		}
		s := byID[b.head.sessionID]
		// One warning a run, at the first item drawn that this version could
		// not read; the placeholders say the rest.
		if b.head.unknown != "" && !opts.warnedUnknown {
			opts.warnf("some items are unknown to this version (first: %s in %s)", neutraliseLine(b.head.unknown), s.tag)
			opts.warnedUnknown = true
		}
		if !usedSet[s.id] {
			usedSet[s.id] = true
			t.used = append(t.used, s)
		}
		// Only a round draws the late flag. One merge is in time order, bar a
		// session's own stamps going backwards; a round is not, against what
		// earlier ones drew: a subagent's burst is stamped back to when it
		// started, and a held step comes out after other sessions moved on.
		late := prev != nil && b.head.timeCreated < t.seam.high
		if s.id != t.seam.prevSess {
			flag := bannerFlag(t.cur.lanes[s.id].above, t.seam.seen[s.id], late)
			gap(sessionGap)
			t.lines = append(t.lines, sessionBanner(s, flag, opts.paint, opts.width)...)
			gap(bannerGap)
			t.seam.prevSess = s.id
			t.seam.seen[s.id] = true
		} else {
			gap(turnGap)
		}
		t.lines = append(t.lines, lines...)
		t.seam.high = max(t.seam.high, b.head.timeCreated)
	}
	if len(t.lines) > 0 {
		gap(endGap)
	}
	if len(t.lines) == 0 {
		if len(t.held) > 0 {
			h := slices.MinFunc(t.held, func(a, b heldStep) int { return cmp.Compare(a.at, b.at) })
			t.empty = fmt.Sprintf("Nothing settled yet — waiting on %s in %s.", h.what, h.tag)
		} else {
			hint := " — try --all"
			if opts.all {
				hint = ""
			}
			t.empty = fmt.Sprintf("Nothing from %d session(s) in this window%s.", len(wanted), hint)
		}
	}
	return t, nil
}

// settledPrefix is how many of one session's blocks, in position order, a
// --follow round may emit: everything up to the first step still being
// written or still waiting on a call. Printed then, the step would be printed
// half, and its session's lane would move past it, so the rest would never be
// read. A step therefore appears whole or not at all, and the round stops
// there rather than skipping it: the rest of the session waits with it. Other
// sessions do not; each has a lane of its own.
//
// The unit is a step, where oc-transcript holds a whole answer: a docker-agent
// answer can stay open for an hour.
//
// A step that is not complete is emitted all the same when
//
//	its head is before --since          // skipped, as a run without --follow skips it
//	a later block proves it over        // see proves: the stall breaker
//
// docker-agent marks no step it cut off: one interrupted mid-stream keeps no
// finish_reason, one interrupted mid-tool keeps calls without results. A
// cut-off step looks like a slow one until its session goes on, and is held
// until then; it is drawn as a run without --follow draws it.
func settledPrefix(bs []block, outside func(block) bool) int {
	over := -1 // the last block proving the ones before it over
	for i, b := range bs {
		if proves(b.head) {
			over = i
		}
	}
	for i, b := range bs {
		if i < over || outside(b) || waitingOn(b) == "" {
			continue
		}
		return i
	}
	return len(bs)
}

// proves reports whether an item shows that every step before it in its
// session is over. A prompt lands only between steps, a step only after the
// one before it had its calls answered, and an error or a compaction only
// once the run stopped. A subagent's subsession row does not: a background
// one lands at any moment, mid-call included. Nor does an evaluator's row,
// written while a batch of calls runs, nor a result, which can be the step's
// own.
func proves(it *itemRow) bool {
	if it.unknown != "" {
		return false
	}
	switch it.typ {
	case "error", "summary":
		return true
	case "message":
		return it.data.Role == "user" || it.data.Role == "assistant"
	}
	return false
}

// waitingOn is what a block still lacks: "the step" for a step still being
// written, which has no finish_reason yet, the first call without a result
// for one whose calls are still running, and "" for a complete block. Only an
// assistant step can be incomplete; every other item is written whole.
func waitingOn(b block) string {
	it := b.head
	if it.unknown != "" || it.typ != "message" || it.data.Role != "assistant" {
		return ""
	}
	if it.data.FinishReason == "" {
		return "the step"
	}
	paired, _ := b.answers()
	for i, r := range paired {
		if r == nil {
			return cmp.Or(neutraliseLine(string(it.data.ToolCalls[i].Function.Name)), "?")
		}
	}
	return ""
}
