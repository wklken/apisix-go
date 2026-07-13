package proxy_cache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestPostInitSetsProxyCacheDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.CacheStrategy != "disk" {
		t.Fatalf("cache_strategy = %q, want disk", p.config.CacheStrategy)
	}
	if p.config.CacheZone != "disk_cache_one" {
		t.Fatalf("cache_zone = %q, want disk_cache_one", p.config.CacheZone)
	}
	if p.config.CacheTTL != 300 {
		t.Fatalf("cache_ttl = %d, want 300", p.config.CacheTTL)
	}
	if got := p.config.CacheKey; len(got) != 2 || got[0] != "$host" || got[1] != "$request_uri" {
		t.Fatalf("cache_key = %v, want [$host $request_uri]", got)
	}
	if got := p.config.CacheMethod; len(got) != 2 || got[0] != http.MethodGet || got[1] != http.MethodHead {
		t.Fatalf("cache_method = %v, want [GET HEAD]", got)
	}
	if got := p.config.CacheHTTPStatus; len(got) != 3 || got[0] != 200 || got[1] != 301 || got[2] != 404 {
		t.Fatalf("cache_http_status = %v, want [200 301 404]", got)
	}
	if !p.config.ConsumerIsolation {
		t.Fatal("consumer_isolation = false, want true")
	}
}

func TestPostInitPreservesExplicitConsumerIsolationFalse(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{"consumer_isolation": false}, p.Config()); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	if p.config.ConsumerIsolation {
		t.Fatal("consumer_isolation = true, want explicit false")
	}
}

func TestHandlerCachesSuccessfulGETResponses(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Origin", "upstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("response-v1"))
	}))

	first := performRequest(t, handler, http.MethodGet, "/anything", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}
	if first.Body.String() != "response-v1" {
		t.Fatalf("first body = %q, want response-v1", first.Body.String())
	}

	second := performRequest(t, handler, http.MethodGet, "/anything", nil)
	if second.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("second cache status = %q, want HIT", second.Header().Get(cacheStatusHeader))
	}
	if second.Header().Get("X-Origin") != "upstream" {
		t.Fatalf("cached X-Origin = %q, want upstream", second.Header().Get("X-Origin"))
	}
	if second.Body.String() != "response-v1" {
		t.Fatalf("second body = %q, want response-v1", second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestHandlerSetsAgeOnCacheHit(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("response"))
	}))

	first := performRequest(t, handler, http.MethodGet, "/age", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}

	key := p.cacheKey(httptest.NewRequest(http.MethodGet, "/age", nil))
	p.lock.Lock()
	entry := p.entries[key]
	entry.storedAt = time.Now().Add(-4 * time.Second)
	p.entries[key] = entry
	p.lock.Unlock()

	second := performRequest(t, handler, http.MethodGet, "/age", nil)
	if second.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("second cache status = %q, want HIT", second.Header().Get(cacheStatusHeader))
	}
	age, err := strconv.Atoi(second.Header().Get("Age"))
	if err != nil || age < 3 {
		t.Fatalf("Age = %q (err=%v), want at least 3 seconds", second.Header().Get("Age"), err)
	}
}

func TestDiskStrategyPersistsAcrossPluginInstances(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-test", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Origin", "upstream")
		_, _ = w.Write([]byte("disk-response"))
	})
	firstPlugin := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-test", CacheTTL: 60})
	first := performRequest(t, firstPlugin.Handler(upstream), http.MethodGet, "/disk", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}

	secondPlugin := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-test", CacheTTL: 60})
	second := performRequest(t, secondPlugin.Handler(upstream), http.MethodGet, "/disk", nil)
	if second.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("second cache status = %q, want HIT from disk zone", second.Header().Get(cacheStatusHeader))
	}
	if second.Body.String() != "disk-response" || second.Header().Get("X-Origin") != "upstream" {
		t.Fatalf(
			"disk response = %q/%q, want persisted body/header",
			second.Body.String(),
			second.Header().Get("X-Origin"),
		)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read disk zone: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("disk zone is empty after storing response")
	}
}

