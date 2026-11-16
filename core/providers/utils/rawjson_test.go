package utils

import (
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// TXE-7502: a raw body is read member by member, so a repeated name is seen, where encoding/json
// would keep the last copy silently; anything but exactly one JSON object is an error.
func TestJSONObjectMembersKeepsEveryMemberInOrder(t *testing.T) {
	members, err := JSONObjectMembers([]byte(` {"size":"1K","n":2,"SIZE":"4K","nested":{"a":1}} `))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var names []string
	for _, member := range members {
		names = append(names, member.Name)
	}
	if strings.Join(names, ",") != "size,n,SIZE,nested" {
		t.Fatalf("members = %v, want every one in document order", names)
	}
	value, seen := OnlyJSONMember(members, "size", schemas.SameJSONFieldName)
	if seen != 2 || string(value) != `"4K"` {
		t.Fatalf("size seen %d times (value %s), want 2 with the last copy", seen, value)
	}
	if value, seen := OnlyJSONMember(members, "steps", schemas.SameJSONFieldName); seen != 0 || value != nil {
		t.Fatalf("steps seen %d times (value %s), want none", seen, value)
	}
	for _, body := range []string{``, `[]`, `"x"`, `{"a":1} {"b":2}`, `{"a":1`, `{"a":1}x`} {
		if _, err := JSONObjectMembers([]byte(body)); err == nil {
			t.Errorf("%q read as one JSON object", body)
		}
	}
}
