package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func parse(t *testing.T, args ...string) (*options, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	return parseArgs(args, &stdout, &stderr)
}

// Bad flag syntax exits 2; bad values inside an accepted flag exit 1.
func isUsage(err error) bool { return errors.As(err, new(usageError)) }

func TestParseArgsEnums(t *testing.T) {
	for _, args := range [][]string{
		{"--tools", "sideways"},
		{"--color", "sometimes"},
		{"--tools", "FULL"},
	} {
		_, err := parse(t, args...)
		if err == nil || !isUsage(err) || !strings.Contains(err.Error(), "must be one of") {
			t.Errorf("parse(%v) = %v, want a usage error naming the choices", args, err)
		}
	}
	opts, err := parse(t, "--tools", "none", "--color", "never")
	if err != nil || opts.tools != "none" || opts.color != "never" {
		t.Errorf("valid enums: %v %+v", err, opts)
	}
}

func TestParseArgsUnknown(t *testing.T) {
	if _, err := parse(t, "--nope"); err == nil || !isUsage(err) {
		t.Errorf("unknown flag: %v", err)
	}
	// No argparse-style abbreviation: --everyw is unknown, not --everywhere.
	if _, err := parse(t, "--everyw"); err == nil || !isUsage(err) {
		t.Errorf("abbreviated flag: %v", err)
	}
	// -tools must not parse as a shorthand cluster.
	if _, err := parse(t, "-tools", "full"); err == nil || !isUsage(err) {
		t.Errorf("-tools: %v", err)
	}
	if _, err := parse(t, "stray"); err == nil || !isUsage(err) ||
		!strings.Contains(err.Error(), "unrecognized arguments: stray") {
		t.Errorf("positional: %v", err)
	}
}

func TestParseArgsSessions(t *testing.T) {
	opts, err := parse(t, "--session", "x", "--session", "y")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"x", "y"}; strings.Join(opts.sessions, ",") != strings.Join(want, ",") {
		t.Errorf("sessions = %v", opts.sessions)
	}
}

func TestParseArgsValidation(t *testing.T) {
	if _, err := parse(t, "--interval", "-1"); err == nil || isUsage(err) ||
		err.Error() != "--interval wants 0.1 to 3600 seconds, got -1" {
		t.Errorf("negative interval: %v", err)
	}
	// NaN, Inf, 1e300 and 1e-10 once got past the check and panicked --follow's
	// ticker, and 1e-6 spun it; the floor is a tenth of a second, the ceiling
	// an hour.
	for _, v := range []string{"0", "0.05", "1e-6", "1e-10", "NaN", "Inf", "-Inf", "3601", "1e300"} {
		if _, err := parse(t, "--interval", v); err == nil {
			t.Errorf("--interval %s accepted", v)
		}
	}
	for v, want := range map[string]time.Duration{"0.1": 100 * time.Millisecond, "2": 2 * time.Second, "3600": time.Hour} {
		if opts, err := parse(t, "--interval", v); err != nil || opts.interval != want {
			t.Errorf("--interval %s: %v %+v", v, err, opts)
		}
	}
	if _, err := parse(t, "--list", "--follow"); err == nil ||
		err.Error() != "--follow cannot be combined with --list" {
		t.Errorf("--list --follow: %v", err)
	}
	if _, err := parse(t, "--until", "12:00", "--follow"); err == nil ||
		err.Error() != "--follow cannot be combined with --until" {
		t.Errorf("--until --follow: %v", err)
	}
	// -f is --follow: the two flags with a shorthand are -f and -o.
	if opts, err := parse(t, "-f"); err != nil || !opts.follow {
		t.Errorf("-f: %v %+v", err, opts)
	}
	if _, err := parse(t, "--since", "nonsense"); err == nil ||
		!strings.HasPrefix(err.Error(), "cannot parse time \"nonsense\"") {
		t.Errorf("bad since: %v", err)
	}
	// The window is [since, until), so one that ends where it starts or
	// earlier is empty whatever the store holds. A bare date as --until runs
	// to the end of that day, so the same date on both sides is a whole day.
	for _, tc := range []struct{ since, until, want string }{
		{"2026-10-10", "2026-10-09", "--since 2026-10-10 00:00:00 is not before --until 2026-10-10 00:00:00"},
		{"2026-10-10 12:00", "2026-10-10 12:00", "--since 2026-10-10 12:00:00 is not before --until 2026-10-10 12:00:00"},
		{"2026-10-11", "2026-10-01", "--since 2026-10-11 00:00:00 is not before --until 2026-10-02 00:00:00"},
	} {
		if _, err := parse(t, "--since", tc.since, "--until", tc.until); err == nil || isUsage(err) ||
			err.Error() != tc.want {
			t.Errorf("--since %s --until %s: %v", tc.since, tc.until, err)
		}
	}
	if _, err := parse(t, "--since", "today", "--until", "yesterday"); err == nil ||
		!strings.Contains(err.Error(), "is not before --until") {
		t.Errorf("--since today --until yesterday: %v", err)
	}
	// --until alone meets the default --since, which the error names as the
	// default rather than as something typed. The clock may tick a second
	// during the call, so either side of it is the exact message.
	before := time.Now()
	_, err := parse(t, "--until", "2026-10-01")
	after := time.Now()
	want := func(now time.Time) string {
		return "--since (default: 24h) " + stamp(now.Add(-24*time.Hour).UnixMilli()) +
			" is not before --until 2026-10-02 00:00:00 (try --since)"
	}
	if err == nil || isUsage(err) || (err.Error() != want(before) && err.Error() != want(after)) {
		t.Errorf("--until with the default --since: %v, want %q", err, want(before))
	}
	for _, w := range [][2]string{
		{"2026-10-10", "2026-10-10"},
		{"20261010", "20261010"},
		{"2026-10-10 12:00", "2026-10-10 12:00:01"},
		{"yesterday", "yesterday"},
	} {
		if _, err := parse(t, "--since", w[0], "--until", w[1]); err != nil {
			t.Errorf("--since %s --until %s: %v", w[0], w[1], err)
		}
	}
}

