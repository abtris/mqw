# mqw

A terminal dashboard for getting your own pull requests through a GitHub merge
queue. Left pane is the queue for a base branch; right pane is your open pull
requests with the reason each one can or cannot go in. Select one, press enter,
and it joins the queue.

The case it exists for: a pull request sits in the queue marked unmergeable,
waits an hour to reach the front, gets thrown out, and only then do you find out.
Worse, the obvious fix is often the wrong one — see below.

```
acme/service  queue for main  ·  @alice (pinned)

╭─────────────────────────────────────────────────────────────────╮ ╭─────────────────────────────────────────────────────────────────╮
│ merge queue (4)                                                 │ │ pull requests: mine (5)                                         │
│                                                                 │ │ f cycles: mine · bots · all                                     │
│    1  #3425  queued                                             │ │                                                                 │
│      chore(infra): bump agent version                           │ │ › #3424  refactor(auth): drop legacy fallback                   │
│      opened by @bob                                             │ │      unmergeable inside the merge group                         │
│      [area/infra]                                               │ │      position 2, eta 55m18s · clean against main, so rebasing … │
│   *2  #3424  unmergeable                                        │ │      [area/auth]                                                │
│      refactor(auth): drop legacy fallback                       │ │   #3426  feat(api): paginate list endpoints                     │
│      opened by @alice                                           │ │      unmergeable, cause not yet known                           │
│      [area/auth]                                                │ │      position 4, eta 21m0s · GitHub has not computed mergeabil… │
│    3  #3421  awaiting_checks                                    │ │   #3427  fix(store): close rows on the error path               │
│      chore(deps): bump toolchain image                          │ │      ready to enqueue                                           │
│      opened by @deps-bot                                        │ │      shares files with #3425, #3424 in the queue                │
│      [dependencies]                                             │ │      [area/store] [skip-e2e]                                    │
│   *4  #3426  unmergeable                                        │ │   #3423  fix(api): resolve location at create time              │
│      feat(api): paginate list endpoints                         │ │      review required                                            │
│      opened by @alice                                           │ │   #3418  chore(build): use hardened base images                 │
│                                                                 │ │      draft                                                      │
│ * yours                                                         │ │                                                                 │
╰─────────────────────────────────────────────────────────────────╯ ╰─────────────────────────────────────────────────────────────────╯

  12:52:49  #3424 unmergeable inside the merge group
  12:52:49  #3426 unmergeable, cause not yet known
  12:52:49  #3427 ready to enqueue
  12:52:49  #3423 review required
  12:52:49  #3418 draft

⣾ tab pane · ↑↓ select · f filter · enter enqueue · d dequeue · r poll · q quit  (1 polls, every 1m0s)
```

The code block drops the colour: your own queue entries are green, anything
needing attention red, ready green, labels magenta.

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

`f` cycles the right pane between three filters. `mine` is what the shots above
show; the other two name the author, since `enter` and `d` only ever act on your
own pull requests:

```
pull requests: bots (1)
f cycles: mine · bots · all

› #3421  [deps-bot]  chore(deps): bump toolchain image
     ready to enqueue
     [dependencies]
```

```
pull requests: all (7)
f cycles: mine · bots · all

› #3424  refactor(auth): drop legacy fallback
     unmergeable inside the merge group
     position 2, eta 55m18s · clean against main, so rebasing will not help
     [area/auth]
  #3426  feat(api): paginate list endpoints
     unmergeable, cause not yet known
     position 4, eta 21m0s · GitHub has not computed mergeability against main
  #3427  fix(store): close rows on the error path
     ready to enqueue
     shares files with #3425, #3424 in the queue
     [area/store] [skip-e2e]
  #3423  fix(api): resolve location at create time
     review required
  #3418  chore(build): use hardened base images
     draft
  #3421  [deps-bot]  chore(deps): bump toolchain image
     ready to enqueue
     [dependencies]
  #3425  [bob]  chore(infra): bump agent version
     ready to enqueue
     shares files with #3424 in the queue
     [area/infra]
```

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

The second and third rows, plus the overlap hint on a pull request not queued
yet:

```
pull requests: mine (5)
f cycles: mine · bots · all

› #3424  refactor(auth): drop legacy fallback
     unmergeable inside the merge group
     position 2, eta 55m18s · clean against main, so rebasing will not help
     [area/auth]
  #3426  feat(api): paginate list endpoints
     unmergeable, cause not yet known
     position 4, eta 21m0s · GitHub has not computed mergeability against main
  #3427  fix(store): close rows on the error path
     ready to enqueue
     shares files with #3425, #3424 in the queue
     [area/store] [skip-e2e]
  #3423  fix(api): resolve location at create time
     review required
  #3418  chore(build): use hardened base images
     draft
```

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
on the first poll so starting the tool does not produce a burst. Every
notification also rings the terminal bell.

### macOS

Install [`terminal-notifier`][tn]. It posts from a real app bundle instead of
borrowing Script Editor's identity, carries mqw's icon, and reuses one
notification slot per repo rather than stacking a new banner for every state
change:

```
brew install terminal-notifier
```

Installing it is not enough. macOS asks for notification permission the first
time it runs, and once that prompt has been dismissed or denied every later call
exits with `Notifications are turned off for this application`. Grant it under
**System Settings > Notifications**, or reset the prompt so it can be asked
again:

```
tccutil reset UserNotification fr.julienxx.oss.terminal-notifier
```

That first call blocks for around twenty seconds while the prompt is up, which is
why mqw raises notifications off the render loop — otherwise the whole dashboard
would freeze waiting on it.

Without `terminal-notifier` mqw falls back to `osascript`. The notification still
arrives, but macOS credits it to Script Editor and there is no grouping.

### Linux

Notifications go over D-Bus, falling back to `notify-send` and then `kdialog`. A
normal desktop session needs no extra install.

[tn]: https://github.com/julienXX/terminal-notifier

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
