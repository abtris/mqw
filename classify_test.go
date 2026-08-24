package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Fixtures. realQueuedPR is a verbatim capture of the GraphQL response for
// acme/service#3424 while it sat at queue position 2 with an
// UNMERGEABLE entry: the case the tool exists to catch. Note mergeable is
// UNKNOWN, so the cause could not be attributed at that moment.
// testViewer is the authenticated login the fixtures below belong to.
const testViewer = "alice"

const (
	mineAuthor  = `"author":{"login":"alice","__typename":"User"}`
	botAuthor   = `"author":{"login":"deps-bot","__typename":"Bot"}`
	otherAuthor = `"author":{"login":"bob","__typename":"User"}`
)

const (
	realQueuedPR = `{"id":"PR_kwABC","number":3424,"title":"refactor(auth): drop legacy fallback",
	"state":"OPEN","isDraft":false,"merged":false,"mergeable":"UNKNOWN","reviewDecision":"APPROVED",
	"headRefName":"alice/drop-legacy-fallback","baseRefName":"main",` + mineAuthor + `,
	"mergeQueueEntry":{"state":"UNMERGEABLE","position":2,"estimatedTimeToMerge":3318},
	"files":{"totalCount":8,"nodes":[{"path":"auth/client.go"}]},
	"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}}`

	readyPR = `{"id":"PR_ready","number":3424,"title":"refactor(auth): drop legacy fallback",
	"state":"OPEN","mergeable":"MERGEABLE","reviewDecision":"APPROVED","baseRefName":"main",
	"headRefName":"topic",` + mineAuthor + `,
	"files":{"totalCount":1,"nodes":[{"path":"auth/client.go"}]},
	"labels":{"nodes":[{"name":"skip-e2e"},{"name":"area/auth"}]},
	"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}}`

	draftPR = `{"id":"PR_draft","number":3418,"title":"chore(build): use hardened base images",
	"state":"OPEN","isDraft":true,"mergeable":"MERGEABLE","reviewDecision":"REVIEW_REQUIRED",
	"baseRefName":"main","headRefName":"docker",` + mineAuthor + `}`

	reviewPR = `{"id":"PR_review","number":3423,"title":"fix(api): resolve location at create time",
	"state":"OPEN","mergeable":"MERGEABLE","reviewDecision":"REVIEW_REQUIRED","baseRefName":"main",
	"headRefName":"loc",` + mineAuthor + `}`

	// A dependency bot appears as a Bot actor rather than a User.
	renovatePR = `{"id":"PR_bot","number":3421,"title":"chore(deps): bump toolchain image",
	"state":"OPEN","mergeable":"MERGEABLE","reviewDecision":"APPROVED","baseRefName":"main",
	"headRefName":"renovate/dep",` + botAuthor + `}`

	otherPR = `{"id":"PR_other","number":3425,"title":"chore(infra): bump agent version",
	"state":"OPEN","mergeable":"MERGEABLE","reviewDecision":"APPROVED","baseRefName":"main",
	"headRefName":"infra-bump",` + otherAuthor + `}`
)

func decode(t *testing.T, body string) *pullRequest {
	t.Helper()
	var pr pullRequest
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &pr
}

func mustSlot(t *testing.T, position int, state, pr string) queueSlot {
	t.Helper()
	return queueSlot{Position: position, State: state, PullRequest: *decode(t, pr)}
}

func TestDecodeRealResponse(t *testing.T) {
	pr := decode(t, realQueuedPR)

	if pr.Number != 3424 || pr.ID != "PR_kwABC" {
		t.Errorf("scalar fields wrong: %+v", pr)
	}
	if !pr.inQueue() || pr.QueueEntry.Position != 2 || pr.QueueEntry.State != "UNMERGEABLE" {
		t.Errorf("queue entry wrong: %+v", pr.QueueEntry)
	}
	if pr.QueueEntry.EstimatedTimeToMerge == nil || *pr.QueueEntry.EstimatedTimeToMerge != 3318 {
		t.Errorf("eta wrong: %v", pr.QueueEntry.EstimatedTimeToMerge)
	}
	if got := pr.checkState(); got != "SUCCESS" {
		t.Errorf("checkState() = %q, want SUCCESS", got)
	}
	if !pr.Files.truncated() {
		t.Error("8 files with 1 node returned must report as truncated")
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		want      statusKind
		attention bool
	}{
		{"merged", `{"state":"MERGED","merged":true}`, kMerged, false},
		{"closed", `{"state":"CLOSED"}`, kClosed, false},
		{"draft", draftPR, kDraft, false},
		{
			"queued and healthy",
			`{"state":"OPEN","mergeable":"MERGEABLE","mergeQueueEntry":{"state":"AWAITING_CHECKS","position":4}}`,
			kQueued, false,
		},
		{
			"queued, unmergeable, conflicts with base",
			`{"state":"OPEN","mergeable":"CONFLICTING","baseRefName":"main","headRefName":"topic",
			  "mergeQueueEntry":{"state":"UNMERGEABLE","position":2}}`,
			kQueuedConflict, true,
		},
		{
			"queued, unmergeable, clean against base",
			`{"state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main","headRefName":"topic",
			  "mergeQueueEntry":{"state":"UNMERGEABLE","position":2}}`,
			kQueuedGroupFail, true,
		},
		{"queued, unmergeable, cause unknown", realQueuedPR, kQueuedUnknownCause, true},
		{
			"conflicts with base, not queued",
			`{"state":"OPEN","mergeable":"CONFLICTING","baseRefName":"main","headRefName":"topic"}`,
			kConflicting, true,
		},
		{
			"checks failing",
			`{"state":"OPEN","mergeable":"MERGEABLE","reviewDecision":"APPROVED",
			  "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE"}}}]}}`,
			kChecksFailed, true,
		},
		{
			"checks errored",
			`{"state":"OPEN","mergeable":"MERGEABLE","reviewDecision":"APPROVED",
			  "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"ERROR"}}}]}}`,
			kChecksFailed, true,
		},
		{"mergeability not computed", `{"state":"OPEN","mergeable":"UNKNOWN"}`, kPending, false},
		{"review required", reviewPR, kNeedsReview, false},
		{
			"changes requested",
			`{"state":"OPEN","mergeable":"MERGEABLE","reviewDecision":"CHANGES_REQUESTED"}`,
			kNeedsReview, false,
		},
		{"ready to enqueue", readyPR, kReady, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(decode(t, tt.body), nil)
			if got.kind != tt.want {
				t.Errorf("kind = %v, want %v (headline %q)", got.kind, tt.want, got.headline)
			}
			if got.attention != tt.attention {
				t.Errorf("attention = %v, want %v", got.attention, tt.attention)
			}
			if got.headline == "" {
				t.Error("headline must not be empty")
			}
		})
	}
}

