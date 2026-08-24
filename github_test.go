package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The helper-process pattern: tests re-exec the test binary with
// GO_WANT_HELPER_PROCESS set, and it plays back canned stdout/stderr/exit code in
// place of gh. This exercises the real exec and error handling rather than
// stubbing the fetch functions out.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	fmt.Fprint(os.Stdout, os.Getenv("HELPER_STDOUT"))
	fmt.Fprint(os.Stderr, os.Getenv("HELPER_STDERR"))
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT"))
	os.Exit(code)
}

type ghReply struct {
	stdout string
	stderr string
	exit   int
}

// fakeGH routes each gh invocation through route, which sees the GraphQL query so
// it can answer the search, queue and mutation calls differently. It returns a
// pointer to the recorded argv of every call.
func fakeGH(t *testing.T, route func(query string) ghReply) *[][]string {
	t.Helper()

	var calls [][]string
	original := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))

		var query string
		for _, a := range args {
			if strings.HasPrefix(a, "query=") {
				query = a
			}
		}
		reply := route(query)

		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
		cmd.Env = []string{
			"GO_WANT_HELPER_PROCESS=1",
			"HELPER_STDOUT=" + reply.stdout,
			"HELPER_STDERR=" + reply.stderr,
			"HELPER_EXIT=" + strconv.Itoa(reply.exit),
		}
		return cmd
	}
	t.Cleanup(func() { execCommand = original })
	return &calls
}

// always answers every call with the same payload.
func always(stdout string) func(string) ghReply {
	return func(string) ghReply { return ghReply{stdout: stdout} }
}

func searchEnvelope(prs ...string) string {
	return searchPage("", false, prs...)
}

// searchPage builds a search response with explicit pagination state.
func searchPage(cursor string, hasNext bool, prs ...string) string {
	return fmt.Sprintf(
		`{"data":{"viewer":{"login":%q},"search":{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}}}`,
		testViewer, hasNext, cursor, strings.Join(prs, ","))
}

func queueEnvelope(slots ...string) string {
	return `{"data":{"repository":{"mergeQueue":{"entries":{"nodes":[` + strings.Join(slots, ",") + `]}}}}}`
}

func slot(position int, state, pr string) string {
	return fmt.Sprintf(`{"position":%d,"state":%q,"pullRequest":%s}`, position, state, pr)
}

// routeByQuery answers the search and queue queries separately, which is what
// poll() needs.
func routeByQuery(search, queue string) func(string) ghReply {
	return func(q string) ghReply {
		if strings.Contains(q, "search(") {
			return ghReply{stdout: search}
		}
		return ghReply{stdout: queue}
	}
}

// pinToken sets the pinned token for one test.
func pinToken(t *testing.T, token string) {
	t.Helper()
	original := ghToken
	ghToken = token
	t.Cleanup(func() { ghToken = original })
}

// The whole point of pinning: gh prefers GH_TOKEN over its active account, so the
// token has to reach the child process.
func TestGhCommandCarriesPinnedToken(t *testing.T) {
	pinToken(t, "gho_pinned")

	cmd := ghCommand("api", "graphql")
	var found bool
	for _, kv := range cmd.Env {
		if kv == "GH_TOKEN=gho_pinned" {
			found = true
		}
	}
	if !found {
		t.Error("GH_TOKEN must be set on the gh child process")
	}
	// The rest of the environment has to survive, or gh loses PATH and HOME.
	if len(cmd.Env) < 2 {
		t.Errorf("the inherited environment was replaced: %v", cmd.Env)
	}
}

func TestGhCommandLeavesEnvAloneWithoutToken(t *testing.T) {
	pinToken(t, "")

	// A nil Env means "inherit", which is what we want when no account is pinned:
	// gh then uses whichever account is active.
	if cmd := ghCommand("api"); cmd.Env != nil {
		t.Errorf("Env = %v, want nil so it is inherited", cmd.Env)
	}
}

