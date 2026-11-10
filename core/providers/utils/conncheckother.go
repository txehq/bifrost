//go:build !unix

package utils

import "net"

// pooledConnIsOpen cannot inspect a socket without blocking on this platform, so
// no pooled connection is known to be open: a write-once request is sent on a
// connection dialed for it. See the unix implementation.
func pooledConnIsOpen(net.Conn) bool {
	return false
}
