package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestParseRepo(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		wantErr string
	}{
		{name: "valid", repo: "acme/service"},
		{name: "missing", wantErr: "-repo is required"},
		{name: "no slash", repo: "service", wantErr: "is not owner/name"},
		{name: "empty owner", repo: "/service", wantErr: "is not owner/name"},
		{name: "empty name", repo: "pure/", wantErr: "is not owner/name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, name, err := parseRepo(tt.repo)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != "acme" || name != "service" {
				t.Errorf("got %q/%q", owner, name)
			}
		})
	}
}

func captureNotifications(t *testing.T) *[]string {
	t.Helper()
	var got []string
	original := notifier
	notifier = func(title, message string) { got = append(got, title+": "+message) }
	t.Cleanup(func() { notifier = original })
	return &got
}

func testModel() model {
	return newModel(settings{owner: "acme", name: "service", base: "main", interval: time.Minute})
}

// testModelWithBots configures extra bot logins, for accounts GitHub does not
// mark as Bot actors.
func testModelWithBots(logins ...string) model {
	bots := map[string]bool{}
	for _, l := range logins {
		bots[l] = true
	}
	m := testModel()
	m.bots = bots
	return m
}

func send(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	got, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	return got, cmd
}

// snapOf builds a snapshot from PR fixture bodies, with the viewer set so
// ownership resolves.
func snapOf(t *testing.T, bodies ...string) *snapshot {
	t.Helper()
	prs := make([]pullRequest, 0, len(bodies))
	for _, body := range bodies {
		prs = append(prs, *decode(t, body))
	}
	return &snapshot{prs: prs, viewer: testViewer}
}

// loaded returns a model holding one snapshot of the given PRs and queue.
func loaded(t *testing.T, bodies []string, slots []queueSlot) model {
	t.Helper()
	snap := snapOf(t, bodies...)
	snap.queue = slots
	m, _ := send(t, testModel(), snapMsg{snap: snap})
	return m
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}
	// A printable key carries both its code and the text it produced; String()
	// reads Text, which is what handleKey switches on.
	return tea.KeyPressMsg{Code: []rune(s)[0], Text: s}
}

// viewOf renders the model as plain text. In bubbletea v2 View returns a
// tea.View rather than a string, and lipgloss v2 no longer strips colour when
// the output is not a terminal, so the escape codes are removed here: without
// that, styling splits a string like "* yours" and the assertions miss it.
func viewOf(m model) string { return ansi.Strip(m.View().Content) }

func TestNewModelDefaults(t *testing.T) {
	m := testModel()
	if m.focus != paneMine {
		t.Error("focus should start on the pull request pane, which is the actionable one")
	}
	if m.loaded {
		t.Error("a fresh model has not loaded")
	}
	if m.lastKind == nil {
		t.Error("lastKind must be initialised or notifyChanges panics")
	}
}

func TestWindowSizeIsRecorded(t *testing.T) {
	m, _ := send(t, testModel(), tea.WindowSizeMsg{Width: 160, Height: 50})
	if m.width != 160 || m.height != 50 {
		t.Errorf("size = %dx%d", m.width, m.height)
	}
}

func TestZeroWindowSizeIsIgnored(t *testing.T) {
	// A pty with no size (script, some CI runners) reports 0x0, which would
	// collapse the panes to nothing.
	m := testModel()
	before := m.width
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 0, Height: 0})
	if m.width != before {
		t.Errorf("width = %d, want the default %d kept", m.width, before)
	}
	if !strings.Contains(viewOf(m), "merge queue") {
		t.Error("the panes must still render after a zero-size report")
	}
}

func TestSnapshotPopulatesPanes(t *testing.T) {
	m := loaded(t, []string{readyPR, reviewPR}, []queueSlot{
		mustSlot(t, 1, "AWAITING_CHECKS", `{"number":3421,"title":"ahead"}`),
	})

	if !m.loaded || m.polls != 1 {
		t.Errorf("loaded = %v, polls = %d", m.loaded, m.polls)
	}
	if len(m.visiblePRs()) != 2 || len(m.queueSlots()) != 1 {
		t.Errorf("panes wrong: %d mine, %d queued", len(m.visiblePRs()), len(m.queueSlots()))
	}
}

func TestCursorsClampWhenListsShrink(t *testing.T) {
	m := loaded(t, []string{readyPR, reviewPR, draftPR}, nil)
	m.mineCur = 2

	// The next poll finds only one PR left.
	m, _ = send(t, m, snapMsg{snap: snapOf(t, readyPR)})
	if m.mineCur != 0 {
		t.Errorf("mineCur = %d, want clamped to 0", m.mineCur)
	}
}

func TestPollErrorKeepsPolling(t *testing.T) {
	m, cmd := send(t, testModel(), snapMsg{err: errors.New("gh: Could not resolve to a Repository")})

	if m.pollErr == nil {
		t.Error("pollErr should be recorded")
	}
	if cmd == nil {
		t.Error("a failed poll must still schedule the next one")
	}
	if m.quitting {
		t.Error("a failed poll must not quit")
	}
	if !strings.Contains(viewOf(m), "Could not resolve") {
		t.Error("the failure should be visible in the view")
	}
}

