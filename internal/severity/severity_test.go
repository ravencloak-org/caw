package severity_test

import (
	"strings"
	"testing"

	"github.com/ravencloak-org/caw/internal/severity"
)

// TestLadderOrdering verifies CRITICAL > MAJOR > minor (stable ints).
func TestLadderOrdering(t *testing.T) {
	if severity.Critical <= severity.Major {
		t.Fatalf("Critical(%d) must be > Major(%d)", severity.Critical, severity.Major)
	}
	if severity.Major <= severity.Minor {
		t.Fatalf("Major(%d) must be > Minor(%d)", severity.Major, severity.Minor)
	}
}

func TestLevelString(t *testing.T) {
	cases := []struct {
		level severity.Level
		want  string
	}{
		{severity.Critical, "CRITICAL"},
		{severity.Major, "MAJOR"},
		{severity.Minor, "minor"},
	}
	for _, tc := range cases {
		if got := tc.level.String(); got != tc.want {
			t.Errorf("Level(%d).String() = %q, want %q", tc.level, got, tc.want)
		}
	}
}

// TestDefaultForFailingCheck ensures unknown/check-fail → MAJOR.
func TestDefaultForFailingCheck(t *testing.T) {
	got := severity.DefaultForFailingCheck()
	if got != severity.Major {
		t.Errorf("DefaultForFailingCheck() = %v, want MAJOR", got)
	}
}

// TestDefaultForBareComment ensures bare/unparseable comment → minor.
func TestDefaultForBareComment(t *testing.T) {
	got := severity.DefaultForBareComment()
	if got != severity.Minor {
		t.Errorf("DefaultForBareComment() = %v, want minor", got)
	}
}

// TestRegistryUnknownSource falls back to minor for an unregistered source.
func TestRegistryUnknownSource(t *testing.T) {
	r := severity.NewRegistry()
	got := r.Parse("unknown-bot", "some comment body")
	if got != severity.Minor {
		t.Errorf("Registry.Parse(unknown) = %v, want minor", got)
	}
}

func TestRegistryRegisterAndParse(t *testing.T) {
	r := severity.NewRegistry()

	// Register a trivial adapter that always returns Critical.
	r.Register("always-critical", severity.AdapterFunc(func(_ string) severity.Level {
		return severity.Critical
	}))

	got := r.Parse("always-critical", "anything")
	if got != severity.Critical {
		t.Errorf("Registry.Parse(always-critical) = %v, want CRITICAL", got)
	}
}

// --- CodeRabbit adapter tests ---

func TestCodeRabbitAdapter(t *testing.T) {
	adapter := severity.NewCodeRabbitAdapter()

	cases := []struct {
		name string
		body string
		want severity.Level
	}{
		// CRITICAL: security vulnerabilities / blocking issues
		{
			name: "security_vulnerability",
			body: "**security vulnerability**: SQL injection possible here.",
			want: severity.Critical,
		},
		{
			name: "blocking_issue_emoji",
			body: "🚨 This is a critical security issue that must be fixed before merge.",
			want: severity.Critical,
		},
		{
			name: "critical_label_uppercase",
			body: "[CRITICAL] Null pointer dereference in production path.",
			want: severity.Critical,
		},
		// MAJOR: potential issues, warnings, important suggestions
		{
			name: "potential_issue",
			body: "**Potential issue**: This loop may run forever if the input is unbounded.",
			want: severity.Major,
		},
		{
			name: "warning_emoji",
			body: "⚠️ The error from `os.Remove` is silently dropped here.",
			want: severity.Major,
		},
		{
			name: "major_label",
			body: "[MAJOR] Missing error check on database write.",
			want: severity.Major,
		},
		{
			name: "refactor_suggestion",
			body: "**Refactor suggestion**: Extract this logic into a helper to reduce duplication.",
			want: severity.Major,
		},
		// minor: nitpicks, style, docs
		{
			name: "nitpick",
			body: "**Nitpick**: Consider renaming `x` to `count` for clarity.",
			want: severity.Minor,
		},
		{
			name: "nitpick_emoji",
			body: "🔧 Nit: trailing whitespace on line 42.",
			want: severity.Minor,
		},
		{
			name: "minor_label",
			body: "[minor] Typo in comment: 'recieve' should be 'receive'.",
			want: severity.Minor,
		},
		{
			name: "style_suggestion",
			body: "Style: prefer `errors.Is` over direct comparison here.",
			want: severity.Minor,
		},
		// unparseable → minor (default)
		{
			name: "bare_lgtm",
			body: "LGTM!",
			want: severity.Minor,
		},
		{
			name: "empty_body",
			body: "",
			want: severity.Minor,
		},
		{
			name: "unrelated_text",
			body: "Thanks for the contribution! This looks good overall.",
			want: severity.Minor,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adapter.Parse(tc.body)
			if got != tc.want {
				t.Errorf("CodeRabbitAdapter.Parse(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// --- Renderer tests ---

func TestRenderContainsSymbolAndLabel(t *testing.T) {
	cases := []struct {
		level      severity.Level
		wantSymbol string
		wantLabel  string
	}{
		{severity.Critical, "■", "CRITICAL"},
		{severity.Major, "▲", "MAJOR"},
		{severity.Minor, "·", "minor"},
	}
	for _, tc := range cases {
		t.Run(tc.level.String(), func(t *testing.T) {
			got := severity.Render(tc.level, false) // color disabled
			if !strings.Contains(got, tc.wantSymbol) {
				t.Errorf("Render(%v, false) = %q missing symbol %q", tc.level, got, tc.wantSymbol)
			}
			if !strings.Contains(got, tc.wantLabel) {
				t.Errorf("Render(%v, false) = %q missing label %q", tc.level, got, tc.wantLabel)
			}
		})
	}
}

func TestRenderNoColorProducesNoEscapeCodes(t *testing.T) {
	for _, level := range []severity.Level{severity.Critical, severity.Major, severity.Minor} {
		got := severity.Render(level, false)
		if strings.Contains(got, "\x1b[") {
			t.Errorf("Render(%v, false) contains ANSI escape: %q", level, got)
		}
	}
}

func TestRenderWithColorProducesEscapeCodes(t *testing.T) {
	for _, level := range []severity.Level{severity.Critical, severity.Major, severity.Minor} {
		got := severity.Render(level, true)
		if !strings.Contains(got, "\x1b[") {
			t.Errorf("Render(%v, true) missing ANSI escape: %q", level, got)
		}
	}
}

func TestRenderPlainAlwaysContainsSymbolAndLabel(t *testing.T) {
	// RenderPlain is the convenience wrapper that never emits color codes.
	cases := []struct {
		level severity.Level
	}{
		{severity.Critical},
		{severity.Major},
		{severity.Minor},
	}
	for _, tc := range cases {
		got := severity.RenderPlain(tc.level)
		if strings.Contains(got, "\x1b[") {
			t.Errorf("RenderPlain(%v) = %q contains ANSI codes", tc.level, got)
		}
		if !strings.Contains(got, tc.level.String()) {
			t.Errorf("RenderPlain(%v) = %q missing label", tc.level, got)
		}
	}
}