// COLUMNS is held to 60..1000, for a file as for stdout: a huge one used to
// panic in strings.Repeat drawing a session banner's rule.
func TestWidthBounds(t *testing.T) {
	out := filepath.Join(t.TempDir(), "t.md")
	for _, c := range []struct {
		columns string
		want    int
	}{{"10", 60}, {"80", 80}, {"1000", 1000}, {"1001", 1000}, {"4611686018427387904", 1000}} {
		t.Setenv("COLUMNS", c.columns)
		for _, args := range [][]string{nil, {"-o", out}} {
			opts, err := parse(t, args...)
			if err != nil {
				t.Fatalf("COLUMNS=%s %v: %v", c.columns, args, err)
			}
			if opts.width != c.want {
				t.Errorf("COLUMNS=%s %v: width %d, want %d", c.columns, args, opts.width, c.want)
			}
		}
	}

	d := newTestDB(t)
	d.session("a", "", "t", "/p", "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hello")
	t.Setenv("COLUMNS", "4611686018427387904")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--db", d.path, "--everywhere", "--all"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Errorf("huge COLUMNS: transcript lacks the message:\n%s", stdout.String())
	}
}

func TestParseArgsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, err := parseArgs([]string{"--help"}, &stdout, &stderr)
	if !errors.Is(err, pflag.ErrHelp) {
		t.Fatalf("--help: %v", err)
	}
	for _, want := range []string{"Usage: da-transcript", "--everywhere", "2h, 30m, 3d, today, yesterday",
		"-f, --follow", "-o, --out"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("usage text lacks %q", want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("--help wrote to stderr: %q", stderr.String())
	}
	// The help is hand-grouped, so this is what keeps it honest: every flag
	// parseArgs defines must be named in it.
	for _, name := range []string{"root", "everywhere", "db", "since", "until", "all",
		"session", "tools", "reasoning", "stats",
		"max-chars", "arg-width", "color", "list", "follow", "interval", "out", "version"} {
		if !strings.Contains(stdout.String(), "--"+name) {
			t.Errorf("usage text lacks --%s", name)
		}
	}
}

// Usage triggered by a bad flag belongs on stderr; stdout stays clean.
func TestUsageOnErrorGoesToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, err := parseArgs([]string{"--nope"}, &stdout, &stderr)
	if err == nil || !isUsage(err) {
		t.Fatalf("--nope: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("bad flag wrote to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Usage: da-transcript") {
		t.Errorf("bad flag did not print usage to stderr: %q", stderr.String())
	}
}

func TestParseArgsVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, err := parseArgs([]string{"--version"}, &stdout, &stderr)
	if !errors.Is(err, errVersionPrinted) {
		t.Fatalf("--version: %v", err)
	}
	if got := stdout.String(); !strings.HasPrefix(got, "da-transcript ") ||
		strings.TrimSpace(got) == "da-transcript" {
		t.Errorf("--version printed %q, want the name and a version", got)
	}
	// It answers whatever else is on the line, as --help does.
	if _, err := parse(t, "--version", "stray"); !errors.Is(err, errVersionPrinted) {
		t.Errorf("--version with a positional: %v", err)
	}
}

func TestRunErrors(t *testing.T) {
	t.Setenv("COLUMNS", "")
	var stdout, stderr bytes.Buffer

	missing := filepath.Join(t.TempDir(), "no.db")
	err := run([]string{"--db", missing}, &stdout, &stderr)
	if err == nil || isUsage(err) ||
		err.Error() != fmt.Sprintf("no docker-agent database at %s (try --db)", missing) {
		t.Errorf("missing db: %v", err)
	}

	d := newTestDB(t)
	d.session("a", "", "t", "/p", "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hello")
	err = run([]string{"--db", d.path, "--everywhere", "--all", "-o", "/proc/nope/x.md"}, &stdout, &stderr)
	if err == nil || !strings.HasPrefix(err.Error(), "cannot write /proc/nope/x.md:") {
		t.Errorf("unwritable out: %v", err)
	}
}

// cancelOnWrite is a sink that interrupts the run as the transcript reaches it.
type cancelOnWrite struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	w.cancel()
	return w.Buffer.Write(p)
}

// Ctrl-C or SIGTERM stops a run without a word: whatever the query it cut
// short reported, run returns errInterrupted, which Main exits 130 on. Once
// --follow is tailing, the signal is how the tail ends, and that is no error.
func TestRunInterrupted(t *testing.T) {
	t.Setenv("COLUMNS", "")
	d := newTestDB(t)
	d.session("a", "", "t", "/p", "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hello")
	t.Cleanup(func() { notifyContext = signal.NotifyContext })

	notifyContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, cancel
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"--db", d.path, "--everywhere", "--all"}, &stdout, &stderr)
	if !errors.Is(err, errInterrupted) || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("one-shot: err %v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}

	sink := &cancelOnWrite{}
	notifyContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		sink.cancel = cancel
		return ctx, cancel
	}
	stderr.Reset()
	err = run([]string{"--db", d.path, "--everywhere", "--all", "--follow"}, sink, &stderr)
	if err != nil || !strings.Contains(sink.String(), "hello") || stderr.Len() != 0 {
		t.Errorf("--follow: err %v, stdout %q, stderr %q", err, sink.String(), stderr.String())
	}
}

func TestRunEmptyMessages(t *testing.T) {
	t.Setenv("COLUMNS", "")
	d := newTestDB(t)
	// The reason a transcript is empty is an answer about the query, not
	// transcript content: it goes to stderr, and stdout stays clean.
	out := func(args ...string) string {
		var stdout, stderr bytes.Buffer
		if err := run(append([]string{"--db", d.path}, args...), &stdout, &stderr); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("run(%v) wrote to stdout: %q", args, stdout.String())
		}
		return stderr.String()
	}

	if got := out("--everywhere"); !strings.Contains(got, "This database has no sessions at all.") {
		t.Errorf("no sessions: %q", got)
	}

	d.session("a", "", "t", "/somewhere/else", "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hi")
	// The root is named the way it was given, made absolute and no more.
	if got := out("--root", "/not/there"); !strings.Contains(got,
		"No docker-agent session has ever run under /not/there — try --root or --everywhere.") {
		t.Errorf("wrong root: %q", got)
	}
	if got := out("--everywhere", "--all", "--session", "nope"); !strings.Contains(got,
		"1 session(s) in scope, none matching --session.") {
		t.Errorf("filtered out: %q", got)
	}
	if got := out("--everywhere", "--since", "2100-01-01"); !strings.Contains(got,
		"Nothing from 1 session(s) in this window — try --all.") {
		t.Errorf("empty window: %q", got)
	}
	if got := out("--everywhere", "--since", "2100-01-01", "--list"); !strings.Contains(got,
		"Nothing from 1 session(s) in this window") {
		t.Errorf("empty list: %q", got)
	}
	// With --all the hint would be useless, so it goes — here everything in
	// scope is a model step with no content, which renders nothing.
	d2 := newTestDB(t)
	d2.session("b", "", "t", "/p", "2026-08-17T10:00:00Z")
	d2.message("b", "root", `{"role":"assistant","content":"","finish_reason":"stop",
	  "created_at":"2026-08-17T10:00:00Z"}`)
	var so, se bytes.Buffer
	if err := run([]string{"--db", d2.path, "--everywhere", "--all"}, &so, &se); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(se.String(), "Nothing from 1 session(s) in this window.\n") ||
		strings.Contains(se.String(), "try --all") {
		t.Errorf("--all drops the hint: %q", se.String())
	}
	// --list rows are the sessions the transcript would show, so a session
	// with an item in the window but no block drawn there is not one of them.
	so.Reset()
	se.Reset()
	if err := run([]string{"--db", d2.path, "--everywhere", "--all", "--list"}, &so, &se); err != nil {
		t.Fatal(err)
	}
	if so.Len() != 0 || !strings.Contains(se.String(), "Nothing from 1 session(s) in this window.\n") {
		t.Errorf("--list with nothing drawn: stdout %q, stderr %q", so.String(), se.String())
	}
}

