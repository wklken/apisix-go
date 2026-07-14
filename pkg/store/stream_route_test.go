package store

import (
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestParseStreamRoutePreservesMatchUpstreamAndPlugins(t *testing.T) {
	route, err := ParseStreamRoute([]byte(`{
		"id":"mqtt",
		"server_addr":"127.0.0.1",
		"server_port":1883,
		"remote_addr":"192.0.2.0/24",
		"plugins":{"mqtt-proxy":{"protocol_level":4}},
		"upstream":{"type":"roundrobin","scheme":"tcp","timeout":{},"nodes":{"127.0.0.1:2883":1}}
	}`))
	if err != nil {
		t.Fatalf("ParseStreamRoute() error = %v", err)
	}
	if route.ID != "mqtt" || route.ServerPort != 1883 || route.ServerAddr != "127.0.0.1" ||
		route.RemoteAddr != "192.0.2.0/24" {
		t.Fatalf("route match fields = %#v", route)
	}
	if _, ok := route.Plugins["mqtt-proxy"]; !ok {
		t.Fatal("mqtt-proxy config was not preserved")
	}
	if len(route.Upstream.Nodes) != 1 || route.Upstream.Nodes[0].Host != "127.0.0.1" ||
		route.Upstream.Nodes[0].Port != 2883 {
		t.Fatalf("upstream = %#v", route.Upstream)
	}
}

func TestParseStreamRouteAcceptsOfficialMinimalUpstream(t *testing.T) {
	if _, err := ParseStreamRoute([]byte(`{
		"id":"minimal",
		"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:2883":1}}
	}`)); err != nil {
		t.Fatalf("ParseStreamRoute() error = %v, want APISIX minimal upstream to parse", err)
	}
}

func TestGetStreamRouteReturnsNotFound(t *testing.T) {
	streamStore := NewStore(t.TempDir()+"/stream-route.db", make(chan *Event))
	t.Cleanup(streamStore.Stop)

	if _, err := GetStreamRoute("missing"); err != ErrNotFound {
		t.Fatalf("GetStreamRoute() error = %v, want %v", err, ErrNotFound)
	}
	if err := streamStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("stream_routes")).Put([]byte("invalid"), []byte("{"))
	}); err != nil {
		t.Fatalf("insert invalid stream route: %v", err)
	}
	if _, err := ListStreamRoutes(); err == nil {
		t.Fatal("ListStreamRoutes() accepted an invalid route snapshot")
	}
}
