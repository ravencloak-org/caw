// Package github models the subset of GitHub webhook payloads the Hub needs.
package github

// Repository identifies the repo an event belongs to.
type Repository struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// PullRequest carries the PR number, state, and head SHA.
type PullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// CheckSuitePR is a PR reference inside a check_suite payload.
type CheckSuitePR struct {
	Number int `json:"number"`
}

// CheckSuite is a check_suite event payload (the settle trigger).
type CheckSuite struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        struct {
		Name string `json:"name"`
	} `json:"app"`
	PullRequests []CheckSuitePR `json:"pull_requests"`
}

// Comment is an issue_comment or review-comment body.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// Review is a pull_request_review payload.
type Review struct {
	ID    int64  `json:"id"`
	Body  string `json:"body"`
	State string `json:"state"`
}

// Issue carries the number for issue_comment events (PRs are issues).
type Issue struct {
	Number int `json:"number"`
}

// Envelope captures the fields required to bucket a webhook into a Round and to
// extract signals. Optional pointers reflect which payloads carry which objects.
type Envelope struct {
	Action      string       `json:"action"`
	Number      int          `json:"number"`
	Repository  Repository   `json:"repository"`
	PullRequest *PullRequest `json:"pull_request"`
	CheckSuite  *CheckSuite  `json:"check_suite"`
	Comment     *Comment     `json:"comment"`
	Review      *Review      `json:"review"`
	Issue       *Issue       `json:"issue"`
	Sender      struct {
		Login string `json:"login"`
	} `json:"sender"`
}