func TestRunWritesFile(t *testing.T) {
	t.Setenv("COLUMNS", "")
	d := newTestDB(t)
	d.session("a", "", "t", "/p", "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hello file")

	path := filepath.Join(t.TempDir(), "out.md")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--db", d.path, "--everywhere", "--all", "-o", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout not empty with --out: %q", stdout.String())
	}
	if got := stderr.String(); got != "wrote "+path+"\n" {
		t.Errorf("stderr = %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello file") || strings.Contains(string(data), "\033[") {
		t.Errorf("file content: %q", data)
	}
}

// A transcript can carry secrets the tools saw, so the file --out makes is
// private however permissive the umask.
func TestOutIsPrivate(t *testing.T) {
	t.Setenv("COLUMNS", "")
	defer syscall.Umask(syscall.Umask(0o022))
	d := newTestDB(t)
	d.session("a", "", "t", "/p", "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hello file")

	path := filepath.Join(t.TempDir(), "out.md")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--db", d.path, "--everywhere", "--all", "-o", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("-o made the file %#o, want 0600", got)
	}
}

// --out must never land on the store being read, under any of its names: the
// database, its -wal and -shm, or a symlink to one of them.
func TestOutSparesTheStore(t *testing.T) {
	t.Setenv("COLUMNS", "")
	d := newTestDB(t)
	d.session("a", "", "t", "/p", "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hello file")
	link := filepath.Join(t.TempDir(), "out.md")
	if err := os.Symlink(d.path, link); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{d.path, d.path + "-wal", d.path + "-shm", link} {
		before, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		err = run([]string{"--db", d.path, "--everywhere", "--all", "-o", out}, &stdout, &stderr)
		if err == nil || err.Error() != "cannot write "+out+": it is part of the database being read" {
			t.Errorf("-o %s: %v", out, err)
		}
		if after, err := os.Stat(out); err != nil {
			t.Error(err)
		} else if after.Size() != before.Size() {
			t.Errorf("-o %s: %d bytes before, %d after", out, before.Size(), after.Size())
		}
	}
}

