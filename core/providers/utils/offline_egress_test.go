package utils

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/network"
	"github.com/valyala/fasthttp"
)

// TestConfigureDialerRefusesNonLoopbackInAnOfflineRun covers the dialer every fasthttp
// provider client goes through (TXE-7083): in an offline run a provider whose address is not
// loopback fails before connecting, and the error names it; a loopback fixture is still
// reached.
func TestConfigureDialerRefusesNonLoopbackInAnOfflineRun(t *testing.T) {
	defer network.SetOfflineEgressGuard(true)()
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fixture"))
	}))
	defer fixture.Close()
	client := ConfigureDialer(&fasthttp.Client{ReadTimeout: 5 * time.Second}, false)

	status, body, err := client.Get(nil, fixture.URL)
	if err != nil || status != http.StatusOK || string(body) != "fixture" {
		t.Fatalf("loopback fixture: status=%d body=%q err=%v", status, body, err)
	}

	// TEST-NET-3 (RFC 5737): never routed, and no DNS involved.
	for _, target := range []string{"http://203.0.113.10/v1/models", "https://203.0.113.11/v1/models"} {
		_, _, err := client.Get(nil, target)
		if err == nil {
			t.Fatalf("%s reached in an offline run", target)
		}
		host := strings.Split(strings.SplitN(target, "://", 2)[1], "/")[0]
		if !strings.Contains(err.Error(), network.OfflineEgressMarker) || !strings.Contains(err.Error(), host) {
			t.Errorf("%s: want the offline refusal naming %s, got %v", target, host, err)
		}
	}
}
