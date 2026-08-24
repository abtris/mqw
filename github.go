package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// prFields is the selection set shared by the queue and search queries, so a PR
// is described identically wherever it comes from.
const prFields = `
  id
  number
  title
  state
  isDraft
  merged
  mergeable
  reviewDecision
  headRefName
  baseRefName
  author{ login __typename }
  mergeQueueEntry{ state position estimatedTimeToMerge }
  files(first:100){ totalCount nodes{ path } }
  commits(last:1){ nodes{ commit{ statusCheckRollup{ state } } } }`

// queueQuery lists what is in the queue for a base branch. Merge queue entries
// only exist in GraphQL, which is why everything goes through `gh api graphql`
// rather than REST. The changed-file lists come along to support the overlap
// hint on PRs that are not queued yet.
var queueQuery = `
query($owner:String!,$name:String!,$branch:String!){
  repository(owner:$owner,name:$name){
    mergeQueue(branch:$branch){
      entries(first:20){
        nodes{
          position
          state
          enqueuedAt
          estimatedTimeToMerge
          enqueuer{ login }
          pullRequest{` + prFields + `}
        }
      }
    }
  }
}`

// searchQuery lists open PRs in one repo, newest first, along with the viewer's
// login. Filtering by author happens client-side so one query serves every
// filter, and so ownership never depends on which filter is active.
var searchQuery = `
query($q:String!,$after:String){
  viewer{ login }
  search(query:$q, type:ISSUE, first:50, after:$after){
    pageInfo{ hasNextPage endCursor }
    nodes{
      ... on PullRequest {` + prFields + `}
    }
  }
}`

// The two mutations are asymmetric: enqueue takes pullRequestId, dequeue takes id.
const enqueueMutation = `
mutation($id:ID!){
  enqueuePullRequest(input:{pullRequestId:$id}){
    mergeQueueEntry{ position state }
  }
}`

const dequeueMutation = `
mutation($id:ID!){
  dequeuePullRequest(input:{id:$id}){
    mergeQueueEntry{ id }
  }
}`

type fileList struct {
	TotalCount int `json:"totalCount"`
	Nodes      []struct {
		Path string `json:"path"`
	} `json:"nodes"`
}

func (f fileList) paths() []string {
	out := make([]string, 0, len(f.Nodes))
	for _, n := range f.Nodes {
		out = append(out, n.Path)
	}
	return out
}

// truncated reports whether the API capped the file list, which makes an empty
// overlap result unreliable.
func (f fileList) truncated() bool { return f.TotalCount > len(f.Nodes) }

type queueEntry struct {
	State                string `json:"state"`
	Position             int    `json:"position"`
	EstimatedTimeToMerge *int   `json:"estimatedTimeToMerge"`
}

type author struct {
	Login    string `json:"login"`
	TypeName string `json:"__typename"`
}

// isBot reports whether the PR was opened by an app rather than a person.
func (a author) isBot() bool { return a.TypeName == "Bot" }

