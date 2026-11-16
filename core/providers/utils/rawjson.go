package utils

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Strict reading of a body the caller wrote whole (raw request body mode). A provider that bills
// what such a body asks for (schemas.RawImageBodyReader, TXE-7502) reads it member by member:
// encoding/json keeps only the last copy of a repeated name, silently, and matches names without
// regard to case, while the vendor's parser may take another copy, so a name repeated in any
// spelling the vendor may read as the same field is refused rather than guessed.

// JSONMember is one member of a JSON object, in document order.
type JSONMember struct {
	Name  string
	Value json.RawMessage
}

// JSONObjectMembers reads data as exactly one JSON object and returns every member in order,
// duplicates included. Anything else (an array, a scalar, a second value after the object) is an
// error.
func JSONObjectMembers(data []byte) ([]JSONMember, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("not a JSON object")
	}
	var members []JSONMember
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		members = append(members, JSONMember{Name: name, Value: value})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return members, nil
}

// OnlyJSONMember returns the value of the member same treats as field, nil when there is none,
// and how many members same treats as field. A caller refuses a count above one.
func OnlyJSONMember(members []JSONMember, field string, same func(a, b string) bool) (json.RawMessage, int) {
	var found json.RawMessage
	seen := 0
	for _, member := range members {
		if same(member.Name, field) {
			seen++
			found = member.Value
		}
	}
	return found, seen
}
