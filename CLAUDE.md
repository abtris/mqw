# mqw

A terminal dashboard for moving your own pull requests through a GitHub merge
queue. Left pane is the queue for a base branch, right pane is the viewer's open
pull requests; enter enqueues the selection, `d` dequeues it.

## Design constraints

Keep these unless the user changes the scope explicitly.

- **Mutations are keypress-only.** `enqueuePullRequest` and `dequeuePullRequest`
  run only from `enter` and `d`. Nothing is enqueued or dequeued automatically —
  this acts on a queue shared with the rest of the team. An earlier iteration
  automated local rebase and re-queue; it was cut deliberately.
- **Never act on another person's pull request.** Both actions check `ownsPR`
  first. The queue pane lists everyone's entries.
- **No local git, no GitHub SDK.** Everything is `gh api graphql`. Merge queue
  entries are not in REST, and shelling out to `gh` avoids token plumbing.
- **Notify on state change, not per poll**, and stay silent on the first poll.
  `notifyChanges` compares against `lastKind`; without the first-poll suppression,
  starting the tool notifies once per pull request.
- **Do not overstate a diagnosis.** See below.

## The distinction the tool exists for

An `UNMERGEABLE` queue entry has two causes that look identical in GitHub's UI,
and `classify` separates them from `pullRequest.mergeable`:

- `CONFLICTING` → conflicts with the base branch; rebasing fixes it.
- `MERGEABLE` → clean against base, so the failure is inside the merge group (a
  conflict with a queued predecessor, or the group's checks). Rebasing onto the
  base **cannot** help, because the conflicting content is not in the base yet.
- `UNKNOWN` → `kQueuedUnknownCause`. Do not collapse this into the `MERGEABLE`
  case: claiming "rebasing will not help" before GitHub has computed mergeability
  is a guess. This was observed live on a real PR.

The changed-file overlap hint (`conflictRisk`) is a heuristic and must stay
labelled as one. A real dequeue observed while building this shared zero files
with the entry ahead of it; `TestOverlapHeuristicMissedTheRealDequeue` records
that. Never present it as a conflict prediction.

## Config layering and account pinning

Three layers, merged per field in `merge`: global `~/.config/mqw/config.toml`, the
nearest `.mqw.toml` walking up to the repository root, then flags. `loadConfigs`
returns the merged config plus the contributing paths, which `newModel` logs so
the effective account is never a mystery. `findLocalConfig` stops at the directory
holding `.git` on purpose — walking past it would let an unrelated parent supply
settings.

`account` pins gh to one account: `resolveToken` runs `gh auth token --user <x>`
once at startup and `ghCommand` puts it in `GH_TOKEN` on each gh child, because gh
prefers `GH_TOKEN` over its active account. Two details worth keeping:

- `ghCommand` uses `cmd.Environ()`, not `os.Environ()`, so it appends to whatever
  the caller set rather than replacing it. Replacing it breaks the test fake,
  whose helper process needs `GO_WANT_HELPER_PROCESS`.
- `resolveToken` deliberately does not go through `ghCommand`; there is no token
  to pass yet. Resolution happens once at startup so a bad account fails with one
  clear message rather than every poll failing obscurely.

A non-empty `bots` list replaces rather than appends when layering. That is a
deliberate choice for predictability, and it is tested.

## Wrong gh account: the asymmetry that makes it confusing

The two queries fail differently for a repo the active account cannot see, and
this cost real debugging time:

- `repository(owner:,name:)` (the queue) returns `NOT_FOUND`.
- `search()` (the pull requests) returns `issueCount: 0` and **no error**.

So a wrong account shows an error on the left and a plausible-looking empty list
on the right. The header therefore prints the authenticated login, and
`wrongAccountHint` explains it next to the queue error. Do not remove either.

## Anonymised identifiers

Fixtures and docs use generic names on purpose: `acme/service`, `alice` (the
viewer, `testViewer`), `bob`, `carol`, `deps-bot`. Do not reintroduce real org,
repo, or user names. The exception is the module path and the goreleaser/tap
owner, which must match the actual repository to work.

## Releasing

GoReleaser publishes a Homebrew **cask**, not a formula — Homebrew expects
formulae to build from source, and the `brews` key is deprecated. Two traps
already hit: `homebrew_casks.binary` is deprecated in favour of `binaries`, and
`gh` must be declared `formula: gh`, not `cask: gh`, or the dependency fails to
resolve. `make release-check` catches config regressions; CI runs it too.

Pushing to `abtris/homebrew-tap` needs the `HOMEBREW_TAP_GITHUB_TOKEN` secret;
the workflow's `GITHUB_TOKEN` cannot write to another repository.

## Layout

- `github.go` — the GraphQL queries and mutations, response types, `poll`.
  `prFields` is shared by the queue and search queries so a PR decodes identically
  from either. Note the schema asymmetry: `enqueuePullRequest` takes
  `pullRequestId`, `dequeuePullRequest` takes `id`.
- `main.go` — flags, `classify`, the bubbletea model, and the two-pane view.
- `notify.go` — desktop notification and terminal bell. Failures are swallowed on
  purpose; a missing notifier must not kill the dashboard.
- `config.go` — the TOML settings file and flag/config merge (`resolve`).
- Tests: `classify_test.go` (state machine and fixtures), `config_test.go`,
  `github_test.go` (the gh
  exec path), `model_test.go` (keys, panes, notifications), `notify_test.go`.

## Build and test

Go 1.27.0, managed by gobrew (`gobrew use 1.27.0`). Use the Makefile: `make help`
lists targets, `make check` runs fmt, vet and tests, `make run ARGS="-repo o/n"`. Config lives in
`~/.config/mqw/config.toml`; `mqw --print-config` emits a sample.

`go run main.go` does not work and is not meant to — it compiles that one file, so
everything in `github.go` and `notify.go` comes back undefined. Use `go run .`.

If a build fails with `compile: version "goX" does not match go tool version "goY"`,
`GOROOT` and the `go` on PATH have drifted apart — point gobrew at the version the
`go` binary reports rather than working around it with `env -u GOROOT`.

## Test seams

Two package-level vars exist only so tests can drive the untestable edges. Keep
them, and keep them restored through `t.Cleanup`.

- `execCommand` in `github.go` wraps `exec.Command`. `fakeGH` in `github_test.go`
  points it at the test binary itself (`TestHelperProcess`) and routes on the
  GraphQL query text, so one fake answers the search, queue and mutation calls
  differently. This covers the real exec and error handling rather than stubbing
  the fetch functions.
- `notifier` in `notify.go` wraps `notify`. Tests swap in a recorder; without it
  every `go test` fires real desktop notifications.

Coverage sits around 89%. `main()` and `notify()` are the meaningful gaps, being
process entry and OS I/O.

## Running against a work repo

`gh` uses whichever account is *active*, not the one matching the repo. Against an
org behind the Enterprise Managed User, `gh auth switch --user alice`
first or every poll fails.

## Verifying the UI

The panes only render correctly with a real terminal size. Under `script -q
/dev/null` the pty reports 0x0, which is why `Update` ignores a zero
`WindowSizeMsg`; without that guard the layout collapses. When capturing output,
`\r` → `\n` conversion double-spaces every row — that is a capture artifact, not
a rendering bug.
