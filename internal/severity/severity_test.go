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
	for _, level := range []severity.Level{severity.Critical, severity.Major, severity.Minor} {
		got := severity.RenderPlain(level)
		if strings.Contains(got, "\x1b[") {
			t.Errorf("RenderPlain(%v) = %q contains ANSI codes", level, got)
		}
		if !strings.Contains(got, level.String()) {
			t.Errorf("RenderPlain(%v) = %q missing label", level, got)
		}
	}
}
