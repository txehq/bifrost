// Package testtime scales the wall-clock timings tests depend on (TXE-7128).
//
// A test that sets a 300 ms idle timeout or a 100 ms lease assumes an idle machine. On a
// shared CI runner running twelve packages at once, the same work can take several times
// as long, and the test fails at random on a PR that changed nothing it covers. Every
// timeout, deadline, lease TTL and expiry below two seconds in a test is therefore written
// as Scale(d): d on a developer machine, and d times BIFROST_TEST_TIMEOUT_SCALE where CI
// sets it (.github/workflows/go-unit-tests.yml).
//
// Scale the durations that must keep their relation together: a lease and the delay that
// must outlast it both go through Scale, so the test checks the same ordering at any
// factor.
//
// Each Go module that needs the helper has its own copy at <module>/internal/testtime, so
// a module's tests never import a package its released dependencies may not have yet.
// core/internal/testtime is the canonical copy. TestCopiesAreIdentical fails when another
// copy differs from it, and TestNoLiteralShortTimingsInTests fails when a test passes a
// literal duration under the threshold to a timeout, deadline, TTL or expiry.
package testtime

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"
)

// EnvVar names the factor. Unset or empty means 1.
const EnvVar = "BIFROST_TEST_TIMEOUT_SCALE"

var (
	once   sync.Once
	factor float64
)

// Factor returns the scale factor. A value that is not a finite number of at least 1
// panics, so a mistyped factor fails the run instead of being ignored.
func Factor() float64 {
	once.Do(func() {
		f, err := parse(os.Getenv(EnvVar))
		if err != nil {
			panic(err)
		}
		factor = f
	})
	return factor
}

// Scale returns d multiplied by Factor.
func Scale(d time.Duration) time.Duration {
	return time.Duration(math.Round(float64(d) * Factor()))
}

// Seconds scales a whole number of seconds, rounding up, for settings counted in seconds
// such as DefaultRequestTimeoutInSeconds.
func Seconds(n int) int {
	return int(math.Ceil(float64(n) * Factor()))
}

// TB is the part of testing.TB that Eventually uses, so this package need not import testing.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// pollInterval is how often Eventually re-checks its condition.
const pollInterval = 5 * time.Millisecond

// Eventually waits until cond returns true and fails the test, naming what it waited
// for, if budget passes first. A test that waits for an asynchronous write waits here
// (or on a channel or WaitGroup the code signals), never on a sleep that guesses how
// long the write takes (TXE-7315, TXE-7299). Pass a budget of at least two seconds or
// Scale(d): the budget only bounds a failure, so a generous one costs nothing when cond
// holds. cond runs on the test goroutine and must not call t.Fatal itself.
func Eventually(t TB, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", budget, what)
			return
		}
		time.Sleep(pollInterval)
	}
}

func parse(raw string) (float64, error) {
	if raw == "" {
		return 1, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 1 {
		return 0, fmt.Errorf("%s=%q: want a number of at least 1", EnvVar, raw)
	}
	return f, nil
}