func TestFirstObservationDoesNotNotify(t *testing.T) {
	notes := captureNotifications(t)

	// Every PR is new on the first poll; notifying for all of them would be a
	// burst of noise the moment the tool starts.
	loaded(t, []string{realQueuedPR, reviewPR}, nil)

	if len(*notes) != 0 {
		t.Errorf("startup must stay quiet, got %v", *notes)
	}
}

func TestNotifiesOncePerStateChange(t *testing.T) {
	notes := captureNotifications(t)

	healthy := `{"id":"x","number":3424,"state":"OPEN","mergeable":"MERGEABLE",` + mineAuthor + `,
		"mergeQueueEntry":{"state":"AWAITING_CHECKS","position":2}}`
	doomed := `{"id":"x","number":3424,"state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main",
		"headRefName":"topic",` + mineAuthor + `,"mergeQueueEntry":{"state":"UNMERGEABLE","position":2}}`

	m := loaded(t, []string{healthy}, nil)
	if len(*notes) != 0 {
		t.Fatalf("a healthy entry must not notify, got %v", *notes)
	}

	// Turning unmergeable is the event worth interrupting for.
	m, _ = send(t, m, snapMsg{snap: snapOf(t, doomed)})
	if len(*notes) != 1 {
		t.Fatalf("got %d notifications, want 1: %v", len(*notes), *notes)
	}
	if !strings.Contains((*notes)[0], "service#3424") {
		t.Errorf("notification should name the PR: %q", (*notes)[0])
	}

	// Still unmergeable on later polls: no repeat.
	for range 3 {
		m, _ = send(t, m, snapMsg{snap: snapOf(t, doomed)})
	}
	if len(*notes) != 1 {
		t.Errorf("an unchanged state must not re-notify, got %v", *notes)
	}
}

func TestQuietTransitionsDoNotNotify(t *testing.T) {
	notes := captureNotifications(t)

	m := loaded(t, []string{reviewPR}, nil)
	// review required -> ready is a change, but not one worth interrupting for.
	m, _ = send(t, m, snapMsg{snap: snapOf(t, readyPR)})

	if len(*notes) != 0 {
		t.Errorf("becoming ready should not notify, got %v", *notes)
	}
}

func TestFilterDefaultsToMine(t *testing.T) {
	m := loaded(t, []string{readyPR, renovatePR, otherPR}, nil)

	if m.filter != filterMine {
		t.Fatalf("filter = %v, want filterMine", m.filter)
	}
	visible := m.visiblePRs()
	if len(visible) != 1 || visible[0].Number != 3424 {
		t.Errorf("mine should show only my PR, got %+v", visible)
	}
}

func TestFilterCycles(t *testing.T) {
	m := loaded(t, []string{readyPR, renovatePR, otherPR}, nil)

	m, _ = send(t, m, key("f"))
	if m.filter != filterBots {
		t.Fatalf("filter = %v, want filterBots", m.filter)
	}
	visible := m.visiblePRs()
	if len(visible) != 1 || visible[0].Number != 3421 {
		t.Errorf("bots should show only the bot PR, got %+v", visible)
	}

	m, _ = send(t, m, key("f"))
	if m.filter != filterAll {
		t.Fatalf("filter = %v, want filterAll", m.filter)
	}
	if len(m.visiblePRs()) != 3 {
		t.Errorf("all should show every PR, got %d", len(m.visiblePRs()))
	}

	m, _ = send(t, m, key("f"))
	if m.filter != filterMine {
		t.Errorf("the filter should wrap back to mine, got %v", m.filter)
	}
}

func TestFilterCycleClampsCursor(t *testing.T) {
	// all has 3 rows, mine has 1: the cursor must not be left past the end.
	m := loaded(t, []string{readyPR, renovatePR, otherPR}, nil)
	m.filter = filterAll
	m.mineCur = 2

	m, _ = send(t, m, key("f"))
	if m.mineCur != 0 {
		t.Errorf("mineCur = %d, want clamped to 0", m.mineCur)
	}
}

// The guard that must not regress: ownership comes from the authenticated login,
// so widening the filter cannot authorise acting on other people's PRs.
func TestOwnershipSurvivesTheAllFilter(t *testing.T) {
	m := loaded(t, []string{readyPR, renovatePR, otherPR}, nil)
	m.filter = filterAll

	for _, tc := range []struct {
		cursor int
		number int
		owned  bool
	}{
		{0, 3424, true},
		{1, 3421, false},
		{2, 3425, false},
	} {
		prs := m.visiblePRs()
		if prs[tc.cursor].Number != tc.number {
			t.Fatalf("row %d is #%d, want #%d", tc.cursor, prs[tc.cursor].Number, tc.number)
		}
		if got := m.ownsPR(&prs[tc.cursor]); got != tc.owned {
			t.Errorf("ownsPR(#%d) = %v, want %v", tc.number, got, tc.owned)
		}
	}

	// And the actions refuse accordingly.
	m.mineCur = 1
	next, cmd := send(t, m, key("enter"))
	if cmd != nil {
		t.Error("enter must not act on a bot PR even under the all filter")
	}
	if !strings.Contains(next.message, "not yours") {
		t.Errorf("message = %q", next.message)
	}
}