func TestDiskStrategyPurgesPersistedEntry(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-purge", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-purge", CacheTTL: 60})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("response"))
	}))
	_ = performRequest(t, handler, http.MethodGet, "/purge-disk", nil)
	purge := performRequest(t, handler, purgeMethod, "/purge-disk", nil)
	if purge.Code != http.StatusOK {
		t.Fatalf("PURGE status = %d, want 200", purge.Code)
	}
	if entries, err := os.ReadDir(filepath.Clean(root)); err != nil {
		t.Fatalf("read purged disk zone: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("disk zone files after PURGE = %d, want 0", len(entries))
	}

	secondPlugin := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-purge", CacheTTL: 60})
	miss := performRequest(t, secondPlugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fresh"))
	})), http.MethodGet, "/purge-disk", nil)
	if miss.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("cache status after PURGE = %q, want MISS", miss.Header().Get(cacheStatusHeader))
	}
}

func TestDiskLookupRemovesExpiredEntry(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-expired", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-expired", CacheTTL: 60})
	req := httptest.NewRequest(http.MethodGet, "/expired", nil)
	key := p.cacheKey(req)
	entryPath := p.entryPath(key)
	if err := p.persistEntry(key, cacheEntry{
		header:    make(http.Header),
		body:      []byte("expired"),
		status:    http.StatusOK,
		storedAt:  time.Now().Add(-2 * time.Minute),
		ttl:       time.Minute,
		expiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("persist expired entry: %v", err)
	}
	if _, err := os.Stat(entryPath); err != nil {
		t.Fatalf("stat persisted expired entry: %v", err)
	}

	if _, status := p.lookup(req, key); status != "EXPIRED" {
		t.Fatalf("lookup status = %q, want EXPIRED", status)
	}
	if _, err := os.Stat(entryPath); !os.IsNotExist(err) {
		t.Fatalf("expired entry stat error = %v, want file removed", err)
	}
}

func TestDiskLookupRunsPeriodicExpirySweep(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-periodic", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-periodic", CacheTTL: 60})
	now := time.Now()
	expiredReq := httptest.NewRequest(http.MethodGet, "/expired-sweep", nil)
	expiredKey := p.cacheKey(expiredReq)
	expiredPath := p.entryPath(expiredKey)
	if err := p.persistEntry(expiredKey, cacheEntry{
		header:    make(http.Header),
		body:      []byte("expired"),
		status:    http.StatusOK,
		storedAt:  now.Add(-2 * time.Minute),
		ttl:       time.Minute,
		expiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("persist expired entry: %v", err)
	}

	freshReq := httptest.NewRequest(http.MethodGet, "/fresh-sweep", nil)
	freshKey := p.cacheKey(freshReq)
	if err := p.persistEntry(freshKey, cacheEntry{
		header:    make(http.Header),
		body:      []byte("fresh"),
		status:    http.StatusOK,
		storedAt:  now,
		ttl:       time.Minute,
		expiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("persist fresh entry: %v", err)
	}

	if _, status := p.lookup(freshReq, freshKey); status != "HIT" {
		t.Fatalf("fresh lookup status = %q, want HIT", status)
	}
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Fatalf("periodic expired entry stat error = %v, want file removed", err)
	}
}