// The distinction between these two is the point of the tool: one is fixed by
// rebasing and the other is not, and they look identical in the GitHub UI.
func TestQueuedUnmergeableAdviceDiffersByCause(t *testing.T) {
	conflict := classify(decode(t, `{"state":"OPEN","mergeable":"CONFLICTING","baseRefName":"main",
		"headRefName":"topic","mergeQueueEntry":{"state":"UNMERGEABLE","position":2}}`), nil)
	if !strings.Contains(conflict.detail, "rebase topic") {
		t.Errorf("a base conflict should advise rebasing: %q", conflict.detail)
	}

	group := classify(decode(t, `{"state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main",
		"headRefName":"topic","mergeQueueEntry":{"state":"UNMERGEABLE","position":2}}`), nil)
	if !strings.Contains(group.detail, "rebasing will not help") {
		t.Errorf("a merge-group failure should say rebasing will not help: %q", group.detail)
	}

	unknown := classify(decode(t, realQueuedPR), nil)
	if strings.Contains(unknown.detail, "will not help") {
		t.Errorf("an unknown cause must not claim rebasing is useless: %q", unknown.detail)
	}
	if !strings.Contains(unknown.detail, "not computed") {
		t.Errorf("an unknown cause should say so: %q", unknown.detail)
	}
}

func TestClassifyCarriesQueuePosition(t *testing.T) {
	got := classify(decode(t, realQueuedPR), nil)
	if !strings.Contains(got.detail, "position 2") {
		t.Errorf("detail should carry the position: %q", got.detail)
	}
	if !strings.Contains(got.detail, "55m18s") {
		t.Errorf("detail should carry the eta: %q", got.detail)
	}
}

func TestReadyPRNamesOverlappingQueuedPRs(t *testing.T) {
	queue := []queueSlot{
		mustSlot(t, 1, "QUEUED", `{"number":3421,"files":{"totalCount":1,"nodes":[{"path":"other.go"}]}}`),
		mustSlot(t, 2, "QUEUED", `{"number":3425,"files":{"totalCount":1,"nodes":[{"path":"auth/client.go"}]}}`),
	}

	got := classify(decode(t, readyPR), queue)
	if got.kind != kReady {
		t.Fatalf("kind = %v, want kReady", got.kind)
	}
	if !strings.Contains(got.detail, "#3425") {
		t.Errorf("detail should name the overlapping PR: %q", got.detail)
	}
	if strings.Contains(got.detail, "#3421") {
		t.Errorf("detail should not name a disjoint PR: %q", got.detail)
	}
}

func TestReadyPRWithNoOverlapHasNoWarning(t *testing.T) {
	queue := []queueSlot{
		mustSlot(t, 1, "QUEUED", `{"number":3421,"files":{"totalCount":1,"nodes":[{"path":"other.go"}]}}`),
	}

	got := classify(decode(t, readyPR), queue)
	if got.detail != "" {
		t.Errorf("a disjoint PR needs no warning, got %q", got.detail)
	}
}

func TestEnqueueable(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "ready", body: readyPR},
		{name: "already queued", body: realQueuedPR, wantErr: "already in the queue"},
		{name: "draft", body: draftPR, wantErr: "is a draft"},
		{
			name:    "conflicts with base",
			body:    `{"number":9,"state":"OPEN","mergeable":"CONFLICTING","baseRefName":"main","headRefName":"topic"}`,
			wantErr: "rebase topic first",
		},
		// Review state does not block the mutation; GitHub decides. The tool only
		// refuses what it is certain about.
		{name: "review required", body: reviewPR},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := enqueueable(decode(t, tt.body))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestJoinPRs(t *testing.T) {
	if got := joinPRs([]int{1}); got != "#1" {
		t.Errorf("joinPRs = %q", got)
	}
	if got := joinPRs([]int{1, 2, 3}); got != "#1, #2, #3" {
		t.Errorf("joinPRs = %q", got)
	}
}
