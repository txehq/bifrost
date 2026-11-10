package utils

import (
	"testing"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// TXE-6521 (contract C7 BF11): a provider's status table maps the vendor's cancelled and expired
// states to their own statuses instead of "failed", and the vendor's raw string is kept verbatim
// in provider_status.
func TestVideoStatusTableApply(t *testing.T) {
	table := VideoStatusTable{
		"queued":    schemas.VideoStatusQueued,
		"running":   schemas.VideoStatusInProgress,
		"succeeded": schemas.VideoStatusCompleted,
		"failed":    schemas.VideoStatusFailed,
		"cancelled": schemas.VideoStatusCancelled,
		"expired":   schemas.VideoStatusExpired,
	}
	cases := []struct {
		raw       string
		want      schemas.VideoStatus
		wantKnown bool
	}{
		{"queued", schemas.VideoStatusQueued, true},
		{"running", schemas.VideoStatusInProgress, true},
		{"succeeded", schemas.VideoStatusCompleted, true},
		{"failed", schemas.VideoStatusFailed, true},
		{"cancelled", schemas.VideoStatusCancelled, true},
		{"expired", schemas.VideoStatusExpired, true},
		// The table holds the vendor's strings exactly: another spelling is another string.
		{"Cancelled", schemas.VideoStatusInProgress, false},
		// A state the table does not know is not reported as terminal.
		{"paused_by_vendor", schemas.VideoStatusInProgress, false},
		{"", schemas.VideoStatusInProgress, false},
	}
	for _, tc := range cases {
		resp := &schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusFailed, ProviderStatus: "stale"}
		known := table.Apply(resp, tc.raw)
		if known != tc.wantKnown {
			t.Errorf("%q: known = %v, want %v", tc.raw, known, tc.wantKnown)
		}
		if resp.Status != tc.want {
			t.Errorf("%q: status = %q, want %q", tc.raw, resp.Status, tc.want)
		}
		if resp.ProviderStatus != tc.raw {
			t.Errorf("%q: provider_status = %q, want the vendor's string verbatim", tc.raw, resp.ProviderStatus)
		}
	}

	if status, ok := table.Lookup("expired"); !ok || status != schemas.VideoStatusExpired {
		t.Errorf("Lookup(expired) = %q, %v", status, ok)
	}
	if _, ok := VideoStatusTable(nil).Lookup("queued"); ok {
		t.Error("a nil table knew a status")
	}
	if table.Apply(nil, "queued") {
		t.Error("Apply on a nil response reported a known status")
	}
}

// TXE-6521 (contract C7/C9 BF14): expires_at is the vendor's value when the vendor returns one,
// else the completion time plus the provider's documented link lifetime, else absent.
func TestVideoExpiresAt(t *testing.T) {
	completed := schemas.Ptr(int64(1762700000))
	vendor := schemas.Ptr(int64(1762703600))
	cases := []struct {
		name      string
		vendor    *int64
		completed *int64
		lifetime  time.Duration
		want      *int64
	}{
		{"vendor value wins over a declared lifetime", vendor, completed, 24 * time.Hour, vendor},
		{"vendor value without a completion time", vendor, nil, 0, vendor},
		{"completion time plus the declared lifetime", nil, completed, 24 * time.Hour, schemas.Ptr(int64(1762700000 + 86400))},
		{"ten-minute lifetime", nil, completed, 10 * time.Minute, schemas.Ptr(int64(1762700000 + 600))},
		{"no vendor value and no declared lifetime", nil, completed, 0, nil},
		{"declared lifetime but no completion time", nil, nil, 24 * time.Hour, nil},
	}
	for _, tc := range cases {
		got := VideoExpiresAt(tc.vendor, tc.completed, tc.lifetime)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s: expires_at = %d, want none", tc.name, *got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("%s: expires_at = %v, want %d", tc.name, got, *tc.want)
		}
	}
	if got := VideoExpiresAt(vendor, nil, 0); got == vendor {
		t.Error("VideoExpiresAt returned the caller's pointer instead of a copy")
	}

	// On the task object: only a completed task has an artifact link to expire.
	resp := &schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusCompleted, CompletedAt: completed}
	SetVideoExpiresAt(resp, nil, 24*time.Hour)
	if resp.ExpiresAt == nil || *resp.ExpiresAt != 1762700000+86400 {
		t.Errorf("completed task expires_at = %v", resp.ExpiresAt)
	}
	for _, status := range []schemas.VideoStatus{schemas.VideoStatusQueued, schemas.VideoStatusInProgress, schemas.VideoStatusFailed, schemas.VideoStatusCancelled, schemas.VideoStatusExpired} {
		resp := &schemas.BifrostVideoGenerationResponse{Status: status, CompletedAt: completed, ExpiresAt: vendor}
		SetVideoExpiresAt(resp, vendor, 24*time.Hour)
		if resp.ExpiresAt != nil {
			t.Errorf("%s task carries expires_at %d", status, *resp.ExpiresAt)
		}
	}
	SetVideoExpiresAt(nil, vendor, time.Hour)
}

