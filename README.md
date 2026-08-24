# mqw

A terminal dashboard for getting your own pull requests through a GitHub merge
queue. Left pane is the queue for a base branch; right pane is your open pull
requests with the reason each one can or cannot go in. Select one, press enter,
and it joins the queue.

The case it exists for: a pull request sits in the queue marked unmergeable,
waits an hour to reach the front, gets thrown out, and only then do you find out.
Worse, the obvious fix is often the wrong one — see below.

## Install

```
brew install abtris/tap/mqw
```

Or from source: `make build`, or `make install` to put it in `GOBIN`. `make help`
lists every target; `make check` runs fmt, vet and tests. Note that
`go run main.go` will not compile — it excludes the other files in the package.
Use `go run .` or `make run ARGS="..."`.

Requires the `gh` CLI, authenticated. Everything goes through `gh api graphql`,
because merge queue entries are not exposed over REST.

## Usage

```
mqw [-repo acme/service] [-base main] [-interval 1m]
```

Run `mqw` with no arguments for full help, `mqw -version` for the version.

## Config

Settings are layered, each overriding the one before it field by field:

1. `~/.config/mqw/config.toml` (or `-config <path>`)
2. the nearest `.mqw.toml`, found by walking up from the working directory to the
   repository root
3. flags

```toml
repo = "acme/service"
base = "main"
interval = "1m"
account = "alice"
bots = ["deps-bot", "release-bot"]
```

`mqw --print-config` prints a starting point. With a repo configured, plain `mqw`
is the normal way to run. The files actually in effect are listed in the log at
the bottom of the screen, so "why is it using that account" is never a hunt.

Because the layers merge per field, a `.mqw.toml` beside a checkout can pin just
the repo and account and inherit the rest. One exception: a non-empty `bots` list
replaces rather than appends, since predictable beats clever when you are asking
why something shows up as a bot.

`bots` exists because GitHub only marks *Apps* as bot actors. A bot running under
an ordinary user account — plenty do — is indistinguishable from a person in the
API, so list those logins here to make the `bots` filter find them.

### Pinning a gh account

`account` nails a session to one `gh` account. `gh` prefers `GH_TOKEN` over
whichever account is active, so `mqw` resolves that account's token once at
startup and passes it to every `gh` call. A `gh auth switch` in another terminal
then cannot break a running dashboard, and `mqw` never changes global `gh` state
itself. The header marks a pinned account:

```
acme/service  queue for main  ·  @alice (pinned)
```

This is the fix for juggling a work and a personal account: give each checkout a
`.mqw.toml` naming its own. Without `account`, `mqw` uses the active account, which
is fine when you only have one.

The token reaches `gh` through that child process's environment, so on a shared
machine it is visible to your own user via `ps eww`. If you would rather isolate
credentials completely, use a separate `GH_CONFIG_DIR` per account instead, at the
cost of maintaining separate `gh auth login` state.

## Keys

| Key | |
| --- | --- |
| `tab` | switch pane |
| `up`/`down` (or `k`/`j`) | move selection |
| `f` | cycle the filter: mine, bots, all |
| `enter` | add the selected pull request to the queue |
| `d` | remove the selected pull request from the queue |
| `r` | poll now |
| `q` | quit |

Both `enter` and `d` refuse to act on a pull request that is not yours, so you
cannot disturb somebody else's merge from the queue pane. Ownership comes from the
authenticated login, not from what the filter happens to show, so widening the
filter to `all` never widens what you can act on. Notifications are scoped the
same way.

Your own entries in the queue pane are bold green and marked `*`.

Both panes list each pull request's labels under it, in magenta. Labels are worth
seeing next to the queue because repositories use them to steer CI — a
`skip-something` label means the entry ahead of you is not running the job you
expect it to.

## Why rebasing often does not help

An unmergeable queue entry has two quite different causes, and GitHub shows them
identically. The tool tells them apart from `mergeable`:

| Reported | Cause | Fix |
| --- | --- | --- |
| `unmergeable: conflicts with <base>` | genuinely conflicts with the base branch | rebase, then re-queue |
| `unmergeable inside the merge group` | clean against the base; the problem is the merge group — a conflict with a pull request ahead of you, or its checks failing | rebasing changes nothing; wait for the entries ahead to land |
| `unmergeable, cause not yet known` | GitHub has not computed mergeability yet | wait a poll |

The middle row is the one that wastes time. The merge group is a temporary branch
of base + every queued pull request ahead of yours + yours. If the conflict is
with a queued predecessor, that content is not in the base branch yet, so there
is nothing to rebase onto. This is also why a merge queue generally means your
branch does **not** need to be up to date with the base: GitHub builds the merge
itself. The habit of rebasing and retrying comes from "require branches to be up
to date" protection, and it does not address this case.

## Checking for conflicts before queueing

For a pull request that is not queued yet, the tool names any queued pull request
it shares changed files with.

Treat it as a hint, nothing more. Touching the same file is not a conflict, and
sharing no files does not rule one out. A real dequeue observed while building
this shared zero files with the entry ahead of it — the merge group's checks
failed instead, which is not predictable in advance. That is what the queue is
for. File lists are also capped at 100 paths per pull request by the API.

## Notifications

A desktop notification fires when one of your pull requests changes into a state
worth acting on. It notifies on a *change*, not on every poll, and stays silent
on the first poll so starting the tool does not produce a burst.

## Multiple GitHub accounts

The tool shells out to `gh`, which uses whichever account is active — not the one
matching the repo. For a repo owned by an org your Enterprise Managed User covers,
make that account active first:

```
gh auth switch --user <emu-account>
```

Get this wrong and the two panes fail differently, which is worth knowing because
it looks like two unrelated problems:

- the **queue** pane errors with `Could not resolve to a Repository`
- the **pull request** pane just looks *empty* — GitHub's search returns no
  results rather than an error for a repo you cannot see

So an empty right-hand pane is a symptom, not a fact about your work. The header
shows which login `gh` is authenticated as, and the queue error carries a hint
naming this cause.

Setting `account` in a `.mqw.toml` avoids the whole problem — see
[Pinning a gh account](#pinning-a-gh-account).

## Releasing

Tag and push; the `release` workflow runs GoReleaser, which publishes the archives
and updates the Homebrew cask in `abtris/homebrew-tap`.

```
git tag -a v0.1.0 -m v0.1.0 && git push origin v0.1.0
```

This needs a `HOMEBREW_TAP_GITHUB_TOKEN` repository secret with `contents: write`
on the tap repo — the workflow's own `GITHUB_TOKEN` cannot push to another
repository. `make release-snapshot` builds everything locally without publishing,
and `make release-check` validates the config.

The binary ships as a Homebrew **cask**, not a formula: Homebrew expects formulae
to build from source and casks to carry pre-built binaries, and GoReleaser's older
`brews` key is deprecated for this reason.
