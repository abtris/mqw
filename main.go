package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const helpText = `mqw — a merge queue dashboard for your own pull requests.

Left pane is the merge queue for the base branch, with your own entries in bold
green. Right pane lists open pull requests, filtered to yours by default. Select
one and press enter to add it to the queue.

Usage:
  mqw [-repo owner/name] [-base main] [-interval 1m]

With a config file naming a repo, plain 'mqw' is the normal way to run.

Flags:
  -repo          repository as owner/name
  -base          base branch whose queue to watch (default main)
  -account       gh account to pin this session to
  -interval      how often to poll (default 1m); r polls immediately
  -config        global config file (default ~/.config/mqw/config.toml)
  -print-config  print a sample config and exit
  -version       print the version and exit

Config is layered, each overriding the one before, field by field:

  1. ~/.config/mqw/config.toml
  2. the nearest .mqw.toml, found by walking up to the repository root
  3. flags

  repo = "acme/service"
  base = "main"
  interval = "1m"
  account = "alice"
  bots = ["deps-bot", "release-bot"]

So a .mqw.toml beside a checkout can pin just the repo and account, leaving the
rest global. The files in effect are listed in the log at the bottom.

'account' pins gh to one account for this session: gh prefers GH_TOKEN over
whichever account is active, so a 'gh auth switch' in another terminal cannot
break a running dashboard. The header marks a pinned account.

GitHub Apps are detected as bots automatically. List service accounts that are
ordinary User accounts under 'bots', since nothing in the API marks those.

Keys:
  tab      switch pane
  up/down  move selection (k/j also work)
  f        cycle the filter: mine, bots, all
  enter    add the selected pull request to the queue
  d        remove the selected pull request from the queue
  r        poll now
  q        quit

Enter and d only act on your own pull requests, whatever the filter shows, and
notifications stay scoped to yours too. Ownership comes from the authenticated
login, never from what happens to be on screen.

It notifies when one of your queued pull requests turns unmergeable, which is
worth knowing early: GitHub leaves such an entry in the queue until it reaches
the front, then throws it out, so you can wait an hour to learn nothing merged.

Two causes look identical in the UI, so the tool tells them apart:

  conflicts with the base branch    rebasing fixes it
  clean against base, still broken  the conflict or failure is inside the merge
                                    group, so rebasing onto the base changes
                                    nothing; wait for the entries ahead to land

Both panes list each pull request's labels under it. Repositories use labels to
steer CI, so a 'skip-something' on the entry ahead of you tells you what it is
not running.

On a pull request that is not queued yet, the tool names any queued pull request
it shares changed files with. Treat that as a hint only: touching the same file
is not a conflict, and sharing none does not rule one out, because a merge group
can fail on checks too.

Requires the gh CLI, authenticated. gh uses whichever account is active, not the
one matching the repo, so for a repo behind an Enterprise Managed User run
'gh auth switch --user <account>' first or every poll fails.

Example:
  gh auth switch --user alice
  mqw -repo acme/service
`

// Set by the linker at release time; "dev" in a local build.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, helpText) }

	showVersion := flag.Bool("version", false, "print the version and exit")
	repo := flag.String("repo", "", "repository as owner/name")
	base := flag.String("base", "main", "base branch whose queue to watch")
	interval := flag.Duration("interval", time.Minute, "poll interval")
	account := flag.String("account", "", "gh account to pin this session to")
	configPath := flag.String("config", defaultConfigPath(), "path to the global config file")
	printConfig := flag.Bool("print-config", false, "print a sample config and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("mqw %s (%s, built %s)\n", version, commit, date)
		return
	}
	if *printConfig {
		fmt.Print(sampleConfig)
		return
	}

	// The global file first, then the nearest per-repository .mqw.toml over it.
	cwd, _ := os.Getwd()
	cfg, sources, err := loadConfigs(*configPath, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mqw: %v\n", err)
		os.Exit(2)
	}

	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	// With no repo from either source there is nothing to show, so treat a bare
	// invocation as a request for help. A config that names a repo makes bare
	// invocation the normal way to run.
	if *repo == "" && cfg.Repo == "" {
		fmt.Print(helpText)
		return
	}

	s, err := resolve(cfg, setFlags, *repo, *base, *interval, *account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mqw: %v\n\nRun 'mqw --print-config' for a sample config.\n", err)
		os.Exit(2)
	}
	s.sources = sources

	// Resolve the pinned account once, up front: failing here with one clear
	// message beats every poll failing for a reason the UI has to guess at.
	if s.account != "" {
		token, err := resolveToken(s.account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mqw: %v\n\nCheck 'gh auth status' for the accounts you have.\n", err)
			os.Exit(2)
		}
		ghToken = token
	}

	m := newModel(s)
	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// parseRepo splits the -repo flag into owner and name.
