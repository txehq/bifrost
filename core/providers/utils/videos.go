package utils

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// StripVideoIDProviderSuffix removes ":<provider>" from a video ID if present.
func StripVideoIDProviderSuffix(videoID string, provider schemas.ModelProvider) string {
	suffix := ":" + string(provider)
	stripped := strings.TrimSuffix(videoID, suffix)
	// URL decode the ID to restore original characters (e.g., %2F -> /)
	if decoded, err := url.PathUnescape(stripped); err == nil {
		return decoded
	}
	return stripped
}

// AddVideoIDProviderSuffix ensures a video ID is scoped as "<id>:<provider>".
func AddVideoIDProviderSuffix(videoID string, provider schemas.ModelProvider) string {
	if videoID == "" {
		return videoID
	}
	suffix := ":" + string(provider)
	if strings.HasSuffix(videoID, suffix) {
		return videoID
	}
	// URL-encode the video ID to make it safe for URL paths
	// This converts / to %2F and other special characters
	escapedVideoID := url.PathEscape(videoID)
	return escapedVideoID + suffix
}

// VideoStatusTable is one provider's mapping from the vendor's raw task status strings to the
// task object's status (fork, TXE-6521, contract C7 BF11). Keys are the vendor's strings exactly
// as the vendor sends them. Each provider declares one table and sets a task's status only
// through Apply, so status and provider_status always come from the same vendor string and a
// vendor's cancelled or expired state is never folded into "failed".
type VideoStatusTable map[string]schemas.VideoStatus

// Lookup returns the status the table maps the vendor's raw string to. ok is false when the
// table has no entry for it.
func (t VideoStatusTable) Lookup(raw string) (status schemas.VideoStatus, ok bool) {
	status, ok = t[raw]
	return status, ok
}

// Apply sets resp.Status from the vendor's raw status string and records that string verbatim in
// resp.ProviderStatus. A string the table does not know maps to in_progress: Bifrost does not
// know the state to be terminal, so it must not report one, and the caller reads the vendor's
// own word in provider_status. It returns false for such a string, so a provider can log it.
func (t VideoStatusTable) Apply(resp *schemas.BifrostVideoGenerationResponse, raw string) bool {
	if resp == nil {
		return false
	}
	resp.ProviderStatus = raw
	status, ok := t.Lookup(raw)
	if !ok {
		status = schemas.VideoStatusInProgress
	}
	resp.Status = status
	return ok
}

// VideoExpiresAt is the shared rule for a task object's expires_at, the expiry of its artifact
// links (fork, TXE-6521, contract C7/C9 BF14): the vendor's own value when the vendor returns
// one; else, when the vendor's documentation states a fixed link lifetime, the vendor's
// completion time plus that lifetime; else nil. Times are Unix seconds. It never uses the
// current time: Bifrost stores no task, so two queries of one task must agree.
func VideoExpiresAt(vendorExpiresAt *int64, completedAt *int64, linkLifetime time.Duration) *int64 {
	if vendorExpiresAt != nil {
		return schemas.Ptr(*vendorExpiresAt)
	}
	if completedAt == nil || linkLifetime <= 0 {
		return nil
	}
	return schemas.Ptr(*completedAt + int64(linkLifetime/time.Second))
}

// SetVideoExpiresAt applies VideoExpiresAt to a task object, using resp.CompletedAt as the
// completion time. It leaves expires_at empty on a task that is not completed, which has no
// artifact link to expire. A provider calls it after it has set the status and completed_at,
// passing the vendor's own expiry (nil when the vendor returns none) and its documented link
// lifetime (0 when the vendor documents none).
func SetVideoExpiresAt(resp *schemas.BifrostVideoGenerationResponse, vendorExpiresAt *int64, linkLifetime time.Duration) {
	if resp == nil {
		return
	}
	if resp.Status != schemas.VideoStatusCompleted {
		resp.ExpiresAt = nil
		return
	}
	resp.ExpiresAt = VideoExpiresAt(vendorExpiresAt, resp.CompletedAt, linkLifetime)
}

// CancelVideoWhileQueued is the cancel of a vendor whose cancel API is also its delete API
// (ModelArk, MiniMax v2 DELETE): calling it on a finished task would delete the artifact and the
// record (fork, TXE-6521, contract C7 BF13). It queries the task once with retrieve and calls
// cancel only while the task is queued. For any other state it returns
// provider_cancel_unsupported without calling cancel, so a finished task stays as it is.
//
// On success it returns the queried task object with status cancelled. provider_status stays the
// vendor's last reported string, the one the query returned: the vendor is not queried again.
// The vendor can still start the task between the query and the cancel; then cancel's own error
// is returned, and the provider maps the vendor's "cannot cancel in this state" error to
// NewVideoCancelUnsupportedError.
func CancelVideoWhileQueued(
	retrieve func() (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError),
	cancel func() *schemas.BifrostError,
) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	task, bifrostErr := retrieve()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if task == nil {
		return nil, schemas.NewVideoCancelUnsupportedError("the provider returned no task to cancel")
	}
	if task.Status != schemas.VideoStatusQueued {
		unsupported := schemas.NewVideoCancelUnsupportedError(fmt.Sprintf(
			"the task is %s; this provider cancels a task only while it is queued", task.Status))
		unsupported.ExtraFields.RawResponse = task.ExtraFields.RawResponse
		return nil, unsupported
	}
	if bifrostErr := cancel(); bifrostErr != nil {
		return nil, bifrostErr
	}
	task.Status = schemas.VideoStatusCancelled
	return task, nil
}
