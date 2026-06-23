package config

import (
	"testing"
	"time"
)

// TestGetenvDuration exercises every branch of getenvDuration directly:
// unset/empty, valid, malformed (unparseable), and non-positive all resolve
// against the supplied default.
func TestGetenvDuration(t *testing.T) {
	const key = "CAW_TEST_DURATION"
	def := 30 * time.Second

	cases := []struct {
		name string
		set  bool
		val  string
		want time.Duration
	}{
		{"unset falls back to default", false, "", def},
		{"empty falls back to default", true, "", def},
		{"valid is parsed", true, "45s", 45 * time.Second},
		{"valid minutes is parsed", true, "2m", 2 * time.Minute},
		{"malformed falls back to default", true, "not-a-duration", def},
		{"zero falls back to default", true, "0s", def},
		{"negative falls back to default", true, "-5s", def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.val)
			} else {
				// Ensure no ambient value leaks in from the environment.
				t.Setenv(key, "")
			}
			got := getenvDuration(key, def)
			if got != tc.want {
				t.Errorf("getenvDuration(%q) = %s, want %s", tc.val, got, tc.want)
			}
		})
	}
}

// TestGetenvInt64 exercises every branch of getenvInt64 directly: unset/empty,
// valid, malformed (unparseable), and non-positive all resolve against the
// supplied default.
func TestGetenvInt64(t *testing.T) {
	const key = "CAW_TEST_INT64"
	def := int64(90)

	cases := []struct {
		name string
		set  bool
		val  string
		want int64
	}{
		{"unset falls back to default", false, "", def},
		{"empty falls back to default", true, "", def},
		{"valid is parsed", true, "120", 120},
		{"malformed falls back to default", true, "abc", def},
		{"float-like falls back to default", true, "1.5", def},
		{"zero falls back to default", true, "0", def},
		{"negative falls back to default", true, "-1", def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.val)
			} else {
				t.Setenv(key, "")
			}
			got := getenvInt64(key, def)
			if got != tc.want {
				t.Errorf("getenvInt64(%q) = %d, want %d", tc.val, got, tc.want)
			}
		})
	}
}