func parseRepo(repo string) (string, string, error) {
	if repo == "" {
		return "", "", errors.New("-repo is required, as owner/name")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("-repo %q is not owner/name", repo)
	}
	return owner, name, nil
}

// statusKind is the coarse state reported per pull request. Notifications fire
// when a kind changes, so each kind must be meaningful on its own.
type statusKind int

const (
	kStartup statusKind = iota
	kMerged
	kClosed
	kDraft
	kQueued
	kQueuedConflict
	kQueuedGroupFail
	kQueuedUnknownCause
	kConflicting
	kChecksFailed
	kNeedsReview
	kReady
	kPending
)

type status struct {
	kind      statusKind
	headline  string
	detail    string
	attention bool
}

// classify describes one pull request. queue is the merge queue for the base
// branch, used only to name possible conflicts for a PR that is not queued yet.
func classify(p *pullRequest, queue []queueSlot) status {
	switch {
	case p.Merged:
		return status{kind: kMerged, headline: "merged"}
	case p.State == "CLOSED":
		return status{kind: kClosed, headline: "closed"}
	case p.IsDraft:
		return status{kind: kDraft, headline: "draft"}
	}

	if e := p.QueueEntry; e != nil {
		where := fmt.Sprintf("position %d", e.Position)
		if e.EstimatedTimeToMerge != nil {
			where += fmt.Sprintf(", eta %s", time.Duration(*e.EstimatedTimeToMerge)*time.Second)
		}

		if e.State != "UNMERGEABLE" {
			return status{kind: kQueued, headline: strings.ToLower(e.State), detail: where}
		}

		// An unmergeable entry stays in the queue until it reaches the front and
		// is then thrown out. Which remedy applies depends on whether the PR also
		// conflicts with the base branch.
		switch p.Mergeable {
		case "CONFLICTING":
			return status{
				kind:      kQueuedConflict,
				headline:  "unmergeable: conflicts with " + p.BaseRefName,
				detail:    where + " · dequeue, rebase " + p.HeadRefName + ", re-queue",
				attention: true,
			}
		case "MERGEABLE":
			return status{
				kind:      kQueuedGroupFail,
				headline:  "unmergeable inside the merge group",
				detail:    where + " · clean against " + p.BaseRefName + ", so rebasing will not help",
				attention: true,
			}
		default:
			// Mergeability against the base is still UNKNOWN, so which remedy
			// applies cannot be stated yet. Saying "rebasing will not help" here
			// would be a guess.
			return status{
				kind:      kQueuedUnknownCause,
				headline:  "unmergeable, cause not yet known",
				detail:    where + " · GitHub has not computed mergeability against " + p.BaseRefName,
				attention: true,
			}
		}
	}

	// Not queued. Work out whether it could be.
	switch {
	case p.Mergeable == "CONFLICTING":
		return status{
			kind:      kConflicting,
			headline:  "conflicts with " + p.BaseRefName,
			detail:    "rebase " + p.HeadRefName,
			attention: true,
		}
	case p.checkState() == "FAILURE" || p.checkState() == "ERROR":
		return status{kind: kChecksFailed, headline: "checks failing", attention: true}
	case p.Mergeable == "UNKNOWN":
		return status{kind: kPending, headline: "mergeability not computed yet"}
	case p.ReviewDecision == "CHANGES_REQUESTED":
		return status{kind: kNeedsReview, headline: "changes requested"}
	case p.ReviewDecision == "REVIEW_REQUIRED":
		return status{kind: kNeedsReview, headline: "review required"}
	}

	ready := status{kind: kReady, headline: "ready to enqueue"}
	if risk := conflictRisk(p, queue); len(risk) > 0 {
		ready.detail = "shares files with " + joinPRs(risk) + " in the queue"
	}
	return ready
}