func TestDiskBackgroundExpirySweepStopsWithPlugin(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-background", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := &Plugin{
		config:          Config{CacheStrategy: "disk", CacheZone: "disk-background", CacheTTL: 60},
		cleanupInterval: 10 * time.Millisecond,
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	now := time.Now()
	expiredReq := httptest.NewRequest(http.MethodGet, "/background-expired", nil)
	expiredKey := p.cacheKey(expiredReq)
	expiredPath := p.entryPath(expiredKey)
	if err := p.persistEntry(expiredKey, cacheEntry{
		header:    make(http.Header),
		body:      []byte("expired"),
		status:    http.StatusOK,
		storedAt:  now.Add(-2 * time.Minute),
		ttl:       time.Minute,
		expiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("persist expired entry: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(expiredPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("background cleanup did not remove %s", expiredPath)
}

func TestMemoryZoneSharesEntriesAcrossPluginInstances(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "memory-shared", MemorySize: "1M"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	firstPlugin := newTestPlugin(t, Config{CacheStrategy: "memory", CacheZone: "memory-shared", CacheTTL: 60})
	secondPlugin := newTestPlugin(t, Config{CacheStrategy: "memory", CacheZone: "memory-shared", CacheTTL: 60})
	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("shared-response"))
	})

	first := performRequest(t, firstPlugin.Handler(upstream), http.MethodGet, "/shared", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}
	second := performRequest(t, secondPlugin.Handler(upstream), http.MethodGet, "/shared", nil)
	if second.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("second cache status = %q, want HIT from shared memory zone", second.Header().Get(cacheStatusHeader))
	}
	if second.Body.String() != "shared-response" {
		t.Fatalf("second body = %q, want shared-response", second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	firstPlugin.Stop()
	secondPlugin.Stop()
	memoryZoneRegistry.Lock()
	_, retained := memoryZoneRegistry.zones["memory-shared"]
	memoryZoneRegistry.Unlock()
	if retained {
		t.Fatal("memory zone remained registered after all plugin instances stopped")
	}
}

func TestMemoryZoneRefreshWithChangedDefinitionStartsNewGeneration(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "memory-refresh-generation", MemorySize: "1M"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	firstPlugin := newTestPlugin(t, Config{
		CacheStrategy: "memory",
		CacheZone:     "memory-refresh-generation",
		CacheTTL:      60,
	})

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write(fmt.Appendf(nil, "generation-%d", calls))
	})
	firstHandler := firstPlugin.Handler(upstream)
	first := performRequest(t, firstHandler, http.MethodGet, "/generation", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}

	if err := RefreshConfiguredZones(
		[]appconfig.Zone{{Name: "memory-refresh-generation", MemorySize: "2M"}},
	); err != nil {
		t.Fatalf("RefreshConfiguredZones() error = %v", err)
	}
	secondPlugin := newTestPlugin(t, Config{
		CacheStrategy: "memory",
		CacheZone:     "memory-refresh-generation",
		CacheTTL:      60,
	})
	second := performRequest(t, secondPlugin.Handler(upstream), http.MethodGet, "/generation", nil)
	if second.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("cache status after changed zone definition = %q, want MISS", second.Header().Get(cacheStatusHeader))
	}
	if second.Body.String() != "generation-2" {
		t.Fatalf("second body = %q, want new generation response", second.Body.String())
	}

	firstAgain := performRequest(t, firstHandler, http.MethodGet, "/generation", nil)
	if firstAgain.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("old generation cache status = %q, want HIT", firstAgain.Header().Get(cacheStatusHeader))
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want separate cache generations", calls)
	}

	t.Cleanup(firstPlugin.Stop)
	t.Cleanup(secondPlugin.Stop)
}

func TestRefreshConfiguredZonesRejectsInvalidSnapshotWithoutReplacingCurrent(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "refresh-valid", MemorySize: "1M"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	if err := RefreshConfiguredZones([]appconfig.Zone{{Name: "refresh-next", MemorySize: "2M"}}); err != nil {
		t.Fatalf("RefreshConfiguredZones(valid) error = %v", err)
	}
	if !CacheZoneDeclared("refresh-next") || CacheZoneDeclared("refresh-valid") {
		t.Fatal("valid refresh did not replace the configured zone snapshot")
	}

	if err := RefreshConfiguredZones([]appconfig.Zone{{Name: "refresh-invalid", MemorySize: "zero"}}); err == nil {
		t.Fatal("RefreshConfiguredZones(invalid) error = nil, want rejection")
	}
	if !CacheZoneDeclared("refresh-next") || CacheZoneDeclared("refresh-invalid") {
		t.Fatal("invalid refresh replaced the last valid configured zone snapshot")
	}
}

func TestPostInitRejectsUnknownConfiguredZone(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "known-zone", MemorySize: "1M"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := &Plugin{config: Config{CacheStrategy: "memory", CacheZone: "unknown-zone"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want unknown cache zone rejection")
	}
}

func TestPostInitRejectsCacheStrategyZoneMismatch(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-only", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := &Plugin{config: Config{CacheStrategy: "memory", CacheZone: "disk-only"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want cache strategy/zone mismatch rejection")
	} else if !strings.Contains(err.Error(), "cache_strategy") {
		t.Fatalf("PostInit() error = %q, want cache_strategy context", err)
	}
}

