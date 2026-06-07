// Package github models the subset of GitHub webhook payloads the Hub needs.
package github

// Envelope captures the fields required to bucket a webhook into a Round
// (owner/repo#number @ sha). Different event types carry the PR number and
// head SHA in different places; the optional pointers reflect that.
type Envelope struct {
	Action     string `json:"action"`
	Number     int    `json:"number"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	PullRequest *struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	CheckSuite *struct {
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
}