// wrongAccountHint explains a NOT_FOUND on the repository, which almost always
// means gh is authenticated as an account without access rather than the repo
// being gone.
//
// This is worth spelling out because the two queries fail differently: the pull
// request search returns an empty list rather than an error for a repo you cannot
// see, so the right pane looks merely empty while only the queue reports trouble.
func wrongAccountHint(err error) string {
	if err == nil || !strings.Contains(err.Error(), "Could not resolve to a Repository") {
		return ""
	}
	return "gh may be authenticated as the wrong account — check 'gh auth status', " +
		"then 'gh auth switch --user <account>'. An empty pull request pane is the " +
		"same cause: the search returns nothing instead of failing. Otherwise check -repo and -base."
}

// timeNow is the clock. Swapped out in tests so relative times are stable.
var timeNow = time.Now

// humanAgo renders an RFC3339 timestamp as an approximate age, in GitHub's
// style. An unparseable timestamp yields "", so callers can drop the field.
func humanAgo(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := timeNow().Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "1 minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "about 1 hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("about %d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "1 day ago"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// queueSubline describes an entry the way GitHub's queue page does: who opened
// it, who enqueued it, when, and the estimated merge time. Fields that the API
// did not return are left out rather than rendered blank.
func queueSubline(e queueSlot) string {
	var parts []string
	if login := e.PullRequest.Author.Login; login != "" {
		parts = append(parts, "opened by @"+login)
	}
	// Worth showing separately: somebody else can enqueue your pull request.
	if login := e.Enqueuer.Login; login != "" {
		parts = append(parts, "enqueued by @"+login)
	}
	if ago := humanAgo(e.EnqueuedAt); ago != "" {
		parts = append(parts, "enqueued "+ago)
	}
	if e.EstimatedTimeToMerge != nil {
		d := time.Duration(*e.EstimatedTimeToMerge) * time.Second
		parts = append(parts, "estimated merge in "+d.Round(time.Second).String())
	}
	return strings.Join(parts, " • ")
}

// labelLine renders a pull request's labels, or "" when it has none. Every
// label is shown rather than a configured subset: a repo can use them to skip
// parts of CI, and which ones matter is not something the tool can know.
func labelLine(p *pullRequest) string {
	names := p.Labels.names()
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, "["+n+"]")
	}
	return strings.Join(parts, " ")
}

func joinPRs(numbers []int) string {
	parts := make([]string, 0, len(numbers))
	for _, n := range numbers {
		parts = append(parts, fmt.Sprintf("#%d", n))
	}
	return strings.Join(parts, ", ")
}

// enqueueable reports whether enter should attempt the mutation, and why not.
func enqueueable(p *pullRequest) error {
	switch {
	case p.inQueue():
		return fmt.Errorf("#%d is already in the queue", p.Number)
	case p.IsDraft:
		return fmt.Errorf("#%d is a draft", p.Number)
	case p.Mergeable == "CONFLICTING":
		return fmt.Errorf("#%d conflicts with %s — rebase %s first", p.Number, p.BaseRefName, p.HeadRefName)
	}
	return nil
}

type pane int

const (
	paneQueue pane = iota
	paneMine
)

// prFilter selects which open pull requests the right pane lists. It never
// affects ownership, which comes from the authenticated login.
type prFilter int

const (
	filterMine prFilter = iota
	filterBots
	filterAll
)

var filterOrder = []prFilter{filterMine, filterBots, filterAll}

func (f prFilter) String() string {
	switch f {
	case filterBots:
		return "bots"
	case filterAll:
		return "all"
	default:
		return "mine"
	}
}

func (f prFilter) next() prFilter {
	return filterOrder[(int(f)+1)%len(filterOrder)]
}

// isBot reports whether a pull request came from a bot. GitHub marks Apps as Bot
// actors, but plenty of bots run as ordinary User accounts with nothing to
// distinguish them, so configured logins count too.
func (m model) isBot(pr *pullRequest) bool {
	return pr.Author.isBot() || m.bots[pr.Author.Login]
}

