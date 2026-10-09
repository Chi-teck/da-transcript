// Package app is da-transcript itself: the flags, the store, the rendering and
// the tail. The root main.go is a shim over Main below, and carries the
// command's own documentation, where `go doc` looks for it.
package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// ---------------------------------------------------------------- options

type options struct {
	root         string // "" means every directory (--everywhere)
	db           string
	since, until *int64 // epoch ms; nil is open-ended
	all          bool
	sessions     []string // id fragments; nil keeps every session
	tools        string
	reasoning    bool
	stats        bool
	maxChars     int
	argWidth     int
	color        string
	list         bool
	follow       bool
	interval     float64
	out          string

	paint  Paint
	width  int
	stderr io.Writer
}

// enumFlag is a pflag.Value that only takes one of a fixed set of words.
type enumFlag struct {
	value   string
	allowed []string
}

func (e *enumFlag) String() string { return e.value }
func (e *enumFlag) Type() string   { return "{" + strings.Join(e.allowed, ",") + "}" }
func (e *enumFlag) Set(v string) error {
	if !slices.Contains(e.allowed, v) {
		return fmt.Errorf("must be one of %s", strings.Join(e.allowed, ", "))
	}
	e.value = v
	return nil
}

// usageError is a bad command line: exit status 2, as argparse does.
type usageError struct{ err error }

func (u usageError) Error() string { return u.err.Error() }
func (u usageError) Unwrap() error { return u.err }

// defaultDB is where docker-agent keeps its store: <home>/.cagent/session.db
// on every OS, or under the temp dir when there is no home to find —
// pkg/paths.GetDataDir and cmd/root/flags.go:sessionDBPath.
func defaultDB() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".cagent", "session.db")
}

// expandUser is Path.expanduser: `--root=~/x` hands the tilde over literally —
// the shell only expands one at the start of a word — so a path that arrives
// here still has to be expanded.
func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// resolvePath is Path.resolve(strict=False): absolute, symlinks followed as
// far as the path exists.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	// Resolve the longest existing prefix and append the rest untouched.
	rest := ""
	dir := abs
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
	}
}

func parseArgs(args []string, stdout, stderr io.Writer) (*options, error) {
	opts := &options{stderr: stderr}
	fs := pflag.NewFlagSet("da-transcript", pflag.ContinueOnError)
	fs.SortFlags = false
	// pflag would print the error and usage itself; both are printed below
	// instead, on the stream the occasion calls for.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	var (
		root, since, until      string
		everywhere, showVersion bool
		tools                   = &enumFlag{value: "compact", allowed: []string{"compact", "full", "none"}}
		color                   = &enumFlag{value: "auto", allowed: []string{"auto", "always", "never"}}
	)
	// cwd, the way git and friends scope themselves — not somewhere relative to
	// wherever this file happens to be installed.
	fs.StringVar(&root, "root", "", "project root (default: the working directory)")
	fs.BoolVar(&everywhere, "everywhere", false, "every session in the database, whatever directory it ran in (ignores --root)")
	fs.StringVar(&opts.db, "db", defaultDB(), "docker-agent database")
	fs.StringVar(&since, "since", "24h", "start of the window")
	fs.StringVar(&until, "until", "", "end of the window")
	fs.BoolVar(&opts.all, "all", false, "ignore --since/--until")
	fs.StringArrayVar(&opts.sessions, "session", nil, "only sessions whose id contains this (repeatable)")
	fs.Var(tools, "tools", "tool call detail")
	fs.BoolVar(&opts.reasoning, "reasoning", false, "include the model's reasoning blocks")
	fs.BoolVar(&opts.stats, "stats", false, "include per-step token and cost lines")
	fs.IntVar(&opts.maxChars, "max-chars", 2000, "cap on a quoted block")
	fs.IntVar(&opts.argWidth, "arg-width", 100, "cap on a one-line tool summary")
	fs.Var(color, "color", "colour sessions in the terminal (auto: on for a tty, off for a pipe or --out)")
	fs.BoolVar(&opts.list, "list", false, "list matching sessions instead of the transcript, with the span, message count and token total of each")
	fs.BoolVarP(&opts.follow, "follow", "f", false, "keep printing new turns as they complete")
	fs.Float64Var(&opts.interval, "interval", 2.0, "--follow poll interval in seconds")
	fs.StringVarP(&opts.out, "out", "o", "", "write to a file instead of stdout")
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, usage(max(60, terminalWidth(stdout, 100))))
			return nil, err
		}
		// A bad flag is a usage problem: the usage goes to stderr, and main
		// prints the error line under it.
		_, _ = fmt.Fprint(stderr, usage(max(60, terminalWidth(stderr, 100))))
		return nil, usageError{err}
	}
	// --version, like --help, answers and stops; the rest of the line is moot.
	if showVersion {
		_, _ = fmt.Fprintf(stdout, "da-transcript %s\n", buildVersion())
		return nil, errVersionPrinted
	}
	if rest := fs.Args(); len(rest) > 0 {
		return nil, usageError{fmt.Errorf("unrecognized arguments: %s", strings.Join(rest, " "))}
	}
	opts.tools, opts.color = tools.value, color.value

	// A root of "" is how the rest of this asks for the whole database.
	if !everywhere {
		if root == "" {
			root, _ = os.Getwd()
		}
		opts.root = resolvePath(expandUser(root))
	}
	opts.db = expandUser(opts.db)
	opts.out = expandUser(opts.out)
	if opts.interval <= 0 {
		return nil, fmt.Errorf("--interval wants a positive number of seconds, got %v", opts.interval)
	}
	if opts.list && opts.follow {
		return nil, errors.New("--follow cannot be combined with --list")
	}
	// A tail has no end, so an end to the window is a contradiction rather
	// than a bound the poll could honour.
	if opts.follow && until != "" {
		return nil, errors.New("--follow cannot be combined with --until")
	}

	now := time.Now()
	if !opts.all {
		ms, err := parseWhen(since, false, now)
		if err != nil {
			return nil, err
		}
		opts.since = &ms
		if until != "" {
			ms, err := parseWhen(until, true, now)
			if err != nil {
				return nil, err
			}
			opts.until = &ms
		}
	}

	// Writing to a file means the reader is not a terminal, whatever stdout is —
	// for colour, and for the width the right margin is measured against.
	mode := opts.color
	if opts.out != "" && mode == "auto" {
		mode = "never"
	}
	opts.paint = Paint{enabled: wantColor(mode, stdout)}
	if opts.out != "" {
		// A file has no terminal; COLUMNS still overrides the default.
		opts.width = max(60, terminalWidth(nil, 100))
	} else {
		opts.width = max(60, terminalWidth(stdout, 100))
	}
	return opts, nil
}

