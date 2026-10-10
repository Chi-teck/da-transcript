# da-transcript

[![CI](https://github.com/Chi-teck/da-transcript/actions/workflows/ci.yml/badge.svg)](https://github.com/Chi-teck/da-transcript/actions/workflows/ci.yml)

Merge every [docker-agent](https://github.com/docker/docker-agent) session belonging to a
project into one chronological transcript, laid out for the terminal.

An agent driven from more than one place at once — a chat bridge, a scheduled run,
someone at the TUI — gets a separate docker-agent session for each. `da-transcript` walks
docker-agent's SQLite store and interleaves them into a single stream, each run of blocks
opening under a banner naming the session it belongs to and the directory it ran in.
A subagent's tag ends in `↓` and its banner rule is dashed rather than solid.

## What it's for

To audit a run after it happened: where the agent went wrong, and what to change so it
does not go wrong the same way again. For a background run — a scheduled job, an agent
driven from a chat bridge, a subagent that did most of the editing — nobody watched it
happen, so the transcript is the only record there is.

`--tools full` prints the arguments each call was made with, the output that came back,
the error when it failed, and how long it took. That is where the tooling problems show
up — a description bad enough that the arguments are always wrong, output truncated
before the useful part, an error the agent can't act on and retries three times over, a
skill invoked two steps too late. `--reasoning` adds what the model thought it was doing
right before the wrong turn.

And because every session under the project lands in one ordered stream, a mistake that
repeats across a week of runs is one `da-transcript --all | grep …` away — a pattern
rather than an anecdote.

## Install

Requires docker-agent v1.113 or later (a store at schema migration 025 or newer).

Grab an archive for your platform (Linux or macOS, amd64 or arm64) from the
[releases page](https://github.com/Chi-teck/da-transcript/releases), or install it with the
Go toolchain:

```bash
go install github.com/Chi-teck/da-transcript@latest
```

Or build from a checkout:

```bash
CGO_ENABLED=0 go build -o da-transcript .   # or: task build
```

## Usage

```bash
da-transcript                      # last 24h of sessions run under the cwd
da-transcript --all --tools full   # everything, with full tool output
da-transcript --list               # just the sessions, no messages
da-transcript --follow             # keep printing new turns as they complete
```

Scope is the working directory, the way git scopes itself: the sessions whose
working directory sits inside `--root` are the ones you get. `--everywhere` ignores
the root and takes the whole database. The path at the right of a banner is the
directory that session ran in, written out whole rather than relative to the root:
a transcript is usually redirected to a file and read somewhere else, where a
relative path names nothing. `docker-agent run --worktree` (and `--worktree-pr`)
records the worktree it creates as the session's directory, not the project it
came from: `<data-dir>/worktrees/<name>`, `~/.cagent/worktrees/<name>` by default.
So `--root <project>` misses those sessions; `--everywhere`, or `--root` at the
worktree (or at its parent, for all of them), finds them. A subagent written before
docker-agent v1.138 has no directory of its own and is listed under its parent's.

The flag before a tag says where the rest of that session is:

| Flag | Meaning                                                                                                                            |
|------|------------------------------------------------------------------------------------------------------------------------------------|
| `⚑`  | the session starts here — this is its first block                                                                                  |
| `⚐`  | its earlier blocks fall before the window, so there is more above; widen `--since`, or read it whole with `--session ID --all`     |
| `↻`  | the stream has shown it before and come back to it — the earlier part is further up this same output                               |
| `◷`  | `--follow` only, never in the opening transcript: what follows is older than something already printed above it, as when a background subagent has just finished, a step was held while other sessions went on, or a session was forked or branched meanwhile. It takes the place of the other three flags. |

Times are local — whatever `$TZ` resolves to, and the same zone `--since`/`--until` are
read in. The zone is not printed, so use `TZ=UTC da-transcript …` if a transcript
written with `--out` will be read elsewhere.

Times have one-second resolution, the store's. An assistant block's time is when the
model finished the step (docker-agent stamps a step at its end), so it means *answered
at*, not *started at*. A tool's duration runs from that time to its result, in whole
seconds, so it includes any wait for approval.

Lines are laid out to the width in `COLUMNS` if it is set, else the terminal's, else 100
— for a pipe, or a file written with `--out` — and never narrower than 60 columns or
wider than 1000.

## Options

Run `da-transcript --help` for the full list. The ones you reach for:

| Flag                           | What it does                                                                   |
|--------------------------------|--------------------------------------------------------------------------------|
| `--root`, `--everywhere`       | which sessions are in scope                                                    |
| `--since`, `--until`, `--all`  | the time window — `2h`, `30m`, `3d`, `today`, `yesterday`, or an ISO timestamp |
| `--tools {compact,full,none}`  | tool call detail                                                               |
| `--reasoning`, `--stats`       | include reasoning blocks / per-step token and cost lines                       |
| `--session`                    | narrow to sessions whose id contains a fragment                                |
| `-f`, `--follow`, `--interval` | tail new messages (cursor-driven, poll every N seconds, 0.1 to 3600)           |
| `-o`, `--out`                  | write to a file instead of stdout (a new file is created 0600; colour off unless `--color always`; width from `COLUMNS`, else 100) |
| `--db`                         | the store, default `~/.cagent/session.db`                                      |
| `--version`                    | print the version and exit                                                     |

For a run with `--data-dir` or `-s/--session-db`, point `--db` at
`<data-dir>/session.db` or that file; `docker-agent serve api` keeps its own
`session.db` in the directory it was started in, or its `--working-dir`.

## Following

`--follow` prints a step only once it is complete. While the model is still writing it,
or one of its calls has no result yet (a `transfer_task` running its subagent, a call
waiting for approval), the step waits, and the rest of its own session waits behind it;
other sessions keep printing. After 30 s, a held step gets one line on stderr:
`warning: waiting on transfer_task in 3fa9c2: unfinished since … — the rest of that
session is held until it completes or the session goes on`. docker-agent writes a
subagent to the store only when it ends, so its whole run shows up at once: after the
`transfer_task` step that called it, or, for a background agent, under `◷` if anything
newer was printed while it ran.

## Known limits

These are `--follow`'s; a run without it is unaffected except where an item says so.

- A step cut off and then abandoned (Esc, then nothing more in that session; rarely a
  reasoning-only step that ended empty) holds its session until the session goes on:
  `--follow` leaves it out and warns once, while a run without `--follow` shows it.
- A message edited through `serve api` after `--follow` printed it is not printed
  again.
- A step read while it streams has no time yet, so `--follow` dates it by the item
  before it, its prompt or the last tool result. If that is older than `--since`, the
  step is skipped even when it started inside the window, and its results are drawn
  on their own when they land, as `tool` blocks whose call reads `?`; a `--since` that
  takes in that item keeps the step. A run without `--follow` shows it once it ends.
- A call whose step is older than `--since` but which is still running when
  `--follow` starts gets its result drawn on its own when it lands, as a `tool` block
  whose call reads `?`; a run without `--follow` with the same `--since` shows neither.
- A fork or branch made during `--follow` prints its copied history (within the
  window) as a new session under `◷`, and a run without `--follow` also shows that
  history twice, once per session.

## Development

```bash
task check         # gofmt, go vet, golangci-lint, go test — the pre-commit gate
task test:update   # regenerate the golden files, then re-run the tests
task build         # CGO_ENABLED=0 go build -o da-transcript .
```

Expected output is fixed by the golden files in `internal/app/testdata/`. CI runs the
same checks on every push to a `*.x` branch and on every pull request;
pushing a `v*` tag publishes a release.

## License

MIT — see [LICENSE](LICENSE).
