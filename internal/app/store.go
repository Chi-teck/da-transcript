package app

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver, pure Go
)

// The store is opened read-only and nothing here writes to it. The traps this
// code steps around: scope by sessions.working_dir with a prefix compare, never
// like, and let an old subagent's empty one inherit its parent's; order a
// session's items by position, never by id; timestamps are RFC 3339 text,
// parsed here once and never compared as strings, and some items carry none;
// chunk every IN (…) list.

// chunkSize bounds the bound-parameter count of one statement. The ceiling
// varies by SQLite build (999 before 3.32), so never rely on the ambient limit.
const chunkSize = 500

type store struct {
	db *sql.DB
}

type sessionRow struct {
	id, parentID, title, directory string // directory: working_dir, or the nearest ancestor's when that is ''
	timeCreated                    int64
	// The figures of the --list table that describe the session rather than
	// the window. build() fills them in from the items (tally), not the query:
	// docker-agent's own cost and token columns are not totals, and it keeps no
	// updated-at. timeUpdated stays 0 for a session with no items, and
	// hasTokens tells a session whose steps reported zero apart from one where
	// no item reported usage at all.
	timeUpdated int64
	tokens      int64
	hasTokens   bool
}

type itemRow struct {
	id          int64
	sessionID   string
	position    int64
	typ         string   // item_type
	agent       string   // agent_name: the agent behind a message; "" on user and system messages and on every other item
	implicit    bool     // a prompt docker-agent injected rather than one the user typed
	subsession  string   // subsession_id: the child a subsession row folds back in; "" once the child is deleted
	data        itemData // message_json decoded (payload.go); a summary's usage_json lands in data.Usage
	unknown     string   // non-empty when this version cannot read the item: what to call it
	timeCreated int64    // epoch ms: its own created_at, else the previous item's, else its session's start
}

// openStore opens the database read-only and probes it, so a missing file, an
// unreadable WAL, a file that is not a database or a database docker-agent
// v1.113 has not migrated all fail here, with a one-line message, rather than
// on the first query.
func openStore(ctx context.Context, path string) (*store, error) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("no docker-agent database at %s (try --db)", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("no docker-agent database at %s (try --db)", path)
	}
	// `file:` DSN: the driver hands it to sqlite3_open_v2 with SQLITE_OPEN_URI,
	// so mode=ro reaches SQLite and the handle is genuinely read-only.
	//
	// busy_timeout: WAL means a reader never blocks on the writer, but the
	// server checkpointing or restarting the WAL can still hand back
	// SQLITE_BUSY. A --follow polling for hours meets that eventually, and
	// waiting five seconds is better than ending the tail.
	dsn := "file:" + uriEscape(abs) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err == nil {
		db.SetMaxOpenConns(1)
		err = db.PingContext(ctx)
		if err == nil {
			// The newest column read, session_items.usage_json, came with
			// migration 025 (v1.113.0). One probe for it refuses both a store
			// from before session_items and one from before the column.
			var n int
			err = db.QueryRowContext(ctx, "select count(*) from pragma_table_info('session_items')"+
				" where name = 'usage_json'").Scan(&n)
			if err == nil && n == 0 {
				_ = db.Close()
				return nil, fmt.Errorf("%s is not a docker-agent v1.113+ database", path)
			}
		}
	}
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		// The newline is deliberate: the second line names the fix.
		//nolint:staticcheck // ST1005: user-facing CLI text, not a wrapped error
		return nil, fmt.Errorf("cannot open %s read-only: %v\n"+
			"docker-agent uses WAL; the -wal and -shm files next to it must be readable too.", path, err)
	}
	return &store{db: db}, nil
}

// uriEscape escapes the characters that would be mis-read inside a file: URI.
func uriEscape(p string) string {
	r := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")
	return r.Replace(p)
}

func (s *store) Close() error { return s.db.Close() }

