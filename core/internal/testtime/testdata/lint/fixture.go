// Fixture for TestLintFixtures: every line marked `// want` must be reported, and no other.
// The file is only parsed, never compiled.
package fixture

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const idleTimeout = 300 * time.Millisecond // want
const pollInterval = 10 * time.Millisecond
const longTimeout = 5 * time.Second

type cfg struct{ ReadTimeout, Interval time.Duration }

func withLease(lease time.Duration) {}
func stall(d time.Duration)         {}
func scale(d time.Duration) time.Duration {
	return d
}

func cases(t *testing.T, conn net.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond) // want
	defer cancel()
	_, c2 := context.WithTimeout(ctx, longTimeout)
	defer c2()
	_, c3 := context.WithTimeout(ctx, scale(500*time.Millisecond))
	defer c3()
	_, c4 := context.WithTimeout(ctx, 10*time.Millisecond) // literal-timing: must fire before the fixed upstream delay
	defer c4()

	select {
	case <-time.After(time.Second): // want
	case <-time.After(3 * time.Second):
	}
	time.AfterFunc(time.Duration(250)*time.Millisecond, func() {}) // want
	_ = time.NewTimer(time.Second / 2)                             // want

	require.Eventually(t, func() bool { return true }, 200*time.Millisecond, pollInterval) // want
	require.Eventually(t, func() bool { return true }, 2*time.Second, pollInterval)
	r := require.New(t)
	r.Never(func() bool { return false }, 100*time.Millisecond, pollInterval) // want

	_ = cfg{ReadTimeout: 50 * time.Millisecond, Interval: 10 * time.Millisecond} // want
	var c cfg
	c.ReadTimeout = idleTimeout // want
	withLease(pollInterval * 5) // want
	stall(100 * time.Millisecond)

	ttl := 100 * time.Millisecond // want
	_ = ttl
	_ = conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond)) // want
	expired := time.Now().Add(-time.Hour)
	_ = expired

	d := 100 * time.Millisecond
	d = 2 * d
	<-time.After(d)
	time.Sleep(100 * time.Millisecond)
}
