// Package severity defines a closed 3-level severity ladder (CRITICAL / MAJOR / minor)
// and an open adapter registry so new source vocabularies can be plugged in without
// touching the ladder itself.
//
// # Design rationale
//
// The ladder is intentionally frozen at three levels. Code-review signal sources
// (CI checks, bots, human reviewers) all map onto "must fix", "should fix", and
// "nice to fix" — a finer-grained scale would require consensus across every
// future adapter author. Adding a source never widens the ladder; it only adds a
// new Adapter implementation to the registry.
//
// The renderer always emits a symbol+label pair (e.g. "■ CRITICAL") so the output
// is unambiguous on colorblind and no-color terminals. ANSI color codes are
// layered on top and suppressed when the caller passes color=false, when the
// NO_COLOR environment variable is set, or when output is not a TTY.
package severity

import (
	"os"
	"strings"
)

// Level is the severity of a review signal. Higher values are more severe.
// The ladder is closed: no source adapter may add a new level.
type Level int

const (
	// Minor is a nitpick, style suggestion, or documentation note.
	// It is the default for bare or unparseable comments.
	Minor Level = iota + 1

	// Major is a potential bug, missing error check, or important refactor.
	// It is the default for a failing CI check with no further context.
	Major

	// Critical is a security vulnerability, data-loss risk, or hard blocker.
	Critical
)

// String returns the human-readable label for the level.
// The label is stable; do not change it — downstream consumers may depend on it.
func (l Level) String() string {
	switch l {
	case Critical:
		return "CRITICAL"
	case Major:
		return "MAJOR"
	case Minor:
		return "minor"
	default:
		return "unknown"
	}
}

// DefaultForFailingCheck returns the severity assigned to a CI check that
// failed but carries no additional context (e.g. a status-check with no body).
func DefaultForFailingCheck() Level { return Major }

// DefaultForBareComment returns the severity assigned to a human or bot
// comment whose body cannot be parsed by any registered adapter.
func DefaultForBareComment() Level { return Minor }

// Adapter maps source-specific vocabulary onto the severity ladder.
// Implementations must be safe for concurrent use.
type Adapter interface {
	// Parse inspects the comment body and returns the appropriate Level.
	// If the body is unrecognizable the adapter should return Minor.
	Parse(body string) Level
}

// AdapterFunc is a function that implements Adapter. It lets callers register
// inline adapters without defining a named type.
type AdapterFunc func(body string) Level

// Parse implements Adapter.
func (f AdapterFunc) Parse(body string) Level { return f(body) }

// Registry maps source names to their Adapter implementations.
// A zero-value Registry is not usable; call NewRegistry.
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register adds or replaces the adapter for the given source name.
// source is compared case-sensitively.
func (r *Registry) Register(source string, a Adapter) {
	r.adapters[source] = a
}

// Parse dispatches to the registered adapter for source.
// If no adapter is registered for source it returns Minor (bare-comment default).
func (r *Registry) Parse(source, body string) Level {
	a, ok := r.adapters[source]
	if !ok {
		return DefaultForBareComment()
	}
	return a.Parse(body)
}

// --- Renderer ---

// ansi color codes (3-bit foreground).
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

type levelMeta struct {
	symbol string
	ansi   string
}

var meta = map[Level]levelMeta{
	Critical: {"■", ansiRed},
	Major:    {"▲", ansiYellow},
	Minor:    {"·", ansiCyan},
}

// colorEnabled reports whether ANSI color should be used.
// It checks the NO_COLOR environment variable per https://no-color.org/.
func colorEnabled() bool {
	return os.Getenv("NO_COLOR") == ""
}

// Render returns a formatted string for level.
// When color is true, ANSI escape codes wrap the output regardless of NO_COLOR.
// Callers that want to honor NO_COLOR should call RenderAuto instead.
// The symbol and label are always present regardless of color setting.
func Render(level Level, color bool) string {
	m, ok := meta[level]
	if !ok {
		m = levelMeta{"?", ""}
	}
	label := level.String()
	text := m.symbol + " " + label

	if !color || m.ansi == "" {
		return text
	}
	return m.ansi + text + ansiReset
}

// RenderPlain returns a plain-text (no ANSI codes) representation of level.
// It is equivalent to Render(level, false) and is the safe default for log output.
func RenderPlain(level Level) string {
	return Render(level, false)
}

// RenderAuto returns a colored representation when stdout appears to be a TTY
// and NO_COLOR is unset, otherwise plain text.
// It uses os.Stdout to detect TTY; callers with custom writers should use Render.
func RenderAuto(level Level) string {
	if !isTTY() || !colorEnabled() {
		return RenderPlain(level)
	}
	return Render(level, true)
}

// isTTY returns true when os.Stdout is a character device (terminal).
func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ParseKeywords is a helper used by adapters. It returns the first Level whose
// keywords appear (case-insensitive) in body, scanning in descending severity
// order (Critical first). If no keyword matches it returns the fallback.
func ParseKeywords(body string, rules []KeywordRule, fallback Level) Level {
	lower := strings.ToLower(body)
	for _, r := range rules {
		for _, kw := range r.Keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return r.Level
			}
		}
	}
	return fallback
}

// KeywordRule maps a set of keywords to a severity Level.
type KeywordRule struct {
	Level    Level
	Keywords []string
}
