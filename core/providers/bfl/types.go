package bfl

import "encoding/json"

// BFLError is the body BFL returns with an error status. BFL's API is FastAPI, so detail
// is either a string ({"detail": "Insufficient credits"}) or a list of validation
// errors ({"detail": [{"loc": ["body", "width"], "msg": "...", "type": "..."}]}).
type BFLError struct {
	Detail json.RawMessage `json:"detail,omitempty"`
}

// BFLValidationError is one entry of a 422 detail list.
type BFLValidationError struct {
	Loc  []any  `json:"loc,omitempty"`
	Msg  string `json:"msg,omitempty"`
	Type string `json:"type,omitempty"`
}
