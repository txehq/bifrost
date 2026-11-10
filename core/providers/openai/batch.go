package openai

import (
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// OpenAI Batch API Types

// OpenAIBatchRequest represents the request body for creating a batch.
type OpenAIBatchRequest struct {
	InputFileID        *string                    `json:"input_file_id,omitempty"`
	Endpoint           string                     `json:"endpoint"`
	CompletionWindow   string                     `json:"completion_window"`
	Metadata           map[string]string          `json:"metadata,omitempty"`
	OutputExpiresAfter *schemas.BatchExpiresAfter `json:"output_expires_after,omitempty"`

	InputBlob    *string                    `json:"input_blob,omitempty"` // azure blob storage
	OutputFolder *schemas.BatchOutputFolder `json:"output_folder,omitempty"`
}

// OpenAIBatchResponse represents an OpenAI batch response.
type OpenAIBatchResponse struct {
	ID               string                    `json:"id"`
	Object           string                    `json:"object"`
	Endpoint         string                    `json:"endpoint"`
	Errors           *schemas.BatchErrors      `json:"errors,omitempty"`
	InputFileID      string                    `json:"input_file_id"`
	CompletionWindow string                    `json:"completion_window"`
	Status           string                    `json:"status"`
	OutputFileID     *string                   `json:"output_file_id,omitempty"`
	ErrorFileID      *string                   `json:"error_file_id,omitempty"`
	CreatedAt        int64                     `json:"created_at"`
	InProgressAt     *int64                    `json:"in_progress_at,omitempty"`
	ExpiresAt        *int64                    `json:"expires_at,omitempty"`
	FinalizingAt     *int64                    `json:"finalizing_at,omitempty"`
	CompletedAt      *int64                    `json:"completed_at,omitempty"`
	FailedAt         *int64                    `json:"failed_at,omitempty"`
	ExpiredAt        *int64                    `json:"expired_at,omitempty"`
	CancellingAt     *int64                    `json:"cancelling_at,omitempty"`
	CancelledAt      *int64                    `json:"cancelled_at,omitempty"`
	RequestCounts    *OpenAIBatchRequestCounts `json:"request_counts,omitempty"`
	Metadata         map[string]string         `json:"metadata,omitempty"`

	// Azure Blob Storage URLs (returned by Azure when using blob storage input/output)
	InputBlob  *string `json:"input_blob,omitempty"`
	OutputBlob *string `json:"output_blob,omitempty"`
	ErrorBlob  *string `json:"error_blob,omitempty"`
}

// OpenAIBatchRequestCounts represents the request counts for a batch.
type OpenAIBatchRequestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// OpenAIBatchListResponse represents the response from listing batches.
type OpenAIBatchListResponse struct {
	Object  string                `json:"object"`
	Data    []OpenAIBatchResponse `json:"data"`
	FirstID *string               `json:"first_id,omitempty"`
	LastID  *string               `json:"last_id,omitempty"`
	HasMore bool                  `json:"has_more"`
}

// ToBifrostBatchStatus converts OpenAI status to Bifrost status.
func ToBifrostBatchStatus(status string) schemas.BatchStatus {
	switch status {
	case "validating":
		return schemas.BatchStatusValidating
	case "failed":
		return schemas.BatchStatusFailed
	case "in_progress":
		return schemas.BatchStatusInProgress
	case "finalizing":
		return schemas.BatchStatusFinalizing
	case "completed":
		return schemas.BatchStatusCompleted
	case "expired":
		return schemas.BatchStatusExpired
	case "cancelling":
		return schemas.BatchStatusCancelling
	case "cancelled":
		return schemas.BatchStatusCancelled
	default:
		return schemas.BatchStatus(status)
	}
}

// ToBifrostBatchCreateResponse converts OpenAI batch response to Bifrost batch response.
func (r *OpenAIBatchResponse) ToBifrostBatchCreateResponse(latency time.Duration, sendBackRawRequest bool, sendBackRawResponse bool, rawRequest interface{}, rawResponse interface{}) *schemas.BifrostBatchCreateResponse {
	resp := &schemas.BifrostBatchCreateResponse{
		ID:               r.ID,
		Object:           r.Object,
		Endpoint:         r.Endpoint,
		InputFileID:      r.InputFileID,
		CompletionWindow: r.CompletionWindow,
		Status:           ToBifrostBatchStatus(r.Status),
		Metadata:         r.Metadata,
		CreatedAt:        r.CreatedAt,
		OutputFileID:     r.OutputFileID,
		ErrorFileID:      r.ErrorFileID,
		InputBlob:        r.InputBlob,
		OutputBlob:       r.OutputBlob,
		ErrorBlob:        r.ErrorBlob,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Latency: latency.Milliseconds(),
		},
	}

	if sendBackRawRequest {
		resp.ExtraFields.RawRequest = rawRequest
	}

	if r.ExpiresAt != nil {
		resp.ExpiresAt = r.ExpiresAt
	}

	if r.RequestCounts != nil {
		resp.RequestCounts = schemas.BatchRequestCounts{
			Total:     r.RequestCounts.Total,
			Completed: r.RequestCounts.Completed,
			Failed:    r.RequestCounts.Failed,
		}
	}

	if sendBackRawResponse {
		resp.ExtraFields.RawResponse = rawResponse
	}

	return resp
}

