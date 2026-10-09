package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain pins the zone so golden files and rendered timestamps do not
// depend on the machine the tests run on.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

// testDB is a synthetic docker-agent store in a temp dir. Writing to a fixture
// is fine; the repo's read-only rule is about the real store.
type testDB struct {
	t    *testing.T
	path string
	db   *sql.DB
}

func newTestDB(t *testing.T) *testDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		"pragma journal_mode = wal",
		// docker-agent v1.144.0's own schema at migration 030, in SQLite's own
		// text (`.schema` of a store that version created; hence the odd
		// indentation, and the columns ALTER added running on after the rest),
		// so the fixture has the real store's shape, columns the tool never
		// reads included. migrations and generated_media_* are left out: nothing
		// here reads them.
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			created_at TEXT
		, tools_approved BOOLEAN DEFAULT 0, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0, title TEXT DEFAULT '', cost REAL DEFAULT 0, send_user_message BOOLEAN DEFAULT 1, max_iterations INTEGER DEFAULT 0, working_dir TEXT DEFAULT '', starred BOOLEAN DEFAULT 0, permissions TEXT DEFAULT '', agent_model_overrides TEXT DEFAULT '{}', custom_models_used TEXT DEFAULT '[]', thinking BOOLEAN DEFAULT 1, parent_id TEXT REFERENCES sessions(id) ON DELETE CASCADE, instruction_context TEXT DEFAULT '', safety_policy TEXT DEFAULT '', attributes TEXT DEFAULT '{}', origin TEXT NOT NULL DEFAULT 'run')`,
		`CREATE INDEX idx_sessions_parent_id ON sessions(parent_id)`,
		`CREATE TABLE session_items (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					session_id TEXT NOT NULL,
					position INTEGER NOT NULL,
					item_type TEXT NOT NULL,
					agent_name TEXT,
					message_json TEXT,
					implicit BOOLEAN DEFAULT 0,
					subsession_id TEXT,
					summary_text TEXT, first_kept_entry INTEGER DEFAULT 0, cost REAL NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '', usage_json TEXT NOT NULL DEFAULT '',
					FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
					FOREIGN KEY (subsession_id) REFERENCES sessions(id) ON DELETE SET NULL
				)`,
		`CREATE INDEX idx_session_items_session ON session_items(session_id, position)`,
		`CREATE INDEX idx_session_items_session_type ON session_items(session_id, item_type)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return &testDB{t: t, path: path, db: db}
}

// session adds one sessions row. created is store text, such as
// 2026-08-17T15:00:00+05:00; a parent of "" is NULL, as docker-agent writes it.
func (d *testDB) session(id, parent, title, dir, created string) {
	d.t.Helper()
	var p any
	if parent != "" {
		p = parent
	}
	if _, err := d.db.Exec(
		"insert into sessions (id, parent_id, title, working_dir, created_at) values (?,?,?,?,?)",
		id, p, title, dir, created); err != nil {
		d.t.Fatal(err)
	}
}

// item appends one session_items row at the session's next position, as
// docker-agent does. data is its message_json; "" is NULL.
func (d *testDB) item(sid, typ, data string) {
	d.t.Helper()
	var m any
	if data != "" {
		m = data
	}
	if _, err := d.db.Exec(
		`insert into session_items (session_id, position, item_type, message_json)
		   values (?, (select coalesce(max(position), -1) + 1 from session_items where session_id = ?), ?, ?)`,
		sid, sid, typ, m); err != nil {
		d.t.Fatal(err)
	}
}

// subsession appends the row that folds a finished subagent back into its
// parent: no message_json, only the child's id.
func (d *testDB) subsession(sid, child string) {
	d.t.Helper()
	if _, err := d.db.Exec(
		`insert into session_items (session_id, position, item_type, subsession_id)
		   values (?, (select coalesce(max(position), -1) + 1 from session_items where session_id = ?), 'subsession', ?)`,
		sid, sid, child); err != nil {
		d.t.Fatal(err)
	}
}

// summary appends a compaction: its digest, and the summarising call's usage
// in usage_json ("" when it reported none, as docker-agent leaves it).
func (d *testDB) summary(sid, usage string) {
	d.t.Helper()
	if _, err := d.db.Exec(
		`insert into session_items (session_id, position, item_type, summary_text, usage_json)
		   values (?, (select coalesce(max(position), -1) + 1 from session_items where session_id = ?), 'summary', ?, ?)`,
		sid, sid, "## Conversation Summary\nnot for the transcript", usage); err != nil {
		d.t.Fatal(err)
	}
}