func TestPostInitRejectsRequestMethodCacheKey(t *testing.T) {
	p := &Plugin{config: Config{CacheKey: []string{"$request_method"}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want unsupported cache key rejection")
	} else if !strings.Contains(err.Error(), "$request_method") {
		t.Fatalf("PostInit() error = %q, want cache key context", err)
	}
}

func TestPostInitRejectsDuplicateCacheFilters(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "methods",
			cfg:  Config{CacheMethod: []string{http.MethodGet, http.MethodGet}},
		},
		{
			name: "statuses",
			cfg:  Config{CacheHTTPStatus: []int{http.StatusOK, http.StatusOK}},
		},
		{
			name: "bypass variable",
			cfg:  Config{CacheBypass: []string{"$arg-foo"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := &Plugin{config: test.cfg}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err == nil {
				t.Fatal("PostInit() error = nil, want strict cache configuration rejection")
			}
		})
	}
}

func TestPostInitRejectsInvalidZoneRegistry(t *testing.T) {
	tests := []struct {
		name  string
		zones []appconfig.Zone
		cache string
	}{
		{
			name: "duplicate names",
			zones: []appconfig.Zone{
				{Name: "duplicate", MemorySize: "1M"},
				{Name: "duplicate", MemorySize: "1M"},
			},
			cache: "duplicate",
		},
		{
			name:  "invalid memory size",
			zones: []appconfig.Zone{{Name: "invalid-memory", MemorySize: "zero"}},
			cache: "invalid-memory",
		},
		{
			name:  "invalid cache levels",
			zones: []appconfig.Zone{{Name: "invalid-levels", MemorySize: "1M", CacheLevels: "1:2:3:1"}},
			cache: "invalid-levels",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldConfig := appconfig.GlobalConfig
			appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
				Zones: test.zones,
			}}}
			t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

			p := &Plugin{config: Config{CacheStrategy: "memory", CacheZone: test.cache}}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err == nil {
				t.Fatal("PostInit() error = nil, want zone registry rejection")
			}
		})
	}
}

func TestValidateConfiguredZonesRejectsUnusedInvalidZone(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "unused-invalid", MemorySize: "zero"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	if err := ValidateConfiguredZones(); err == nil {
		t.Fatal("ValidateConfiguredZones() error = nil, want invalid unused zone rejection")
	}
}

func TestPostInitReadsDiskSize(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-size", DiskPath: root, DiskSize: "2K"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-size"})
	if p.diskSize != 2*1024 {
		t.Fatalf("disk size = %d, want %d", p.diskSize, 2*1024)
	}
}