// admits reports whether the active filter shows this pull request. Note that
// filterMine goes through ownsPR, so "mine" and "may I act on it" can never
// drift apart.
func (m model) admits(pr *pullRequest) bool {
	switch m.filter {
	case filterMine:
		return m.ownsPR(pr)
	case filterBots:
		return m.isBot(pr)
	default:
		return true
	}
}

type snapMsg struct {
	snap *snapshot
	err  error
}

type actionMsg struct {
	text string
	err  error
}

type tickMsg struct{}

type model struct {
	owner, name, base string
	interval          time.Duration

	spinner       spinner.Model
	width, height int

	snap    *snapshot
	pollErr error
	loaded  bool
	polls   int

	focus    pane
	filter   prFilter
	queueCur int
	mineCur  int

	// account is the gh account this session is pinned to, "" for whichever is
	// active. Shown in the header so the identity in use is never a guess.
	account string

	// bots are logins configured as bots, for accounts GitHub does not mark as
	// Bot actors.
	bots map[string]bool

	// lastKind remembers each PR's last reported kind so a state notifies once
	// rather than once per poll.
	lastKind map[int]statusKind

	message  string
	logs     []string
	busy     bool
	quitting bool
}

func newModel(s settings) model {
	m := model{
		owner:    s.owner,
		name:     s.name,
		base:     s.base,
		interval: s.interval,
		account:  s.account,
		bots:     s.bots,
		spinner:  spinner.New(spinner.WithSpinner(spinner.Dot)),
		width:    100,
		height:   28,
		focus:    paneMine,
		lastKind: map[int]statusKind{},
	}
	// Naming the files in effect answers "why is it using that account" without
	// a hunt through two directories.
	for _, src := range s.sources {
		m.log("config: %s", src)
	}
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.pollCmd())
}

func (m model) pollCmd() tea.Cmd {
	owner, name, base := m.owner, m.name, m.base
	return func() tea.Msg {
		snap, err := poll(owner, name, base)
		return snapMsg{snap: snap, err: err}
	}
}

func (m model) wait() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}

func enqueueCmd(pr pullRequest) tea.Cmd {
	return func() tea.Msg {
		if err := enqueue(pr.ID); err != nil {
			return actionMsg{text: fmt.Sprintf("could not enqueue #%d", pr.Number), err: err}
		}
		return actionMsg{text: fmt.Sprintf("added #%d to the queue", pr.Number)}
	}
}

func dequeueCmd(pr pullRequest) tea.Cmd {
	return func() tea.Msg {
		if err := dequeue(pr.ID); err != nil {
			return actionMsg{text: fmt.Sprintf("could not dequeue #%d", pr.Number), err: err}
		}
		return actionMsg{text: fmt.Sprintf("removed #%d from the queue", pr.Number)}
	}
}

func (m *model) log(format string, args ...any) {
	line := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, args...)
	m.logs = append(m.logs, line)
	if len(m.logs) > 6 {
		m.logs = m.logs[len(m.logs)-6:]
	}
}

// visiblePRs returns the pull requests the current filter admits, or nil before
// the first poll.
func (m model) visiblePRs() []pullRequest {
	if m.snap == nil {
		return nil
	}
	var out []pullRequest
	for i := range m.snap.prs {
		if m.admits(&m.snap.prs[i]) {
			out = append(out, m.snap.prs[i])
		}
	}
	return out
}

func (m model) queueSlots() []queueSlot {
	if m.snap == nil {
		return nil
	}
	return m.snap.queue
}

// selected returns the pull request the cursor is on in the focused pane.
func (m model) selected() *pullRequest {
	switch m.focus {
	case paneMine:
		prs := m.visiblePRs()
		if m.mineCur < len(prs) {
			return &prs[m.mineCur]
		}
	case paneQueue:
		slots := m.queueSlots()
		if m.queueCur < len(slots) {
			return &slots[m.queueCur].PullRequest
		}
	}
	return nil
}

// ownsPR reports whether a pull request is the viewer's, so the tool never acts
// on somebody else's entry.
//
// This compares against the authenticated login, deliberately not against the
// visible list: the filter can show bot and other people's pull requests, and
// deriving ownership from what happens to be on screen would silently authorise
// acting on all of them.
func (m model) ownsPR(pr *pullRequest) bool {
	return m.snap != nil && m.snap.owns(pr)
}