// userText appends a user message holding only text.
func (d *testDB) userText(sid, at, text string) {
	d.t.Helper()
	d.item(sid, "message", `{"role":"user","content":`+jsonStr(text)+`,"created_at":"`+at+`"}`)
}

func jsonStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (d *testDB) open(t *testing.T) *store {
	t.Helper()
	st, err := openStore(context.Background(), d.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ms is a store timestamp as the epoch milliseconds the store reads it as.
func ms(t *testing.T, stamp string) int64 {
	t.Helper()
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return at.UnixMilli()
}

// ---------------------------------------------------------------- tests

func TestOpenStoreErrors(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "nope.db")
	if _, err := openStore(ctx, missing); err == nil ||
		err.Error() != fmt.Sprintf("no docker-agent database at %s (try --db)", missing) {
		t.Errorf("missing file: %v", err)
	}
	notdb := filepath.Join(t.TempDir(), "not.db")
	if err := os.WriteFile(notdb, []byte("just text, no sqlite header at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(ctx, notdb); err == nil ||
		!strings.HasPrefix(err.Error(), fmt.Sprintf("cannot open %s read-only:", notdb)) ||
		!strings.Contains(err.Error(), "-wal and -shm") {
		t.Errorf("non-database file: %v", err)
	}
}

// A store older than the columns read has to be refused at open, by name,
// rather than failing later on a missing table or column: before migration 014
// a session was one `messages` column, and session_items had no usage_json
// before 025 (docker-agent v1.113.0).
func TestOpenStoreTooOld(t *testing.T) {
	for _, c := range []struct {
		name  string
		stmts []string
	}{
		{"pre-015", []string{
			`create table sessions (id text primary key, messages text, created_at text)`,
		}},
		{"014 to 024", []string{
			`create table sessions (id text primary key, created_at text, title text default '',
			   working_dir text default '', parent_id text)`,
			`create table session_items (id integer primary key autoincrement,
			   session_id text not null, position integer not null, item_type text not null,
			   agent_name text, message_json text, implicit boolean default 0,
			   subsession_id text, summary_text text, first_kept_entry integer default 0)`,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.db")
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range c.stmts {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			st, err := openStore(context.Background(), path)
			if err == nil {
				_ = st.Close()
				t.Fatal("a too-old database opened")
			}
			if err.Error() != path+" is not a docker-agent v1.113+ database" {
				t.Errorf("too-old database: %v", err)
			}
		})
	}
}

func TestStoreIsReadOnly(t *testing.T) {
	d := newTestDB(t)
	st := d.open(t)
	if _, err := st.db.Exec("insert into sessions (id, working_dir, created_at) values ('x','/x','2026-08-17T10:00:00Z')"); err == nil {
		t.Fatal("write on the read-only handle succeeded")
	}
}

func TestSessionScoping(t *testing.T) {
	d := newTestDB(t)
	// Spelled the way docker-agent records them: absolute, cleaned, and with
	// the separator the scoping matches on.
	root := "/home/u/proj_abc"
	d.session("a", "", "in root", root, "2026-08-17T10:00:01Z")
	d.session("b", "", "in subdir", root+"/deep/er", "2026-08-17T10:00:02Z")
	// One level down: a like prefix ends in "/%", so only a path below
	// projXabc shows _ matching the X.
	d.session("c", "", "wildcard decoy", "/home/u/projXabc/deep", "2026-08-17T10:00:03Z")
	d.session("d", "", "percent decoy", "/home/u/proj%abc", "2026-08-17T10:00:04Z")
	d.session("e", "", "sibling", root+"2", "2026-08-17T10:00:05Z")
	// Differs from the root in case alone: on a case-sensitive filesystem that
	// is somebody else's project, and a case-insensitive match would splice it
	// into this transcript.
	d.session("f", "", "case decoy", "/home/u/Proj_ABC/deep", "2026-08-17T10:00:06Z")
	// Written in the order that leaves the id order to the tiebreaker.
	d.session("z", "", "same second", root, "2026-08-17T10:00:07Z")
	d.session("y", "", "same second", root, "2026-08-17T10:00:07Z")
	// The instant decides, not the text: 10:00+05:00 is 05:00Z, an hour before
	// 06:00Z, though it sorts after it as a string.
	d.session("utc", "", "later", root, "2026-10-08T06:00:00Z")
	d.session("offset", "", "earlier", root, "2026-10-08T10:00:00+05:00")
	// Subagents docker-agent wrote before v1.138 have no directory of their
	// own: they take their nearest ancestor's, down a chain of them too, and
	// one whose ancestor is out of scope stays out with it.
	d.session("kid", "a", "old subagent", "", "2026-08-17T10:00:08Z")
	d.session("grandkid", "kid", "its own subagent", "", "2026-08-17T10:00:09Z")
	d.session("decoykid", "c", "decoy's subagent", "", "2026-08-17T10:00:10Z")
	// A top-level session left without one on purpose (a remote or headless
	// run) is under no root, and its directory reads as "".
	d.session("nodir", "", "headless", "", "2026-08-17T10:00:11Z")
	st := d.open(t)
	ctx := context.Background()

	rows, err := st.sessions(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.id)
	}
	if want := []string{"a", "b", "y", "z", "kid", "grandkid", "offset", "utc"}; !slices.Equal(got, want) {
		t.Errorf("scoped ids = %v, want %v", got, want)
	}
	for _, r := range rows {
		if (r.id == "kid" || r.id == "grandkid") && r.directory != root {
			t.Errorf("%s: directory = %q, want the inherited %q", r.id, r.directory, root)
		}
	}

	all, err := st.sessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 14 {
		t.Errorf("unscoped rows = %d, want 14", len(all))
	}
	for _, r := range all {
		if r.id == "nodir" && r.directory != "" {
			t.Errorf("nodir: directory = %q, want none", r.directory)
		}
	}
	// Time order, not id order.
	if all[0].id != "a" || all[4].id != "e" {
		t.Errorf("unscoped order = %v", all)
	}
}

// TestItemsChunked is the regression test for the unchunked IN list: 1200
// sessions exceed the pre-3.32 variable limit of 999, and the chunks' results
// put end to end must be exactly the order one statement would have given —
// session id, then position, never item id.
func TestItemsChunked(t *testing.T) {
	d := newTestDB(t)
	tx, err := d.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	const n = 1200
	start := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	for i := range n {
		sid := fmt.Sprintf("s%04d", i)
		// Start times run opposite to the ids, so the sessions reach items() in
		// the reverse of id order: a chunk taken before sorting would hand the
		// last ids back first.
		created := start.Add(time.Duration(n-i) * time.Second).Format(time.RFC3339)
		if _, err := tx.Exec("insert into sessions (id, working_dir, created_at) values (?,?,?)",
			sid, "/p", created); err != nil {
			t.Fatal(err)
		}
		// Inserted last position first, so item ids run against positions.
		for _, pos := range []int{2, 1, 0} {
			if _, err := tx.Exec("insert into session_items (session_id, position, item_type, message_json) values (?,?,?,?)",
				sid, pos, "message", `{"role":"user","content":"x"}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	st := d.open(t)
	ctx := context.Background()
	rows, err := st.sessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].id != fmt.Sprintf("s%04d", n-1) {
		t.Fatalf("scope order starts at %s, want the latest id first", rows[0].id)
	}
	got, err := st.items(ctx, tagSessions(rows))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3*n {
		t.Fatalf("items = %d, want %d", len(got), 3*n)
	}
	for i, it := range got {
		sid, pos := fmt.Sprintf("s%04d", i/3), int64(i%3)
		if it.sessionID != sid || it.position != pos {
			t.Fatalf("row %d = (%s, %d), want (%s, %d)", i, it.sessionID, it.position, sid, pos)
		}
		if pos > 0 && it.id >= got[i-1].id {
			t.Fatalf("row %d: id %d after %d, want position order, not id order", i, it.id, got[i-1].id)
		}
	}
}

// An item without a timestamp of its own takes the one before it in its
// session, and the first takes the session's start; one whose created_at
// does not parse counts as without.
func TestItemsCarryTime(t *testing.T) {
	d := newTestDB(t)
	start, t1, t2 := "2026-10-08T09:00:00+05:00", "2026-10-08T09:30:00+05:00", "2026-10-08T09:45:00+05:00"
	// An evaluation's stamp, written with nanoseconds as docker-agent does.
	t3 := "2026-10-08T10:00:00.123456789+05:00"
	d.session("a", "", "", "/p", start)
	d.session("b", "", "", "/p", "2026-10-08T08:00:00+05:00")
	d.session("kid", "a", "", "/p", t1)
	d.summary("a", "")
	d.userText("a", t1, "hi")
	d.item("a", "message", `{"role":"assistant","content":"cut off"}`)
	d.subsession("a", "kid")
	d.item("a", "error", `{"message":"boom","created_at":"not a time"}`)
	d.item("a", "message", `{"role":"tool","tool_call_id":"c1","content":"ok","created_at":"`+t2+`"}`)
	d.item("a", "evaluation", `{"id":"e1","evaluator":"judge","created_at":"`+t3+`"}`)
	// The next session starts again from its own start, not from a's last.
	d.item("b", "message", `{"role":"assistant","content":"cut off"}`)
	st := d.open(t)
	ctx := context.Background()
	rows, err := st.sessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.items(ctx, tagSessions(rows))
	if err != nil {
		t.Fatal(err)
	}
	var times []int64
	for _, it := range got {
		times = append(times, it.timeCreated)
	}
	want := []int64{ms(t, start), ms(t, t1), ms(t, t1), ms(t, t1), ms(t, t1), ms(t, t2), ms(t, t3),
		ms(t, "2026-10-08T08:00:00+05:00")}
	if !slices.Equal(times, want) {
		t.Errorf("times = %v, want %v", times, want)
	}
	if want := time.Date(2026, 10, 8, 5, 0, 0, 123e6, time.UTC).UnixMilli(); times[6] != want {
		t.Errorf("evaluation time = %d, want %d, to the millisecond", times[6], want)
	}
}

// An item type this version does not know is read like any other, named, and
// keeps its own time; the items around it are untouched.
func TestItemsUnknownType(t *testing.T) {
	d := newTestDB(t)
	d.session("a", "", "", "/p", "2026-10-08T09:00:00+05:00")
	d.userText("a", "2026-10-08T09:01:00+05:00", "before")
	d.item("a", "mystery", `{"created_at":"2026-10-08T09:02:00+05:00","shape":["new"]}`)
	d.userText("a", "2026-10-08T09:03:00+05:00", "after")
	st := d.open(t)
	ctx := context.Background()
	rows, err := st.sessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.items(ctx, tagSessions(rows))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("items = %d, want 3", len(got))
	}
	if got[1].unknown != "mystery" || got[1].timeCreated != ms(t, "2026-10-08T09:02:00+05:00") {
		t.Errorf("mystery item = %q at %d", got[1].unknown, got[1].timeCreated)
	}
	for _, i := range []int{0, 2} {
		if got[i].unknown != "" || got[i].data.Role != "user" {
			t.Errorf("item %d = %q, role %q, want a plain user message", i, got[i].unknown, got[i].data.Role)
		}
	}
	if got[2].timeCreated != ms(t, "2026-10-08T09:03:00+05:00") {
		t.Errorf("item after the mystery at %d", got[2].timeCreated)
	}
}

// Message rows are what docker-agent itself counts: prompts, a subagent's
// task, model steps and tool results. Subagent markers, summaries, errors and
// evaluator calls are not messages.
func TestMessageCounts(t *testing.T) {
	d := newTestDB(t)
	d.session("a", "", "", "/p", "2026-10-08T09:00:00+05:00")
	d.session("b", "", "", "/p", "2026-10-08T09:00:01+05:00")
	d.session("kid", "a", "", "/p", "2026-10-08T09:00:02+05:00")
	for _, role := range []string{"system", "user", "assistant", "tool"} {
		d.item("a", "message", `{"role":"`+role+`"}`)
	}
	d.subsession("a", "kid")
	d.summary("a", "")
	d.item("a", "error", `{"message":"boom"}`)
	d.item("a", "evaluation", `{"id":"e1"}`)
	d.summary("b", "")
	got, err := d.open(t).messageCounts(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != 4 || got["b"] != 0 {
		t.Errorf("counts = %v, want a 4 and b 0", got)
	}
}
