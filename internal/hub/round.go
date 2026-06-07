package hub

import (
	"fmt"

	"github.com/ravencloak-org/caw/internal/github"
)

// RoundKey identifies one settle lifecycle: a PR at a specific head SHA.
type RoundKey struct {
	Owner  string
	Repo   string
	Number int
	SHA    string
}

// String renders the canonical key form owner/repo#number@sha.
func (k RoundKey) String() string {
	return fmt.Sprintf("%s/%s#%d@%s", k.Owner, k.Repo, k.Number, k.SHA)
}

// DeriveRound extracts a RoundKey from a webhook envelope. ok is false when the
// event does not pertain to a specific PR head SHA, in which case the caller
// skips bucketing.
func DeriveRound(env github.Envelope) (key RoundKey, ok bool) {
	owner := env.Repository.Owner.Login
	repo := env.Repository.Name
	if owner == "" || repo == "" {
		return RoundKey{}, false
	}
	switch {
	case env.PullRequest != nil && env.PullRequest.Head.SHA != "":
		return RoundKey{owner, repo, env.PullRequest.Number, env.PullRequest.Head.SHA}, true
	case env.CheckSuite != nil && env.CheckSuite.HeadSHA != "" && len(env.CheckSuite.PullRequests) > 0:
		return RoundKey{owner, repo, env.CheckSuite.PullRequests[0].Number, env.CheckSuite.HeadSHA}, true
	default:
		return RoundKey{}, false
	}
}