// TXE-6521 (contract C7 BF13): where the vendor's cancel API is also its delete API, the task is
// queried first and the API is called only while the task is queued. A finished task is never
// deleted: the cancel is refused with provider_cancel_unsupported and no cancel call.
func TestCancelVideoWhileQueued(t *testing.T) {
	for _, status := range schemas.VideoStatuses {
		t.Run(string(status), func(t *testing.T) {
			retrieves, cancels := 0, 0
			task := &schemas.BifrostVideoGenerationResponse{ID: "cgt-1:byteplus", Status: status, ProviderStatus: "vendor-" + string(status)}
			resp, bifrostErr := CancelVideoWhileQueued(
				func() (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
					retrieves++
					return task, nil
				},
				func() *schemas.BifrostError {
					cancels++
					return nil
				},
			)
			if retrieves != 1 {
				t.Fatalf("queried the task %d times, want once", retrieves)
			}
			if status == schemas.VideoStatusQueued {
				if bifrostErr != nil {
					t.Fatalf("cancel of a queued task failed: %+v", bifrostErr.Error)
				}
				if cancels != 1 {
					t.Fatalf("called the vendor's cancel %d times, want once", cancels)
				}
				if resp.Status != schemas.VideoStatusCancelled || resp.ProviderStatus != "vendor-queued" || resp.ID != "cgt-1:byteplus" {
					t.Fatalf("task object = %+v, want the queried task with status cancelled", resp)
				}
				return
			}
			if cancels != 0 {
				t.Fatalf("called the vendor's cancel-and-delete API on a %s task", status)
			}
			if resp != nil || bifrostErr == nil {
				t.Fatalf("resp/err = %v/%v, want a refusal", resp, bifrostErr)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderCancelUnsupported {
				t.Fatalf("status/error_type = %v/%q, want 400/provider_cancel_unsupported", bifrostErr.StatusCode, bifrostErr.ExtraFields.ErrorType)
			}
		})
	}

	t.Run("the query fails", func(t *testing.T) {
		cancels := 0
		queryErr := &schemas.BifrostError{StatusCode: schemas.Ptr(404), Error: &schemas.ErrorField{Message: "task not found"}}
		_, bifrostErr := CancelVideoWhileQueued(
			func() (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) { return nil, queryErr },
			func() *schemas.BifrostError { cancels++; return nil },
		)
		if bifrostErr != queryErr || cancels != 0 {
			t.Fatalf("err/cancels = %v/%d, want the query's own error and no cancel call", bifrostErr, cancels)
		}
	})

	t.Run("the vendor refuses the cancel", func(t *testing.T) {
		vendorErr := &schemas.BifrostError{StatusCode: schemas.Ptr(409), Error: &schemas.ErrorField{Message: "task is running"}}
		resp, bifrostErr := CancelVideoWhileQueued(
			func() (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
				return &schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusQueued}, nil
			},
			func() *schemas.BifrostError { return vendorErr },
		)
		if resp != nil || bifrostErr != vendorErr {
			t.Fatalf("resp/err = %v/%v, want the vendor's own error", resp, bifrostErr)
		}
	})

	t.Run("the query returns no task", func(t *testing.T) {
		cancels := 0
		_, bifrostErr := CancelVideoWhileQueued(
			func() (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) { return nil, nil },
			func() *schemas.BifrostError { cancels++; return nil },
		)
		if bifrostErr == nil || cancels != 0 {
			t.Fatalf("err/cancels = %v/%d, want a refusal and no cancel call", bifrostErr, cancels)
		}
	})
}