func TestNotificationsStayScopedToMineUnderAllFilter(t *testing.T) {
	notes := captureNotifications(t)

	doomedMine := `{"id":"a","number":3424,"state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main",
		"headRefName":"topic",` + mineAuthor + `,"mergeQueueEntry":{"state":"UNMERGEABLE","position":1}}`
	doomedBot := `{"id":"b","number":3421,"state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main",
		"headRefName":"dep",` + botAuthor + `,"mergeQueueEntry":{"state":"UNMERGEABLE","position":2}}`

	m := loaded(t, []string{readyPR, renovatePR}, nil)
	m.filter = filterAll

	m, _ = send(t, m, snapMsg{snap: snapOf(t, doomedMine, doomedBot)})

	if len(*notes) != 1 {
		t.Fatalf("only my PR should notify, got %v", *notes)
	}
	if !strings.Contains((*notes)[0], "#3424") {
		t.Errorf("notification should be about my PR: %q", (*notes)[0])
	}
}

func TestFilterMineShowsNothingWithoutViewer(t *testing.T) {
	// If the viewer login could not be read, "mine" must show nothing rather than
	// claiming every PR is mine.
	m, _ := send(t, testModel(), snapMsg{snap: &snapshot{
		prs:    []pullRequest{*decode(t, readyPR)},
		viewer: "",
	}})

	if got := m.visiblePRs(); len(got) != 0 {
		t.Errorf("no viewer means nothing is mine, got %+v", got)
	}
	m.filter = filterAll
	if len(m.visiblePRs()) != 1 {
		t.Error("all admits everything regardless of viewer")
	}
}

// A bot running as an ordinary User account is invisible to the __typename
// check, which is why the config exists.
func TestConfiguredBotLoginsCount(t *testing.T) {
	humanBot := `{"id":"h","number":50,"state":"OPEN","mergeable":"MERGEABLE",
		"author":{"login":"release-bot","__typename":"User"}}`

	// Without config, a User-account bot is not recognised.
	m := testModel()
	m, _ = send(t, m, snapMsg{snap: snapOf(t, humanBot, renovatePR)})
	m.filter = filterBots
	if got := m.visiblePRs(); len(got) != 1 || got[0].Number != 3421 {
		t.Errorf("only the App bot should match by default, got %+v", got)
	}

	// Configured, it is.
	withBots := testModelWithBots("release-bot")
	withBots, _ = send(t, withBots, snapMsg{snap: snapOf(t, humanBot, renovatePR)})
	withBots.filter = filterBots
	if got := withBots.visiblePRs(); len(got) != 2 {
		t.Errorf("a configured login should count as a bot, got %+v", got)
	}
}

func TestConfiguredBotDoesNotAffectOwnership(t *testing.T) {
	// Listing your own login as a bot must not hand away your PRs, nor grant
	// action rights over anyone else's.
	m := testModelWithBots(testViewer, "deps-bot")
	m, _ = send(t, m, snapMsg{snap: snapOf(t, readyPR, renovatePR)})

	mine := decode(t, readyPR)
	bot := decode(t, renovatePR)
	if !m.ownsPR(mine) {
		t.Error("my PR is still mine even if my login is listed as a bot")
	}
	if m.ownsPR(bot) {
		t.Error("a configured bot login must not become owned")
	}
}

func TestFilterNames(t *testing.T) {
	if filterMine.String() != "mine" || filterBots.String() != "bots" || filterAll.String() != "all" {
		t.Error("filter names are shown in the pane title and must be stable")
	}
}

func TestTabSwitchesPane(t *testing.T) {
	m := testModel()
	m, _ = send(t, m, key("tab"))
	if m.focus != paneQueue {
		t.Error("tab should move to the queue pane")
	}
	m, _ = send(t, m, key("tab"))
	if m.focus != paneMine {
		t.Error("tab should move back")
	}
}

func TestCursorMovementStaysInBounds(t *testing.T) {
	m := loaded(t, []string{readyPR, reviewPR}, nil)

	m, _ = send(t, m, key("up"))
	if m.mineCur != 0 {
		t.Errorf("cursor went above the first row: %d", m.mineCur)
	}
	m, _ = send(t, m, key("down"))
	m, _ = send(t, m, key("down"))
	m, _ = send(t, m, key("down"))
	if m.mineCur != 1 {
		t.Errorf("cursor went past the last row: %d", m.mineCur)
	}
	if _, cmd := send(t, m, key("j")); cmd != nil {
		t.Error("moving the cursor should not issue a command")
	}
}

