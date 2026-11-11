package lib

import (
	"strings"

	"github.com/valyala/fasthttp"
)

// CopyQueryFirstValues is the one place a query string becomes a map with one value per
// name (fork, TXE-7042). It writes each name of args into dst, which must be empty, with
// its FIRST value: the value Args.Peek returns, so a plugin (HTTPRequest.Query), a routing
// rule (BifrostContextKeyRequestQuery) and a handler all read the same value of a name
// sent more than once. It returns every value, in the order sent, of each name that was
// sent more than once, and nil when no name repeats.
//
// lowerKeys lowercases the names, which is what routing rules read; names that differ
// only in case then count as one name. Handlers look a name up as written, in lower case,
// so when a name arrives both in lower case and in another case (Model=a&model=b) the
// lower-case spelling's first value is kept: the routing rules read b, as the handler does.
//
// A site that copies the query string any other way reintroduces the fault this replaced:
// the copies kept the last value while handlers read the first, and the write-back after
// the transport plugins replaced the first value of a repeated parameter with the last
// (status=a&status=b reached the vendor as status=b&status=b).
// TestQueryStringIsCopiedInOnePlace fails on such a site.
func CopyQueryFirstValues(args *fasthttp.Args, dst map[string]string, lowerKeys bool) map[string][]string {
	var repeated map[string][]string
	// fromVariant names the lowercased keys whose value so far came from a spelling that
	// is not lower case; it stays nil unless such a spelling is sent.
	var fromVariant map[string]struct{}
	for k, v := range args.All() {
		key := string(k)
		variant := false
		if lowerKeys {
			if lowered := strings.ToLower(key); lowered != key {
				key, variant = lowered, true
			}
		}
		first, seen := dst[key]
		if !seen {
			dst[key] = string(v)
			if variant {
				if fromVariant == nil {
					fromVariant = make(map[string]struct{}, 1)
				}
				fromVariant[key] = struct{}{}
			}
			continue
		}
		if _, replace := fromVariant[key]; replace && !variant {
			dst[key] = string(v)
			delete(fromVariant, key)
		}
		if repeated == nil {
			repeated = make(map[string][]string, 1)
		}
		if _, ok := repeated[key]; !ok {
			repeated[key] = []string{first}
		}
		repeated[key] = append(repeated[key], string(v))
	}
	return repeated
}

// ApplyQueryChanges writes back the query parameters a transport plugin changed and
// leaves the rest of the query string exactly as the caller sent it (fork, TXE-7042).
// query is HTTPRequest.Query after the plugins ran. A name whose value still equals its
// first value on the request was not changed, so every value of a repeated parameter
// stays, in order. A name a plugin set to another value, or added, ends up with that one
// value: the plugin's value replaces every value the caller sent, so a handler and a
// vendor never see the plugin's value next to a stale one. A name a plugin deleted from
// the map stays on the request, as before.
//
// A map cannot say whether a plugin assigned a name the value it already held, so that
// case is "not changed" and a repeated name keeps its other values. A plugin that must
// pin a value reads HTTPRequest.RepeatedQuery and refuses the request.
func ApplyQueryChanges(args *fasthttp.Args, query map[string]string) {
	for key, value := range query {
		if args.Has(key) && string(args.Peek(key)) == value {
			continue
		}
		if len(args.PeekMulti(key)) > 1 {
			args.Del(key)
		}
		args.Set(key, value)
	}
}

// CopyQueryAllValues collects the query parameters whose name skip does not claim, with
// every value of a repeated name in the order sent. It is for parameters the gateway does
// not interpret and forwards to a vendor; it returns nil when there are none.
func CopyQueryAllValues(args *fasthttp.Args, skip func(name []byte) bool) map[string][]string {
	var all map[string][]string
	for name, value := range args.All() {
		if skip != nil && skip(name) {
			continue
		}
		if all == nil {
			all = make(map[string][]string)
		}
		key := string(name)
		all[key] = append(all[key], string(value))
	}
	return all
}
