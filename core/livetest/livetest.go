// Package livetest gates the tests that call real vendors (TXE-7530).
//
// A live test used to switch on when its vendor key was in the environment. A key exported for
// an unrelated reason was enough: a plain `go test ./...` in core/ with DEEPSEEK_API_KEY set
// called api.deepseek.com until the package timed out. Now a live test runs only when
// BIFROST_LIVE_TESTS=1 is set as well as its keys. A key on its own is never enough.
//
// Every live test calls Require (or Key) before it builds a client or reads a credential, and
// .github/workflows/scripts/check-live-tests-opt-in.py fails a test that reads a vendor
// credential, or sets up the live provider harness, without going through this package. The
// shared harness (core/internal/llmtests) refuses to build a client unless Enabled reports true,
// so a suite that forgets the call fails loudly instead of spending money.
package livetest

import (
	"os"
	"strings"
	"testing"
)

// OptIn is the environment variable that switches live tests on. Only the value "1" counts.
const OptIn = "BIFROST_LIVE_TESTS"

// Enabled reports whether live tests are switched on for this process.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv(OptIn)) == "1"
}

// Require skips t unless live tests are switched on and every named environment variable is
// set to a non-blank value. Call it first in a live test, before any client is built.
func Require(t testing.TB, vars ...string) {
	t.Helper()
	if !Enabled() {
		if len(vars) == 0 {
			t.Skipf("live test: set %s=1 to run it", OptIn)
		}
		t.Skipf("live test: set %s=1 and %s to run it", OptIn, strings.Join(vars, ", "))
	}
	var missing []string
	for _, name := range vars {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Skipf("live test: %s is set, but %s is not", OptIn, strings.Join(missing, ", "))
	}
}

// Key returns the value of a vendor credential for a live test, skipping t as Require does when
// live tests are off or the variable is blank.
func Key(t testing.TB, name string) string {
	t.Helper()
	Require(t, name)
	return strings.TrimSpace(os.Getenv(name))
}
