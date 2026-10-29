package network

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// cgnat is the RFC 6598 Carrier-Grade NAT shared address space (100.64.0.0/10).
// It is reserved for provider-internal networks and is used for pod IPs on some
// Kubernetes platforms (e.g. EKS), so it must not be reachable. netip's
// IsPrivate() does not cover it.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// teredo is the RFC 4380 Teredo tunnelling prefix. Note the /32: only
// 2001:0000::/32 is Teredo, so ordinary global unicast under 2001::/16 (e.g.
// 2001:4860::/32) is untouched.
//
// Rejected wholesale rather than unwrapped-and-reclassified the way 6to4 and
// NAT64 are, for two reasons. Teredo carries two IPv4 addresses - the client's
// in bits 96-127 (obfuscated by XOR with 0xFFFFFFFF) and the relay server's in
// bits 32-63 - so reclassifying on one still leaves the other attacker-chosen.
// And unlike 6to4, Teredo has no legitimate server-to-server use: it is a
// consumer NAT-traversal mechanism, deprecated and off by default on modern
// systems, so nothing is lost by refusing the range outright.
var teredo = netip.MustParsePrefix("2001:0000::/32")

// metadataEndpoints are cloud instance-metadata service addresses that sit
// outside the link-local range, so the link-local block alone does not cover
// them. 169.254.169.254 (AWS, GCP, Azure, Oracle, DigitalOcean) is already
// refused as link-local. These two fall inside ranges the private-network
// policy otherwise permits: Alibaba Cloud ECS serves metadata from CGNAT
// space, and the AWS IMDS IPv6 endpoint is a unique-local address.
var metadataEndpoints = []netip.Addr{
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud ECS
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDS over IPv6
}

// IsMetadataEndpoint reports whether ip is one of the cloud instance-metadata
// addresses that sit outside the link-local range (see metadataEndpoints).
func IsMetadataEndpoint(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, endpoint := range metadataEndpoints {
		if addr == endpoint {
			return true
		}
	}
	return false
}

// IsPublicIP reports whether ip is safe to dial from server-side code that
// fetches user-controlled URLs: not loopback, private, CGNAT, link-local,
// unique-local, site-local, multicast, broadcast, or unspecified. IPv6 forms
// that embed an IPv4 address (IPv4-mapped, 6to4, NAT64) are reduced to that
// IPv4 and re-checked, so an internal IPv4 such as the 169.254.169.254
// metadata endpoint cannot be reached by wrapping it in an IPv6 transition
// representation.
//
// This is stricter than the complement of IsPrivateIP: IsPrivateIP is a
// coarse range check for save-time URL validation (where private targets may
// be deliberately allowed), while IsPublicIP is the dial-time gate.
func IsPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()

	// If the address embeds an IPv4 via a transition mechanism (6to4 / NAT64),
	// classify the embedded IPv4 — otherwise an internal target like
	// 169.254.169.254 can be reached as 2002:a9fe:a9fe:: or 64:ff9b::a9fe:a9fe,
	// neither of which Unmap() collapses.
	if embedded, ok := embeddedIPv4(addr); ok {
		addr = embedded
	}

	if addr.IsLoopback() || addr.IsPrivate() || cgnat.Contains(addr) ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() || addr.IsInterfaceLocalMulticast() ||
		addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return false
	}

	// Deprecated IPv6 site-local fec0::/10 (RFC 3879) isn't matched by any
	// helper above; reject it explicitly.
	if addr.Is6() {
		b := addr.As16()
		if b[0] == 0xfe && (b[1]&0xc0) == 0xc0 {
			return false
		}
		// Teredo. Checked after the transition unwrap above so a Teredo address
		// is judged as itself: none of the stdlib predicates match it, and its
		// embedded IPv4 is obfuscated, so without this an internal target
		// wrapped as 2001:0000:...:5601:5601 reads as ordinary global unicast.
		if teredo.Contains(addr) {
			return false
		}
	}

	return true
}

// embeddedIPv4 extracts the IPv4 address carried inside an IPv6 transition
// address: 6to4 (2002::/16) or NAT64 (RFC 6052 well-known 64:ff9b::/96 and
// RFC 8215 local-use 64:ff9b:1::/48). Returns false for anything else.
//
// Limitation: NAT64 with an operator-chosen Network-Specific Prefix (any other
// /32../96) is indistinguishable from a normal global address and is not
// unwrapped — only the two standard prefixes are covered.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	b := addr.As16()
	switch {
	case b[0] == 0x20 && b[1] == 0x02: // 6to4 2002::/16 -> bytes 2..5
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b:
		// NAT64 well-known 64:ff9b::/96 -> IPv4 in the low 32 bits (bytes 12..15).
		if b[4] == 0x00 && b[5] == 0x00 {
			return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
		}
		// NAT64 local-use 64:ff9b:1::/48 -> per RFC 6052 the IPv4 occupies
		// bytes 6,7,9,10 (byte 8 is the reserved u-octet, skipped).
		if b[4] == 0x00 && b[5] == 0x01 {
			return netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), true
		}
	}
	return netip.Addr{}, false
}

// IPLookuper resolves a hostname to IPs. *net.Resolver satisfies it; tests
// substitute a fake to exercise the dial path without real DNS.
type IPLookuper interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