// sessions returns every session whose directory sits inside root, in time
// order. There is no project id to scope by, only the directory a session ran
// in. An empty root drops the scoping and takes the whole database.
func (s *store) sessions(ctx context.Context, root string) ([]sessionRow, error) {
	// A session's directory is its working_dir or, when that is empty, its
	// nearest ancestor's: docker-agent before v1.138 stored subagents without
	// one and never backfilled them, and resolves them this way itself. The
	// recursive step adds only sessions whose own directory is empty, so it
	// ends on any parent graph; a top-level session left without one gets no
	// row here and reads as "".
	query := `with recursive dirs(id, dir) as (
	    select id, working_dir from sessions where working_dir <> ''
	    union
	    select s.id, d.dir from sessions s join dirs d on s.parent_id = d.id
	     where coalesce(s.working_dir, '') = ''
	  )
	  select s.id, s.parent_id, s.title, d.dir, s.created_at
	    from sessions s left join dirs d on d.id = s.id`
	var args []any
	if root != "" {
		// substr and not like: like is ASCII case-insensitive by default, so it
		// would fold /foo into /Foo — two unrelated projects on a case-sensitive
		// filesystem — while the `dir = ?` half beside it stays exact. Both
		// halves of the predicate have to agree, and comparing the prefix
		// outright is also the end of the escaping the like form needed.
		prefix := root + string(filepath.Separator)
		query += " where d.dir = ? or substr(d.dir, 1, length(?)) = ?"
		args = append(args, root, prefix, prefix)
	}
	var out []sessionRow
	rows, err := s.db.QueryContext(ctx, query, args...)
	err = scanAll(rows, err, func(rows *sql.Rows) error {
		var r sessionRow
		var parent, title, dir, created sql.NullString
		if err := rows.Scan(&r.id, &parent, &title, &dir, &created); err != nil {
			return err
		}
		r.parentID, r.title, r.directory = parent.String, title.String, dir.String
		// One that does not parse reads as 0, as docker-agent's own loader
		// falls back to the zero time; every writer formats it as RFC 3339.
		r.timeCreated, _ = parseStamp(created.String)
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Sorted here and not in SQL: RFC 3339 text with an offset does not sort
	// as a string. id only as a tiebreaker: two sessions can share a second,
	// and the order they come back in has to be the same on every run.
	slices.SortStableFunc(out, func(a, b sessionRow) int {
		return cmp.Or(cmp.Compare(a.timeCreated, b.timeCreated), strings.Compare(a.id, b.id))
	})
	return out, nil
}

// messageCounts returns how many messages each of the given sessions holds.
//
// The whole session is counted, never the --since/--until window: the figures
// beside a session in the --list table describe the session, the way its start
// time already does, and a count that moved with the window would be the only
// one of them that did.
//
// It counts message rows, as docker-agent's own session browser does: prompts,
// a subagent's task, model steps and tool results count, and subagent markers,
// summaries and errors do not. A model step that left no content behind is
// still a message, and a subagent is a session of its own, so a parent's count
// stops at its own items.
func (s *store) messageCounts(ctx context.Context, ids []string) (map[string]int, error) {
	out := make(map[string]int, len(ids))
	for chunk := range slices.Chunk(ids, chunkSize) {
		query := "select session_id, count(*) from session_items where session_id in (" +
			placeholders(len(chunk)) + ") and item_type = 'message' group by session_id"
		rows, err := s.db.QueryContext(ctx, query, anySlice(chunk)...)
		err = scanAll(rows, err, func(rows *sql.Rows) error {
			var sid string
			var n int
			if err := rows.Scan(&sid, &n); err != nil {
				return err
			}
			out[sid] = n
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// items returns every item of the given sessions, decoded, ordered by
// (session_id, position): a session's items in the order it holds them, and
// the sessions in id order. The ids are sorted before they are chunked, so the
// chunks' results put end to end are what one statement would have produced;
// SQLite's default BINARY collation is byte order, the same as Go's.
//
// The whole session is read, not the window: there is no numeric time column
// to bound, and an item without a timestamp takes the previous item's — the
// first takes its session's start — so its time needs the items before it.
func (s *store) items(ctx context.Context, sessions []*session) ([]itemRow, error) {
	start := make(map[string]int64, len(sessions))
	ids := make([]string, 0, len(sessions))
	for _, ses := range sessions {
		start[ses.id] = ses.timeCreated
		ids = append(ids, ses.id)
	}
	slices.Sort(ids)
	var out []itemRow
	var cur string // the session prev belongs to
	var prev int64
	for chunk := range slices.Chunk(ids, chunkSize) {
		query := "select id, session_id, position, item_type, coalesce(agent_name, ''), coalesce(implicit, 0)," +
			" coalesce(subsession_id, ''), message_json, usage_json from session_items" +
			" where session_id in (" + placeholders(len(chunk)) + ") order by session_id, position"
		rows, err := s.db.QueryContext(ctx, query, anySlice(chunk)...)
		err = scanAll(rows, err, func(rows *sql.Rows) error {
			var r itemRow
			var message, usage []byte
			if err := rows.Scan(&r.id, &r.sessionID, &r.position, &r.typ, &r.agent, &r.implicit,
				&r.subsession, &message, &usage); err != nil {
				return err
			}
			decodeItem(&r, message, usage)
			if r.sessionID != cur {
				cur, prev = r.sessionID, start[r.sessionID]
			}
			// A created_at that does not parse is as good as none.
			if ms, ok := parseStamp(string(r.data.CreatedAt)); ok {
				prev = ms
			}
			r.timeCreated = prev
			out = append(out, r)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseStamp reads one of the store's RFC 3339 stamps as epoch milliseconds,
// the one place the text becomes the time everything else works in. The
// fractional seconds an evaluation item carries parse too.
func parseStamp(text string) (int64, bool) {
	t, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// scanAll drains a query, calling scan once per row, and folds the error
// handling — query failure, scan failure, iteration failure — into one place.
func scanAll(rows *sql.Rows, err error, scan func(*sql.Rows) error) error {
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