func TestDiskStrategyEvictsOldestEntryWhenDiskSizeExceeded(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-evict", DiskPath: root, DiskSize: "12K"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-evict", CacheTTL: 60})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(strings.Repeat(r.URL.Path, 1000)))
	}))

	first := performRequest(t, handler, http.MethodGet, "/first", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}
	second := performRequest(t, handler, http.MethodGet, "/second", nil)
	if second.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("second cache status = %q, want MISS", second.Header().Get(cacheStatusHeader))
	}

	firstKeyRequest := httptest.NewRequest(http.MethodGet, "/first", nil)
	if _, status := p.lookup(firstKeyRequest, p.cacheKey(firstKeyRequest)); status != "MISS" {
		t.Fatalf("evicted first lookup status = %q, want MISS", status)
	}
	secondKeyRequest := httptest.NewRequest(http.MethodGet, "/second", nil)
	if _, status := p.lookup(secondKeyRequest, p.cacheKey(secondKeyRequest)); status != "HIT" {
		t.Fatalf("newer second lookup status = %q, want HIT", status)
	}
	secondAgain := performRequest(t, handler, http.MethodGet, "/second", nil)
	if secondAgain.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("newer second cache status = %q, want HIT", secondAgain.Header().Get(cacheStatusHeader))
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestDiskStrategyPersistsVaryVariantsAcrossPluginInstances(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-vary", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "X-Variant")
		_, _ = w.Write([]byte(r.Header.Get("X-Variant")))
	})
	firstPlugin := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-vary", CacheTTL: 60})
	first := performRequest(
		t,
		firstPlugin.Handler(upstream),
		http.MethodGet,
		"/vary-disk",
		map[string]string{"X-Variant": "a"},
	)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}

	secondPlugin := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-vary", CacheTTL: 60})
	second := performRequest(
		t,
		secondPlugin.Handler(upstream),
		http.MethodGet,
		"/vary-disk",
		map[string]string{"X-Variant": "a"},
	)
	if second.Header().Get(cacheStatusHeader) != "HIT" || second.Body.String() != "a" {
		t.Fatalf(
			"persisted Vary response = %q/%q, want HIT/a",
			second.Header().Get(cacheStatusHeader),
			second.Body.String(),
		)
	}
	other := performRequest(
		t,
		secondPlugin.Handler(upstream),
		http.MethodGet,
		"/vary-disk",
		map[string]string{"X-Variant": "b"},
	)
	if other.Header().Get(cacheStatusHeader) != "MISS" || other.Body.String() != "b" {
		t.Fatalf("other Vary response = %q/%q, want MISS/b", other.Header().Get(cacheStatusHeader), other.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerIsolatesCacheByConsumerByDefault(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("response"))
	}))

	alice := performConsumerRequest(t, handler, http.MethodGet, "/anything", "alice")
	if alice.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("alice cache status = %q, want MISS", alice.Header().Get(cacheStatusHeader))
	}
	bob := performConsumerRequest(t, handler, http.MethodGet, "/anything", "bob")
	if bob.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("bob cache status = %q, want MISS for separate consumer bucket", bob.Header().Get(cacheStatusHeader))
	}
	aliceHit := performConsumerRequest(t, handler, http.MethodGet, "/anything", "alice")
	if aliceHit.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("alice second cache status = %q, want HIT", aliceHit.Header().Get(cacheStatusHeader))
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerRefreshesExpiredEntries(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 1})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("response"))
	}))

	_ = performRequest(t, handler, http.MethodGet, "/expiring", nil)
	key := p.cacheKey(httptest.NewRequest(http.MethodGet, "/expiring", nil))
	entry := p.entries[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	p.entries[key] = entry

	res := performRequest(t, handler, http.MethodGet, "/expiring", nil)
	if res.Header().Get(cacheStatusHeader) != "EXPIRED" {
		t.Fatalf("cache status = %q, want EXPIRED", res.Header().Get(cacheStatusHeader))
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerHonorsNoCacheAndCacheBypass(t *testing.T) {
	p := newTestPlugin(t, Config{
		CacheTTL:        60,
		NoCache:         []string{"$arg_no_cache"},
		CacheBypass:     []string{"$http_bypass"},
		CacheKey:        []string{"$host", "$uri"},
		CacheMethod:     []string{http.MethodGet},
		CacheHTTPStatus: []int{http.StatusOK},
	})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("response"))
	}))

	noCache := performRequest(t, handler, http.MethodGet, "/anything?no_cache=1", nil)
	if noCache.Header().Get(cacheStatusHeader) != "EXPIRED" {
		t.Fatalf("no-cache status = %q, want EXPIRED", noCache.Header().Get(cacheStatusHeader))
	}
	normal := performRequest(t, handler, http.MethodGet, "/anything", nil)
	if normal.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("normal status = %q, want MISS", normal.Header().Get(cacheStatusHeader))
	}
	bypass := performRequest(t, handler, http.MethodGet, "/anything", map[string]string{"Bypass": "1"})
	if bypass.Header().Get(cacheStatusHeader) != "BYPASS" {
		t.Fatalf("bypass status = %q, want BYPASS", bypass.Header().Get(cacheStatusHeader))
	}
	hit := performRequest(t, handler, http.MethodGet, "/anything", nil)
	if hit.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("hit status = %q, want HIT", hit.Header().Get(cacheStatusHeader))
	}
	if calls != 3 {
		t.Fatalf("upstream calls = %d, want 3", calls)
	}
}

