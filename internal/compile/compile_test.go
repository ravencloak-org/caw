package compile

import (
	"strings"
	"testing"

	"github.com/ravencloak-org/caw/internal/store"
)

func sig(typ, src, sev, body string) store.Signal {
	return store.Signal{
		Owner: "o", Repo: "r", Number: 1, SHA: "sha",
		SignalType: typ, Source: src, Severity: sev, Body: body,
	}
}

func TestCompileGroupsByTypeWithDynamicSources(t *testing.T) {
	s := Compile("o/r#1@sha", 2, []store.Signal{
		sig("comments", "CodeRabbit", "MAJOR", "nit"),
		sig("checks", "CI", "", "lint failed"),
		sig("checks", "CI", "", "vet failed"),
		sig("comments", "human", "", "lgtm?"),
	})

	if s.Key != "o/r#1@sha" || s.Seq != 2 {
		t.Fatalf("key/seq wrong: key=%q seq=%d", s.Key, s.Seq)
	}
	if len(s.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(s.Groups))
	}
	if s.Groups[0].Type != "checks" || s.Groups[1].Type != "comments" {
		t.Fatalf("ladder order wrong: %s then %s", s.Groups[0].Type, s.Groups[1].Type)
	}
	if s.Groups[0].Count != 2 {
		t.Fatalf("checks count = %d, want 2", s.Groups[0].Count)
	}
	if got := s.Groups[0].Sources; len(got) != 1 || got[0] != "CI" {
		t.Fatalf("checks sources = %v, want [CI] (deduped)", got)
	}
	if got := s.Groups[1].Sources; len(got) != 2 || got[0] != "CodeRabbit" || got[1] != "human" {
		t.Fatalf("comments sources = %v, want sorted [CodeRabbit human]", got)
	}
}

func TestCompileEmpty(t *testing.T) {
	s := Compile("k", 1, nil)
	if len(s.Groups) != 0 {
		t.Fatalf("groups = %d, want 0", len(s.Groups))
	}
	if !strings.Contains(s.Text, "no signals") {
		t.Fatalf("text = %q", s.Text)
	}
}

func TestCompileTextRendering(t *testing.T) {
	s := Compile("o/r#1@sha", 1, []store.Signal{sig("checks", "CI", "", "x")})
	if !strings.Contains(s.Text, "checks: 1") || !strings.Contains(s.Text, "[CI]") {
		t.Fatalf("text = %q", s.Text)
	}
}

func TestCompileUnknownTypeOrderedAfterKnown(t *testing.T) {
	s := Compile("k", 1, []store.Signal{
		sig("zzz", "x", "", ""),
		sig("checks", "CI", "", ""),
		sig("mergeability", "poll", "", ""),
	})
	if s.Groups[0].Type != "checks" || s.Groups[1].Type != "mergeability" || s.Groups[2].Type != "zzz" {
		t.Fatalf("order wrong: %s, %s, %s", s.Groups[0].Type, s.Groups[1].Type, s.Groups[2].Type)
	}
}
