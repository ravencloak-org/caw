// Package github models the subset of GitHub webhook payloads the Hub needs.
package github

// Repository identifies the repo an event belongs to.
type Repository struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// PullRequest carries the PR number, state, head SHA, and author. User is the
// PR author (auth-v2 Phase 3.5 surfaces author_login on the control stream).
type PullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	User struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	} `json:"user"`
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

// Installation is the GitHub App installation object present in installation
// and installation_repositories webhook payloads.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
}

// Envelope captures the fields required to bucket a webhook into a Round and to
// extract signals. Optional pointers reflect which payloads carry which objects.
type Envelope struct {
	Action       string        `json:"action"`
	Number       int           `json:"number"`
	Repository   Repository    `json:"repository"`
	PullRequest  *PullRequest  `json:"pull_request"`
	CheckSuite   *CheckSuite   `json:"check_suite"`
	Comment      *Comment      `json:"comment"`
	Review       *Review       `json:"review"`
	Issue        *Issue        `json:"issue"`
	Installation *Installation `json:"installation"`
	// Repositories is set on installation_repositories events.
	Repositories []Repository `json:"repositories"`
	// RepositoriesAdded and RepositoriesRemoved are set on
	// installation_repositories events for the delta.
	RepositoriesAdded   []Repository `json:"repositories_added"`
	RepositoriesRemoved []Repository `json:"repositories_removed"`
	// Sender is the actor of the event. ID matches github_user_id on
	// auth-v2 tokens; auth-v2 Phase 3.5 fans pr_opened / pr_closed /
	// installation_added events through the control hub keyed on Sender.ID.
	Sender struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	} `json:"sender"`
}
