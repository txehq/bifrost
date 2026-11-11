package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// OfflineEnv is the variable scripts/test-offline.sh sets on the command it runs (TXE-7053).
// When it is "1", every TCP connection Bifrost opens to an address that is not loopback is
// refused before it is attempted, with an error that names the host (TXE-7083). A fixture run
// therefore cannot reach a real vendor, even through a provider that ignores
// network_config.base_url and builds its host from a region (Bedrock sent two requests to AWS
// that way during TXE-7080).
//
// The check runs on the address actually dialled (net.Dialer.Control), after DNS: a name that
// resolves to loopback, such as a test's fake host behind a stub resolver, is allowed, and
// anything that would leave the machine is not, whatever its name.
const OfflineEnv = "BIFROST_TEST_OFFLINE"

// OfflineEgressMarker starts every refusal, so a test, a log search or a harness assertion can
// tell the guard's refusal from any other dial failure.
const OfflineEgressMarker = "offline run (" + OfflineEnv + "=1): egress to"

var offlineEgress atomic.Bool

func init() {
	offlineEgress.Store(os.Getenv(OfflineEnv) == "1")
	// http.DefaultTransport (http.DefaultClient, a bare &http.Client{}, and every clone made
	// after this) dials through the guard too. Its dialer is the one net/http installs, so with
	// the guard off nothing changes; installing it unconditionally lets tests turn the guard on.
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport.DialContext = GuardDialer(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
}

// OfflineEgressGuarded reports whether non-loopback egress is being refused.
func OfflineEgressGuarded() bool {
	return offlineEgress.Load()
}

// SetOfflineEgressGuard turns the guard on or off and returns a function that restores the
// previous state. Tests use it; the process reads OfflineEnv once at start-up.
func SetOfflineEgressGuard(on bool) (restore func()) {
	prev := offlineEgress.Swap(on)
	return func() { offlineEgress.Store(prev) }
}

// OfflineEgressError is the guard's refusal. Addr is the address the dial was refused for;
// Host, when the caller knows it, is the name that resolved to it.
type OfflineEgressError struct {
	Host string
	Addr string
}

func (e *OfflineEgressError) Error() string {
	target := e.Addr
	if e.Host != "" {
		if h, _, err := net.SplitHostPort(e.Addr); err != nil || !strings.EqualFold(h, e.Host) {
			target = e.Host + " (" + e.Addr + ")"
		}
	}
	return fmt.Sprintf("%s %s refused: only loopback (127.0.0.0/8, ::1) is reachable; "+
		"point the provider at a local fixture (tests/e2e/api/README.md, \"Offline runs and egress\")", OfflineEgressMarker, target)
}

// IsLoopbackHost reports whether host (a name or an IP literal, without a port; IPv6 may be
// bracketed or carry a zone) is loopback: "localhost", a name under ".localhost" (RFC 6761),
// anything in 127.0.0.0/8, or ::1. Unlike IsLocalhost it does not count the unspecified
// addresses.
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// CheckOfflineEgress returns nil when the guard is off or addr ("host:port" or a bare host)
// is loopback, and otherwise an *OfflineEgressError naming it.
func CheckOfflineEgress(addr string) error {
	if !offlineEgress.Load() {
		return nil
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if IsLoopbackHost(host) {
		return nil
	}
	return &OfflineEgressError{Addr: addr}
}

// offlineControl is a net.Dialer Control func: it runs on the resolved address before the
// connection is attempted.
func offlineControl(network, address string, _ syscall.RawConn) error {
	if !strings.HasPrefix(network, "tcp") && !strings.HasPrefix(network, "udp") {
		return nil // unix sockets never leave the machine
	}
	return CheckOfflineEgress(address)
}

// GuardDialer returns a copy of d whose dials refuse non-loopback addresses while the guard is
// on. An existing Control func still runs after the check. A nil d is a zero net.Dialer.
func GuardDialer(d *net.Dialer) *net.Dialer {
	guarded := &net.Dialer{}
	if d != nil {
		*guarded = *d //nolint:govet // copying a Dialer value before first use is safe
	}
	if innerCtx := guarded.ControlContext; innerCtx != nil {
		// net.Dialer prefers ControlContext over Control, so the check goes in front of it.
		guarded.ControlContext = func(ctx context.Context, network, address string, c syscall.RawConn) error {
			if err := offlineControl(network, address, c); err != nil {
				return err
			}
			return innerCtx(ctx, network, address, c)
		}
		return guarded
	}
	inner := guarded.Control
	guarded.Control = func(network, address string, c syscall.RawConn) error {
		if err := offlineControl(network, address, c); err != nil {
			return err
		}
		if inner != nil {
			return inner(network, address, c)
		}
		return nil
	}
	return guarded
}

// GuardedFasthttpDial is a fasthttp.DialFunc that dials through the guard, for a fasthttp
// client that would otherwise use fasthttp's own dialer (TXE-7083): set it as Dial on any
// fasthttp.Client that does not go through ConfigureDialer or another guarded dial.
// core/providers/utils fails on a fasthttp client built without one.
func GuardedFasthttpDial(addr string) (net.Conn, error) {
	conn, err := guardedFasthttpDialer.Dial("tcp", addr)
	if err != nil {
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			host = addr
		}
		return nil, NameOfflineEgress(err, host)
	}
	return conn, nil
}

var guardedFasthttpDialer = GuardDialer(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})

// GuardDialerInPlace puts the guard on d itself, for libraries that hand out a *net.Dialer to
// configure (the AWS SDK's BuildableClient.WithDialerOptions).
func GuardDialerInPlace(d *net.Dialer) {
	*d = *GuardDialer(d)
}

// NameOfflineEgress fills in the host name on a guard refusal inside err, so the error says
// which name resolved to the refused address. Other errors pass through unchanged.
func NameOfflineEgress(err error, host string) error {
	var refusal *OfflineEgressError
	if host != "" && errors.As(err, &refusal) && refusal.Host == "" {
		refusal.Host = host
	}
	return err
}
