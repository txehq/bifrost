package bfl

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseBFLError converts a non-2xx BFL response into a BifrostError. The vendor's status
// code is kept, a 429 included (contract C6, BFL vendor sheet B9). error_type comes from
// the status: BFL's errors carry no code of their own, and its statuses already name the
// cause (401/403 key, 402 credits, 422 parameters, 429 rate or too many active tasks).
// BFL's moderation arrives as a task status, not an HTTP error, so it is not parsed here.
func parseBFLError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp BFLError
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	message, param, code := bflErrorDetail(errorResp.Detail)
	if message != "" {
		bifrostErr.Error.Message = message
	}
	if param != "" {
		bifrostErr.Error.Param = param
	}
	if code != "" {
		bifrostErr.Error.Code = schemas.Ptr(code)
	}
	if bifrostErr.Error.Message == "" {
		bifrostErr.Error.Message = "BFL API request failed"
	}
	return bifrostErr
}

// bflErrorDetail reads FastAPI's detail: a string is the message; a validation list gives
// each entry as "field: msg", the first entry's field as the param and its type as the code.
func bflErrorDetail(detail []byte) (message, param, code string) {
	if len(detail) == 0 {
		return "", "", ""
	}
	var text string
	if err := sonic.Unmarshal(detail, &text); err == nil {
		return text, "", ""
	}
	var entries []BFLValidationError
	if err := sonic.Unmarshal(detail, &entries); err != nil || len(entries) == 0 {
		return string(detail), "", ""
	}
	parts := make([]string, 0, len(entries))
	for i, entry := range entries {
		field := validationField(entry.Loc)
		if i == 0 {
			param, code = field, entry.Type
		}
		if field != "" {
			parts = append(parts, field+": "+entry.Msg)
		} else {
			parts = append(parts, entry.Msg)
		}
	}
	return strings.Join(parts, "; "), param, code
}

// validationField joins a FastAPI loc, dropping the leading "body", so
// ["body", "width"] reads "width".
func validationField(loc []any) string {
	parts := make([]string, 0, len(loc))
	for i, part := range loc {
		s := fmt.Sprint(part)
		if i == 0 && s == "body" {
			continue
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ".")
}