func TestCursorMovementWithEmptyPane(t *testing.T) {
	m := testModel()
	m, _ = send(t, m, key("down"))
	if m.mineCur != 0 {
		t.Errorf("an empty pane must keep the cursor at 0, got %d", m.mineCur)
	}
	if m.selected() != nil {
		t.Error("nothing can be selected in an empty pane")
	}
}

func TestEnterEnqueuesReadyPR(t *testing.T) {
	calls := fakeGH(t, always(`{"data":{}}`))
	m := loaded(t, []string{readyPR}, nil)

	m, cmd := send(t, m, key("enter"))
	if !m.busy {
		t.Error("the model should be busy while the mutation runs")
	}
	if cmd == nil {
		t.Fatal("enter should issue the enqueue command")
	}

	msg, ok := cmd().(actionMsg)
	if !ok {
		t.Fatalf("command produced %T, want actionMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("unexpected error: %v", msg.err)
	}
	if !strings.Contains(msg.text, "#3424") {
		t.Errorf("message should name the PR: %q", msg.text)
	}
	if len(*calls) != 1 || !strings.Contains(strings.Join((*calls)[0], " "), "enqueuePullRequest") {
		t.Errorf("expected one enqueue call, got %v", *calls)
	}
}

func TestEnterRefusesWhatItIsSureAbout(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"draft", draftPR, "is a draft"},
		{"already queued", realQueuedPR, "already in the queue"},
		{
			"conflicts with base",
			`{"id":"x","number":7,"state":"OPEN","mergeable":"CONFLICTING","baseRefName":"main","headRefName":"topic",` + mineAuthor + `}`,
			"rebase topic first",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No fakeGH: any API call here would fail the test by panicking on a
			// real gh invocation, which is the point.
			m := loaded(t, []string{tt.body}, nil)
			m, cmd := send(t, m, key("enter"))

			if cmd != nil {
				t.Error("a refusal must not issue a command")
			}
			if m.busy {
				t.Error("a refusal must not mark the model busy")
			}
			if !strings.Contains(m.message, tt.want) {
				t.Errorf("message = %q, want one containing %q", m.message, tt.want)
			}
		})
	}
}

func TestActionsRefuseSomebodyElsesPR(t *testing.T) {
	// The queue pane shows everyone's entries; acting on another person's PR would
	// be interfering with their merge.
	m := loaded(t, []string{readyPR}, []queueSlot{
		mustSlot(t, 1, "AWAITING_CHECKS", `{"id":"other","number":9999,"title":"not mine"}`),
	})
	m.focus = paneQueue

	for _, k := range []string{"enter", "d"} {
		next, cmd := send(t, m, key(k))
		if cmd != nil {
			t.Errorf("%q must not act on another person's PR", k)
		}
		if !strings.Contains(next.message, "not yours") {
			t.Errorf("%q message = %q", k, next.message)
		}
	}
}

func TestDequeueFromQueuePane(t *testing.T) {
	calls := fakeGH(t, always(`{"data":{}}`))

	// The PR is both mine and in the queue, so the queue pane can act on it.
	m := loaded(t, []string{realQueuedPR}, []queueSlot{
		mustSlot(t, 2, "UNMERGEABLE", realQueuedPR),
	})
	m.focus = paneQueue

	m, cmd := send(t, m, key("d"))
	if cmd == nil {
		t.Fatal("d should issue the dequeue command")
	}
	msg := cmd().(actionMsg)
	if msg.err != nil {
		t.Fatalf("unexpected error: %v", msg.err)
	}
	if !strings.Contains(msg.text, "removed #3424") {
		t.Errorf("message = %q", msg.text)
	}
	if !strings.Contains(strings.Join((*calls)[0], " "), "dequeuePullRequest") {
		t.Errorf("wrong mutation: %v", (*calls)[0])
	}
}

func TestDequeueRefusesUnqueuedPR(t *testing.T) {
	m := loaded(t, []string{readyPR}, nil)

	m, cmd := send(t, m, key("d"))
	if cmd != nil {
		t.Error("a refusal must not issue a command")
	}
	if !strings.Contains(m.message, "not in the queue") {
		t.Errorf("message = %q", m.message)
	}
}

func TestMutationCommandsReportFailure(t *testing.T) {
	fakeGH(t, func(string) ghReply {
		return ghReply{stderr: "gh: Pull request is in an unmergeable state", exit: 1}
	})
	pr := *decode(t, readyPR)

	for name, cmd := range map[string]tea.Cmd{
		"enqueue": enqueueCmd(pr),
		"dequeue": dequeueCmd(pr),
	} {
		t.Run(name, func(t *testing.T) {
			msg, ok := cmd().(actionMsg)
			if !ok {
				t.Fatalf("produced %T, want actionMsg", cmd())
			}
			if msg.err == nil {
				t.Fatal("want the failure carried back")
			}
			if !strings.Contains(msg.text, "#3424") {
				t.Errorf("text should name the PR: %q", msg.text)
			}
		})
	}
}