// ToBifrostBatchRetrieveResponse converts OpenAI batch response to Bifrost batch retrieve response.
func (r *OpenAIBatchResponse) ToBifrostBatchRetrieveResponse(latency time.Duration, sendBackRawRequest bool, sendBackRawResponse bool, rawRequest interface{}, rawResponse interface{}) *schemas.BifrostBatchRetrieveResponse {
	resp := &schemas.BifrostBatchRetrieveResponse{
		ID:               r.ID,
		Object:           r.Object,
		Endpoint:         r.Endpoint,
		InputFileID:      r.InputFileID,
		CompletionWindow: r.CompletionWindow,
		Status:           ToBifrostBatchStatus(r.Status),
		Metadata:         r.Metadata,
		CreatedAt:        r.CreatedAt,
		InProgressAt:     r.InProgressAt,
		FinalizingAt:     r.FinalizingAt,
		CompletedAt:      r.CompletedAt,
		FailedAt:         r.FailedAt,
		ExpiredAt:        r.ExpiredAt,
		CancellingAt:     r.CancellingAt,
		CancelledAt:      r.CancelledAt,
		OutputFileID:     r.OutputFileID,
		ErrorFileID:      r.ErrorFileID,
		Errors:           r.Errors,
		InputBlob:        r.InputBlob,
		OutputBlob:       r.OutputBlob,
		ErrorBlob:        r.ErrorBlob,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Latency: latency.Milliseconds(),
		},
	}

	if sendBackRawRequest {
		resp.ExtraFields.RawRequest = rawRequest
	}

	if r.ExpiresAt != nil {
		resp.ExpiresAt = r.ExpiresAt
	}

	if r.RequestCounts != nil {
		resp.RequestCounts = schemas.BatchRequestCounts{
			Total:     r.RequestCounts.Total,
			Completed: r.RequestCounts.Completed,
			Failed:    r.RequestCounts.Failed,
		}
	}

	if sendBackRawResponse {
		resp.ExtraFields.RawResponse = rawResponse
	}

	return resp
}

// OpenAI writes a batch's lines into two files: the output file holds the lines the model
// answered, the error file the lines that failed or expired. Both use one line shape:
//
//	{"id": "batch_req_...", "custom_id": "...", "response": {"status_code", "request_id", "body"}, "error": {"code", "message"}}

// batchErrorCodeExpired is the error code of a line the batch did not reach before its
// completion window ended.
const batchErrorCodeExpired = "batch_expired"

