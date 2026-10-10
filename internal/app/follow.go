package app

import (
	"bufio"
	"context"
	"fmt"
	"time"
)

// follow polls for items newer than what has been printed and appends each
// block once it is complete — see settledPrefix — until interrupted.
//
// The cursor is the highest item id looked at, which says which sessions have
// new rows, and how far each session has been printed; each round reads only
// the unprinted tails of the sessions that changed or hold a step, so nothing
// is printed twice and a step rewritten in place is read again until it is
// whole. Until a round returns a cursor — the opening found no session in
// scope — each round reads like the opening. The session banner carries over
// from round to round, and the tail goes to the same writer the opening
// transcript went to — the file when --out was given.
func follow(ctx context.Context, st *store, opts *options, w *bufio.Writer, cur *cursor, carry seam) error {
	ticker := time.NewTicker(time.Duration(opts.interval * float64(time.Second)))
	defer ticker.Stop()

	// A held step is invisible: a transfer_task running for ten minutes and a
	// step cut off and left look the same from the store, and the second holds
	// the rest of its session until that session goes on. Say so once per
	// step, on stderr, where the transcript's sink is not.
	const holdNotice = 30 * time.Second
	type wait struct {
		since time.Time
		said  bool
	}
	waits := map[int64]wait{} // by the held step's item id

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		round, err := build(ctx, st, opts, cur, carry)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if len(round.lines) > 0 {
			if err := writeLines(w, round.lines); err != nil {
				return fmt.Errorf("cannot write: %v", err)
			}
		}
		if round.cur != nil {
			cur = round.cur
		}
		carry = round.seam
		held := make(map[int64]wait, len(round.held))
		for _, h := range round.held {
			hw, ok := waits[h.itemID]
			if !ok {
				hw.since = time.Now()
			}
			if !hw.said && time.Since(hw.since) >= holdNotice {
				opts.warnf("waiting on %s in %s: unfinished since %s — the rest of that session is held until it completes or the session goes on",
					h.what, h.tag, stamp(h.at))
				hw.said = true
			}
			held[h.itemID] = hw
		}
		waits = held
	}
}
