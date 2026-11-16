package utils

import (
	"encoding/json"
	"errors"
)

// Strict reading of a body the caller wrote whole (raw request body mode). A provider that bills
// what such a body asks for (schemas.RawImageBodyReader, TXE-7502) reads it member by member:
// encoding/json keeps only the last copy of a repeated name, silently, and matches names without
// regard to case, while the vendor's parser may take another copy, so a name repeated in any
// spelling the vendor may read as the same field is refused rather than guessed.
//
// A raw image body can be as large as the request limit (inline base64 images), and a reader
// descends several levels into it, so nothing here copies a value: a member's or an element's
// value is a slice of the caller's bytes. Only names are decoded.

// MaxRawJSONMembers bounds the members of one object and the elements of one array that the raw
// readers keep a record of. A body is still only as large as the request limit, but each member
// costs a record and a decoded name, so a body of a million tiny members would cost far more than
// its size. No image request has an object or an array anywhere near this many entries; one that
// has more is refused, which fails closed.
const MaxRawJSONMembers = 10000

// errTooManyMembers is the error for an object or an array past MaxRawJSONMembers.
var errTooManyMembers = errors.New("more than 10000 members or elements in one JSON object or array")

// JSONMember is one member of a JSON object, in document order. Value is a slice of the bytes
// that were read, not a copy: it is valid while they are, and must not be modified.
type JSONMember struct {
	Name  string
	Value json.RawMessage
}

// JSONObjectMembers reads data as exactly one JSON object and returns every member in order,
// duplicates included. Anything else (invalid JSON, an array, a scalar, a second value after the
// object) is an error.
func JSONObjectMembers(data []byte) ([]JSONMember, error) {
	if !json.Valid(data) {
		return nil, errors.New("not one valid JSON value")
	}
	i := skipJSONSpace(data, 0)
	if data[i] != '{' {
		return nil, errors.New("not a JSON object")
	}
	var members []JSONMember
	i = skipJSONSpace(data, i+1)
	for data[i] != '}' {
		if len(members) == MaxRawJSONMembers {
			return nil, errTooManyMembers
		}
		nameEnd := jsonValueEnd(data, i)
		var name string
		if err := json.Unmarshal(data[i:nameEnd], &name); err != nil {
			return nil, err
		}
		i = skipJSONSpace(data, nameEnd) + 1 // the colon
		i = skipJSONSpace(data, i)
		valueEnd := jsonValueEnd(data, i)
		members = append(members, JSONMember{Name: name, Value: json.RawMessage(data[i:valueEnd:valueEnd])})
		i = skipJSONSpace(data, valueEnd)
		if data[i] == ',' {
			i = skipJSONSpace(data, i+1)
		}
	}
	return members, nil
}

// JSONArrayElements reads data as exactly one JSON array and returns its elements in order, each
// a slice of data, not a copy. Anything else is an error.
func JSONArrayElements(data []byte) ([]json.RawMessage, error) {
	if !json.Valid(data) {
		return nil, errors.New("not one valid JSON value")
	}
	i := skipJSONSpace(data, 0)
	if data[i] != '[' {
		return nil, errors.New("not a JSON array")
	}
	var elements []json.RawMessage
	i = skipJSONSpace(data, i+1)
	for data[i] != ']' {
		if len(elements) == MaxRawJSONMembers {
			return nil, errTooManyMembers
		}
		end := jsonValueEnd(data, i)
		elements = append(elements, json.RawMessage(data[i:end:end]))
		i = skipJSONSpace(data, end)
		if data[i] == ',' {
			i = skipJSONSpace(data, i+1)
		}
	}
	return elements, nil
}

// IsJSONString reports whether value (one valid JSON value) is a string, without decoding it.
func IsJSONString(value json.RawMessage) bool {
	i := skipJSONSpace(value, 0)
	return i < len(value) && value[i] == '"'
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

// skipJSONSpace returns the index of the first byte at or after i that is not JSON whitespace.
func skipJSONSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// jsonValueEnd returns the index just past the JSON value starting at i. data must be valid JSON
// (json.Valid), so only string and nesting boundaries need tracking.
func jsonValueEnd(data []byte, i int) int {
	switch data[i] {
	case '"':
		for j := i + 1; j < len(data); j++ {
			switch data[j] {
			case '\\':
				j++
			case '"':
				return j + 1
			}
		}
		return len(data)
	case '{', '[':
		depth := 0
		for j := i; j < len(data); j++ {
			switch data[j] {
			case '"':
				j = jsonValueEnd(data, j) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return len(data)
	default:
		j := i
		for j < len(data) {
			switch data[j] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return j
			}
			j++
		}
		return j
	}
}
