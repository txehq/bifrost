//go:build unix

package utils

import (
	"errors"
	"net"
	"syscall"
)

// pooledConnIsOpen reports whether a connection taken from the pool is known to be
// still open and quiet: the upstream has not closed it and has sent nothing since
// the last response. It is one non-blocking read on the socket: it never waits and
// does not depend on any deadline set on the connection.
//
//   - nothing to read yet (EAGAIN): open.
//   - end of stream: the upstream closed it while it sat in the pool.
//   - bytes waiting: nobody asked for them. On a TLS connection that is usually the
//     upstream's close_notify; either way the next response could not be trusted,
//     and the byte this read consumed leaves the connection unusable.
//   - any other error, or a connection that cannot be inspected (no file
//     descriptor behind it): not known to be open.
//
// Every "not open" answer makes the caller close the connection and take another,
// before anything was written on it.
func pooledConnIsOpen(conn net.Conn) bool {
	raw, ok := pooledRawConn(conn)
	if !ok {
		return false
	}
	var (
		n       int
		readErr error
		buf     [1]byte
	)
	// Control, not Read: RawConn.Read first honours the connection's read deadline,
	// and a pooled connection still carries the deadline of the request before it,
	// often already past. Control runs the read whatever the deadline says, and the
	// socket is non-blocking, so nothing here waits.
	if err := raw.Control(func(fd uintptr) {
		n, readErr = syscall.Read(int(fd), buf[:])
	}); err != nil {
		return false
	}
	// A read that would block reports n = -1 with EAGAIN: nothing has arrived and the peer
	// has not closed. n = 0 with no error is the end of the stream; n > 0 is unsolicited data.
	return n < 0 && (errors.Is(readErr, syscall.EAGAIN) || errors.Is(readErr, syscall.EWOULDBLOCK))
}

// pooledConnInspectable reports whether pooledConnIsOpen can inspect conn at all. A
// connection with no file descriptor behind it (an in-memory pipe, a custom dialer's
// wrapper) cannot be, and pooledConnIsOpen answers false for it without knowing.
func pooledConnInspectable(conn net.Conn) bool {
	_, ok := pooledRawConn(conn)
	return ok
}

// pooledRawConn returns the socket under conn.
func pooledRawConn(conn net.Conn) (syscall.RawConn, bool) {
	// A TLS connection is checked on the socket under it: an encrypted
	// close_notify shows up there as readable bytes.
	for unwrapped := true; unwrapped; {
		switch wrapper := conn.(type) {
		case interface{ NetConn() net.Conn }: // *tls.Conn
			conn = wrapper.NetConn()
		case interface{ UnderlyingConn() net.Conn }: // the proxy tunnel wrappers of core/network
			conn = wrapper.UnderlyingConn()
		default:
			unwrapped = false
		}
		if conn == nil {
			return nil, false
		}
	}
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return nil, false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return nil, false
	}
	return raw, true
}
