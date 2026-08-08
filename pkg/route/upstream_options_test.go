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

func TestUpstreamTLSInsecureSkipVerify(t *testing.T) {
	tests := []struct {
		name     string
		upstream resource.Upstream
		want     bool
	}{
		{name: "tls omitted", upstream: resource.Upstream{}, want: true},
		{name: "verify false", upstream: resource.Upstream{TLS: &resource.UpstreamTLS{}}, want: true},
		{name: "verify true", upstream: resource.Upstream{TLS: &resource.UpstreamTLS{Verify: true}}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := upstreamTLSInsecureSkipVerify(test.upstream); got != test.want {
				t.Fatalf("upstreamTLSInsecureSkipVerify() = %v, want %v", got, test.want)
			}
		})
	}
}