type ipLookuper = IPLookuper

// SSRFSafeDialContext returns a DialContext for outbound requests to
// user-controlled URLs. On every dial it resolves the host, rejects the
// connection if any resolved address fails IsPublicIP, and then dials the
// first validated IP directly — so a second DNS resolution cannot swap in a
// private address after validation (DNS rebinding TOCTOU). Because the check
// runs per dial, it also holds across redirects and connection-pool re-dials.
func SSRFSafeDialContext(dialTimeout time.Duration) func(ctx context.Context, netw, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return ssrfSafeDialContext(net.DefaultResolver, dialer.DialContext, nil)
}

// SSRFSafeDialContextWithAllowlist is the allowlist-aware variant of
// SSRFSafeDialContext, for callers that must reach specific
// operator-declared private hosts (e.g. downloading custom plugin binaries
// from an internal artifact server). allow may be nil, in which case
// behavior is identical to SSRFSafeDialContext.
//
// Every resolved IP must satisfy IsPublicIP OR allow.Permits(host, ip); the
// winning IP is dialed directly and this check re-runs on every dial
// (redirects, pooled-connection re-dials), preserving
// SSRFSafeDialContext's DNS-rebinding protection.
//
// This is the only place allowlist behavior exists - do not add an
// allowlist parameter to SSRFSafeDialContext itself, and do not use this
// function at SSRF-guarded call sites other than plugin downloads (provider
// fetch, webhooks, skills serving); those must keep blocking all private
// targets unconditionally.
func SSRFSafeDialContextWithAllowlist(dialTimeout time.Duration, allow *Allowlist) func(ctx context.Context, netw, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return ssrfSafeDialContext(net.DefaultResolver, dialer.DialContext, allow)
}

// ssrfSafeDialContext is the seam behind SSRFSafeDialContext and
// SSRFSafeDialContextWithAllowlist, with injectable resolver and dial for
// tests. allow may be nil.
func ssrfSafeDialContext(resolver ipLookuper, dial func(ctx context.Context, network, addr string) (net.Conn, error), allow *Allowlist) func(ctx context.Context, netw, addr string) (net.Conn, error) {
	return func(ctx context.Context, netw, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
		}
		ips, err := resolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("DNS lookup failed for %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("DNS lookup for %s returned no addresses", host)
		}
		for _, ip := range ips {
			if !IsPublicIP(ip) && !allow.Permits(host, ip) {
				return nil, fmt.Errorf("blocked connection to non-public address %s (host %s)", ip, host)
			}
		}
		return dial(ctx, netw, net.JoinHostPort(ips[0].String(), port))
	}
}

// ResolvePublicTarget resolves host and returns its addresses when every one is public,
// or an error naming the first that is not. It is the check for a user-controlled URL
// fetched through a proxy: the caller tunnels to one of the returned addresses, so the
// proxy never resolves the name again. It needs the target to resolve locally: on a
// host with no external DNS, proxied fetches are refused rather than sent unchecked.
func ResolvePublicTarget(ctx context.Context, host string) ([]net.IP, error) {
	return publicTargetCheck(net.DefaultResolver, nil)(ctx, host)
}

// publicTargetCheck resolves host and refuses it unless every address is public or
// permitted by allow (nil permits none). It returns the checked addresses.
func publicTargetCheck(resolver ipLookuper, allow *Allowlist) func(ctx context.Context, host string) ([]net.IP, error) {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		ips, err := resolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("DNS lookup failed for %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("DNS lookup for %s returned no addresses", host)
		}
		for _, ip := range ips {
			if !IsPublicIP(ip) && !allow.Permits(host, ip) {
				return nil, fmt.Errorf("blocked connection to non-public address %s (host %s)", ip, host)
			}
		}
		return ips, nil
	}
}

// Allowlist is a static, deploy-time-validated set of hosts/CIDRs permitted
// to bypass IsPublicIP's private/loopback/CGNAT/link-local block, for
// callers that explicitly opt in via SSRFSafeDialContextWithAllowlist. It
// never affects SSRFSafeDialContext or its other callers.
type Allowlist struct {
	hostnames map[string]struct{}
	prefixes  []netip.Prefix
}

// NewAllowlist parses entries (each a bare hostname, bare IP, or CIDR) into
// an Allowlist. It fails on the first entry that is none of the three: this
// is security-relaxing config, so a typo must fail loudly (e.g. abort
// server startup) rather than silently becoming a no-op. Blank entries are
// skipped. There is no wildcard/glob support - unlike CORS allowlists,
// wrongly matching here means Bifrost fetches-and-dlopen()s from an
// unintended host, so operators must enumerate hosts exactly.
func NewAllowlist(entries []string) (*Allowlist, error) {
	al := &Allowlist{hostnames: make(map[string]struct{})}
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		// IPv4-mapped entries are stored as IPv4, because Permits unmaps the
		// dialed address and a prefix never contains an address of the
		// other family.
		if prefix, err := netip.ParsePrefix(e); err == nil {
			if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			al.prefixes = append(al.prefixes, prefix)
			continue
		}
		if addr, err := netip.ParseAddr(e); err == nil {
			addr = addr.Unmap()
			bits := 32
			if addr.Is6() {
				bits = 128
			}
			al.prefixes = append(al.prefixes, netip.PrefixFrom(addr, bits))
			continue
		}
		if looksLikeIPLiteral(e) {
			return nil, fmt.Errorf("invalid allowlist entry %q: looks like an IP address but failed to parse", raw)
		}
		if isValidHostnameLiteral(e) {
			al.hostnames[strings.ToLower(e)] = struct{}{}
			continue
		}
		return nil, fmt.Errorf("invalid allowlist entry %q: not a valid hostname, IP address, or CIDR (schemes and ports are not supported, host only)", raw)
	}
	return al, nil
}