// An empty result writes nothing, so --out leaves the file system as it found
// it: no file made, none truncated, no "wrote" — and no refusal of a path into
// the store, since nothing would land there. A tail still opens its file, as
// what it finds goes there, but has written nothing to report.
func TestOutLeftAloneWhenEmpty(t *testing.T) {
	t.Setenv("COLUMNS", "")
	d := newTestDB(t)
	d.session("a", "", "t", "/p", "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hello file")
	const empty = "Nothing from 1 session(s) in this window — try --all.\n"
	base := []string{"--db", d.path, "--everywhere", "--since", "2100-01-01"}

	dir := t.TempDir()
	kept, absent := filepath.Join(dir, "kept.md"), filepath.Join(dir, "absent.md")
	if err := os.WriteFile(kept, []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-o", kept}, {"-o", absent}, {"--list", "-o", kept}, {"-o", d.path}} {
		var stdout, stderr bytes.Buffer
		err := run(append(base, args...), &stdout, &stderr)
		if err != nil || stdout.Len() != 0 || stderr.String() != empty {
			t.Errorf("%v: err %v, stdout %q, stderr %q", args, err, stdout.String(), stderr.String())
		}
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "precious\n" {
		t.Errorf("existing file: %q, %v", data, err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file made for nothing: %v", err)
	}

	// The empty message on stderr is what interrupts the tail.
	t.Cleanup(func() { notifyContext = signal.NotifyContext })
	stderr := &cancelOnWrite{}
	notifyContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		stderr.cancel = cancel
		return ctx, cancel
	}
	var stdout bytes.Buffer
	err := run(append(base, "--follow", "-o", kept), &stdout, stderr)
	if err != nil || stdout.Len() != 0 || stderr.String() != empty {
		t.Errorf("--follow: err %v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	if data, err := os.ReadFile(kept); err != nil || len(data) != 0 {
		t.Errorf("--follow file: %q, %v", data, err)
	}
}

// docker-agent records the directory as the shell saw it, symlinks and all,
// so a root reached through a symlink has to be compared the same way: as
// given, not resolved.
func TestRootKeepsSymlinks(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if opts, err := parse(t, "--root", link); err != nil || opts.root != link {
		t.Fatalf("--root %s parsed as %v, %v", link, opts, err)
	}
	d := newTestDB(t)
	d.session("a", "", "Through the link", link, "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hi")
	t.Setenv("COLUMNS", "")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--db", d.path, "--list", "--all", "--root", link}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Through the link") {
		t.Errorf("session under the symlinked root not listed:\n%s%s", stdout.String(), stderr.String())
	}
}

// A root of "" means the whole database, so a working directory that has been
// removed must not fall through to it.
func TestRootWithoutWorkingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Skipf("cannot remove the working directory here: %v", err)
	}
	if _, err := parse(t); err == nil || isUsage(err) ||
		!strings.HasPrefix(err.Error(), "cannot determine the working directory: ") ||
		!strings.HasSuffix(err.Error(), " (try --root or --everywhere)") {
		t.Errorf("no working directory: %v", err)
	}
	if opts, err := parse(t, "--everywhere"); err != nil || opts.root != "" {
		t.Errorf("--everywhere: %v %+v", err, opts)
	}
	if opts, err := parse(t, "--root", "/elsewhere"); err != nil || opts.root != "/elsewhere" {
		t.Errorf("--root: %v %+v", err, opts)
	}
}

// A directory can be called anything, the working directory included, and so
// can the file --out names: where one is echoed — the summary line, the empty
// result, the "wrote" line — it is shown, not obeyed, and on the one line.
func TestUserPathsNeutralised(t *testing.T) {
	t.Setenv("COLUMNS", "")
	root := "/p/x\x1b]0;pwned\x07\ty"
	d := newTestDB(t)
	d.session("a", "", "t", root, "2026-08-17T10:00:00Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hi")
	run := func(args ...string) (string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := run(append([]string{"--db", d.path, "--all"}, args...), &stdout, &stderr); err != nil {
			t.Fatalf("run(%q): %v", args, err)
		}
		return stdout.String(), stderr.String()
	}

	if stdout, _ := run("--root", root); strings.Contains(stdout, "\x1b") ||
		!strings.HasPrefix(stdout, "x␛]0;pwned␇␉y  all time  1 session(s)\n") {
		t.Errorf("summary line:\n%q", stdout)
	}
	if _, stderr := run("--root", "/q/z\x1b]0;pwned\x07\nforged"); stderr !=
		"No docker-agent session has ever run under /q/z␛]0;pwned␇␊forged — try --root or --everywhere.\n" {
		t.Errorf("empty result: %q", stderr)
	}
	dir := t.TempDir()
	if _, stderr := run("--root", root, "-o", filepath.Join(dir, "out\x1b]0;pwned\x07.md")); stderr !=
		"wrote "+filepath.Join(dir, "out␛]0;pwned␇.md")+"\n" {
		t.Errorf("wrote: %q", stderr)
	}
}

// A tag copied out of --list selects its session, the subagent mark included.
func TestSessionTakesListedTag(t *testing.T) {
	d := zooDB(t)
	t.Setenv("COLUMNS", "")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--db", d.path, "--everywhere", "--all", "--list", "--session", "zoo003↓"},
		&stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "zoo003↓") || strings.Contains(out, "zoo001") ||
		!strings.Contains(out, "1 session since") {
		t.Errorf("--session zoo003↓ listed:\n%s%s", out, stderr.String())
	}
}
