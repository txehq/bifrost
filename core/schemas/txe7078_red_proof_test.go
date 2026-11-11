package schemas

import "testing"

// Throwaway (TXE-7078): proves the PR-time Go unit test job fails a PR that breaks a core test.
// This file is never merged; its branch's PR is closed after the run.
func TestTXE7078DeliberatelyFailingAssertion(t *testing.T) {
	if got, want := 1+1, 3; got != want {
		t.Fatalf("deliberate failure for TXE-7078: got %d, want %d", got, want)
	}
}