// openAIBatchResultLine is one line of an output or error file.
type openAIBatchResultLine struct {
	ID       string                       `json:"id"`
	CustomID string                       `json:"custom_id"`
	Response *schemas.BatchResultResponse `json:"response"`
	Error    *schemas.BatchResultError    `json:"error"`
}

// ParseBatchResultLine converts one line of an OpenAI or Azure batch output or error file
// into a result item: the vendor's line id is kept, status is one of succeeded, failed or
// expired, and a succeeded line's usage is the unified usage of the batch's endpoint. A
// line that failed with an HTTP error response keeps that response and also carries the
// vendor's error code and message in Error.
func ParseBatchResultLine(line []byte, endpoint schemas.BatchEndpoint) (schemas.BatchResultItem, error) {
	var parsed openAIBatchResultLine
	if err := sonic.Unmarshal(line, &parsed); err != nil {
		return schemas.BatchResultItem{}, err
	}
	item := schemas.BatchResultItem{
		ID:       parsed.ID,
		CustomID: parsed.CustomID,
		Response: parsed.Response,
		Error:    parsed.Error,
	}
	switch {
	case parsed.Error != nil:
		item.Status = schemas.BatchResultStatusFailed
		if parsed.Error.Code == batchErrorCodeExpired {
			item.Status = schemas.BatchResultStatusExpired
		}
	case parsed.Response != nil && parsed.Response.StatusCode >= 200 && parsed.Response.StatusCode < 300:
		item.Status = schemas.BatchResultStatusSucceeded
		if parsed.Response.Body != nil {
			schemas.UnifyBatchResultUsage(endpoint, parsed.Response.Body)
		}
	default:
		item.Status = schemas.BatchResultStatusFailed
		if parsed.Response != nil {
			item.Error = batchResultErrorFromBody(parsed.Response.Body)
		}
	}
	return item, nil
}

// batchResultErrorFromBody lifts the vendor's error out of a failed line's response body
// ({"error": {"code", "message", "type"}}). The code falls back to the error type, which
// is what OpenAI fills when a request error has no code.
func batchResultErrorFromBody(body map[string]any) *schemas.BatchResultError {
	vendorError, ok := body["error"].(map[string]any)
	if !ok {
		return nil
	}
	resultError := &schemas.BatchResultError{}
	if message, ok := vendorError["message"].(string); ok {
		resultError.Message = message
	}
	for _, field := range []string{"code", "type"} {
		if code, ok := vendorError[field].(string); ok && code != "" {
			resultError.Code = code
			break
		}
	}
	if resultError.Code == "" && resultError.Message == "" {
		return nil
	}
	return resultError
}

// ReadBatchResults reads one page of a batch's result lines from its ordered sources (the
// output file, then the error file) and converts each with ParseBatchResultLine. With
// request.Limit unset it reads both files to the end. Shared by the OpenAI and Azure
// providers, which differ only in how a file is opened.
func ReadBatchResults(sources []providerUtils.BatchResultsSource, request *schemas.BifrostBatchResultsRequest, endpoint schemas.BatchEndpoint, logger schemas.Logger) (*schemas.BifrostBatchResultsResponse, *schemas.BifrostError) {
	results := []schemas.BatchResultItem{}
	var parseErrors []schemas.BatchError
	page, bifrostErr := providerUtils.ReadBatchResultsPage(sources, request.After, request.Limit, func(_ int, line []byte) {
		item, err := ParseBatchResultLine(line, endpoint)
		if err != nil {
			if logger != nil {
				logger.Warn("failed to parse batch result line: %v", err)
			}
			parseErrors = append(parseErrors, schemas.BatchError{Code: "parse_error", Message: err.Error()})
			return
		}
		results = append(results, item)
	})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	response := &schemas.BifrostBatchResultsResponse{
		BatchID:    request.BatchID,
		Endpoint:   endpoint,
		Results:    results,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Latency: page.Latency.Milliseconds(),
		},
	}
	if len(parseErrors) > 0 {
		response.ExtraFields.ParseErrors = parseErrors
	}
	return response, nil
}
