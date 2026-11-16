package livetest

import (
	"fmt"
	"strings"
	"testing"
)

// recorder stands in for a test so Require's skip can be observed instead of ending this test.
type recorder struct {
	testing.TB
	skipped string
}

func (r *recorder) Helper() {}

func (r *recorder) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
	panic(r)
}

// gate runs fn against a recorder and returns the skip message, empty when fn was not skipped.
func gate(fn func(tb testing.TB)) (skipped string) {
	r := &recorder{}
	defer func() {
		if p := recover(); p != nil && p != r {
			panic(p)
		}
		skipped = r.skipped
	}()
	fn(r)
	return ""
}

const key = "LIVETEST_SELF_TEST_API_KEY"

func TestRequire(t *testing.T) {
	cases := []struct {
		name, optIn, key string
		skip             string // substring of the skip message; empty means the test runs
	}{
		{"a key alone never runs a live test", "", "sk-real-looking", "set BIFROST_LIVE_TESTS=1"},
		{"only the value 1 opts in", "true", "sk-real-looking", "set BIFROST_LIVE_TESTS=1"},
		{"the opt-in without the key skips and names it", "1", "", key + " is not"},
		{"a blank key counts as unset", "1", "   ", key + " is not"},
		{"the opt-in and the key run it", "1", "sk-real-looking", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(OptIn, tc.optIn)
			t.Setenv(key, tc.key)
			got := gate(func(tb testing.TB) { Require(tb, key) })
			if tc.skip == "" && got != "" {
				t.Fatalf("Require skipped with %q, want it to run", got)
			}
			if tc.skip != "" && !strings.Contains(got, tc.skip) {
				t.Fatalf("Require skip message = %q, want it to contain %q", got, tc.skip)
			}
		})
	}
}

func TestKey(t *testing.T) {
	t.Setenv(OptIn, "")
	t.Setenv(key, "sk-real-looking")
	if got := gate(func(tb testing.TB) { Key(tb, key) }); !strings.Contains(got, OptIn) {
		t.Fatalf("Key with no opt-in skipped with %q, want it to name %s", got, OptIn)
	}

	t.Setenv(OptIn, "1")
	t.Setenv(key, "  sk-real-looking  ")
	var value string
	if got := gate(func(tb testing.TB) { value = Key(tb, key) }); got != "" {
		t.Fatalf("Key with the opt-in and the key skipped with %q", got)
	}
	if value != "sk-real-looking" {
		t.Fatalf("Key returned %q, want the trimmed value", value)
	}
}

func TestRequireWithNoVariablesNeedsOnlyTheOptIn(t *testing.T) {
	t.Setenv(OptIn, "")
	if got := gate(func(tb testing.TB) { Require(tb) }); !strings.Contains(got, "set BIFROST_LIVE_TESTS=1 to run it") {
		t.Fatalf("Require() with no opt-in skipped with %q", got)
	}
	t.Setenv(OptIn, "1")
	if got := gate(func(tb testing.TB) { Require(tb) }); got != "" {
		t.Fatalf("Require() with the opt-in skipped with %q", got)
	}
}