// looksLikeIPLiteral reports whether s has the shape of an IPv4/IPv6 address
// (four dot-separated all-numeric labels, or contains a colon) even though it
// failed to parse as one. NewAllowlist uses this to reject likely IP typos
// (e.g. an out-of-range octet) with a clear error instead of silently
// accepting them as a dead hostname entry that can never match a dial host.
func looksLikeIPLiteral(s string) bool {
	if strings.Contains(s, ":") {
		return true
	}
	labels := strings.Split(s, ".")
	if len(labels) != 4 {
		return false
	}
	for _, label := range labels {
		if label == "" {
			return false
		}
		for _, r := range label {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// Permits reports whether host or resolvedIP is covered by the allowlist:
// host matches an allowlisted hostname exactly (case-insensitive), or
// resolvedIP falls inside an allowlisted CIDR/bare-IP entry. The same
// IPv4-in-IPv6 transition unwrapping IsPublicIP applies is applied to
// resolvedIP, so a 6to4/NAT64-wrapped form of an allowlisted IPv4 also
// matches. Nil-safe: a nil *Allowlist permits nothing.
func (a *Allowlist) Permits(host string, resolvedIP net.IP) bool {
	if a == nil {
		return false
	}
	if _, ok := a.hostnames[strings.ToLower(host)]; ok {
		return true
	}
	addr, ok := netip.AddrFromSlice(resolvedIP)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if embedded, ok := embeddedIPv4(addr); ok {
		addr = embedded
	}
	for _, prefix := range a.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// isValidHostnameLiteral reports whether s is a syntactically valid DNS
// hostname (RFC 1123 labels), rejecting anything with a scheme, port, path,
// or invalid characters so a config typo fails NewAllowlist loudly instead
// of silently parsing as a hostname that will never match anything.
func isValidHostnameLiteral(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i, r := range label {
			switch {
			case r == '-':
				if i == 0 || i == len(label)-1 {
					return false
				}
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			default:
				return false
			}
		}
	}
	return true
}

// checkPrivateNetworkPolicy applies the PrivateNetworkDialContext destination
// policy to one address: unspecified, link-local, and cloud metadata
// endpoints are refused; everything else, loopback and private ranges
// included, is permitted. IPv6 forms that embed an IPv4 address (IPv4-mapped,
// 6to4, NAT64) are judged by the embedded IPv4 as well, as IsPublicIP does,
// so a blocked endpoint cannot be reached through a transition
// representation.
func checkPrivateNetworkPolicy(ip net.IP, host string) error {
	if ip.IsUnspecified() {
		return fmt.Errorf("blocked connection to unspecified address %s (host %s)", ip, host)
	}
	if IsLinkLocal(ip) {
		return fmt.Errorf("blocked connection to link-local address %s (host %s)", ip, host)
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("blocked connection to unparseable address %s (host %s)", ip, host)
	}
	addr = addr.Unmap()
	if embedded, ok := embeddedIPv4(addr); ok {
		addr = embedded
		if addr.IsLinkLocalUnicast() {
			return fmt.Errorf("blocked connection to link-local address %s (host %s)", ip, host)
		}
	}
	for _, ep := range metadataEndpoints {
		if addr == ep {
			return fmt.Errorf("blocked connection to cloud metadata endpoint %s (host %s)", ip, host)
		}
	}
	return nil
}

// ResolvePrivateNetworkTarget resolves host and applies the
// PrivateNetworkDialContext destination policy to every address it resolves
// to, returning the validated addresses for the caller to dial. It is the
// resolve step of that dialer, kept separate so it can be tested on its own.
// An IP literal resolves to itself without a DNS query.
func ResolvePrivateNetworkTarget(ctx context.Context, host string) ([]net.IP, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS lookup for %s returned no addresses", host)
	}
	for _, ip := range ips {
		if err := checkPrivateNetworkPolicy(ip, host); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// CheckPrivateNetworkLiteral applies the PrivateNetworkDialContext destination
// policy to host without any DNS lookup: an IP literal (with or without IPv6
// brackets) is checked, and a hostname is accepted as-is. It exists for the
// case where the connection is handed to an intermediary, such as an HTTP
// proxy, that resolves names on its own side: resolving locally there would
// make the request depend on DNS the host may not have (a proxy-only
// deployment), and would still not bind what the proxy connects to. So the
// caller enforces what it can verify without DNS, and name resolution for the
// proxied path is left to the proxy, which is operator configuration.
func CheckPrivateNetworkLiteral(host string) error {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return nil
	}
	return checkPrivateNetworkPolicy(ip, host)
}

// PrivateNetworkDialContext returns a DialContext that, unlike
// SSRFSafeDialContext, permits loopback, RFC1918/unique-local, and CGNAT
// destinations. It still blocks link-local addresses (including the
// 169.254.169.254 cloud metadata endpoint), the cloud metadata endpoints that
// live outside link-local (see metadataEndpoints), and unspecified addresses,
// which have no legitimate destination under any deployment topology. Same
// DNS-rebinding protection as SSRFSafeDialContext: resolves once and dials
// the validated IP directly.
//
// Use this only for features where connecting to a self-hosted or
// private-network target is the primary, documented use case (e.g. an MCP
// server running on the same host or inside the same private network) AND
// that capability is already gated by genuine authentication elsewhere - this
// function is a defense-in-depth backstop against link-local/metadata
// targets, not an authorization check.
func PrivateNetworkDialContext(dialTimeout time.Duration) func(ctx context.Context, netw, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return func(ctx context.Context, netw, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
		}
		ips, err := ResolvePrivateNetworkTarget(ctx, host)
		if err != nil {
			return nil, err
		}
		// Every address resolved above passed the policy, so trying them in
		// turn is no weaker than dialing just the first: nothing outside this
		// one validated set is ever dialed, which is what defeats rebinding.
		// Pinning ips[0] instead would break the feature's primary documented
		// target: "localhost" resolves to [::1, 127.0.0.1] on a dual-stack
		// host, and an MCP server bound only to IPv4 is unreachable at ::1.
		var lastErr error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, netw, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// AllowlistedPrivateDialContext returns a DialContext that permits public
// destinations, plus private ones that the allowlist returned by allow
// permits. Link-local, cloud metadata, and unspecified addresses stay blocked
// even when allowlisted (the PrivateNetworkDialContext policy). allow is read
// on every dial, so a hot-reloaded allowlist applies to the next connection
// without rebuilding the transport; a nil allowlist permits only public
// destinations.
func AllowlistedPrivateDialContext(dialTimeout time.Duration, allow func() *Allowlist) func(ctx context.Context, netw, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return allowlistedPrivateDialContext(net.DefaultResolver, dialer.DialContext, allow)
}

// ResolveAllowlistedPrivateTarget resolves and validates every address before
// a proxy tunnel is opened. Metadata and link-local targets remain forbidden
// even when their hostname or network is allowlisted.
func ResolveAllowlistedPrivateTarget(ctx context.Context, host string, allow *Allowlist) ([]net.IP, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS lookup for %s returned no addresses", host)
	}
	for _, ip := range ips {
		if err := checkAllowlistedPrivatePolicy(ip, host, allow); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// allowlistedPrivateDialContext is the seam behind AllowlistedPrivateDialContext,
// with injectable resolver and dial for tests.
func allowlistedPrivateDialContext(resolver ipLookuper, dial func(ctx context.Context, network, addr string) (net.Conn, error), allow func() *Allowlist) func(ctx context.Context, netw, addr string) (net.Conn, error) {
	return func(ctx context.Context, netw, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
		}
		ips, err := resolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("DNS lookup failed for %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("DNS lookup for %s returned no addresses", host)
		}
		al := allow()
		for _, ip := range ips {
			if err := checkAllowlistedPrivatePolicy(ip, host, al); err != nil {
				return nil, err
			}
		}
		// Every address passed the policy, so trying them in turn dials nothing
		// outside the validated set (see PrivateNetworkDialContext for why
		// pinning the first one would break a dual-stack "localhost").
		var lastErr error
		for _, ip := range ips {
			conn, err := dial(ctx, netw, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// PrivateTargetNotAllowedError identifies a private destination missing operator permission.
type PrivateTargetNotAllowedError struct {
	Host string
	IP   net.IP
}

// Error describes the rejected destination without prescribing a product-specific setting.
func (e *PrivateTargetNotAllowedError) Error() string {
	return fmt.Sprintf("blocked connection to non-public address %s (host %s)", e.IP, e.Host)
}

// checkAllowlistedPrivatePolicy applies the AllowlistedPrivateDialContext
// policy to one resolved address of host.
func checkAllowlistedPrivatePolicy(ip net.IP, host string, allow *Allowlist) error {
	if IsPublicIP(ip) {
		return nil
	}
	if err := checkPrivateNetworkPolicy(ip, host); err != nil {
		return err
	}
	if !allow.Permits(host, ip) {
		return &PrivateTargetNotAllowedError{Host: host, IP: ip}
	}
	return nil
}

// CheckAllowlistedPrivateLiteral applies the AllowlistedPrivateDialContext
// policy to host without any DNS lookup: an IP literal (with or without IPv6
// brackets) must be public, or allowlisted and outside the link-local,
// metadata, and unspecified ranges; a hostname is accepted as-is. It is the
// proxied-path counterpart of the dialer, for the same reason as
// CheckPrivateNetworkLiteral.
func CheckAllowlistedPrivateLiteral(host string, allow *Allowlist) error {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return nil
	}
	return checkAllowlistedPrivatePolicy(ip, host, allow)
}

// ValidatePrivateTargetAllowlist parses entries with NewAllowlist and also
// refuses entries the AllowlistedPrivateDialContext policy would never let
// through anyway (link-local and cloud metadata ranges, the unspecified
// address, and the metadata hostnames), so an operator who lists one gets an
// error instead of an entry that silently does nothing.
func ValidatePrivateTargetAllowlist(entries []string) (*Allowlist, error) {
	al, err := NewAllowlist(entries)
	if err != nil {
		return nil, err
	}
	for name := range al.hostnames {
		if name != "localhost" && IsLocalOrMetadataHost(name) {
			return nil, fmt.Errorf("invalid allowlist entry %q: cloud metadata hosts can never be allowlisted", name)
		}
	}
	for _, prefix := range al.prefixes {
		for _, blocked := range neverAllowlistedPrefixes {
			if prefix.Overlaps(blocked) {
				return nil, fmt.Errorf("invalid allowlist entry %q: overlaps %s, which can never be allowlisted", prefix, blocked)
			}
		}
		for _, endpoint := range metadataEndpoints {
			if prefix.Contains(endpoint) {
				return nil, fmt.Errorf("invalid allowlist entry %q: contains the cloud metadata endpoint %s", prefix, endpoint)
			}
		}
	}
	return al, nil
}

// neverAllowlistedPrefixes are the ranges ValidatePrivateTargetAllowlist
// refuses: link-local (which holds 169.254.169.254) and the unspecified
// addresses, written as the single-address prefixes an all-addresses entry
// like 0.0.0.0/0 also overlaps.
var neverAllowlistedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("0.0.0.0/32"),
	netip.MustParsePrefix("::/128"),
}

// maxRedirectHops caps how many redirects a guarded HTTP client follows. Five
// is enough for any legitimate well-known/CDN hop chain and keeps a hostile
// server from using a long chain to probe many internal addresses per request.
const maxRedirectHops = 5

// guardedIdleConnTimeout bounds how long a guarded client keeps an idle keep-alive
// connection, as http.DefaultTransport does. Several callers build a client per
// fetch and drop it without closing it, so an unbounded idle connection would stay
// open, with its transport goroutines, for the life of the process.
const guardedIdleConnTimeout = 90 * time.Second

// guardedRedirectDNSTimeout bounds the resolver call made while vetting a
// redirect target. The dial that follows re-resolves under the request
// context; this is only the early check.
const guardedRedirectDNSTimeout = 5 * time.Second

// NewSSRFSafeHTTPClient returns an *http.Client for fetching URLs that an
// untrusted party can influence (a configured catalog URL, a discovery
// document, a redirect Location). It is the strict policy: every connection is
// dialed through SSRFSafeDialContext, so loopback, private, CGNAT, link-local,
// unique-local, and IPv6-transition-wrapped internal targets are refused at
// dial time (which also defeats DNS rebinding), and every redirect hop is
// vetted by the same rule before it is followed. Only http and https are
// dialable: the transport has no handler for file or any other scheme, and a
// redirect to one is refused in CheckRedirect. Redirects stop after
// maxRedirectHops. The process HTTP(S)_PROXY environment is honored the way
// http.DefaultTransport honors it; on a proxied request the destination policy
// runs against the local resolver's answer before the proxy is contacted, the
// tunnel is opened to an approved address, and a name only the proxy can resolve
// is refused - see guardedDestinationCheck and PinnedProxyDialContext.
//
// Callers that must reach a self-hosted target on the operator's own network
// use NewPrivateNetworkHTTPClient instead; this client never does.
func NewSSRFSafeHTTPClient(timeout time.Duration) *http.Client {
	return newGuardedHTTPClient(timeout, SSRFSafeDialContext(timeout), func(ip net.IP, host string) error {
		if !IsPublicIP(ip) {
			return fmt.Errorf("blocked redirect to non-public address %s (host %s)", ip, host)
		}
		return nil
	})
}

// NewPrivateNetworkHTTPClient is the PrivateNetworkDialContext counterpart of
// NewSSRFSafeHTTPClient, for fetchers whose documented primary use includes
// targets on the operator's own network (a self-hosted MCP server and its
// authorization server, a datasheet mirror). Loopback, RFC 1918, unique-local,
// and CGNAT destinations are dialable; unspecified, link-local (169.254/16,
// fe80::/10, and the 6to4/NAT64/Teredo forms of 169.254/16) and the
// non-link-local cloud metadata endpoints are refused at dial time, on every
// redirect hop, and on every destination handed to a proxy. Same hop
// cap and scheme rules as NewSSRFSafeHTTPClient. Like PrivateNetworkDialContext
// itself, this is defense in depth against the categorically illegitimate
// ranges, not an authorization check: callers remain responsible for gating
// who may configure the URL.
func NewPrivateNetworkHTTPClient(timeout time.Duration) *http.Client {
	return newGuardedHTTPClient(timeout, PrivateNetworkDialContext(timeout), checkPrivateNetworkPolicy)
}

// newGuardedHTTPClient assembles the client shared by NewSSRFSafeHTTPClient
// and NewPrivateNetworkHTTPClient: dial is the per-connection gate, checkIP is
// the same policy expressed as a per-address predicate, applied to redirect
// targets (every resolved address must pass) and, through
// guardedDestinationCheck, to every destination on the proxied path. The
// proxy is the global proxy when it is enabled for API traffic, else the
// environment's (DefaultProxyFunc).
func newGuardedHTTPClient(timeout time.Duration, dial func(ctx context.Context, netw, addr string) (net.Conn, error), checkIP func(ip net.IP, host string) error) *http.Client {
	return newGuardedHTTPClientWith(timeout, dial, checkIP, DefaultProxyFunc(ClientPurposeAPI), net.DefaultResolver)
}

// newGuardedHTTPClientWith is the seam behind newGuardedHTTPClient with the
// proxy selector and resolver injectable for tests.
//
// A request the proxy selector sends through a proxy goes over a separate transport
// whose dialer tunnels through the proxy to an address the routing check resolved and
// approved (PinnedProxyDialContext), so the proxy never resolves the destination. A
// plain-http request is tunnelled too, as policyProxyDial does, because an
// absolute-form request line would hand the proxy the name to resolve. The proxy
// itself is dialed under the private-network policy: an operator's proxy on a
// private or loopback address is the normal self-hosted setup. A destination the
// local resolver cannot answer is refused on every path: it cannot be checked, and a
// proxy resolving it on its own side could reach a blocked address, so a proxy-only
// DNS deployment must let the gateway resolve what it proxies.
func newGuardedHTTPClientWith(timeout time.Duration, dial func(ctx context.Context, netw, addr string) (net.Conn, error), checkIP func(ip net.IP, host string) error, proxy func(*http.Request) (*url.URL, error), resolver ipLookuper) *http.Client {
	newTransport := func(selector func(*http.Request) (*url.URL, error), dial func(ctx context.Context, netw, addr string) (net.Conn, error)) *http.Transport {
		return &http.Transport{
			Proxy:                 selector,
			DialContext:           dial,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			IdleConnTimeout:       guardedIdleConnTimeout,
			ForceAttemptHTTP2:     true,
		}
	}
	var transport http.RoundTripper = newTransport(nil, dial)
	if proxy != nil {
		transport = &ProxyAwareTransport{
			Proxy:            proxy,
			CheckDestination: guardedDestinationCheck(checkIP, resolver),
			Direct:           newTransport(nil, dial),
			ViaProxy:         newTransport(nil, PinnedProxyDialContext(checkIP, resolver, timeout)),
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("blocked redirect to unsupported scheme %q", req.URL.Scheme)
			}
			if len(via) >= maxRedirectHops {
				return fmt.Errorf("stopped after %d redirects", maxRedirectHops)
			}
			host := req.URL.Hostname()
			if host == "" {
				return fmt.Errorf("blocked redirect to URL without a host")
			}
			if proxy != nil {
				if _, err := proxy(req); err != nil {
					return fmt.Errorf("blocked redirect to %s: %w", host, err)
				}
			}
			// Proxied or direct, the hop must resolve locally and pass checkIP.
			ctx, cancel := context.WithTimeout(req.Context(), guardedRedirectDNSTimeout)
			defer cancel()
			if _, err := vetGuardedDestination(ctx, host, resolver, checkIP); err != nil {
				return fmt.Errorf("blocked redirect to %s: %w", host, err)
			}
			return nil
		},
	}
}

// guardedDestinationCheck is the ProxyAwareTransport.CheckDestination of a guarded
// client: it vets a proxied destination with vetGuardedDestination, under a bounded
// lookup, and returns the addresses it approved for the pinned dialer to tunnel to.
// An IP-literal destination is checked without DNS; a hostname must resolve locally
// and every address must pass checkIP; a hostname the local resolver cannot answer is
// refused, since nothing could check it.
func guardedDestinationCheck(checkIP func(ip net.IP, host string) error, resolver ipLookuper) func(ctx context.Context, host string) ([]net.IP, error) {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		ctx, cancel := context.WithTimeout(ctx, guardedRedirectDNSTimeout)
		defer cancel()
		return vetGuardedDestination(ctx, host, resolver, checkIP)
	}
}

// vetGuardedDestination applies checkIP to a destination host before a guarded
// client contacts it. An IP literal is checked as is and returns no addresses. A
// hostname is resolved and every returned address must pass; the checked addresses
// are returned. A failed or empty lookup is refused with a clear reason, on the
// direct and proxied paths alike, since an unchecked name must not reach a proxy
// that could resolve it to a blocked address.
func vetGuardedDestination(ctx context.Context, host string, resolver ipLookuper, checkIP func(ip net.IP, host string) error) ([]net.IP, error) {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return nil, checkIP(ip, host)
	}
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS lookup for %s returned no addresses", host)
	}
	for _, ip := range ips {
		if err := checkIP(ip, host); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// CheckProxiedDestination applies the dial-time destination policy to a
// request that will travel through a proxy, where the dialer only ever sees
// the proxy's address. strict selects the public-only policy of
// SSRFSafeDialContext; otherwise the private-network policy of
// PrivateNetworkDialContext applies. An IP literal is checked as is and a
// hostname against the resolver's answer. A hostname the resolver cannot
// answer is refused under either policy: it cannot be checked, and a proxy
// that resolved it on its own side could reach a metadata address.
func CheckProxiedDestination(ctx context.Context, host string, strict bool, resolver IPLookuper) error {
	_, err := vetGuardedDestination(ctx, host, resolver, ProxiedDestinationPolicy(strict))
	return err
}

// CheckResolvedProxiedDestination is CheckProxiedDestination. It is kept for its
// callers from before the private-network policy stopped deferring an
// unresolvable name to the proxy: both policies now refuse one, because the
// proxy's resolver could map it to an address the policy blocks (TXE-5086).
func CheckResolvedProxiedDestination(ctx context.Context, host string, strict bool, resolver IPLookuper) error {
	return CheckProxiedDestination(ctx, host, strict, resolver)
}

// CheckResolvedAllowlistedProxiedDestination is CheckResolvedProxiedDestination
// for the AllowlistedPrivateDialContext policy: an IP literal is checked as is,
// a hostname against every address the resolver answers, and a hostname the
// resolver cannot answer is refused, because the proxy's resolver could map it
// to an address the allowlist does not permit.
func CheckResolvedAllowlistedProxiedDestination(ctx context.Context, host string, allow *Allowlist, resolver IPLookuper) error {
	_, err := vetGuardedDestination(ctx, host, resolver, func(ip net.IP, host string) error {
		return checkAllowlistedPrivatePolicy(ip, host, allow)
	})
	return err
}

// ResolveProxiedDestination is CheckProxiedDestination returning the addresses it
// approved (none for an IP literal), for a ProxyAwareTransport's CheckDestination,
// whose pinned dialer tunnels to them.
func ResolveProxiedDestination(ctx context.Context, host string, strict bool, resolver IPLookuper) ([]net.IP, error) {
	return vetGuardedDestination(ctx, host, resolver, ProxiedDestinationPolicy(strict))
}

// ProxiedDestinationPolicy is the per-address check CheckProxiedDestination
// applies: public-only when strict, the private-network policy otherwise.
func ProxiedDestinationPolicy(strict bool) func(ip net.IP, host string) error {
	if !strict {
		return checkPrivateNetworkPolicy
	}
	return func(ip net.IP, host string) error {
		if !IsPublicIP(ip) {
			return fmt.Errorf("blocked connection to non-public address %s (host %s)", ip, host)
		}
		return nil
	}
}

type pinnedRouteKey struct{}

// pinnedRoute is what ProxyAwareTransport hands a pinned ViaProxy dialer for one
// request: the proxy it selected, and the addresses CheckDestination resolved and
// approved for host. A second lookup is a second answer: one that differs (DNS
// rebinding) or fails (so the name would be left to the proxy unchecked) must not
// decide where the connection goes.
type pinnedRoute struct {
	proxy *url.URL
	host  string
	ips   []net.IP
}

// pinnedDestination returns the addresses a proxied dial to host may tunnel to,
// each passing checkIP: the ones the routing check approved for this request when
// it resolved host, else this dialer's own lookup. It returns none when host is an
// IP literal that passed. A name no lookup could answer is refused.
func pinnedDestination(ctx context.Context, route *pinnedRoute, host string, checkIP func(ip net.IP, host string) error, resolver IPLookuper) ([]net.IP, error) {
	if route.host == host && len(route.ips) > 0 {
		for _, ip := range route.ips {
			if err := checkIP(ip, host); err != nil {
				return nil, err
			}
		}
		return route.ips, nil
	}
	return vetGuardedDestination(ctx, host, resolver, checkIP)
}

// PinnedProxyDialContext is the dialer for a ProxyAwareTransport's ViaProxy
// transport, which then carries no Proxy selector of its own. It takes the
// addresses the transport's CheckDestination approved for the request (resolving
// and checking itself only when that check did not resolve the name), requires
// checkIP to pass for every one, and opens the tunnel to a checked address
// (CONNECT ip:port, or SOCKS5 to the IP) through the proxy the transport selected
// for the request, for plain-http requests as well as TLS ones. The proxy never
// resolves the destination, so a DNS answer that changes after the check cannot
// redirect the connection; the hostname still reaches the target in TLS SNI and the
// Host header. A name the resolver cannot answer is refused, never tunnelled by
// name. The proxy itself is resolved and dialed under the private-network policy,
// pinned to the address checked, as PrivateNetworkDialContext dials it on the
// unpinned path; an https proxy is verified against its hostname, with the global
// proxy's TLS settings when it is the global proxy (ProxyTLSFor). dialTimeout bounds
// reaching the proxy and its handshake when the request sets no earlier deadline
// (0: ProxyHandshakeTimeout).
func PinnedProxyDialContext(checkIP func(ip net.IP, host string) error, resolver IPLookuper, dialTimeout time.Duration) func(ctx context.Context, netw, addr string) (net.Conn, error) {
	return func(ctx context.Context, netw, addr string) (net.Conn, error) {
		route, _ := ctx.Value(pinnedRouteKey{}).(*pinnedRoute)
		if route == nil || route.proxy == nil {
			return nil, fmt.Errorf("blocked dial to %s: no proxy was selected for this request", addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := pinnedDestination(ctx, route, host, checkIP, resolver)
		if err != nil {
			return nil, err
		}
		if dialTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, dialTimeout)
			defer cancel()
		}
		proxyHost, proxyPort, err := net.SplitHostPort(proxyDialAddr(route.proxy))
		if err != nil {
			return nil, fmt.Errorf("invalid proxy address: %w", err)
		}
		proxyIPs, err := vetGuardedDestination(ctx, proxyHost, resolver, checkPrivateNetworkPolicy)
		if err != nil {
			return nil, fmt.Errorf("blocked dial to proxy %s: %w", proxyHost, err)
		}
		proxyTLS := ProxyTLSFor(route.proxy)
		if proxyTLS == nil {
			proxyTLS = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			proxyTLS = proxyTLS.Clone()
		}
		if proxyTLS.ServerName == "" {
			// The proxy is dialed by address; its certificate is still checked
			// against the name the operator configured.
			proxyTLS.ServerName = strings.Trim(proxyHost, "[]")
		}
		proxies := []*url.URL{route.proxy}
		if len(proxyIPs) > 0 {
			proxies = proxies[:0]
			for _, ip := range proxyIPs {
				pinned := *route.proxy
				pinned.Host = net.JoinHostPort(ip.String(), proxyPort)
				proxies = append(proxies, &pinned)
			}
		}
		targets := []string{addr}
		if len(ips) > 0 {
			targets = make([]string, 0, len(ips))
			for _, ip := range ips {
				targets = append(targets, net.JoinHostPort(ip.String(), port))
			}
		}
		var lastErr error
		for _, proxy := range proxies {
			for _, target := range targets {
				conn, err := DialViaProxyTLS(ctx, proxy, target, proxyTLS)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
		}
		return nil, lastErr
	}
}

// ProxyAwareTransport routes each request, by that request's own proxy
// decision, to one of two transports: Direct, which never uses a proxy and
// dials the destination under the destination policy, and ViaProxy, which
// always uses the proxy and so dials nothing but the proxy, under the proxy
// policy. Keeping the two policies on separate transports is what makes the
// classification per request: there is no shared record of "addresses that
// are proxies" for a later direct request to match by coincidence, and
// http.Transport hands DialContext the proxy address on exactly the transport
// whose dialer expects it.
//
// With CheckDestination set, a proxied request's destination is vetted before
// ViaProxy sees it, and ViaProxy's dialer is PinnedProxyDialContext: it reads the
// selected proxy and the approved addresses from the request and tunnels to one of
// them. ViaProxy is then a template: each distinct proxy, credentials included, gets
// its own clone of it, because its pooled connections are tunnels through one proxy
// and are keyed only by destination. A proxy changed from A to B therefore never
// reuses a tunnel through A.
type ProxyAwareTransport struct {
	// Proxy decides the route: nil means direct. It is the operator's proxy
	// configuration (http.ProxyFromEnvironment), not a policy check.
	Proxy func(*http.Request) (*url.URL, error)
	// CheckDestination, when set, vets the destination host of every proxied
	// request and returns the addresses it approved (none for an IP literal),
	// which ViaProxy's PinnedProxyDialContext tunnels to. An error refuses the
	// request before any dial.
	CheckDestination func(ctx context.Context, host string) ([]net.IP, error)
	Direct           *http.Transport
	ViaProxy         *http.Transport

	mu         sync.Mutex
	viaProxies map[string]*http.Transport
}

// maxProxyTransports bounds the per-proxy transports a ProxyAwareTransport keeps;
// past it the idle ones are closed and the set starts over. Proxies are operator
// configuration, so it is only reached by repeated reconfiguration.
const maxProxyTransports = 16

// viaProxyFor returns the transport whose pool holds tunnels through proxyURL.
// The key is the proxy URL with its credentials, held only in memory, for the life
// of the ProxyAwareTransport.
func (t *ProxyAwareTransport) viaProxyFor(proxyURL *url.URL) *http.Transport {
	key := proxyURL.String()
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr, ok := t.viaProxies[key]; ok {
		return tr
	}
	if t.viaProxies == nil || len(t.viaProxies) >= maxProxyTransports {
		for _, tr := range t.viaProxies {
			tr.CloseIdleConnections()
		}
		t.viaProxies = make(map[string]*http.Transport)
	}
	tr := t.ViaProxy.Clone()
	t.viaProxies[key] = tr
	return tr
}

// RoundTrip implements http.RoundTripper. A direct request is handed to Direct
// unchanged, so the direct path costs nothing beyond the Proxy decision.
func (t *ProxyAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Proxy != nil {
		proxyURL, err := t.Proxy(req)
		if err != nil {
			return nil, err
		}
		if proxyURL != nil {
			if t.CheckDestination == nil {
				return t.ViaProxy.RoundTrip(req)
			}
			host := req.URL.Hostname()
			ips, err := t.CheckDestination(req.Context(), host)
			if err != nil {
				return nil, err
			}
			route := &pinnedRoute{proxy: proxyURL, host: host, ips: ips}
			return t.viaProxyFor(proxyURL).RoundTrip(req.WithContext(context.WithValue(req.Context(), pinnedRouteKey{}, route)))
		}
	}
	return t.Direct.RoundTrip(req)
}

// CloseIdleConnections closes idle connections on both transports and on every
// per-proxy clone of ViaProxy.
func (t *ProxyAwareTransport) CloseIdleConnections() {
	t.Direct.CloseIdleConnections()
	t.ViaProxy.CloseIdleConnections()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.viaProxies {
		tr.CloseIdleConnections()
	}
}
