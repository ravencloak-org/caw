package severity

// CodeRabbit comment vocabulary mapping
//
// CodeRabbit review comments follow a loose but observable vocabulary.
// The mapping below was derived from CodeRabbit's documented severity signals
// and from inspection of real review output.
//
// CRITICAL — security vulnerabilities, hard blockers, data-loss risks.
//   Signals: "security vulnerability", "🚨", "[critical]", "[blocker]",
//            "critical issue", "critical bug"
//
// MAJOR — potential bugs, missing error handling, important refactors.
//   Signals: "potential issue", "⚠️", "[warning]", "[important]", "[major]",
//            "refactor suggestion", "possible bug", "error handling"
//
// minor (default) — nitpicks, style, docs, cosmetic.
//   Signals: "nitpick", "nit:", "[minor]", "style:", "typo", "spelling",
//            "suggestion", "documentation"
//   Any comment that matches none of the above also maps to minor.
//
// The keyword scan is case-insensitive and stops at the first match, scanning
// from CRITICAL → MAJOR → minor. To update the vocabulary add keywords to the
// slices below; do NOT add new Level constants.

// codeRabbitRules is evaluated in descending severity order.
var codeRabbitRules = []KeywordRule{
	{
		Level: Critical,
		Keywords: []string{
			"security vulnerability",
			"🚨",
			"[critical]",
			"[blocker]",
			"critical issue",
			"critical bug",
			"critical security",
			"data loss",
			"remote code execution",
			"sql injection",
			"authentication bypass",
		},
	},
	{
		Level: Major,
		Keywords: []string{
			"potential issue",
			"⚠️",
			"[warning]",
			"[important]",
			"[major]",
			"refactor suggestion",
			"possible bug",
			"error handling",
			"missing error",
			"unhandled error",
			"race condition",
			"memory leak",
			"performance issue",
		},
	},
	{
		Level: Minor,
		Keywords: []string{
			"nitpick",
			"nit:",
			"[minor]",
			"[nit]",
			"style:",
			"typo",
			"spelling",
			"suggestion:",
			"documentation",
			"doc comment",
			"cosmetic",
		},
	},
}

// codeRabbitAdapter implements Adapter for CodeRabbit review comments.
type codeRabbitAdapter struct{}

// NewCodeRabbitAdapter returns an Adapter that maps CodeRabbit review comment
// bodies onto the severity ladder. The adapter is stateless and safe for
// concurrent use.
func NewCodeRabbitAdapter() Adapter {
	return &codeRabbitAdapter{}
}

// Parse maps a CodeRabbit comment body to a Level.
// It scans for known vocabulary in descending severity order; unrecognized
// bodies default to Minor (bare comment convention).
func (a *codeRabbitAdapter) Parse(body string) Level {
	return ParseKeywords(body, codeRabbitRules, Minor)
}