func TestCursorMovesInQueuePane(t *testing.T) {
	m := loaded(t, []string{readyPR}, []queueSlot{
		mustSlot(t, 1, "QUEUED", `{"number":1,"title":"one"}`),
		mustSlot(t, 2, "QUEUED", `{"number":2,"title":"two"}`),
	})
	m.focus = paneQueue

	m, _ = send(t, m, key("down"))
	if m.queueCur != 1 {
		t.Errorf("queueCur = %d, want 1", m.queueCur)
	}
	if got := m.selected(); got == nil || got.Number != 2 {
		t.Errorf("selected() = %+v, want PR 2", got)
	}
	// The other pane's cursor must not move with it.
	if m.mineCur != 0 {
		t.Errorf("mineCur = %d, want 0", m.mineCur)
	}
}

func TestActionResultRepolls(t *testing.T) {
	m := loaded(t, []string{readyPR}, nil)
	m.busy = true

	m, cmd := send(t, m, actionMsg{text: "added #3424 to the queue"})
	if m.busy {
		t.Error("the action is finished, so busy must clear")
	}
	if cmd == nil {
		t.Error("a completed action should re-poll so the panes catch up")
	}
	if !strings.Contains(m.message, "added #3424") {
		t.Errorf("message = %q", m.message)
	}
}

func TestActionErrorIsShown(t *testing.T) {
	m := loaded(t, []string{readyPR}, nil)

	m, _ = send(t, m, actionMsg{
		text: "could not enqueue #3424",
		err:  errors.New("gh: Pull request is in an unmergeable state"),
	})
	if !strings.Contains(m.message, "unmergeable state") {
		t.Errorf("the message should carry the cause: %q", m.message)
	}
}

func TestKeysQuitAndRefresh(t *testing.T) {
	m, cmd := send(t, testModel(), key("q"))
	if !m.quitting || cmd == nil {
		t.Error("q must quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("q produced %T, want tea.QuitMsg", cmd())
	}

	m, cmd = send(t, testModel(), key("r"))
	if m.quitting {
		t.Error("r must not quit")
	}
	if cmd == nil {
		t.Error("r must trigger a poll")
	}

	if m, _ := send(t, testModel(), key("z")); m.quitting {
		t.Error("an unbound key must do nothing")
	}
}

func TestTickTriggersPoll(t *testing.T) {
	if _, cmd := send(t, testModel(), tickMsg{}); cmd == nil {
		t.Error("a tick must trigger a poll")
	}
}

func TestPollCommandProducesSnapshot(t *testing.T) {
	fakeGH(t, routeByQuery(searchEnvelope(readyPR), queueEnvelope()))

	msg, ok := testModel().pollCmd()().(snapMsg)
	if !ok {
		t.Fatal("pollCmd must produce a snapMsg")
	}
	if msg.err != nil {
		t.Fatalf("unexpected error: %v", msg.err)
	}
	if len(msg.snap.prs) != 1 {
		t.Errorf("snapshot should hold the PR, got %d", len(msg.snap.prs))
	}
}

func TestWaitYieldsTick(t *testing.T) {
	m := testModel()
	m.interval = time.Millisecond
	if _, ok := m.wait()().(tickMsg); !ok {
		t.Error("wait must produce a tickMsg")
	}
}

func TestInitStartsSpinnerAndPoll(t *testing.T) {
	if testModel().Init() == nil {
		t.Error("Init must return a command")
	}
}

func TestSpinnerTickKeepsAnimating(t *testing.T) {
	m := testModel()
	m, cmd := send(t, m, m.spinner.Tick())
	if cmd == nil {
		t.Error("a spinner tick must schedule the next frame")
	}
	if m.quitting {
		t.Error("a spinner tick must not quit")
	}
}

func TestLogIsBounded(t *testing.T) {
	m := testModel()
	for i := range 30 {
		m.log("entry %d", i)
	}
	if len(m.logs) != 6 {
		t.Fatalf("len(logs) = %d, want 6", len(m.logs))
	}
	if !strings.Contains(m.logs[5], "entry 29") {
		t.Errorf("newest entry lost: %q", m.logs[5])
	}
}

func TestViewBeforeFirstPoll(t *testing.T) {
	out := viewOf(testModel())
	if !strings.Contains(out, "loading") {
		t.Errorf("both panes should say they are loading:\n%s", out)
	}
	if !strings.Contains(out, "acme/service") {
		t.Errorf("header should name the repo:\n%s", out)
	}
	if !strings.Contains(out, "queue for main") {
		t.Errorf("header should name the base branch:\n%s", out)
	}
}

func TestViewRendersBothPanes(t *testing.T) {
	m := loaded(t, []string{readyPR, reviewPR}, []queueSlot{
		mustSlot(t, 1, "AWAITING_CHECKS", `{"number":3421,"title":"raise test timeouts"}`),
		mustSlot(t, 2, "UNMERGEABLE", realQueuedPR),
	})
	m.width = 200

	out := viewOf(m)
	for _, want := range []string{
		"merge queue (2)",
		"pull requests: mine (2)",
		"#3421",
		"awaiting_checks",
		"ready to enqueue",
		"review required",
		"enter enqueue",
		"d dequeue",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q:\n%s", want, out)
		}
	}
}