func TestDiskCacheControlRequestDirectivesAreIgnored(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-cache-control", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{
		CacheStrategy: "disk",
		CacheZone:     "disk-cache-control",
		CacheControl:  true,
		CacheTTL:      60,
	})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("disk-response"))
	}))

	res := performRequest(t, handler, http.MethodGet, "/disk-cache-control", map[string]string{
		"Cache-Control": "only-if-cached",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 because disk cache ignores cache_control", res.Code)
	}
	if res.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("cache status = %q, want MISS", res.Header().Get(cacheStatusHeader))
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestDiskCacheSetCookieIsNeverStored(t *testing.T) {
	root := t.TempDir()
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "disk-cache-cookie", DiskPath: root}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{
		CacheStrategy:  "disk",
		CacheZone:      "disk-cache-cookie",
		CacheSetCookie: true,
		CacheTTL:       60,
	})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Set-Cookie", fmt.Sprintf("visit=%d", calls))
		_, _ = w.Write(fmt.Appendf(nil, "response-v%d", calls))
	}))

	first := performRequest(t, handler, http.MethodGet, "/disk-cache-cookie", nil)
	second := performRequest(t, handler, http.MethodGet, "/disk-cache-cookie", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" || second.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf(
			"cache statuses = %q/%q, want MISS/MISS",
			first.Header().Get(cacheStatusHeader),
			second.Header().Get(cacheStatusHeader),
		)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerCacheControlRequestNoCacheBypassesStoredEntry(t *testing.T) {
	p := newTestPlugin(t, Config{CacheControl: true, CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = w.Write(fmt.Appendf(nil, "response-v%d", calls))
	}))

	first := performRequest(t, handler, http.MethodGet, "/cache-control", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}
	bypass := performRequest(
		t,
		handler,
		http.MethodGet,
		"/cache-control",
		map[string]string{"Cache-Control": "no-cache"},
	)
	if bypass.Header().Get(cacheStatusHeader) != "BYPASS" {
		t.Fatalf("bypass cache status = %q, want BYPASS", bypass.Header().Get(cacheStatusHeader))
	}
	hit := performRequest(t, handler, http.MethodGet, "/cache-control", nil)
	if hit.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("hit cache status = %q, want HIT", hit.Header().Get(cacheStatusHeader))
	}
	if hit.Body.String() != "response-v1" {
		t.Fatalf("cached body = %q, want response-v1", hit.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerCacheControlResponseDirectivesSkipStore(t *testing.T) {
	tests := []struct {
		name         string
		cacheControl string
	}{
		{name: "no-store", cacheControl: "no-store"},
		{name: "private", cacheControl: "private, max-age=600"},
		{name: "no-cache", cacheControl: "no-cache"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{CacheControl: true, CacheTTL: 60})
			calls := 0

			handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Cache-Control", tt.cacheControl)
				_, _ = w.Write([]byte("response"))
			}))

			first := performRequest(t, handler, http.MethodGet, "/cache-control-response", nil)
			second := performRequest(t, handler, http.MethodGet, "/cache-control-response", nil)

			if first.Header().Get(cacheStatusHeader) != "MISS" {
				t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
			}
			if second.Header().Get(cacheStatusHeader) != "MISS" {
				t.Fatalf("second cache status = %q, want MISS", second.Header().Get(cacheStatusHeader))
			}
			if calls != 2 {
				t.Fatalf("upstream calls = %d, want 2", calls)
			}
		})
	}
}

func TestHandlerCacheControlOnlyIfCachedMissReturnsGatewayTimeout(t *testing.T) {
	p := newTestPlugin(t, Config{CacheControl: true, CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("response"))
	}))

	res := performRequest(
		t,
		handler,
		http.MethodGet,
		"/only-if-cached",
		map[string]string{"Cache-Control": "only-if-cached"},
	)
	if res.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusGatewayTimeout)
	}
	if res.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("cache status = %q, want MISS", res.Header().Get(cacheStatusHeader))
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHandlerCacheControlRequiresPositiveResourceTTL(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "missing"},
		{name: "zero max age", headers: map[string]string{"Cache-Control": "max-age=0"}},
		{name: "expired expires", headers: map[string]string{
			"Expires": time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{CacheControl: true, CacheTTL: 60})
			calls := 0

			handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				for name, value := range tt.headers {
					w.Header().Set(name, value)
				}
				_, _ = w.Write([]byte("response"))
			}))

			first := performRequest(t, handler, http.MethodGet, "/resource-ttl", nil)
			second := performRequest(t, handler, http.MethodGet, "/resource-ttl", nil)

			if first.Header().Get(cacheStatusHeader) != "MISS" {
				t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
			}
			if second.Header().Get(cacheStatusHeader) != "MISS" {
				t.Fatalf("second cache status = %q, want MISS", second.Header().Get(cacheStatusHeader))
			}
			if calls != 2 {
				t.Fatalf("upstream calls = %d, want 2", calls)
			}
		})
	}
}

