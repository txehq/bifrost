package utils

import (
	"net/url"
	"strings"
)

// KeySecretMaySignFor reports whether a delivery to target may be signed with a secret the key
// holds, given the URL prefixes the operator trusts with that secret (TXE-7310).
//
// The caller picks target, so the key's secret must sign only deliveries to destinations the
// operator named: otherwise a caller collects deliveries signed with the operator's secret and
// replays them to any receiver that verifies with it. Every provider that injects a key-held
// signing secret next to a caller-supplied URL asks this function first, and sends no key
// secret when it answers false.
//
// A prefix matches when target is an absolute http or https URL with the same scheme, the same
// host and port (case-insensitive), no user info, and a path equal to the prefix's path or below
// it at a segment boundary. A target whose path has a "." or ".." segment, or an escaped "/", "\"
// or ".", never matches, since the receiver may resolve it outside the prefix. An empty or
// unparseable prefix matches nothing, and no prefixes means no destination is trusted.
func KeySecretMaySignFor(target string, trustedPrefixes []string) bool {
	t, ok := parseSigningURL(target)
	if !ok || t.User != nil || hasAmbiguousPath(t) {
		return false
	}
	for _, raw := range trustedPrefixes {
		p, ok := parseSigningURL(strings.TrimSpace(raw))
		if !ok || p.User != nil {
			continue
		}
		if !strings.EqualFold(t.Scheme, p.Scheme) || !strings.EqualFold(t.Host, p.Host) {
			continue
		}
		if pathUnderPrefix(t.Path, p.Path) {
			return true
		}
	}
	return false
}

// parseSigningURL parses an absolute http or https URL with a host.
func parseSigningURL(raw string) (*url.URL, bool) {
	if raw == "" {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return nil, false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u, true
	}
	return nil, false
}

// hasAmbiguousPath reports a path a receiver might resolve differently from its literal text.
func hasAmbiguousPath(u *url.URL) bool {
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(escaped, "%2e") || strings.Contains(u.Path, `\`) {
		return true
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// pathUnderPrefix reports whether path is prefix or lies below it at a "/" boundary.
func pathUnderPrefix(path, prefix string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	if path == prefix || path == strings.TrimSuffix(prefix, "/") {
		return true
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(path, prefix)
}