func TestViewMarksOwnQueueEntries(t *testing.T) {
	m := loaded(t, []string{realQueuedPR}, []queueSlot{
		mustSlot(t, 1, "AWAITING_CHECKS", `{"number":9999,"title":"someone else"}`),
		mustSlot(t, 2, "UNMERGEABLE", realQueuedPR),
	})
	m.width = 200

	out := viewOf(m)
	if !strings.Contains(out, "* yours") {
		t.Errorf("the legend should explain the marker:\n%s", out)
	}
	if !strings.Contains(out, "*2  #3424") {
		t.Errorf("own entry should be marked:\n%s", out)
	}
	if strings.Contains(out, "*1  #9999") {
		t.Errorf("another person's entry must not be marked:\n%s", out)
	}
}

func TestLabelLine(t *testing.T) {
	if got := labelLine(decode(t, readyPR)); got != "[skip-e2e] [area/auth]" {
		t.Errorf("labelLine() = %q", got)
	}
	// An unlabelled PR must render nothing at all, not an empty bracket pair or
	// a blank row eating a line of the pane.
	if got := labelLine(decode(t, reviewPR)); got != "" {
		t.Errorf("labelLine() on an unlabelled PR = %q, want empty", got)
	}
}

func TestViewShowsLabelsInBothPanes(t *testing.T) {
	labelled := `{"id":"PR_q","number":3421,"title":"raise test timeouts",
	"labels":{"nodes":[{"name":"skip-vcluster"}]}}`
	m := loaded(t, []string{readyPR}, []queueSlot{mustSlot(t, 1, "AWAITING_CHECKS", labelled)})
	m.width = 200

	out := viewOf(m)
	if !strings.Contains(out, "[skip-vcluster]") {
		t.Errorf("a queued entry's labels should show:\n%s", out)
	}
	if !strings.Contains(out, "[skip-e2e] [area/auth]") {
		t.Errorf("a listed pull request's labels should show:\n%s", out)
	}
}

func TestViewShowsEmptyQueue(t *testing.T) {
	m := loaded(t, []string{readyPR}, nil)
	if !strings.Contains(viewOf(m), "empty") {
		t.Errorf("an empty queue should say so:\n%s", viewOf(m))
	}
}

func TestViewShowsAuthenticatedLogin(t *testing.T) {
	// A wrong-account session otherwise presents as an empty PR pane, so the
	// header names who gh is acting as.
	m := loaded(t, []string{readyPR}, nil)
	if !strings.Contains(viewOf(m), "@"+testViewer) {
		t.Errorf("header should name the viewer:\n%s", viewOf(m))
	}

	// Before the first poll there is no login to show, and it must not render "@".
	if strings.Contains(viewOf(testModel()), "·  @") {
		t.Error("an unknown viewer must not render an empty handle")
	}
}

func TestHeaderMarksAPinnedAccount(t *testing.T) {
	// Unpinned: the login shows, with nothing implying it was chosen.
	m := loaded(t, []string{readyPR}, nil)
	if out := viewOf(m); strings.Contains(out, "(pinned)") {
		t.Errorf("an unpinned session must not claim to be pinned:\n%s", out)
	}

	// Pinned: say so, since "the active account happens to be right" and "this
	// session is nailed to an account" behave differently under gh auth switch.
	m.account = "alice"
	out := viewOf(m)
	if !strings.Contains(out, "@"+testViewer) || !strings.Contains(out, "(pinned)") {
		t.Errorf("a pinned session should be marked:\n%s", out)
	}
}

func TestConfigSourcesAreLogged(t *testing.T) {
	m := newModel(settings{
		owner: "acme", name: "service", base: "main", interval: time.Minute,
		sources: []string{"/home/alice/.config/mqw/config.toml", "/work/repo/.mqw.toml"},
	})

	logs := strings.Join(m.logs, "\n")
	if !strings.Contains(logs, ".mqw.toml") || !strings.Contains(logs, "config.toml") {
		t.Errorf("both config files should be logged:\n%s", logs)
	}
}

func TestWrongAccountHint(t *testing.T) {
	notFound := errors.New("gh api graphql: gh: Could not resolve to a Repository with the name 'acme/service'.")
	hint := wrongAccountHint(notFound)
	if !strings.Contains(hint, "gh auth switch") {
		t.Errorf("hint should name the fix: %q", hint)
	}
	if !strings.Contains(hint, "empty pull request pane") {
		t.Errorf("hint should explain the empty pane, the confusing half: %q", hint)
	}

	// Unrelated failures must not be blamed on the account.
	for _, err := range []error{nil, errors.New("gh: connection refused")} {
		if got := wrongAccountHint(err); got != "" {
			t.Errorf("wrongAccountHint(%v) = %q, want empty", err, got)
		}
	}
}