type pullRequest struct {
	ID             string      `json:"id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	State          string      `json:"state"`
	IsDraft        bool        `json:"isDraft"`
	Merged         bool        `json:"merged"`
	Mergeable      string      `json:"mergeable"`
	ReviewDecision string      `json:"reviewDecision"`
	HeadRefName    string      `json:"headRefName"`
	BaseRefName    string      `json:"baseRefName"`
	Author         author      `json:"author"`
	QueueEntry     *queueEntry `json:"mergeQueueEntry"`
	Files          fileList    `json:"files"`
	Commits        struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// checkState returns the rollup state of the head commit's checks, or "" when
// no checks have reported yet.
func (p *pullRequest) checkState() string {
	nodes := p.Commits.Nodes
	if len(nodes) == 0 || nodes[0].Commit.StatusCheckRollup == nil {
		return ""
	}
	return nodes[0].Commit.StatusCheckRollup.State
}

func (p *pullRequest) inQueue() bool { return p.QueueEntry != nil }

// queueSlot is one entry in the queue for a base branch. The enqueuer is often
// not the author: someone else can add your pull request to the queue.
type queueSlot struct {
	Position             int         `json:"position"`
	State                string      `json:"state"`
	EnqueuedAt           string      `json:"enqueuedAt"`
	EstimatedTimeToMerge *int        `json:"estimatedTimeToMerge"`
	Enqueuer             author      `json:"enqueuer"`
	PullRequest          pullRequest `json:"pullRequest"`
}

// snapshot is one poll: the queue for the watched base branch, and every open
// pull request in the repository. Filtering happens at render time.
type snapshot struct {
	queue []queueSlot
	prs   []pullRequest
	// viewer is the authenticated login, and the only source of truth for
	// ownership. Deriving ownership from the visible list would break the moment
	// the list shows anyone else's pull requests.
	viewer string
	// truncated reports that more open pull requests exist than were fetched.
	truncated bool
	// queueErr records a failure to read the queue alone. The PR list may still
	// be good, so this degrades one pane rather than the whole poll.
	queueErr error
}

// owns reports whether a pull request belongs to the authenticated user.
func (s *snapshot) owns(pr *pullRequest) bool {
	return s.viewer != "" && pr.Author.Login == s.viewer
}

// sharedFiles returns the paths two pull requests have in common.
//
// This is a hint, not a verdict. Touching the same file is not a conflict, and
// touching none does not rule one out: a merge group can also fail on checks,
// which is what dequeued #3424 while it shared no files with the PR ahead of it.
// Both file lists are capped at 100 paths by the API.
func sharedFiles(mine, other fileList) []string {
	if len(mine.Nodes) == 0 || len(other.Nodes) == 0 {
		return nil
	}
	theirs := make(map[string]bool, len(other.Nodes))
	for _, p := range other.paths() {
		theirs[p] = true
	}
	var out []string
	for _, p := range mine.paths() {
		if theirs[p] {
			out = append(out, p)
		}
	}
	return out
}

// conflictRisk names the queued PRs a candidate shares files with.
func conflictRisk(pr *pullRequest, queue []queueSlot) []int {
	var out []int
	for _, e := range queue {
		if e.PullRequest.Number == pr.Number {
			continue
		}
		if len(sharedFiles(pr.Files, e.PullRequest.Files)) > 0 {
			out = append(out, e.PullRequest.Number)
		}
	}
	return out
}

// execCommand builds the gh invocation. Swapped out in tests.
var execCommand = exec.Command

// ghToken pins gh to one account when set. gh prefers GH_TOKEN over whichever
// account is active, so this isolates the tool from a `gh auth switch` done in
// another terminal — the failure that otherwise shows up as an empty pull request
// pane and a NOT_FOUND on the queue.
var ghToken string

// ghCommand builds a gh invocation carrying the pinned token, if any.
func ghCommand(args ...string) *exec.Cmd {
	cmd := execCommand("gh", args...)
	if ghToken != "" {
		// Environ() keeps whatever the caller already set and adds ours, rather
		// than replacing the environment wholesale.
		cmd.Env = append(cmd.Environ(), "GH_TOKEN="+ghToken)
	}
	return cmd
}

// resolveToken asks gh for the stored token of one account. It deliberately does
// not go through ghCommand: there is no token to pass yet.
func resolveToken(account string) (string, error) {
	cmd := execCommand("gh", "auth", "token", "--user", account)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("no gh token for account %q: %s", account, msg)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gh returned an empty token for account %q", account)
	}
	return token, nil
}

// runGraphQL sends a query through gh. fields are "name=value" pairs passed as
// -F, so gh infers ints for numeric values.
func runGraphQL(query string, fields ...string) ([]byte, error) {
	args := []string{"api", "graphql", "-f", "query=" + query}
	for _, f := range fields {
		args = append(args, "-F", f)
	}

	cmd := ghCommand(args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gh api graphql: %s", msg)
	}
	return out, nil
}

// fetchQueue lists the merge queue for a base branch. A branch with no queue
// configured returns no entries and no error.
func fetchQueue(owner, name, branch string) ([]queueSlot, error) {
	out, err := runGraphQL(queueQuery, "owner="+owner, "name="+name, "branch="+branch)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			Repository struct {
				MergeQueue *struct {
					Entries struct {
						Nodes []queueSlot `json:"nodes"`
					} `json:"entries"`
				} `json:"mergeQueue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("decode queue response: %w", err)
	}
	if resp.Data.Repository.MergeQueue == nil {
		return nil, nil
	}
	return resp.Data.Repository.MergeQueue.Entries.Nodes, nil
}

// maxPRPages caps pagination so an unfiltered listing on a busy repo cannot spin
// forever. Hitting the cap is reported, never silently truncated.
const maxPRPages = 4

// fetchOpenPRs lists open pull requests in a repository, newest first, and the
// viewer's login. truncated reports that more pages exist than the cap allows.
func fetchOpenPRs(owner, name string) (prs []pullRequest, viewer string, truncated bool, err error) {
	q := fmt.Sprintf("repo:%s/%s is:pr is:open sort:updated-desc", owner, name)

	cursor := ""
	for range maxPRPages {
		fields := []string{"q=" + q}
		if cursor != "" {
			fields = append(fields, "after="+cursor)
		}
		out, err := runGraphQL(searchQuery, fields...)
		if err != nil {
			return nil, "", false, err
		}

		var resp struct {
			Data struct {
				Viewer struct {
					Login string `json:"login"`
				} `json:"viewer"`
				Search struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []pullRequest `json:"nodes"`
				} `json:"search"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return nil, "", false, fmt.Errorf("decode search response: %w", err)
		}
		viewer = resp.Data.Viewer.Login

		// Search can return other node types, which decode to a zero pullRequest.
		for _, pr := range resp.Data.Search.Nodes {
			if pr.Number != 0 {
				prs = append(prs, pr)
			}
		}

		if !resp.Data.Search.PageInfo.HasNextPage {
			return prs, viewer, false, nil
		}
		cursor = resp.Data.Search.PageInfo.EndCursor
	}
	return prs, viewer, true, nil
}

// poll gathers one snapshot. A failure to read the queue is recorded on the
// snapshot rather than failing the poll, so the PR pane still renders.
func poll(owner, name, branch string) (*snapshot, error) {
	prs, viewer, truncated, err := fetchOpenPRs(owner, name)
	if err != nil {
		return nil, err
	}
	entries, queueErr := fetchQueue(owner, name, branch)
	return &snapshot{
		queue:     entries,
		prs:       prs,
		viewer:    viewer,
		truncated: truncated,
		queueErr:  queueErr,
	}, nil
}

func enqueue(id string) error {
	_, err := runGraphQL(enqueueMutation, "id="+id)
	return err
}

func dequeue(id string) error {
	_, err := runGraphQL(dequeueMutation, "id="+id)
	return err
}
