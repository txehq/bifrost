package proxytest

import (
	"net"
	"sync"
	"testing"
)

// IsolateEnv clears every proxy variable Go's proxy selection reads (EnvNames) for the
// rest of t, so the environment the test runs in cannot change where a request goes.
// NewSet and every recorder constructor call it, and SetEnv starts from it; a test that
// sets a proxy variable by hand calls it first (TestProxyEnvIsIsolated enforces that in
// core, transports, framework and plugins). It uses t.Setenv, so the test must not be
// parallel.
func IsolateEnv(t testing.TB) {
	t.Helper()
	for _, name := range EnvNames {
		t.Setenv(name, "")
	}
}

// listenLoopback listens on network's loopback address ("tcp4": 127.0.0.1, "tcp6": ::1)
// on a free port, and also holds that port on the other family's loopback, serving both
// through the one listener it returns. Without the second socket the kernel can give the
// same port number to a recorder of the other family, and a URL naming "localhost:port"
// (which dials ::1 first) reaches that recorder instead of this one. On a host with no
// loopback for the other family, only the first socket is opened.
func listenLoopback(t testing.TB, name, network string) net.Listener {
	t.Helper()
	IsolateEnv(t)
	host, otherNetwork, otherHost := "127.0.0.1", "tcp6", "::1"
	if network == "tcp6" {
		host, otherNetwork, otherHost = "::1", "tcp4", "127.0.0.1"
	}
	otherAvailable := false
	if probe, err := net.Listen(otherNetwork, net.JoinHostPort(otherHost, "0")); err == nil {
		otherAvailable = true
		_ = probe.Close()
	}
	for attempt := 0; attempt < 100; attempt++ {
		primary, err := net.Listen(network, net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatalf("%s: listen %s: %v", name, network, err)
		}
		if !otherAvailable {
			return newPairListener(primary, nil)
		}
		_, port, _ := net.SplitHostPort(primary.Addr().String())
		mirror, err := net.Listen(otherNetwork, net.JoinHostPort(otherHost, port))
		if err == nil {
			return newPairListener(primary, mirror)
		}
		// Another socket already holds the port on the other family: try another port.
		_ = primary.Close()
	}
	t.Fatalf("%s: no port free on both loopback families", name)
	return nil
}

// pairListener accepts from a primary listener and an optional mirror on the other
// address family. Addr is the primary's.
type pairListener struct {
	primary   net.Listener
	listeners []net.Listener
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newPairListener(primary, mirror net.Listener) *pairListener {
	l := &pairListener{primary: primary, listeners: []net.Listener{primary}, conns: make(chan net.Conn), done: make(chan struct{})}
	if mirror != nil {
		l.listeners = append(l.listeners, mirror)
	}
	for _, listener := range l.listeners {
		go l.pump(listener)
	}
	return l
}

func (l *pairListener) pump(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		select {
		case l.conns <- conn:
		case <-l.done:
			_ = conn.Close()
			return
		}
	}
}

func (l *pairListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pairListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		for _, listener := range l.listeners {
			_ = listener.Close()
		}
	})
	return nil
}

func (l *pairListener) Addr() net.Addr { return l.primary.Addr() }