func (m *model) moveCursor(delta int) {
	switch m.focus {
	case paneMine:
		m.mineCur = clamp(m.mineCur+delta, len(m.visiblePRs()))
	case paneQueue:
		m.queueCur = clamp(m.queueCur+delta, len(m.queueSlots()))
	}
}

func clamp(i, length int) int {
	if length == 0 {
		return 0
	}
	if i < 0 {
		return 0
	}
	if i >= length {
		return length - 1
	}
	return i
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A pty with no size reports zero, which would collapse the layout.
		if msg.Width > 0 && msg.Height > 0 {
			m.width, m.height = msg.Width, msg.Height
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tickMsg:
		return m, m.pollCmd()

	case snapMsg:
		m.polls++
		m.loaded = true
		if msg.err != nil {
			m.pollErr = msg.err
			m.log("poll failed: %v", msg.err)
			return m, m.wait()
		}
		m.pollErr = nil
		m.snap = msg.snap
		m.mineCur = clamp(m.mineCur, len(m.visiblePRs()))
		m.queueCur = clamp(m.queueCur, len(msg.snap.queue))
		m.notifyChanges()
		return m, m.wait()

	case actionMsg:
		m.busy = false
		if msg.err != nil {
			m.message = msg.text + ": " + msg.err.Error()
			m.log("%s: %v", msg.text, msg.err)
		} else {
			m.message = msg.text
			m.log("%s", msg.text)
		}
		// Re-poll straight away so the panes reflect the mutation.
		return m, m.pollCmd()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit

	case "tab", "left", "right", "h", "l":
		if m.focus == paneMine {
			m.focus = paneQueue
		} else {
			m.focus = paneMine
		}
		return m, nil

	case "up", "k":
		m.moveCursor(-1)
		return m, nil

	case "down", "j":
		m.moveCursor(1)
		return m, nil

	case "f":
		// Cycling the filter can change the list length under the cursor.
		m.filter = m.filter.next()
		m.mineCur = clamp(m.mineCur, len(m.visiblePRs()))
		m.message = ""
		return m, nil

	case "r":
		m.message = ""
		return m, m.pollCmd()

	case "enter":
		pr := m.selected()
		if pr == nil || m.busy {
			return m, nil
		}
		if !m.ownsPR(pr) {
			m.message = fmt.Sprintf("#%d is not yours", pr.Number)
			return m, nil
		}
		if err := enqueueable(pr); err != nil {
			m.message = err.Error()
			return m, nil
		}
		m.busy = true
		m.message = fmt.Sprintf("adding #%d to the queue…", pr.Number)
		return m, enqueueCmd(*pr)

	case "d":
		pr := m.selected()
		if pr == nil || m.busy {
			return m, nil
		}
		if !m.ownsPR(pr) {
			m.message = fmt.Sprintf("#%d is not yours", pr.Number)
			return m, nil
		}
		if !pr.inQueue() {
			m.message = fmt.Sprintf("#%d is not in the queue", pr.Number)
			return m, nil
		}
		m.busy = true
		m.message = fmt.Sprintf("removing #%d from the queue…", pr.Number)
		return m, dequeueCmd(*pr)
	}
	return m, nil
}

// notifyChanges raises a notification for each of the viewer's PRs whose state
// changed into one worth acting on.
//
// Scoped to the viewer's own pull requests regardless of the active filter:
// looking at the bot or all filter is not a reason to be interrupted about
// somebody else's merge.
func (m *model) notifyChanges() {
	for i := range m.snap.prs {
		pr := &m.snap.prs[i]
		if !m.snap.owns(pr) {
			continue
		}
		st := classify(pr, m.snap.queue)
		if m.lastKind[pr.Number] == st.kind {
			continue
		}
		known := m.lastKind[pr.Number] != kStartup
		m.lastKind[pr.Number] = st.kind
		m.log("#%d %s", pr.Number, st.headline)
		if st.attention && known {
			notifier(fmt.Sprintf("%s/%s#%d", m.owner, m.name, pr.Number), st.headline)
		}
	}
}

