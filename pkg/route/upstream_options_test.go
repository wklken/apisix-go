package route

import (
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestResolveUpstreamTimeoutsUsesPerFieldRouteOverrides(t *testing.T) {
	got := resolveUpstreamTimeouts(
		resource.Timeout{Connect: 1, Send: 0, Read: 3},
		resource.Timeout{Connect: 10, Send: 20, Read: 30},
	)
	want := upstreamTimeouts{
		connect:        time.Second,
		send:           20 * time.Second,
		read:           3 * time.Second,
		responseHeader: 3 * time.Second,
	}
	if got != want {
		t.Fatalf("resolveUpstreamTimeouts() = %#v, want %#v", got, want)
	}
}

func TestResolveUpstreamTimeoutsKeepsExistingDefaults(t *testing.T) {
	got := resolveUpstreamTimeouts(resource.Timeout{}, resource.Timeout{})
	if got.connect != proxy.DefaultDialTimeout || got.send != 0 || got.read != 0 || got.responseHeader != 0 {
		t.Fatalf("default upstream timeouts = %#v", got)
	}
}
