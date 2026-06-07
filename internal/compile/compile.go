// Package compile turns a Round's stored signals into the single summary that
// is pushed per settle (ADR-0004). Sources are attributed dynamically; the
// signal-type ladder is fixed (checks, comments, mergeability).
package compile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ravencloak-org/caw/internal/store"
)

// Item is one signal within a group.
type Item struct {
	Source   string `json:"source"`
	Severity string `json:"severity,omitempty"`
	Body     string `json:"body,omitempty"`
}

// SignalGroup collects all signals of one signal-type for a Round.
type SignalGroup struct {
	Type    string   `json:"type"`
	Sources []string `json:"sources"`
	Count   int      `json:"count"`
	Items   []Item   `json:"items"`
}

// Summary is the compiled, serializable artifact for one settle of a Round.
type Summary struct {
	Key    string        `json:"key"` // owner/repo#number@sha
	Seq    int           `json:"seq"` // monotonic per Round (ADR-0004)
	Groups []SignalGroup `json:"groups"`
	Text   string        `json:"text"`
}

// typeOrder fixes the rendering order of the signal-type ladder.
var typeOrder = map[string]int{"checks": 0, "comments": 1, "mergeability": 2}

// Compile groups signals by signal-type into one Summary tagged with seq.
func Compile(key string, seq int, signals []store.Signal) Summary {
	groups := make(map[string]*SignalGroup)
	sources := make(map[string]map[string]struct{})

	for _, sig := range signals {
		g := groups[sig.SignalType]
		if g == nil {
			g = &SignalGroup{Type: sig.SignalType}
			groups[sig.SignalType] = g
			sources[sig.SignalType] = make(map[string]struct{})
		}
		g.Count++
		g.Items = append(g.Items, Item{Source: sig.Source, Severity: sig.Severity, Body: sig.Body})
		if sig.Source != "" {
			sources[sig.SignalType][sig.Source] = struct{}{}
		}
	}

	out := make([]SignalGroup, 0, len(groups))
	for t, g := range groups {
		for s := range sources[t] {
			g.Sources = append(g.Sources, s)
		}
		sort.Strings(g.Sources)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oki := typeOrder[out[i].Type]
		oj, okj := typeOrder[out[j].Type]
		// Known ladder types first (in ladder order); unknown types after, alphabetical.
		if oki != okj {
			return oki
		}
		if oi != oj {
			return oi < oj
		}
		return out[i].Type < out[j].Type
	})

	return Summary{Key: key, Seq: seq, Groups: out, Text: render(key, seq, out)}
}

func render(key string, seq int, groups []SignalGroup) string {
	if len(groups) == 0 {
		return fmt.Sprintf("%s (seq %d): no signals", key, seq)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (seq %d)", key, seq)
	for _, g := range groups {
		fmt.Fprintf(&b, "\n- %s: %d", g.Type, g.Count)
		if len(g.Sources) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(g.Sources, ", "))
		}
	}
	return b.String()
}