var (
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Faint(true)
	styleOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleActive   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	// Your own entries stand out in a queue that is mostly other people's.
	styleOwn = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	// Labels are neither status nor metadata, so they get their own colour
	// rather than sinking into the dim sublines around them.
	styleLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
)

func (s status) style() lipgloss.Style {
	switch s.kind {
	case kMerged, kReady:
		return styleOK
	case kClosed, kQueuedConflict, kQueuedGroupFail, kQueuedUnknownCause, kConflicting, kChecksFailed:
		return styleErr
	case kDraft, kNeedsReview:
		return styleWarn
	case kQueued:
		return styleActive
	default:
		return styleDim
	}
}

func paneBorder(focused bool) lipgloss.Style {
	s := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	if focused {
		return s.BorderForeground(lipgloss.Color("6"))
	}
	return s.BorderForeground(lipgloss.Color("8"))
}

// wrap breaks a string onto at most maxLines lines of width w, splitting on
// spaces. The last line is truncated if the text still does not fit, so nothing
// is dropped silently without an ellipsis to show it.
func wrap(s string, w, maxLines int) []string {
	if w <= 0 || maxLines <= 0 || s == "" {
		return nil
	}

	words := strings.Fields(s)
	var lines []string
	current := ""
	for _, word := range words {
		candidate := word
		if current != "" {
			candidate = current + " " + word
		}
		if lipgloss.Width(candidate) <= w {
			current = candidate
			continue
		}
		if current == "" {
			// A single word longer than the line: hard-truncate it.
			current = truncate(word, w)
			continue
		}
		lines = append(lines, current)
		current = word
	}
	if current != "" {
		lines = append(lines, current)
	}

	// Dropping the tail silently would read as if the text simply ended, so the
	// last kept line is elided to show that something was cut.
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = truncate(lines[maxLines-1]+"…", w)
	}
	return lines
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	return string([]rune(s)[:w-1]) + "…"
}

func (m model) View() tea.View {
	v := tea.NewView("")
	// The alternate screen is a property of the view in v2, not a program
	// option, so it has to be set on every frame.
	v.AltScreen = true
	if m.quitting {
		return v
	}

	header := styleTitle.Render(fmt.Sprintf("%s/%s", m.owner, m.name)) +
		styleDim.Render("  queue for "+m.base)
	// Showing who gh is authenticated as makes a wrong-account session obvious;
	// otherwise it presents as an empty pull request pane.
	// "pinned" distinguishes a configured account from whichever happened to be
	// active, so the two cases are not confused when debugging.
	if m.snap != nil && m.snap.viewer != "" {
		who := "  ·  @" + m.snap.viewer
		if m.account != "" {
			who += " (pinned)"
		}
		header += styleDim.Render(who)
	}

	// Two panes side by side, splitting the terminal width.
	inner := max((m.width-8)/2-2, 24)
	// Rows must be truncated to the content area, not the pane: the border and
	// the horizontal padding each eat two columns, and anything longer wraps.
	content := inner - 4
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		paneBorder(m.focus == paneQueue).Width(inner).Render(m.queuePane(content)),
		" ",
		paneBorder(m.focus == paneMine).Width(inner).Render(m.minePane(content)),
	)

	var b strings.Builder
	b.WriteString(header + "\n\n")
	b.WriteString(body + "\n")

	if m.pollErr != nil {
		b.WriteString(styleErr.Render("  poll failed: "+m.pollErr.Error()) + "\n")
	}
	if m.message != "" {
		b.WriteString("  " + styleWarn.Render(m.message) + "\n")
	}
	b.WriteString("\n")
	for _, l := range m.logs {
		b.WriteString(styleDim.Render("  "+truncate(l, m.width-4)) + "\n")
	}

	b.WriteString("\n" + m.spinner.View() + styleDim.Render(fmt.Sprintf(
		"tab pane · ↑↓ select · f filter · enter enqueue · d dequeue · r poll · q quit  (%d polls, every %s)",
		m.polls, m.interval)))
	v.SetContent(b.String())
	return v
}

