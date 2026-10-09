package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// transcript is what one build() produced: the sessions that contributed and,
// when there are none, why.
type transcript struct {
	used  []*session
	empty string
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

// build works out the sessions the transcript draws on, plus a line saying why
// there are none when there are none.
//
// Coming back with nothing has three unrelated causes — no session ever ran
// under the root, the filters excluded the ones that did, or the window holds
// no message — and a reader told the wrong one goes looking in the wrong
// place. Whenever no session comes back, `empty` is set.
//
// Every item of the sessions in scope is read and tallied into their --list
// figures; a session contributes when any of its items falls in the
// --since/--until window, an item without a timestamp at the time of the one
// before it.
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
	hit := map[string]bool{}
	for i := range items {
		it := &items[i]
		tally(byID[it.sessionID], it)
		if (opts.since == nil || it.timeCreated >= *opts.since) &&
			(opts.until == nil || it.timeCreated < *opts.until) {
			hit[it.sessionID] = true
		}
	}
	for _, s := range wanted {
		if hit[s.id] {
			t.used = append(t.used, s)
		}
	}
	if len(t.used) == 0 {
		hint := " — try --all"
		if opts.all {
			hint = ""
		}
		t.empty = fmt.Sprintf("Nothing from %d session(s) in this window%s.", len(wanted), hint)
	}
	return t, nil
}
