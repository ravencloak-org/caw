// Package severity defines a closed 3-level severity ladder (CRITICAL / MAJOR / minor)
// and a renderer that emits a symbol+label pair, optionally with ANSI color.
//
// # Design rationale
//
// The ladder is intentionally frozen at three levels. Code-review signal
// sources (CI checks, bots, human reviewers) all map onto "must fix",
// "should fix", and "nice to fix" — a finer-grained scale would require
// consensus across every future consumer.
//
// The renderer always emits a symbol+label pair (e.g. "■ CRITICAL") so the
// output is unambiguous on colorblind and no-color terminals. ANSI color
// codes are layered on top and suppressed when the caller passes
// color=false.
package severity

// Level is the severity of a review signal. Higher values are more severe.
// The ladder is closed at three levels.
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

// Render returns a formatted string for level.
// When color is true, ANSI escape codes wrap the output.
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
