package proxy_cache

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestParitySharedDiskPurge(t *testing.T) {
	zones := []appconfig.Zone{{Name: "r5disk", DiskPath: t.TempDir(), DiskSize: "1M"}}
	cfg := Config{CacheStrategy: "disk", CacheZone: "r5disk", CacheTTL: 300}
	a := newTestPlugin(t, cfg, zones)
	b := newTestPlugin(t, cfg, zones)
	r := httptest.NewRequest("GET", "http://example.test/same", nil)
	key := a.cacheKey(r)
	if err := a.storeState(
		r,
		key,
		base.ResponseState{Status: 200, Header: http.Header{}, Body: []byte("old")},
		300*time.Second,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if _, s := b.lookup(r, key); s != "HIT" {
		t.Fatal(s)
	}
	purgeReq := httptest.NewRequest("PURGE", "http://example.test/same", nil)
	w := httptest.NewRecorder()
	b.RunRequestPhase(w, purgeReq)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if _, err := os.Stat(a.entryPath(key)); !os.IsNotExist(err) {
		t.Fatalf("disk entry still exists: %v", err)
	}
	e, s := a.lookup(r, key)
	if s == "HIT" {
		t.Fatalf("successful PURGE removed disk file but other live instance still HIT body=%q", e.body)
	}
}

func TestParitySMaxAgePrecedence(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheControl: true})
	r := httptest.NewRequest("GET", "http://example.test/ttl", nil)
	phase := p.RunRequestPhase(httptest.NewRecorder(), r)
	if err := p.RunFinalResponseStore(
		phase.Request,
		base.ResponseState{
			Status: 200,
			Header: http.Header{"Cache-Control": {"s-maxage=1, max-age=3600"}},
			Body:   []byte("data"),
		},
	); err != nil {
		t.Fatal(err)
	}
	e, s := p.lookup(r, p.cacheKey(r))
	if s != "HIT" {
		t.Fatal(s)
	}
	if e.ttl != time.Second {
		t.Fatalf("shared cache TTL=%s, want s-maxage 1s", e.ttl)
	}
}

func TestParityHideCacheHeadersOnMiss(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", HideCacheHeaders: true, CacheControl: true})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Expires", "Wed, 21 Oct 2030 07:28:00 GMT")
		_, _ = w.Write([]byte("data"))
	}))
	for i := range 2 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://example.test/hide", nil))
		if w.Header().Get("Cache-Control") != "" || w.Header().Get("Expires") != "" {
			t.Fatalf("response %d exposes cache headers: %v", i, w.Header())
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want cached response on second request", calls)
	}
}

func TestParityColdDiskPurgeStatus(t *testing.T) {
	zones := []appconfig.Zone{{Name: "r5cold", DiskPath: t.TempDir()}}
	cfg := Config{CacheStrategy: "disk", CacheZone: "r5cold", CacheTTL: 300}
	a := newTestPlugin(t, cfg, zones)
	b := newTestPlugin(t, cfg, zones)
	r := httptest.NewRequest("GET", "http://example.test/cold", nil)
	if err := a.storeState(
		r,
		a.cacheKey(r),
		base.ResponseState{Status: 200, Header: http.Header{}, Body: []byte("old")},
		300*time.Second,
		false,
	); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	b.RunRequestPhase(w, httptest.NewRequest("PURGE", "http://example.test/cold", nil))
	if w.Code != 200 {
		t.Fatalf("persisted existing entry PURGE status=%d, want 200", w.Code)
	}
}