func TestViewShowsWrongAccountHint(t *testing.T) {
	m, _ := send(t, testModel(), snapMsg{snap: &snapshot{
		viewer:   "alice",
		queueErr: errors.New("gh api graphql: gh: Could not resolve to a Repository with the name 'acme/service'."),
	}})
	m.width = 200

	out := viewOf(m)
	if !strings.Contains(out, "cannot read queue") {
		t.Errorf("the failure should be visible:\n%s", out)
	}
	// A short fragment: the full hint wraps, so longer phrases straddle lines.
	if !strings.Contains(out, "wrong account") {
		t.Errorf("the hint should be shown alongside it:\n%s", out)
	}
}

func TestViewShowsQueueReadFailure(t *testing.T) {
	m, _ := send(t, testModel(), snapMsg{snap: &snapshot{
		prs:      []pullRequest{*decode(t, readyPR)},
		viewer:   testViewer,
		queueErr: errors.New("gh: boom"),
	}})
	m.width = 200

	out := viewOf(m)
	if !strings.Contains(out, "cannot read queue") {
		t.Errorf("a queue failure should be visible:\n%s", out)
	}
	if !strings.Contains(out, "ready to enqueue") {
		t.Errorf("the PR pane should still render:\n%s", out)
	}
}

func TestViewShowsNoOpenPRs(t *testing.T) {
	m := loaded(t, nil, nil)
	if !strings.Contains(viewOf(m), "none open") {
		t.Errorf("an empty PR list should say so:\n%s", viewOf(m))
	}
}

func TestViewIsEmptyOnQuit(t *testing.T) {
	m, _ := send(t, testModel(), key("q"))
	if viewOf(m) != "" {
		t.Error("the alt screen should be left clean on quit")
	}
}

// freezeClock pins timeNow so relative times are deterministic.
func freezeClock(t *testing.T, at time.Time) {
	t.Helper()
	original := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = original })
}

func TestHumanAgo(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	freezeClock(t, now)

	tests := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{90 * time.Second, "1 minute ago"},
		{20 * time.Minute, "20 minutes ago"},
		{75 * time.Minute, "about 1 hour ago"},
		{5 * time.Hour, "about 5 hours ago"},
		{30 * time.Hour, "1 day ago"},
		{72 * time.Hour, "3 days ago"},
	}
	for _, tt := range tests {
		ts := now.Add(-tt.ago).Format(time.RFC3339)
		if got := humanAgo(ts); got != tt.want {
			t.Errorf("humanAgo(%s ago) = %q, want %q", tt.ago, got, tt.want)
		}
	}

	// An absent or malformed timestamp yields nothing, so the caller drops it
	// rather than rendering "enqueued ".
	if got := humanAgo(""); got != "" {
		t.Errorf("humanAgo(\"\") = %q, want empty", got)
	}
	if got := humanAgo("not a date"); got != "" {
		t.Errorf("humanAgo(garbage) = %q, want empty", got)
	}
}

func TestQueueSubline(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	freezeClock(t, now)

	eta := 2987
	e := queueSlot{
		Position:             1,
		State:                "AWAITING_CHECKS",
		EnqueuedAt:           now.Add(-70 * time.Minute).Format(time.RFC3339),
		EstimatedTimeToMerge: &eta,
		Enqueuer:             author{Login: "carol", TypeName: "User"},
		PullRequest:          *decode(t, renovatePR),
	}

	got := queueSubline(e)
	want := "opened by @deps-bot • enqueued by @carol • enqueued about 1 hour ago • estimated merge in 49m47s"
	if got != want {
		t.Errorf("queueSubline() =\n  %q\nwant\n  %q", got, want)
	}
}

func TestQueueSublineOmitsMissingFields(t *testing.T) {
	// The queue read can come back without an enqueuer or an estimate; the line
	// must not show empty fragments.
	e := queueSlot{Position: 1, PullRequest: *decode(t, `{"number":1,"title":"x"}`)}
	if got := queueSubline(e); got != "" {
		t.Errorf("an entry with nothing known should yield no subline, got %q", got)
	}

	e.PullRequest.Author = author{Login: "someone", TypeName: "User"}
	if got := queueSubline(e); got != "opened by @someone" {
		t.Errorf("queueSubline() = %q", got)
	}
}

func TestViewRendersQueueSubline(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	freezeClock(t, now)

	eta := 2987
	m := loaded(t, []string{readyPR}, []queueSlot{{
		Position:             1,
		State:                "AWAITING_CHECKS",
		EnqueuedAt:           now.Add(-70 * time.Minute).Format(time.RFC3339),
		EstimatedTimeToMerge: &eta,
		Enqueuer:             author{Login: "carol", TypeName: "User"},
		PullRequest:          *decode(t, renovatePR),
	}})
	// Wide enough that the whole subline fits; truncation is tested separately.
	m.width = 320

	out := viewOf(m)
	for _, want := range []string{
		"opened by @deps-bot",
		"enqueued by @carol",
		"enqueued about 1 hour ago",
		"estimated merge in 49m47s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("queue pane missing %q:\n%s", want, out)
		}
	}
}