func (m model) queuePane(w int) string {
	slots := m.queueSlots()
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("merge queue (%d)", len(slots))) + "\n\n")

	if m.snap != nil && m.snap.queueErr != nil {
		for _, line := range wrap("cannot read queue: "+m.snap.queueErr.Error(), w, 3) {
			b.WriteString(styleErr.Render(line) + "\n")
		}
		if hint := wrongAccountHint(m.snap.queueErr); hint != "" {
			b.WriteString("\n")
			for _, line := range wrap(hint, w, 4) {
				b.WriteString(styleWarn.Render(line) + "\n")
			}
		}
		return b.String()
	}
	if !m.loaded {
		b.WriteString(styleDim.Render("loading…") + "\n")
		return b.String()
	}
	if len(slots) == 0 {
		b.WriteString(styleDim.Render("empty") + "\n")
		return b.String()
	}

	for i := range slots {
		e := slots[i]
		pr := &e.PullRequest
		own := m.ownsPR(pr)
		marker := " "
		if own {
			marker = "*"
		}
		row := fmt.Sprintf("%s%d  #%d  %s", marker, e.Position, pr.Number, strings.ToLower(e.State))

		// Selection wins over ownership so the cursor stays findable; otherwise
		// your own entries are bold green against everyone else's plain rows.
		switch {
		case m.focus == paneQueue && i == m.queueCur:
			b.WriteString(styleSelected.Render(truncate("› "+row, w)) + "\n")
		case own:
			b.WriteString(styleOwn.Render(truncate("  "+row, w)) + "\n")
		default:
			b.WriteString(truncate("  "+row, w) + "\n")
		}

		title := truncate("     "+pr.Title, w)
		if own {
			b.WriteString(styleOwn.Render(title) + "\n")
		} else {
			b.WriteString(styleDim.Render(title) + "\n")
		}
		// The subline rarely fits one pane-width line, so allow it two rather than
		// cutting off the estimate at the end.
		for _, line := range wrap(queueSubline(e), w-5, 2) {
			b.WriteString(styleDim.Render("     "+line) + "\n")
		}
		for _, line := range wrap(labelLine(pr), w-5, 2) {
			b.WriteString(styleLabel.Render("     "+line) + "\n")
		}
	}
	b.WriteString("\n" + styleOwn.Render("*") + styleDim.Render(truncate(" yours", w)))
	return b.String()
}

func (m model) minePane(w int) string {
	prs := m.visiblePRs()
	var b strings.Builder

	// The active filter is named in the title, so what the pane is showing is
	// never ambiguous.
	b.WriteString(styleTitle.Render(fmt.Sprintf("pull requests: %s (%d)", m.filter, len(prs))) + "\n")
	b.WriteString(styleDim.Render(truncate("f cycles: mine · bots · all", w)) + "\n\n")

	if !m.loaded {
		b.WriteString(styleDim.Render("loading…") + "\n")
		return b.String()
	}
	if len(prs) == 0 {
		b.WriteString(styleDim.Render("none open") + "\n")
		return b.String()
	}

	for i := range prs {
		pr := &prs[i]
		st := classify(pr, m.queueSlots())
		row := fmt.Sprintf("#%d  %s", pr.Number, pr.Title)
		// Outside the mine filter the author matters, since enter and d only work
		// on your own pull requests.
		if m.filter != filterMine && !m.ownsPR(pr) {
			row = fmt.Sprintf("#%d  [%s]  %s", pr.Number, pr.Author.Login, pr.Title)
		}
		if m.focus == paneMine && i == m.mineCur {
			b.WriteString(styleSelected.Render(truncate("› "+row, w)) + "\n")
		} else {
			b.WriteString(truncate("  "+row, w) + "\n")
		}
		b.WriteString("     " + st.style().Render(truncate(st.headline, w-5)) + "\n")
		if st.detail != "" {
			b.WriteString(styleDim.Render(truncate("     "+st.detail, w)) + "\n")
		}
		for _, line := range wrap(labelLine(pr), w-5, 2) {
			b.WriteString(styleLabel.Render("     "+line) + "\n")
		}
	}

	// Never let a capped listing read as the complete picture.
	if m.snap != nil && m.snap.truncated {
		b.WriteString("\n" + styleWarn.Render(truncate(
			fmt.Sprintf("more than %d open PRs; older ones not listed", maxPRPages*50), w)))
	}
	return b.String()
}
