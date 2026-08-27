package caddy_netx_geolocation

import (
	"testing"
	"time"
)

// waitFor polls cond until it holds or the test times out. Used for the
// background initial fetch, which Provision deliberately does not block on.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
