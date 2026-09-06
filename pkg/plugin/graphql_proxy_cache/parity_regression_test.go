package graphql_proxy_cache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestParityVaryMetadataEscapesZoneCapacity(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{CacheStrategy: "memory", CacheZone: "r5graphql"},
		[]appconfig.Zone{{Name: "r5graphql", MemorySize: "1K"}},
	)
	for i := range 1000 {
		r := httptest.NewRequest("POST", "http://example.test/graphql", nil)
		r.Header.Set("X-Variant", fmt.Sprint(i))
		if err := p.storeState(
			r,
			"key",
			base.ResponseState{Status: 200, Header: http.Header{"Vary": {"X-Variant"}}, Body: []byte("ok")},
			time.Minute,
		); err != nil {
			t.Fatal(err)
		}
	}
	hits := 0
	for i := range 1000 {
		r := httptest.NewRequest("POST", "http://example.test/graphql", nil)
		r.Header.Set("X-Variant", fmt.Sprint(i))
		if _, status := p.lookup(r, "key"); status == "HIT" {
			hits++
		}
	}
	if hits == 0 || hits > 64 {
		t.Fatalf("1KiB memory zone retains %d reachable variants, want 1..64", hits)
	}
}

func TestParityVaryMetadataAdmissionUsesZoneBytes(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{CacheStrategy: "memory", CacheZone: "vary-budget"},
		[]appconfig.Zone{{Name: "vary-budget", MemorySize: "1K"}},
	)
	r := httptest.NewRequest("POST", "http://example.test/graphql", nil)
	varyName := strings.Repeat("X", 512)
	r.Header.Set(varyName, "one")
	if err := p.storeState(
		r,
		"key",
		base.ResponseState{
			Status: 200,
			Header: http.Header{"Vary": {varyName}},
			Body:   []byte(strings.Repeat("b", 300)),
		},
		time.Minute,
	); err != nil {
		t.Fatal(err)
	}
	if _, status := p.lookup(r, "key"); status == "HIT" {
		t.Fatal("entry plus Vary metadata exceeds 1KiB but was admitted")
	}
}

func TestParityMemoryVaryCapEvictsOldestVariant(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheZone: "vary-cap"},
		[]appconfig.Zone{{Name: "vary-cap", MemorySize: "1M"}})
	store := func(key, variant string) {
		t.Helper()
		r := httptest.NewRequest("POST", "http://example.test/graphql", nil)
		r.Header.Set("X-Variant", variant)
		if err := p.storeState(
			r,
			key,
			base.ResponseState{Status: 200, Header: http.Header{"Vary": {"X-Variant"}}, Body: []byte(variant)},
			time.Minute,
		); err != nil {
			t.Fatal(err)
		}
	}
	store("unrelated", "keep")
	for i := range 64 {
		store("key", fmt.Sprint(i))
	}
	// Updating an existing variant must not move it to the back of the FIFO.
	store("key", "0")
	store("key", "64")
	for _, tc := range []struct{ key, variant, want string }{
		{"key", "0", "MISS"}, {"key", "1", "HIT"}, {"key", "64", "HIT"}, {"unrelated", "keep", "HIT"},
	} {
		r := httptest.NewRequest("POST", "http://example.test/graphql", nil)
		r.Header.Set("X-Variant", tc.variant)
		if _, status := p.lookup(r, tc.key); status != tc.want {
			t.Errorf("key=%s variant=%s status=%s, want %s", tc.key, tc.variant, status, tc.want)
		}
	}
}
