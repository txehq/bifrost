package utils

import (
	"runtime"
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
	for _, body := range []string{``, ` `, `[]`, `"x"`, `{"a":1} {"b":2}`, `{"a":1`, `{"a":1}x`, `{"a":}`} {
		if _, err := JSONObjectMembers([]byte(body)); err == nil {
			t.Errorf("%q read as one JSON object", body)
		}
	}
	// Escaped names decode as the vendor decodes them; values with nested strings, escapes and
	// brackets inside strings end where they end.
	escaped, err := JSONObjectMembers([]byte(`{"size":"a\"}]","o":{"k":["}",{"x":"\\"}]},"n":-1.5e3,"t":true}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := map[string]string{"size": `"a\"}]"`, "o": `{"k":["}",{"x":"\\"}]}`, "n": `-1.5e3`, "t": `true`}
	for _, member := range escaped {
		if want[member.Name] != string(member.Value) {
			t.Errorf("member %q = %s, want %s", member.Name, member.Value, want[member.Name])
		}
	}
	if len(escaped) != len(want) {
		t.Fatalf("read %d members, want %d", len(escaped), len(want))
	}
	elements, err := JSONArrayElements([]byte(` [ "a" , {"b":[1,2]}, 3 ,null] `))
	if err != nil || len(elements) != 4 || string(elements[1]) != `{"b":[1,2]}` || !IsJSONString(elements[0]) || IsJSONString(elements[2]) {
		t.Fatalf("elements = %q (%v)", elements, err)
	}
	for _, body := range []string{`{}`, `"x"`, `[1,]`, `[1] [2]`} {
		if _, err := JSONArrayElements([]byte(body)); err == nil {
			t.Errorf("%q read as one JSON array", body)
		}
	}
}

// TXE-7502 security review: a raw body may be as large as the request limit, and a reader descends
// several levels into it. A value is a slice of the body, so reading a 20 MiB inline image four
// levels deep allocates nothing near its size (the earlier decoder copied it at every level).
func TestRawJSONReadingDoesNotCopyValues(t *testing.T) {
	image := strings.Repeat("A", 20<<20)
	body := []byte(`{"contents":[{"parts":[{"inlineData":{"data":"` + image + `"}}]}]}`)
	var stats runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&stats)
	before := stats.TotalAlloc
	top, err := JSONObjectMembers(body)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := JSONArrayElements(top[0].Value)
	content, _ := JSONObjectMembers(contents[0])
	parts, _ := JSONArrayElements(content[0].Value)
	part, _ := JSONObjectMembers(parts[0])
	inline, _ := JSONObjectMembers(part[0].Value)
	runtime.ReadMemStats(&stats)
	if grew := stats.TotalAlloc - before; grew > 1<<20 {
		t.Fatalf("reading a %d byte body four levels deep allocated %d bytes, want well under its size", len(body), grew)
	}
	if len(inline) != 1 || len(inline[0].Value) != len(image)+2 {
		t.Fatalf("the image value was not read whole: %d bytes", len(inline[0].Value))
	}
}
