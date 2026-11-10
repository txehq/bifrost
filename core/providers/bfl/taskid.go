package bfl

import (
	"net/url"
	"regexp"
	"strings"
)

// A BFL task is polled at the polling_url its submit returned, and BFL's global endpoint hands
// out regional polling hosts (api.us1.bfl.ai). Bifrost stores no task, so the task id carries
// what the query needs (contract C7, BFL vendor sheet B3): the vendor part of
// "{vendor part}:bfl" is
//
//	{BFL id}|{model}            poll the configured base_url
//	{BFL id}|{model}|{host}     poll https://{host}, a BFL host other than the configured base
//
// The model gives the link lifetime of a completed task (one hour for FLUX 3, ten minutes
// otherwise). A bare {BFL id} also works: it polls the configured base and reports no link
// expiry, which needs the model.
//
// The id comes back from the caller, so the host is honoured only when it is one of BFL's own
// (api.bfl.ai or api.<region>.bfl.ai): the query sends the key, and a crafted id must not send
// it anywhere else.

const taskIDSeparator = "|"

// bflHostPattern matches BFL's API hosts: the global api.bfl.ai and the regional api.<region>.bfl.ai.
var bflHostPattern = regexp.MustCompile(`^api(\.[a-z0-9-]+)?\.bfl\.ai$`)

// bflTask is what a task id carries.
type bflTask struct {
	id    string // BFL's task id
	model string // the model the task was created with; empty for a bare id
	host  string // the polling host when it is not the configured base; empty otherwise
}

// encodeTaskID builds the vendor part of the task id for a task BFL created. pollingURL is the
// submit response's polling_url ("" when the response had none); baseURL the configured base.
func encodeTaskID(bflID, model, pollingURL, baseURL string) string {
	vendorID := bflID + taskIDSeparator + model
	if host := pollingHost(pollingURL, baseURL); host != "" {
		vendorID += taskIDSeparator + host
	}
	return vendorID
}

// pollingHost returns the host to record for pollingURL: a BFL host reached over https that
// differs from the configured base's host. Anything else returns "": the query then polls the
// configured base, which BFL's global endpoint serves for every region.
func pollingHost(pollingURL, baseURL string) string {
	if pollingURL == "" {
		return ""
	}
	parsed, err := url.Parse(pollingURL)
	if err != nil || parsed.Scheme != "https" || parsed.Port() != "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if !bflHostPattern.MatchString(host) {
		return ""
	}
	if base, err := url.Parse(baseURL); err == nil && strings.EqualFold(base.Host, host) {
		return ""
	}
	return host
}

// decodeTaskID reads the vendor part of a task id. ok is false when it is empty or names a host
// that is not BFL's.
func decodeTaskID(vendorID string) (task bflTask, ok bool) {
	parts := strings.SplitN(vendorID, taskIDSeparator, 3)
	task.id = parts[0]
	if task.id == "" {
		return task, false
	}
	if len(parts) > 1 {
		task.model = parts[1]
	}
	if len(parts) > 2 {
		task.host = strings.ToLower(parts[2])
		if !bflHostPattern.MatchString(task.host) {
			return task, false
		}
	}
	return task, true
}

// pollURL is where the task is polled: BFL's get_result on the recorded host, or on the
// configured base when the id records none.
func (t bflTask) pollURL(baseURL string) string {
	origin := baseURL
	if t.host != "" {
		origin = "https://" + t.host
	}
	return origin + "/v1/get_result?id=" + url.QueryEscape(t.id)
}