func TestHandlerCacheControlUsesUpstreamMaxAgeTTL(t *testing.T) {
	p := newTestPlugin(t, Config{CacheControl: true, CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "max-age=1")
		_, _ = w.Write([]byte("response"))
	}))

	before := time.Now()
	first := performRequest(t, handler, http.MethodGet, "/resource-max-age", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}

	key := p.cacheKey(httptest.NewRequest(http.MethodGet, "/resource-max-age", nil))
	entry, ok := p.entries[key]
	if !ok {
		t.Fatal("cache entry missing")
	}
	if entry.expiresAt.Before(before) || entry.expiresAt.After(before.Add(2*time.Second)) {
		t.Fatalf("expiresAt = %s, want about one second after %s", entry.expiresAt, before)
	}

	second := performRequest(t, handler, http.MethodGet, "/resource-max-age", nil)
	if second.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("second cache status = %q, want HIT", second.Header().Get(cacheStatusHeader))
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestHandlerCacheControlIsIgnoredForIdentityCacheKey(t *testing.T) {
	p := newTestPlugin(t, Config{
		CacheControl: true,
		CacheKey:     []string{"$consumer_name", "$request_uri"},
		CacheTTL:     60,
	})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "max-age=1")
		_, _ = w.Write([]byte("response"))
	}))

	first := performRequest(t, handler, http.MethodGet, "/identity-cache-control", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}

	key := p.cacheKey(httptest.NewRequest(http.MethodGet, "/identity-cache-control", nil))
	entry, ok := p.entries[key]
	if !ok {
		t.Fatal("cache entry missing")
	}
	if entry.ttl != 60*time.Second {
		t.Fatalf("entry ttl = %s, want configured 60s when cache_control is disabled for identity keys", entry.ttl)
	}

	hit := performRequest(
		t,
		handler,
		http.MethodGet,
		"/identity-cache-control",
		map[string]string{"Cache-Control": "no-cache"},
	)
	if hit.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf(
			"cache status = %q, want HIT because cache_control is ignored for identity keys",
			hit.Header().Get(cacheStatusHeader),
		)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestHandlerCacheControlRequestFreshnessDirectivesForceStaleRefresh(t *testing.T) {
	tests := []struct {
		name         string
		requestValue string
		storedAge    time.Duration
		storedTTL    time.Duration
	}{
		{name: "max age", requestValue: "max-age=5", storedAge: 10 * time.Second, storedTTL: 60 * time.Second},
		{name: "max stale", requestValue: "max-stale=3", storedAge: 10 * time.Second, storedTTL: 5 * time.Second},
		{name: "min fresh", requestValue: "min-fresh=10", storedAge: 55 * time.Second, storedTTL: 60 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{CacheControl: true, CacheTTL: 60})
			calls := 0

			handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Cache-Control", "max-age=60")
				_, _ = w.Write(fmt.Appendf(nil, "response-v%d", calls))
			}))

			first := performRequest(t, handler, http.MethodGet, "/request-freshness", nil)
			if first.Header().Get(cacheStatusHeader) != "MISS" {
				t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
			}

			key := p.cacheKey(httptest.NewRequest(http.MethodGet, "/request-freshness", nil))
			entry := p.entries[key]
			entry.storedAt = time.Now().Add(-tt.storedAge)
			entry.ttl = tt.storedTTL
			entry.expiresAt = time.Now().Add(time.Minute)
			p.entries[key] = entry

			stale := performRequest(
				t,
				handler,
				http.MethodGet,
				"/request-freshness",
				map[string]string{"Cache-Control": tt.requestValue},
			)
			if stale.Header().Get(cacheStatusHeader) != "STALE" {
				t.Fatalf("stale cache status = %q, want STALE", stale.Header().Get(cacheStatusHeader))
			}
			if stale.Body.String() != "response-v2" {
				t.Fatalf("stale body = %q, want response-v2", stale.Body.String())
			}
			if calls != 2 {
				t.Fatalf("upstream calls = %d, want 2", calls)
			}
		})
	}
}

