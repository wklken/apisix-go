package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
)

func TestResolveStreamRoutesResolvesReferencedUpstream(t *testing.T) {
	routes, err := resolveStreamRoutes(
		[]resource.StreamRoute{{ID: "route", UpstreamID: "upstream"}},
		func(id string) (resource.Upstream, error) {
			if id != "upstream" {
				t.Fatalf("upstream lookup id = %q, want upstream", id)
			}
			return resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1883, Weight: 1}},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveStreamRoutes() error = %v", err)
	}
	if len(routes) != 1 || len(routes[0].Upstream.Nodes) != 1 || routes[0].Upstream.Nodes[0].Port != 1883 {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestResolveStreamRoutesRejectsMissingReferencedUpstream(t *testing.T) {
	_, err := resolveStreamRoutes(
		[]resource.StreamRoute{{ID: "route", UpstreamID: "missing"}},
		func(string) (resource.Upstream, error) {
			return resource.Upstream{}, ErrMissingStreamUpstream
		},
	)
	if err == nil {
		t.Fatal("resolveStreamRoutes() error = nil for missing upstream")
	}
}

func TestStreamProxyModeEnabled(t *testing.T) {
	for _, test := range []struct {
		mode    string
		enabled bool
	}{
		{mode: "http", enabled: false},
		{mode: "stream", enabled: true},
		{mode: "http&stream", enabled: true},
		{mode: "stream&http", enabled: true},
	} {
		if got := streamProxyModeEnabled(&config.Config{Apisix: config.Apisix{ProxyMode: test.mode}}); got != test.enabled {
			t.Fatalf("streamProxyModeEnabled(%q) = %v, want %v", test.mode, got, test.enabled)
		}
	}
}

func TestIsStreamRouteEvent(t *testing.T) {
	for _, test := range []struct {
		key     string
		refresh bool
	}{
		{key: "/apisix/stream_routes/mqtt", refresh: true},
		{key: "/apisix/upstreams/mqtt", refresh: true},
		{key: "/apisix/routes/http", refresh: false},
		{key: "/apisix/stream_routes", refresh: false},
	} {
		if got := isStreamRouteEvent(&store.Event{Key: []byte(test.key)}); got != test.refresh {
			t.Fatalf("isStreamRouteEvent(%q) = %v, want %v", test.key, got, test.refresh)
		}
	}
}

func TestServerShutdownClosesStreamRuntime(t *testing.T) {
	runtime := &fakeStreamRuntime{}
	s := &Server{
		server:        &http.Server{},
		routes:        newRouteHandler(http.NotFoundHandler(), nil),
		streamRuntime: runtime,
	}
	if err := s.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if !runtime.closed {
		t.Fatal("shutdown() did not close stream runtime")
	}
}

type fakeStreamRuntime struct {
	closed bool
}

func (r *fakeStreamRuntime) Reload([]resource.StreamRoute) error {
	return nil
}

func (r *fakeStreamRuntime) Close(context.Context) error {
	r.closed = true
	return nil
}