func TestWrap(t *testing.T) {
	if got := wrap("one two three", 20, 2); len(got) != 1 || got[0] != "one two three" {
		t.Errorf("text that fits should stay on one line: %q", got)
	}

	got := wrap("alpha beta gamma delta", 12, 2)
	if len(got) != 2 || got[0] != "alpha beta" || got[1] != "gamma delta" {
		t.Errorf("wrap() = %q", got)
	}

	// Beyond maxLines the remainder is dropped rather than growing the pane, but
	// the cut must be visible: a silent drop reads as if the text just ended.
	got = wrap("alpha beta gamma delta epsilon zeta", 12, 2)
	if len(got) != 2 {
		t.Fatalf("wrap must respect maxLines, got %d lines: %q", len(got), got)
	}
	if !strings.HasSuffix(got[1], "…") {
		t.Errorf("a dropped tail must be elided: %q", got)
	}

	// A single word longer than the line is truncated with an ellipsis, so it is
	// visibly cut rather than silently mangled.
	got = wrap("supercalifragilistic", 8, 2)
	if len(got) != 1 || !strings.HasSuffix(got[0], "…") {
		t.Errorf("wrap() = %q, want one elided line", got)
	}

	for _, bad := range []struct{ w, lines int }{{0, 2}, {10, 0}} {
		if got := wrap("text", bad.w, bad.lines); got != nil {
			t.Errorf("wrap(w=%d, lines=%d) = %q, want nil", bad.w, bad.lines, got)
		}
	}
	if got := wrap("", 10, 2); got != nil {
		t.Errorf("wrap(\"\") = %q, want nil", got)
	}
}

// The subline is longer than a pane is wide, so it must wrap rather than lose
// the merge estimate off the end.
func TestQueueSublineWrapsRatherThanTruncating(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	freezeClock(t, now)

	eta := 2987
	m := loaded(t, []string{readyPR}, []queueSlot{{
		Position:             1,
		State:                "AWAITING_CHECKS",
		EnqueuedAt:           now.Add(-70 * time.Minute).Format(time.RFC3339),
		EstimatedTimeToMerge: &eta,
		Enqueuer:             author{Login: "carol", TypeName: "User"},
		PullRequest:          *decode(t, renovatePR),
	}})
	// A width where the subline needs two lines but fits in them.
	m.width = 160

	out := viewOf(m)
	if !strings.Contains(out, "opened by @deps-bot") {
		t.Errorf("first part of the subline missing:\n%s", out)
	}
	if !strings.Contains(out, "estimated merge in 49m47s") {
		t.Errorf("the estimate should survive by wrapping:\n%s", out)
	}
	if strings.Contains(out, "in 49m47s…") {
		t.Errorf("nothing was cut, so nothing should be elided:\n%s", out)
	}
}

// Narrower than two lines can hold: the estimate is lost, but visibly.
func TestQueueSublineElidesWhenTooNarrow(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	freezeClock(t, now)

	eta := 2987
	m := loaded(t, []string{readyPR}, []queueSlot{{
		Position:             1,
		State:                "AWAITING_CHECKS",
		EnqueuedAt:           now.Add(-70 * time.Minute).Format(time.RFC3339),
		EstimatedTimeToMerge: &eta,
		Enqueuer:             author{Login: "carol", TypeName: "User"},
		PullRequest:          *decode(t, renovatePR),
	}})
	m.width = 120

	out := viewOf(m)
	if !strings.Contains(out, "…") {
		t.Errorf("a cut subline must be elided rather than just ending:\n%s", out)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("hello world", 6); got != "hello…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("hello", 0); got != "" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("hello", 1); got != "…" {
		t.Errorf("truncate = %q", got)
	}
}

func TestClamp(t *testing.T) {
	cases := []struct{ i, length, want int }{
		{0, 0, 0}, {5, 0, 0}, {-1, 3, 0}, {1, 3, 1}, {9, 3, 2},
	}
	for _, c := range cases {
		if got := clamp(c.i, c.length); got != c.want {
			t.Errorf("clamp(%d, %d) = %d, want %d", c.i, c.length, got, c.want)
		}
	}
}

func TestStyleCoversEveryKind(t *testing.T) {
	kinds := []statusKind{
		kStartup, kMerged, kClosed, kDraft, kQueued, kQueuedConflict, kQueuedGroupFail,
		kQueuedUnknownCause, kConflicting, kChecksFailed, kNeedsReview, kReady, kPending,
	}
	for _, k := range kinds {
		if got := (status{kind: k}).style().Render("x"); !strings.Contains(got, "x") {
			t.Errorf("kind %v dropped its text: %q", k, got)
		}
	}
}

func TestHelpTextCoversFlagsAndKeys(t *testing.T) {
	for _, want := range []string{
		"-repo", "-base", "-interval",
		"enter", "tab", "d ", "q ",
		"gh auth switch",
		"rebasing onto the base changes",
	} {
		if !strings.Contains(helpText, want) {
			t.Errorf("help text should mention %q", want)
		}
	}
}
