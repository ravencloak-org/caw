package server

import "testing"

// TestWithLeaseTTL covers the Option returned by WithLeaseTTL. The option
// unconditionally records the requested TTL on serverOptions; New() is what
// decides (via leaseTTLSeconds > 0) whether to apply it to the Hub, so here we
// assert the option writes the value through for both a positive override and
// the non-positive sentinel that New() treats as "keep the default".
func TestWithLeaseTTL(t *testing.T) {
	cases := []struct {
		name    string
		seconds int64
	}{
		{"positive override", 120},
		{"zero keeps default", 0},
		{"negative keeps default", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var o serverOptions
			WithLeaseTTL(tc.seconds)(&o)
			if o.leaseTTLSeconds != tc.seconds {
				t.Errorf("leaseTTLSeconds = %d, want %d", o.leaseTTLSeconds, tc.seconds)
			}
		})
	}
}