// usage is the --help text: flags grouped by what they do, descriptions
// wrapped to the terminal. Hand-laid rather than pflag's single block; a test
// checks every defined flag appears in it.
func usage(width int) string {
	type flagHelp struct{ spec, desc string }
	groups := []struct {
		title string
		flags []flagHelp
	}{
		{"Selecting sessions", []flagHelp{
			{"--root PATH", "project root (default: the working directory)"},
			{"--everywhere", "every session in the database, whatever directory it ran in (ignores --root)"},
			{"--db FILE", "docker-agent database (default: ~/.cagent/session.db)"},
			{"--session ID", "only sessions whose id contains this (repeatable)"},
		}},
		{"Time window", []flagHelp{
			{"--since SPEC", "start of the window: 2h, 30m, 3d, today, yesterday or an ISO timestamp (default: 24h)"},
			{"--until SPEC", "end of the window, same forms (default: now)"},
			{"--all", "ignore --since/--until"},
		}},
		{"Output", []flagHelp{
			{"--tools MODE", "tool call detail: compact, full or none (default: compact)"},
			{"--reasoning", "include the model's reasoning blocks"},
			{"--stats", "include per-step token and cost lines"},
			{"--max-chars N", "cap on a quoted block (default: 2000)"},
			{"--arg-width N", "cap on a one-line tool summary (default: 100)"},
			{"--color WHEN", "auto, always or never (default: auto — on for a terminal, off for a pipe or --out)"},
			{"--list", "list matching sessions instead of the transcript, with the span, message count and token total of each"},
			{"-o, --out FILE", "write to a file instead of stdout"},
		}},
		{"Following", []flagHelp{
			{"-f, --follow", "keep printing new turns as they complete"},
			{"--interval SECONDS", "--follow poll interval (default: 2)"},
		}},
		{"", []flagHelp{
			{"--version", "print the version and exit"},
			{"--help", "print this help"},
		}},
	}
	left := func(spec string) string {
		if strings.HasPrefix(spec, "--") {
			return "      " + spec
		}
		return "  " + spec // carries a shorthand: "-o, --out FILE"
	}
	col := 0
	for _, g := range groups {
		for _, f := range g.flags {
			col = max(col, cells(left(f.spec)))
		}
	}
	col += 2

	var b strings.Builder
	b.WriteString("Usage: da-transcript [flags]\n\n")
	b.WriteString("Merge every docker-agent session for a project into one chronological transcript.\n")
	for _, g := range groups {
		b.WriteString("\n")
		if g.title != "" {
			b.WriteString(g.title + ":\n")
		}
		for _, f := range g.flags {
			for i, line := range wrap("", f.desc, max(20, width-col)) {
				if i == 0 {
					b.WriteString(ljust(left(f.spec), col) + line + "\n")
				} else {
					b.WriteString(strings.Repeat(" ", col) + line + "\n")
				}
			}
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- entry

// Main is the command: it runs the line it was given and sets the exit status.
// A command line pflag itself rejects — an unknown flag, a missing value —
// exits 2, the way argparse does; a flag whose value this package rejects, and
// every other failure, exits 1.
func Main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, pflag.ErrHelp), errors.Is(err, errVersionPrinted):
	case errors.As(err, new(usageError)):
		fmt.Fprintf(os.Stderr, "da-transcript: error: %v\n", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	_, err := parseArgs(args, stdout, stderr)
	return err
}