func TestHandlerPurgesCachedEntry(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write(fmt.Appendf(nil, "response-v%d", calls))
	}))

	first := performRequest(t, handler, http.MethodGet, "/purgeable", nil)
	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}

	purge := performRequest(t, handler, purgeMethod, "/purgeable", nil)
	if purge.Code != http.StatusOK {
		t.Fatalf("purge status = %d, want %d", purge.Code, http.StatusOK)
	}
	if calls != 1 {
		t.Fatalf("upstream calls after purge = %d, want 1", calls)
	}

	second := performRequest(t, handler, http.MethodGet, "/purgeable", nil)
	if second.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("second cache status = %q, want MISS", second.Header().Get(cacheStatusHeader))
	}
	if second.Body.String() != "response-v2" {
		t.Fatalf("second body = %q, want response-v2", second.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerCachesVaryVariantsByRequestHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "Accept-Language")
		_, _ = w.Write([]byte("lang-" + r.Header.Get("Accept-Language")))
	}))

	en := performRequest(t, handler, http.MethodGet, "/vary", map[string]string{"Accept-Language": "en"})
	if en.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("en cache status = %q, want MISS", en.Header().Get(cacheStatusHeader))
	}
	fr := performRequest(t, handler, http.MethodGet, "/vary", map[string]string{"Accept-Language": "fr"})
	if fr.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("fr cache status = %q, want MISS", fr.Header().Get(cacheStatusHeader))
	}
	enHit := performRequest(t, handler, http.MethodGet, "/vary", map[string]string{"Accept-Language": "en"})
	if enHit.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("en hit cache status = %q, want HIT", enHit.Header().Get(cacheStatusHeader))
	}
	if enHit.Body.String() != "lang-en" {
		t.Fatalf("en hit body = %q, want lang-en", enHit.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerVaryStarSkipsStore(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "*")
		_, _ = w.Write([]byte("response"))
	}))

	first := performRequest(t, handler, http.MethodGet, "/vary-star", nil)
	second := performRequest(t, handler, http.MethodGet, "/vary-star", nil)

	if first.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", first.Header().Get(cacheStatusHeader))
	}
	if second.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("second cache status = %q, want MISS", second.Header().Get(cacheStatusHeader))
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerPurgeRemovesVaryVariants(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "Accept-Language")
		_, _ = w.Write([]byte("lang-" + r.Header.Get("Accept-Language")))
	}))

	_ = performRequest(t, handler, http.MethodGet, "/vary-purge", map[string]string{"Accept-Language": "en"})
	_ = performRequest(t, handler, http.MethodGet, "/vary-purge", map[string]string{"Accept-Language": "fr"})

	purge := performRequest(t, handler, purgeMethod, "/vary-purge", nil)
	if purge.Code != http.StatusOK {
		t.Fatalf("purge status = %d, want %d", purge.Code, http.StatusOK)
	}

	en := performRequest(t, handler, http.MethodGet, "/vary-purge", map[string]string{"Accept-Language": "en"})
	if en.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("en cache status after purge = %q, want MISS", en.Header().Get(cacheStatusHeader))
	}
	if calls != 3 {
		t.Fatalf("upstream calls = %d, want 3", calls)
	}
}

func TestHandlerPurgeMissReturnsNotFound(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("response"))
	}))

	res := performRequest(t, handler, purgeMethod, "/missing", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("purge status = %d, want %d", res.Code, http.StatusNotFound)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHandlerSkipsUnsupportedMethods(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("response"))
	}))

	first := performRequest(t, handler, http.MethodPost, "/anything", nil)
	second := performRequest(t, handler, http.MethodPost, "/anything", nil)

	if first.Header().Get(cacheStatusHeader) != "" || second.Header().Get(cacheStatusHeader) != "" {
		t.Fatalf(
			"cache statuses = %q/%q, want empty",
			first.Header().Get(cacheStatusHeader),
			second.Header().Get(cacheStatusHeader),
		)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func performConsumerRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	target string,
	consumerName string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	apisixctx.AttachConsumer(req, resource.Consumer{Username: consumerName})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func performRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	target string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