func TestResolveToken(t *testing.T) {
	calls := fakeGH(t, always("gho_thetoken\n"))

	token, err := resolveToken("alice")
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if token != "gho_thetoken" {
		t.Errorf("token = %q, want the trailing newline trimmed", token)
	}
	got := strings.Join((*calls)[0], " ")
	for _, want := range []string{"auth", "token", "--user", "alice"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q: %s", want, got)
		}
	}
}

func TestResolveTokenErrors(t *testing.T) {
	// An account gh does not know about.
	fakeGH(t, func(string) ghReply {
		return ghReply{stderr: "no oauth token for account nobody", exit: 1}
	})
	if _, err := resolveToken("nobody"); err == nil {
		t.Fatal("want an error for an unknown account")
	} else if !strings.Contains(err.Error(), "nobody") {
		t.Errorf("the error should name the account: %v", err)
	}
}

func TestResolveTokenRejectsEmptyOutput(t *testing.T) {
	fakeGH(t, always("\n"))

	if _, err := resolveToken("alice"); err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("err = %v, want an empty-token error", err)
	}
}

func TestFetchOpenPRsDecodesAndFiltersNonPRs(t *testing.T) {
	// Search returns Issue nodes too, which decode to a zero pullRequest.
	fakeGH(t, always(searchEnvelope(readyPR, `{}`, draftPR)))

	prs, viewer, truncated, err := fetchOpenPRs("acme", "service")
	if err != nil {
		t.Fatalf("fetchOpenPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("got %d PRs, want 2 (the empty node must be dropped)", len(prs))
	}
	if prs[0].Number != 3424 || prs[0].ID == "" {
		t.Errorf("first PR decoded wrong: %+v", prs[0])
	}
	if viewer != testViewer {
		t.Errorf("viewer = %q, want %q — ownership depends on it", viewer, testViewer)
	}
	if truncated {
		t.Error("a single complete page is not truncated")
	}
}

func TestFetchOpenPRsDecodesAuthor(t *testing.T) {
	fakeGH(t, always(searchEnvelope(readyPR, renovatePR)))

	prs, _, _, err := fetchOpenPRs("o", "r")
	if err != nil {
		t.Fatalf("fetchOpenPRs: %v", err)
	}
	if prs[0].Author.Login != testViewer || prs[0].Author.isBot() {
		t.Errorf("human author decoded wrong: %+v", prs[0].Author)
	}
	if prs[1].Author.Login != "deps-bot" || !prs[1].Author.isBot() {
		t.Errorf("bot author decoded wrong: %+v", prs[1].Author)
	}
}

// The search is scoped to the repo and to open PRs, but deliberately not to an
// author: filtering happens client-side so one query serves every filter and
// ownership never depends on the active filter.
func TestFetchOpenPRsScopesSearchToRepoOnly(t *testing.T) {
	calls := fakeGH(t, always(searchEnvelope(readyPR)))

	if _, _, _, err := fetchOpenPRs("someowner", "somerepo"); err != nil {
		t.Fatalf("fetchOpenPRs: %v", err)
	}

	got := strings.Join((*calls)[0], " ")
	for _, want := range []string{"repo:someowner/somerepo", "is:pr", "is:open", "viewer", "author"} {
		if !strings.Contains(got, want) {
			t.Errorf("search query missing %q\nargv: %s", want, got)
		}
	}
	if strings.Contains(got, "author:@me") {
		t.Error("the search must not be scoped to one author")
	}
}

func TestFetchOpenPRsPaginates(t *testing.T) {
	page := 0
	calls := fakeGH(t, func(string) ghReply {
		page++
		if page == 1 {
			return ghReply{stdout: searchPage("CURSOR1", true, readyPR)}
		}
		return ghReply{stdout: searchPage("", false, renovatePR)}
	})

	prs, _, truncated, err := fetchOpenPRs("o", "r")
	if err != nil {
		t.Fatalf("fetchOpenPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("both pages should be collected, got %d", len(prs))
	}
	if truncated {
		t.Error("pagination completed, so nothing was truncated")
	}
	if len(*calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(*calls))
	}
	if !strings.Contains(strings.Join((*calls)[1], " "), "after=CURSOR1") {
		t.Errorf("the second call should pass the cursor: %v", (*calls)[1])
	}
	if strings.Contains(strings.Join((*calls)[0], " "), "after=") {
		t.Errorf("the first call must not pass a cursor: %v", (*calls)[0])
	}
}

// A capped listing must be reported, never silently presented as complete.
func TestFetchOpenPRsReportsTruncation(t *testing.T) {
	calls := fakeGH(t, func(string) ghReply {
		return ghReply{stdout: searchPage("MORE", true, readyPR)}
	})

	_, _, truncated, err := fetchOpenPRs("o", "r")
	if err != nil {
		t.Fatalf("fetchOpenPRs: %v", err)
	}
	if !truncated {
		t.Error("an endless listing must report as truncated")
	}
	if len(*calls) != maxPRPages {
		t.Errorf("want %d calls before giving up, got %d", maxPRPages, len(*calls))
	}
}

func TestFetchQueueDecodesEntries(t *testing.T) {
	fakeGH(t, always(queueEnvelope(
		slot(1, "AWAITING_CHECKS", `{"number":3421,"title":"first"}`),
		slot(2, "QUEUED", `{"number":3425,"title":"second"}`),
	)))

	slots, err := fetchQueue("o", "r", "main")
	if err != nil {
		t.Fatalf("fetchQueue: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("got %d entries, want 2", len(slots))
	}
	if slots[0].Position != 1 || slots[0].PullRequest.Number != 3421 {
		t.Errorf("first entry wrong: %+v", slots[0])
	}
	if slots[1].State != "QUEUED" {
		t.Errorf("second entry state = %q", slots[1].State)
	}
}

func TestFetchQueueRequestsTheBranch(t *testing.T) {
	calls := fakeGH(t, always(queueEnvelope()))

	if _, err := fetchQueue("o", "r", "release-2.1"); err != nil {
		t.Fatalf("fetchQueue: %v", err)
	}

	got := strings.Join((*calls)[0], " ")
	if !strings.Contains(got, "branch=release-2.1") {
		t.Errorf("argv should carry the branch: %s", got)
	}
	// The entry state and mergeable fields are what the whole tool reasons from.
	for _, field := range []string{"mergeQueue", "mergeQueueEntry", "mergeable", "statusCheckRollup", "files"} {
		if !strings.Contains(got, field) {
			t.Errorf("query does not request %q", field)
		}
	}
}

func TestFetchQueueWhenBranchHasNoQueue(t *testing.T) {
	fakeGH(t, always(`{"data":{"repository":{"mergeQueue":null}}}`))

	slots, err := fetchQueue("o", "r", "main")
	if err != nil {
		t.Fatalf("a branch with no queue is not an error: %v", err)
	}
	if slots != nil {
		t.Errorf("want no entries, got %+v", slots)
	}
}

func TestPollCombinesBothCalls(t *testing.T) {
	calls := fakeGH(t, routeByQuery(
		searchEnvelope(readyPR),
		queueEnvelope(slot(1, "AWAITING_CHECKS", `{"number":3421,"title":"ahead"}`)),
	))

	snap, err := poll("o", "r", "main")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("want 2 gh calls, got %d", len(*calls))
	}
	if len(snap.prs) != 1 || len(snap.queue) != 1 {
		t.Errorf("snapshot wrong: %d mine, %d queued", len(snap.prs), len(snap.queue))
	}
	if snap.queueErr != nil {
		t.Errorf("unexpected queueErr: %v", snap.queueErr)
	}
}

func TestPollDegradesWhenQueueFails(t *testing.T) {
	// The PR pane is still useful when only the queue read fails.
	fakeGH(t, func(q string) ghReply {
		if strings.Contains(q, "search(") {
			return ghReply{stdout: searchEnvelope(readyPR)}
		}
		return ghReply{stderr: "gh: something broke", exit: 1}
	})

	snap, err := poll("o", "r", "main")
	if err != nil {
		t.Fatalf("a queue failure must not fail the poll: %v", err)
	}
	if len(snap.prs) != 1 {
		t.Errorf("PR list should survive, got %d", len(snap.prs))
	}
	if snap.queueErr == nil {
		t.Error("queueErr should record the failure")
	}
}

func TestPollFailsWhenSearchFails(t *testing.T) {
	fakeGH(t, func(string) ghReply {
		return ghReply{stderr: "gh: Could not resolve to a Repository", exit: 1}
	})

	if _, err := poll("o", "r", "main"); err == nil {
		t.Fatal("want an error when the PR search fails")
	}
}

func TestRunGraphQLSurfacesStderr(t *testing.T) {
	// What an EMU account hitting a repo it cannot see actually looks like.
	fakeGH(t, func(string) ghReply {
		return ghReply{stderr: "gh: Could not resolve to a Repository with the name 'o/r'.", exit: 1}
	})

	_, err := fetchQueue("o", "r", "main")
	if err == nil || !strings.Contains(err.Error(), "Could not resolve to a Repository") {
		t.Fatalf("error should carry gh's stderr, got %v", err)
	}
}

func TestRunGraphQLFallsBackWhenStderrEmpty(t *testing.T) {
	fakeGH(t, func(string) ghReply { return ghReply{exit: 3} })

	_, err := fetchQueue("o", "r", "main")
	if err == nil || !strings.Contains(err.Error(), "gh api graphql") {
		t.Fatalf("error should still be attributed to gh, got %v", err)
	}
}

func TestDecodeErrorsAreDistinct(t *testing.T) {
	fakeGH(t, always("not json"))

	if _, err := fetchQueue("o", "r", "main"); err == nil || !strings.Contains(err.Error(), "decode queue response") {
		t.Errorf("queue decode error = %v", err)
	}
	if _, _, _, err := fetchOpenPRs("o", "r"); err == nil || !strings.Contains(err.Error(), "decode search response") {
		t.Errorf("search decode error = %v", err)
	}
}

func TestEnqueueAndDequeueUseTheirOwnInputFields(t *testing.T) {
	// The two mutations are asymmetric in the GitHub schema: enqueue takes
	// pullRequestId, dequeue takes id. Getting them the wrong way round fails only
	// at runtime, against a real queue.
	calls := fakeGH(t, always(`{"data":{}}`))

	if err := enqueue("PR_node_1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := dequeue("PR_node_1"); err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	enq := strings.Join((*calls)[0], " ")
	if !strings.Contains(enq, "enqueuePullRequest") || !strings.Contains(enq, "pullRequestId:$id") {
		t.Errorf("enqueue mutation wrong: %s", enq)
	}
	deq := strings.Join((*calls)[1], " ")
	if !strings.Contains(deq, "dequeuePullRequest") || !strings.Contains(deq, "input:{id:$id}") {
		t.Errorf("dequeue mutation wrong: %s", deq)
	}
	for _, c := range *calls {
		if !strings.Contains(strings.Join(c, " "), "id=PR_node_1") {
			t.Errorf("mutation did not pass the node id: %v", c)
		}
	}
}

func TestMutationSurfacesError(t *testing.T) {
	fakeGH(t, func(string) ghReply {
		return ghReply{stderr: "gh: Pull request is in an unmergeable state", exit: 1}
	})

	if err := enqueue("PR_1"); err == nil || !strings.Contains(err.Error(), "unmergeable state") {
		t.Errorf("enqueue error = %v", err)
	}
}

func TestCheckStateMissing(t *testing.T) {
	cases := map[string]string{
		"no commits":  `{"commits":{"nodes":[]}}`,
		"null rollup": `{"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}`,
		"absent":      `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := decode(t, body).checkState(); got != "" {
				t.Errorf("checkState() = %q, want empty", got)
			}
		})
	}
}

func TestInQueue(t *testing.T) {
	if decode(t, `{}`).inQueue() {
		t.Error("a PR with no entry must not report as queued")
	}
	if !decode(t, `{"mergeQueueEntry":{"state":"QUEUED","position":1}}`).inQueue() {
		t.Error("a PR with an entry must report as queued")
	}
}

func TestFileListTruncation(t *testing.T) {
	full := decode(t, `{"files":{"totalCount":2,"nodes":[{"path":"a"},{"path":"b"}]}}`)
	if full.Files.truncated() {
		t.Error("a complete list must not report as truncated")
	}
	if got := full.Files.paths(); strings.Join(got, ",") != "a,b" {
		t.Errorf("paths() = %v", got)
	}

	capped := decode(t, `{"files":{"totalCount":420,"nodes":[{"path":"a"}]}}`)
	if !capped.Files.truncated() {
		t.Error("a capped list must report as truncated")
	}
}

func TestSharedFiles(t *testing.T) {
	mine := decode(t, `{"files":{"totalCount":3,"nodes":[{"path":"a.go"},{"path":"b.go"},{"path":"c.go"}]}}`).Files
	overlapping := decode(t, `{"files":{"totalCount":2,"nodes":[{"path":"b.go"},{"path":"z.go"}]}}`).Files
	disjoint := decode(t, `{"files":{"totalCount":1,"nodes":[{"path":"z.go"}]}}`).Files
	empty := fileList{}

	if got := sharedFiles(mine, overlapping); strings.Join(got, ",") != "b.go" {
		t.Errorf("sharedFiles() = %v, want [b.go]", got)
	}
	if got := sharedFiles(mine, disjoint); got != nil {
		t.Errorf("disjoint PRs share nothing, got %v", got)
	}
	if got := sharedFiles(mine, empty); got != nil {
		t.Errorf("an empty list shares nothing, got %v", got)
	}
	if got := sharedFiles(empty, mine); got != nil {
		t.Errorf("an empty list shares nothing, got %v", got)
	}
}

func TestConflictRisk(t *testing.T) {
	mine := decode(t, `{"number":3424,"files":{"totalCount":1,"nodes":[{"path":"auth.go"}]}}`)

	queue := []queueSlot{
		mustSlot(t, 1, "QUEUED", `{"number":3421,"files":{"totalCount":1,"nodes":[{"path":"other.go"}]}}`),
		mustSlot(t, 2, "QUEUED", `{"number":3425,"files":{"totalCount":1,"nodes":[{"path":"auth.go"}]}}`),
	}

	risk := conflictRisk(mine, queue)
	if len(risk) != 1 || risk[0] != 3425 {
		t.Errorf("conflictRisk() = %v, want [3425]", risk)
	}
}

func TestConflictRiskIgnoresItself(t *testing.T) {
	// A queued PR overlaps its own entry completely; that is not a risk.
	mine := decode(t, `{"number":3424,"files":{"totalCount":1,"nodes":[{"path":"auth.go"}]}}`)
	queue := []queueSlot{
		mustSlot(t, 1, "UNMERGEABLE", `{"number":3424,"files":{"totalCount":1,"nodes":[{"path":"auth.go"}]}}`),
	}

	if risk := conflictRisk(mine, queue); risk != nil {
		t.Errorf("a PR must not be flagged against itself, got %v", risk)
	}
}

// TestOverlapHeuristicMissedTheRealDequeue records the observed limit of the
// hint: #3424 shared no files with #3421, the entry ahead of it, and was still
// thrown out of the queue. The hint predicts textual conflicts only.
func TestOverlapHeuristicMissedTheRealDequeue(t *testing.T) {
	mine := decode(t, `{"number":3424,"files":{"totalCount":8,"nodes":[{"path":"auth/client.go"}]}}`)
	ahead := []queueSlot{
		mustSlot(t, 1, "AWAITING_CHECKS", `{"number":3421,"files":{"totalCount":1,"nodes":[{"path":"ci/timeouts.yaml"}]}}`),
	}

	if risk := conflictRisk(mine, ahead); risk != nil {
		t.Errorf("the real data shares no files, got risk %v", risk)
	}
}
