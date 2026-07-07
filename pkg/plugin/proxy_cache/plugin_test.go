package proxy_cache

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
