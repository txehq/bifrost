package lib

import (
	"bytes"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
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

// ApplyQueryChanges writes back the query parameters a transport plugin set and leaves
// the rest of the query string exactly as the caller sent it (fork, TXE-7042). req is the
// request after the plugins ran.
//
// A name a plugin set through HTTPRequest.SetQuery ends up with that one value, whatever
// the caller sent and however many times: the caller sends tier=allowed&tier=other, a
// plugin calls SetQuery("tier", "allowed"), and only tier=allowed is forwarded. SetQuery
// is the supported way for a plugin to set a query value, and
// TestPluginsSetQueryValuesThroughSetQuery fails on in-repo code that writes
// HTTPRequest.Query directly.
//
// A direct write by a plugin built outside this repository is still applied, by
// comparing text: a name whose value differs from its first value on the request, or
// that is new, ends up with that one value. A name whose value equals its first value
// reads as untouched, because a map cannot say it was written, so every value of a
// repeated parameter stays, in order. A name deleted from the map stays on the request,
// as before.
//
// The work is linear in the size of the query string: a caller chooses how many names a
// request carries, and this runs before authentication.
func ApplyQueryChanges(args *fasthttp.Args, req *schemas.HTTPRequest) {
	// Past a few names, looking each one up on args (a scan of the whole query string)
	// would be quadratic, so the first value and the count of every name are read in one
	// pass. A short query string is scanned per name instead, which allocates nothing.
	var sent map[string]sentQueryName
	if args.Len() > maxQueryNamesScannedPerName {
		sent = make(map[string]sentQueryName, args.Len())
		for k, v := range args.All() {
			name, seen := sent[string(k)]
			if !seen {
				name.first = string(v)
			}
			name.count++
			sent[string(k)] = name
		}
	}
	for key, value := range req.Query {
		count, firstIsValue := 0, false
		if sent != nil {
			name := sent[key]
			count, firstIsValue = name.count, name.first == value
		} else {
			for k, v := range args.All() {
				if string(k) != key {
					continue
				}
				if count == 0 {
					firstIsValue = string(v) == value
				}
				count++
			}
		}
		switch {
		case count == 0:
		case firstIsValue && (count == 1 || !req.QueryAssigned(key)):
			continue
		case count > 1:
			args.Del(key)
		}
		args.Set(key, value)
	}
}

// maxQueryNamesScannedPerName is the number of query values up to which ApplyQueryChanges
// scans the query string once per name instead of building a map of it.
const maxQueryNamesScannedPerName = 16

// sentQueryName is what the request carries for one query name: its first value and how
// many times it was sent.
type sentQueryName struct {
	first string
	count int
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

// ForwardedQueryString is the one place a route reads the query string it forwards to a
// vendor as bytes: the passthrough routes, the native Anthropic Messages forward and the
// Claude Code OAuth forward (fork, TXE-7087). It returns the query the transport plugins
// left on the request, without the "?".
//
// fasthttp keeps two copies of a query: the string the caller sent (URI.QueryString) and
// the parsed arguments (URI.QueryArgs), and it does not rebuild the string when the
// arguments change. The transport writes a plugin's HTTPRequest.Query changes to the
// arguments, so a route that forwarded URI.QueryString sent the vendor the caller's value
// of a parameter a plugin had pinned, replaced or added, while every handler that reads
// the arguments saw the plugin's.
//
// When nothing changed the arguments, the caller's string is returned byte for byte:
// order, repeats, percent-encoding and a name without "=" all reach the vendor as sent.
// When something did, the arguments are encoded: names keep their order and repeats, a
// changed name carries the plugin's value, an added one comes last, and the encoding is
// fasthttp's (a space is "+", a reserved byte is percent-encoded in upper case).
//
// A site that forwards URI.QueryString itself reintroduces the fault;
// TestForwardedQueryStringIsReadInOnePlace fails on it.
func ForwardedQueryString(ctx *fasthttp.RequestCtx) string {
	uri := ctx.Request.URI()
	sent := uri.QueryString()
	current := uri.QueryArgs().QueryString()
	asSent := fasthttp.AcquireArgs()
	defer fasthttp.ReleaseArgs(asSent)
	asSent.ParseBytes(sent)
	if bytes.Equal(asSent.QueryString(), current) {
		return string(sent)
	}
	return string(current)
}
