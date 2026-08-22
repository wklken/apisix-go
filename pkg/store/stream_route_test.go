package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
	streamStore, err := GetStore(t.TempDir()+"/stream-route.db", make(chan *Event))
	if err != nil {
		t.Fatalf("GetStore() error = %v", err)
	}
	t.Cleanup(func() { _ = streamStore.Stop() })

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

	invalidRoute := []byte(`{"id":"invalid-route","uri":"/invalid","plugins":[]}`)
	if err := streamStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("routes")).Put([]byte("invalid-route"), invalidRoute)
	}); err != nil {
		t.Fatalf("insert invalid HTTP route: %v", err)
	}
	routes, err := ListRoutes()
	if err == nil {
		t.Fatalf("ListRoutes() = %#v, nil; want strict route decode error", routes)
	}
	if !strings.Contains(err.Error(), `parse route "invalid-route"`) ||
		!strings.Contains(err.Error(), "expected { character for map value") {
		t.Fatalf("ListRoutes() error = %q, want route ID and decoder context", err)
	}
	if routes != nil {
		t.Fatalf("ListRoutes() routes = %#v, want nil on decode failure", routes)
	}
}

func TestStreamRoutePutValidationRetainsLastGood(t *testing.T) {
	events := make(chan *Event, 4)
	storage, err := Open(t.TempDir()+"/stream-put.db", events)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	previous := ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		ReplaceGlobalStoreForTest(previous)
		_ = storage.Stop()
	})

	good := `{"id":"mqtt","server_addr":"127.0.0.1","server_port":1883,"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:2883":1}}}`
	applyStreamRouteEvent(t, storage, EventTypePut, "/apisix/stream_routes/mqtt", good)

	bad := NewAcknowledgedEvent()
	bad.Type = EventTypePut
	bad.Key = []byte("/apisix/stream_routes/mqtt")
	bad.Value = []byte(`{"id":"mqtt","plugins":[]}`)
	storage.events <- bad
	err = bad.Wait(context.Background())
	var validationErr *ResourceValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("malformed stream route PUT error = %v, want ResourceValidationError", err)
	}
	if validationErr.Bucket != "stream_routes" || validationErr.ID != "mqtt" {
		t.Fatalf("validation context = %q/%q, want stream_routes/mqtt", validationErr.Bucket, validationErr.ID)
	}
	after, err := storage.GetFromBucket("stream_routes", []byte("mqtt"))
	if err != nil {
		t.Fatalf("read retained stream route: %v", err)
	}
	if !bytes.Equal(after, []byte(good)) {
		t.Fatalf("retained stream route = %q, want %q", after, good)
	}
}

func TestListStreamRoutesKeepsLastGoodAndDropsDeletedIDs(t *testing.T) {
	events := make(chan *Event, 4)
	storage, err := Open(t.TempDir()+"/stream-last-good.db", events)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	previous := ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		ReplaceGlobalStoreForTest(previous)
		_ = storage.Stop()
	})

	applyStreamRouteEvent(
		t,
		storage,
		EventTypePut,
		"/apisix/stream_routes/mqtt",
		`{"id":"mqtt","server_addr":"127.0.0.1","server_port":1883,"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:2883":1}}}`,
	)
	applyStreamRouteEvent(
		t,
		storage,
		EventTypePut,
		"/apisix/stream_routes/other",
		`{"id":"other","server_addr":"127.0.0.1","server_port":1884,"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:2884":1}}}`,
	)
	routes, err := ListStreamRoutes()
	if err != nil || len(routes) != 2 {
		t.Fatalf("ListStreamRoutes() = %+v/%v, want two last-good routes", routes, err)
	}

	if err := storage.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("stream_routes")).Put([]byte("mqtt"), []byte(`{`))
	}); err != nil {
		t.Fatalf("corrupt mqtt stream route: %v", err)
	}
	routes, err = ListStreamRoutes()
	if err != nil {
		t.Fatalf("ListStreamRoutes() after corruption = %v, want last-good mqtt", err)
	}
	if len(routes) != 2 {
		t.Fatalf("ListStreamRoutes() after corruption = %+v, want two routes", routes)
	}
	var foundMQTT bool
	for _, route := range routes {
		if route.ID == "mqtt" {
			foundMQTT = true
			if route.ServerPort != 1883 || route.ServerAddr != "127.0.0.1" {
				t.Fatalf("retained mqtt = %#v, want last-good listen", route)
			}
		}
	}
	if !foundMQTT {
		t.Fatalf("ListStreamRoutes() = %+v, want retained mqtt", routes)
	}

	applyStreamRouteEvent(t, storage, EventTypeDelete, "/apisix/stream_routes/mqtt", "")
	routes, err = ListStreamRoutes()
	if err != nil {
		t.Fatalf("ListStreamRoutes() after delete = %v", err)
	}
	if len(routes) != 1 || routes[0].ID != "other" {
		t.Fatalf("ListStreamRoutes() after delete = %+v, want only other", routes)
	}

	if err := storage.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("stream_routes")).Put([]byte("mqtt"), []byte(`{`))
	}); err != nil {
		t.Fatalf("reinsert deleted mqtt as malformed: %v", err)
	}
	if _, err := ListStreamRoutes(); err == nil {
		t.Fatal("ListStreamRoutes() accepted malformed mqtt after last-good was deleted")
	}
}

func applyStreamRouteEvent(t *testing.T, storage *Store, eventType EventType, key, value string) {
	t.Helper()
	event := NewAcknowledgedEvent()
	event.Type = eventType
	event.Key = []byte(key)
	event.Value = []byte(value)
	storage.events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("apply %s event: %v", key, err)
	}
}
