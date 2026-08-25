package proxy_cache

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type cacheHitCountingWriter struct {
	http.ResponseWriter
	headerCalls      int
	writeHeaderCalls int
	writeCalls       int
}

func TestRequestVarUsesEffectiveRemoteIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	r = r.WithContext(context.WithValue(r.Context(), apisixctx.RemoteAddrKey, "198.51.100.2"))

	if got := requestVar(r, "remote_addr"); got != "198.51.100.2" {
		t.Fatalf("requestVar(remote_addr) = %q, want effective remote address", got)
	}
}

func TestPostInitUsesGenerationLocalConfiguredZones(t *testing.T) {
	if err := RefreshConfiguredZones([]appconfig.Zone{{Name: "legacy-zone", MemorySize: "1M"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RefreshConfiguredZones(nil) })

	instance := &Plugin{config: Config{CacheZone: "candidate-zone", CacheStrategy: "memory"}}
	instance.SetConfiguredZones([]appconfig.Zone{{Name: "candidate-zone", MemorySize: "2M"}})
	if err := instance.PostInit(); err != nil {
		t.Fatalf("PostInit() observed legacy zone snapshot: %v", err)
	}
	t.Cleanup(instance.Stop)
	if instance.memoryZone == nil || instance.memoryZone.capacity != 2<<20 {
		t.Fatalf("candidate memory zone = %#v, want generation-local 2M zone", instance.memoryZone)
	}
}

func (w *cacheHitCountingWriter) Header() http.Header {
	w.headerCalls++
	return w.ResponseWriter.Header()
}

func (w *cacheHitCountingWriter) WriteHeader(status int) {
	w.writeHeaderCalls++
	w.ResponseWriter.WriteHeader(status)
}

func (w *cacheHitCountingWriter) Write(body []byte) (int, error) {
	w.writeCalls++
	return w.ResponseWriter.Write(body)
}

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

func setConfiguredZones(t *testing.T, zones []appconfig.Zone) {
	t.Helper()
	if err := RefreshConfiguredZones(zones); err != nil {
		t.Fatalf("RefreshConfiguredZones() error = %v", err)
	}
	t.Cleanup(func() { _ = RefreshConfiguredZones(nil) })
}

func TestProxyCacheHitPublishesWithoutWriterAndReturnsCacheHitStop(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60})
	request := httptest.NewRequest(http.MethodGet, "/cache-hit", nil)
	key := p.cacheKey(request)
	p.lock.Lock()
	p.entries[key] = cacheEntry{
		header:    http.Header{"X-Cached": {"yes"}},
		body:      []byte("cached"),
		status:    http.StatusOK,
		storedAt:  time.Now().Add(-time.Second),
		ttl:       time.Minute,
		expiresAt: time.Now().Add(time.Minute),
	}
	p.lock.Unlock()
	holder := base.NewCacheHitResponseHolder()
	request = base.WithCacheHitResponseHolder(request, holder)
	writer := &cacheHitCountingWriter{ResponseWriter: httptest.NewRecorder()}

	result := p.RunRequestPhase(writer, request)
	if result.Decision != base.RequestStop || result.Source != apisixctx.ResponseSourceCacheHit {
		t.Fatalf("RunRequestPhase() = decision:%v source:%q, want cache-hit stop", result.Decision, result.Source)
	}
	if writer.headerCalls != 0 || writer.writeHeaderCalls != 0 || writer.writeCalls != 0 {
		t.Fatalf(
			"cache hit made writer calls header=%d writeHeader=%d write=%d",
			writer.headerCalls,
			writer.writeHeaderCalls,
			writer.writeCalls,
		)
	}
	state, published, err := holder.ConsumePublished()
	if err != nil || !published || state.Status != http.StatusOK || string(state.Body) != "cached" {
		t.Fatalf("published cache hit = %#v published=%v err=%v", state, published, err)
	}
	if state.Header.Get(cacheStatusHeader) != "HIT" || state.Header.Get("X-Cached") != "yes" {
		t.Fatalf("published cache hit headers = %#v", state.Header)
	}
}

func TestProxyCacheStoreIntentIsPerPluginInstanceAndConsumedOnce(t *testing.T) {
	first := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60})
	second := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60})
	reqFirst := httptest.NewRequest(http.MethodGet, "/intent-first", nil)
	reqSecond := httptest.NewRequest(http.MethodGet, "/intent-second", nil)
	firstRequest := first.RunRequestPhase(httptest.NewRecorder(), reqFirst).Request
	secondRequest := second.RunRequestPhase(httptest.NewRecorder(), reqSecond).Request
	state := base.ResponseState{Status: http.StatusOK, Header: http.Header{"X-Origin": {"one"}}, Body: []byte("one")}
	if err := first.RunFinalResponseStore(firstRequest, state); err != nil {
		t.Fatalf("first RunFinalResponseStore() error = %v", err)
	}
	if err := second.RunFinalResponseStore(secondRequest, state); err != nil {
		t.Fatalf("second RunFinalResponseStore() error = %v", err)
	}
	if err := first.RunFinalResponseStore(firstRequest, state); err == nil {
		t.Fatal("duplicate first RunFinalResponseStore() error = nil")
	}
}

func TestProxyCacheFinalStoreSkipsTrailerBearingResponse(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60})
	request := p.RunRequestPhase(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/grpc-cache", nil),
	).Request
	if err := p.RunFinalResponseStore(request, base.ResponseState{
		Status:  http.StatusOK,
		Header:  http.Header{"Content-Type": {"application/json"}},
		Trailer: http.Header{"Grpc-Status": {"0"}},
		Body:    []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("RunFinalResponseStore() error = %v", err)
	}
	p.lock.RLock()
	defer p.lock.RUnlock()
	if len(p.entries) != 0 {
		t.Fatalf("cached trailer-bearing entries = %d, want 0", len(p.entries))
	}
}

func TestProxyCacheStoreIntentUsesMissTimeVaryHeaderSnapshot(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60})
	request := httptest.NewRequest(http.MethodGet, "/vary-intent", nil)
	request.Header.Set("X-Variant", "miss")
	request = p.RunRequestPhase(httptest.NewRecorder(), request).Request
	request.Header.Values("X-Variant")[0] = "final"
	if err := p.RunFinalResponseStore(request, base.ResponseState{
		Status: http.StatusOK,
		Header: http.Header{"Vary": {"X-Variant"}},
		Body:   []byte("vary-intent"),
	}); err != nil {
		t.Fatalf("RunFinalResponseStore() error = %v", err)
	}

	key := p.cacheKey(request)
	missRequest := httptest.NewRequest(http.MethodGet, "/vary-intent", nil)
	missRequest.Header.Set("X-Variant", "miss")
	finalRequest := httptest.NewRequest(http.MethodGet, "/vary-intent", nil)
	finalRequest.Header.Set("X-Variant", "final")
	p.lock.RLock()
	_, missStored := p.entries[key+"::"+cacheutil.VarySignature([]string{"x-variant"}, missRequest)]
	_, finalStored := p.entries[key+"::"+cacheutil.VarySignature([]string{"x-variant"}, finalRequest)]
	p.lock.RUnlock()
	if !missStored || finalStored {
		t.Fatalf("Vary storage keys miss=%v final=%v, want only miss-time signature", missStored, finalStored)
	}
}

func TestProxyCacheFinalStoreReevaluatesNoCacheAgainstFinalRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		CacheStrategy: "memory",
		CacheTTL:      60,
		NoCache:       []string{"$http_x_no_cache"},
	})
	request := httptest.NewRequest(http.MethodGet, "/late-no-cache", nil)
	request = p.RunRequestPhase(httptest.NewRecorder(), request).Request
	request.Header.Set("X-No-Cache", "1")
	if err := p.RunFinalResponseStore(request, base.ResponseState{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte("must-not-store"),
	}); err != nil {
		t.Fatalf("RunFinalResponseStore() error = %v", err)
	}

	if _, status := p.lookup(request, p.cacheKey(request)); status != "MISS" {
		t.Fatalf("lookup() status = %q, want MISS after final request enables no_cache", status)
	}
}

func TestCacheFinalStorePersistsCanonicalStateWithoutDerivedHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60, CacheSetCookie: true})
	request := httptest.NewRequest(http.MethodGet, "/canonical", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	request = p.RunRequestPhase(httptest.NewRecorder(), request).Request
	state := base.ResponseState{
		Status: http.StatusOK,
		Header: http.Header{
			"aGe":                 {"3"},
			"APISIX-Cache-STATUS": {"MISS"},
			"apisix-cache-key":    {"derived"},
			"Vary":                {"Accept-Encoding"},
			"Cache-Control":       {"max-age=60"},
			"Set-Cookie":          {"session=one"},
		},
		Body: []byte("canonical"),
	}
	if err := p.RunFinalResponseStore(request, state); err != nil {
		t.Fatalf("RunFinalResponseStore() error = %v", err)
	}
	key := p.cacheKey(request)
	p.lock.RLock()
	entry, ok := p.entries[key+"::"+cacheutil.VarySignature([]string{"accept-encoding"}, request)]
	p.lock.RUnlock()
	if !ok {
		t.Fatalf("canonical cache entry missing for key %q", key)
	}
	for field := range entry.header {
		if strings.EqualFold(field, "Age") || strings.EqualFold(field, "Apisix-Cache-Status") ||
			strings.EqualFold(field, "APISIX-Cache-Key") {
			t.Fatalf("derived header %q persisted in canonical cache entry", field)
		}
	}
	if entry.header.Get("Vary") != "Accept-Encoding" || entry.header.Get("Cache-Control") != "max-age=60" ||
		entry.header.Get("Set-Cookie") != "session=one" {
		t.Fatalf("canonical cache headers = %#v", entry.header)
	}
}

func TestCachePolicyVaryTTLSetCookiePurgeAndStorageOwnershipRemain(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60, CacheSetCookie: true})
	request := httptest.NewRequest(http.MethodGet, "/policy", nil)
	request.Header.Set("X-Variant", "one")
	request = p.RunRequestPhase(httptest.NewRecorder(), request).Request
	if err := p.RunFinalResponseStore(request, base.ResponseState{
		Status: http.StatusOK,
		Header: http.Header{"Vary": {"X-Variant"}, "Set-Cookie": {"session=one"}},
		Body:   []byte("policy"),
	}); err != nil {
		t.Fatalf("RunFinalResponseStore() error = %v", err)
	}
	key := p.cacheKey(request)
	variant := httptest.NewRequest(http.MethodGet, "/policy", nil)
	variant.Header.Set("X-Variant", "one")
	if _, status := p.lookup(variant, key); status != "HIT" {
		t.Fatalf("lookup() status = %q, want HIT for persisted Vary variant", status)
	}
	if !p.purgeAll(key) {
		t.Fatal("purgeAll() = false, want true for stored variant")
	}
	if _, status := p.lookup(variant, key); status != "MISS" {
		t.Fatalf("lookup() after purge status = %q, want MISS", status)
	}
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

func TestHandlerMemoryZoneRejectsOversizedResponseWithoutEvictingSmallEntry(t *testing.T) {
	setConfiguredZones(t, []appconfig.Zone{{Name: "bounded-handler", MemorySize: "320B"}})

	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheZone: "bounded-handler", CacheTTL: 60})
	calls := 0
	largeBody := strings.Repeat("x", 512)
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Origin", "upstream")
		if r.URL.Path == "/large" {
			_, _ = w.Write([]byte(largeBody))
			return
		}
		_, _ = w.Write([]byte("small-body"))
	}))

	small := performRequest(t, handler, http.MethodGet, "/small", nil)
	if small.Header().Get(cacheStatusHeader) != "MISS" || small.Body.String() != "small-body" {
		t.Fatalf(
			"initial small response = %q/%q, want MISS/small-body",
			small.Header().Get(cacheStatusHeader),
			small.Body.String(),
		)
	}
	large := performRequest(t, handler, http.MethodGet, "/large", nil)
	if large.Header().Get(cacheStatusHeader) != "MISS" || large.Body.String() != largeBody {
		t.Fatalf(
			"oversized response = %q/%q, want MISS/large body",
			large.Header().Get(cacheStatusHeader),
			large.Body.String(),
		)
	}
	retained := performRequest(t, handler, http.MethodGet, "/small", nil)
	if retained.Header().Get(cacheStatusHeader) != "HIT" || retained.Body.String() != "small-body" {
		t.Fatalf(
			"small response after oversized store = %q/%q, want HIT/small-body",
			retained.Header().Get(cacheStatusHeader),
			retained.Body.String(),
		)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestStoreStateWithHeaderRejectsOversizedVaryOverwriteWithoutMutatingExistingEntry(t *testing.T) {
	setConfiguredZones(t, []appconfig.Zone{{Name: "bounded-store-state", MemorySize: "320B"}})

	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheZone: "bounded-store-state", CacheTTL: 60})
	requestHeader := http.Header{"X-Variant": {"one"}}
	smallState := base.ResponseState{
		Header: http.Header{"Vary": {"X-Variant"}, "X-Origin": {"small"}},
		Status: http.StatusOK,
		Body:   []byte("small-body"),
	}
	if err := p.storeStateWithHeader(requestHeader, "same-key", smallState, time.Minute, false); err != nil {
		t.Fatalf("storeStateWithHeader(small) error = %v", err)
	}
	oversizedState := smallState
	oversizedState.Body = []byte(strings.Repeat("x", 100))
	if err := p.storeStateWithHeader(requestHeader, "same-key", oversizedState, time.Minute, false); err != nil {
		t.Fatalf("storeStateWithHeader(oversized) error = %v", err)
	}

	storageKey := "same-key::" + varySignatureFromHeader([]string{"x-variant"}, requestHeader)
	p.lock.RLock()
	entry, entryOK := p.entries[storageKey]
	index, indexOK := p.vary["same-key"]
	p.lock.RUnlock()
	if !entryOK || entry.header.Get("X-Origin") != "small" || string(entry.body) != "small-body" {
		t.Fatalf("oversized Vary overwrite changed existing entry = %#v, found %t", entry, entryOK)
	}
	if !indexOK || len(index.signatures) != 1 ||
		index.signatures[0] != varySignatureFromHeader([]string{"x-variant"}, requestHeader) {
		t.Fatalf("Vary index after oversized overwrite = %#v, want original signature", index)
	}
}

func TestHandlerDoesNotStoreHEADMissUnderGETCacheKey(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Length", "8")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("get-body"))
		}
	}))

	head := performRequest(t, handler, http.MethodHead, "/head-first", nil)
	get := performRequest(t, handler, http.MethodGet, "/head-first", nil)
	getHit := performRequest(t, handler, http.MethodGet, "/head-first", nil)

	if head.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("HEAD cache status = %q, want MISS", head.Header().Get(cacheStatusHeader))
	}
	if get.Header().Get(cacheStatusHeader) != "MISS" || get.Body.String() != "get-body" {
		t.Fatalf("first GET = %q/%q, want MISS/get-body", get.Header().Get(cacheStatusHeader), get.Body.String())
	}
	if getHit.Header().Get(cacheStatusHeader) != "HIT" || getHit.Body.String() != "get-body" {
		t.Fatalf("second GET = %q/%q, want HIT/get-body", getHit.Header().Get(cacheStatusHeader), getHit.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerReusesGETCacheEntryForHEAD(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Length", "8")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("get-body"))
		}
	}))

	get := performRequest(t, handler, http.MethodGet, "/get-first", nil)
	head := performRequest(t, handler, http.MethodHead, "/get-first", nil)

	if get.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("GET cache status = %q, want MISS", get.Header().Get(cacheStatusHeader))
	}
	if head.Header().Get(cacheStatusHeader) != "HIT" {
		t.Fatalf("HEAD cache status = %q, want HIT", head.Header().Get(cacheStatusHeader))
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
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-test", DiskPath: root}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-purge", DiskPath: root}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-expired", DiskPath: root}})

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

func TestForgetDiskEntryTracksReplacedEntry(t *testing.T) {
	p := &Plugin{
		entries:       map[string]cacheEntry{},
		diskEntryKeys: map[string]string{},
		diskRoot:      t.TempDir(),
		lock:          &sync.RWMutex{},
	}
	key := "replace-key"
	first := cacheEntry{status: http.StatusOK, storedAt: time.Now(), expiresAt: time.Now().Add(time.Hour)}
	if err := p.persistEntry(key, first); err != nil {
		t.Fatalf("persist first entry: %v", err)
	}
	p.entries[key] = first
	second := cacheEntry{status: http.StatusCreated, storedAt: time.Now(), expiresAt: time.Now().Add(2 * time.Hour)}
	if err := p.persistEntry(key, second); err != nil {
		t.Fatalf("persist replacement entry: %v", err)
	}
	p.entries[key] = second

	path := p.entryPath(key)
	p.forgetDiskEntryLocked(path)
	if _, ok := p.entries[key]; ok {
		t.Fatal("replaced entry was not forgotten")
	}
	if _, ok := p.diskEntryKeys[path]; ok {
		t.Fatal("disk entry key index was not cleared on forget")
	}
}

func TestDiskPurgeClearsDiskEntryIndex(t *testing.T) {
	root := t.TempDir()
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-purge-idx", DiskPath: root}})

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-purge-idx", CacheTTL: 60})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("response"))
	}))
	_ = performRequest(t, handler, http.MethodGet, "/purge-idx", nil)
	if len(p.diskEntryKeys) == 0 {
		t.Fatal("disk entry index is empty after storing response")
	}
	purge := performRequest(t, handler, purgeMethod, "/purge-idx", nil)
	if purge.Code != http.StatusOK {
		t.Fatalf("PURGE status = %d, want 200", purge.Code)
	}
	if len(p.diskEntryKeys) != 0 {
		t.Fatalf("disk entry index entries after PURGE = %d, want 0", len(p.diskEntryKeys))
	}
}

func TestDiskLookupExpiredClearsDiskEntryIndex(t *testing.T) {
	root := t.TempDir()
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-expired-idx", DiskPath: root}})

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-expired-idx", CacheTTL: 60})
	req := httptest.NewRequest(http.MethodGet, "/expired-idx", nil)
	key := p.cacheKey(req)
	path := p.entryPath(key)
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
	if _, ok := p.diskEntryKeys[path]; !ok {
		t.Fatal("disk entry index missing after persist")
	}
	if _, status := p.lookup(req, key); status != "EXPIRED" {
		t.Fatalf("lookup status = %q, want EXPIRED", status)
	}
	if _, ok := p.diskEntryKeys[path]; ok {
		t.Fatal("disk entry index not cleared on expiry")
	}
}

func TestConcurrentDiskForgetKeepsIndexConsistent(t *testing.T) {
	root := t.TempDir()
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-race", DiskPath: root, DiskSize: "2K"}})

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-race", CacheTTL: 60})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
	}))

	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := range 30 {
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/race-%d-%d", worker, j), nil)
				handler.ServeHTTP(httptest.NewRecorder(), req)
			}
		}(worker)
	}
	wg.Wait()

	p.lock.RLock()
	defer p.lock.RUnlock()
	for path, key := range p.diskEntryKeys {
		if _, ok := p.entries[key]; !ok {
			t.Fatalf("index path %q -> key %q has no matching in-memory entry", path, key)
		}
		if p.entryPath(key) != path {
			t.Fatalf("index path %q -> key %q path mismatch", path, key)
		}
	}
}

func TestDiskLookupRunsPeriodicExpirySweep(t *testing.T) {
	root := t.TempDir()
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-periodic", DiskPath: root}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-background", DiskPath: root}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "memory-shared", MemorySize: "1M"}})

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

func TestConfiguredMemoryZoneBoundsVaryEntriesAndIndex(t *testing.T) {
	setConfiguredZones(t, []appconfig.Zone{{Name: "memory-vary-bounded", MemorySize: "520B"}})

	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheZone: "memory-vary-bounded", CacheTTL: 60})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "X-Variant")
		_, _ = w.Write([]byte(r.Header.Get("X-Variant") + strings.Repeat("x", 180)))
	}))

	first := performRequest(t, handler, http.MethodGet, "/bounded-vary", map[string]string{"X-Variant": "first"})
	second := performRequest(t, handler, http.MethodGet, "/bounded-vary", map[string]string{"X-Variant": "second"})
	firstAgain := performRequest(t, handler, http.MethodGet, "/bounded-vary", map[string]string{"X-Variant": "first"})
	if first.Header().Get(cacheStatusHeader) != "MISS" || second.Header().Get(cacheStatusHeader) != "MISS" ||
		firstAgain.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf(
			"cache statuses = %q/%q/%q, want MISS/MISS/MISS after oldest Vary entry eviction",
			first.Header().Get(cacheStatusHeader),
			second.Header().Get(cacheStatusHeader),
			firstAgain.Header().Get(cacheStatusHeader),
		)
	}
	if calls != 3 {
		t.Fatalf("upstream calls = %d, want 3 after the first Vary entry was evicted", calls)
	}
}

func TestMemoryZoneRefreshWithChangedDefinitionStartsNewGeneration(t *testing.T) {
	setConfiguredZones(t, []appconfig.Zone{{Name: "memory-refresh-generation", MemorySize: "1M"}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "refresh-valid", MemorySize: "1M"}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "known-zone", MemorySize: "1M"}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-only", DiskPath: root}})

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
			if err := RefreshConfiguredZones(test.zones); err == nil {
				t.Fatal("RefreshConfiguredZones() error = nil, want invalid zone registry rejection")
			}
			t.Cleanup(func() { _ = RefreshConfiguredZones(nil) })
		})
	}
}

func TestRefreshConfiguredZonesRejectsUnusedInvalidZone(t *testing.T) {
	if err := RefreshConfiguredZones([]appconfig.Zone{{Name: "unused-invalid", MemorySize: "zero"}}); err == nil {
		t.Fatal("RefreshConfiguredZones() error = nil, want invalid unused zone rejection")
	}
}

func TestPostInitReadsDiskSize(t *testing.T) {
	root := t.TempDir()
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-size", DiskPath: root, DiskSize: "2K"}})

	p := newTestPlugin(t, Config{CacheStrategy: "disk", CacheZone: "disk-size"})
	if p.diskSize != 2*1024 {
		t.Fatalf("disk size = %d, want %d", p.diskSize, 2*1024)
	}
}

func TestDiskStrategyEvictsOldestEntryWhenDiskSizeExceeded(t *testing.T) {
	root := t.TempDir()
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-evict", DiskPath: root, DiskSize: "12K"}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-vary", DiskPath: root}})

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
	if noCache.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("no-cache status = %q, want MISS", noCache.Header().Get(cacheStatusHeader))
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
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-cache-control", DiskPath: root}})

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
	setConfiguredZones(t, []appconfig.Zone{{Name: "disk-cache-cookie", DiskPath: root}})

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

func TestHeaderCacheControlDirectiveValuePreservesLastMatchAcrossFields(t *testing.T) {
	header := http.Header{}
	header.Add("Cache-Control", "max-age=60, s-maxage=30")
	header.Add("Cache-Control", "max-age=10")

	got, ok := headerCacheControlDirectiveValue(header, "s-maxage", "max-age")
	if !ok {
		t.Fatal("headerCacheControlDirectiveValue() did not find a matching directive")
	}
	if got != "10" {
		t.Fatalf("headerCacheControlDirectiveValue() = %q, want last matching value 10", got)
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

func TestHandlerNoVaryReplacesVariantIndex(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory", CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Accept-Encoding") == "gzip" {
			w.Header().Set("Vary", "Accept-Encoding")
			_, _ = w.Write([]byte("encoded-origin"))
			return
		}
		_, _ = w.Write([]byte("identity-origin"))
	}))

	encoded := performRequest(
		t,
		handler,
		http.MethodGet,
		"/vary-replacement",
		map[string]string{"Accept-Encoding": "gzip"},
	)
	if encoded.Header().Get(cacheStatusHeader) != "MISS" || encoded.Body.String() != "encoded-origin" {
		t.Fatalf(
			"encoded response = %q/%q, want MISS/encoded-origin",
			encoded.Header().Get(cacheStatusHeader),
			encoded.Body.String(),
		)
	}
	encodedHit := performRequest(
		t,
		handler,
		http.MethodGet,
		"/vary-replacement",
		map[string]string{"Accept-Encoding": "gzip"},
	)
	if encodedHit.Header().Get(cacheStatusHeader) != "HIT" || encodedHit.Body.String() != "encoded-origin" {
		t.Fatalf(
			"encoded hit = %q/%q, want HIT/encoded-origin",
			encodedHit.Header().Get(cacheStatusHeader),
			encodedHit.Body.String(),
		)
	}

	identity := performRequest(t, handler, http.MethodGet, "/vary-replacement", nil)
	if identity.Header().Get(cacheStatusHeader) != "MISS" || identity.Body.String() != "identity-origin" {
		t.Fatalf(
			"identity response = %q/%q, want MISS/identity-origin",
			identity.Header().Get(cacheStatusHeader),
			identity.Body.String(),
		)
	}

	key := p.cacheKey(httptest.NewRequest(http.MethodGet, "/vary-replacement", nil))
	if _, ok := p.vary[key]; ok {
		t.Fatal("Vary index remained after storing the no-Vary identity response")
	}
	entry, ok := p.entries[key]
	if !ok || string(entry.body) != "identity-origin" {
		t.Fatalf("identity base entry = %#v, present = %t, want identity-origin", entry, ok)
	}

	encodedAfterIdentity := performRequest(
		t,
		handler,
		http.MethodGet,
		"/vary-replacement",
		map[string]string{"Accept-Encoding": "gzip"},
	)
	if encodedAfterIdentity.Header().Get(cacheStatusHeader) != "HIT" ||
		encodedAfterIdentity.Body.String() != "identity-origin" {
		t.Fatalf(
			"encoded lookup after identity = %q/%q, want HIT/identity-origin",
			encodedAfterIdentity.Header().Get(cacheStatusHeader),
			encodedAfterIdentity.Body.String(),
		)
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

func TestHandlerReportsUnsupportedMethodsWithoutCaching(t *testing.T) {
	for _, test := range []struct {
		name       string
		strategy   string
		wantStatus string
	}{
		{name: "disk", strategy: "disk", wantStatus: "BYPASS"},
		{name: "memory", strategy: "memory", wantStatus: "MISS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{CacheStrategy: test.strategy, CacheTTL: 60})
			calls := 0

			handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = w.Write([]byte("response"))
			}))

			first := performRequest(t, handler, http.MethodPost, "/anything", nil)
			second := performRequest(t, handler, http.MethodPost, "/anything", nil)

			if first.Header().Get(cacheStatusHeader) != test.wantStatus ||
				second.Header().Get(cacheStatusHeader) != test.wantStatus {
				t.Fatalf(
					"cache statuses = %q/%q, want %q",
					first.Header().Get(cacheStatusHeader),
					second.Header().Get(cacheStatusHeader),
					test.wantStatus,
				)
			}
			if calls != 2 {
				t.Fatalf("upstream calls = %d, want 2", calls)
			}
		})
	}
}

func TestInitRegistersPurgeHTTPMethod(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	router := chi.NewRouter()
	router.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(purgeMethod, "/", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("PURGE response status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestMemoryZoneStoreSharesClonedEntriesAndReleasesLastReference(t *testing.T) {
	if store := AcquireMemoryZoneStore(""); store != nil {
		t.Fatal("AcquireMemoryZoneStore(empty) returned a store")
	}

	first := AcquireMemoryZoneStore("shared-store-contract")
	second := AcquireMemoryZoneStore("shared-store-contract")
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)

	now := time.Now().Round(time.Second)
	entry := SharedCacheEntry{
		Header:    http.Header{"X-Origin": {"upstream"}},
		Body:      []byte("cached-body"),
		Status:    http.StatusCreated,
		StoredAt:  now,
		TTL:       time.Minute,
		ExpiresAt: now.Add(time.Minute),
	}
	first.Store("cache-key", entry)
	entry.Header.Set("X-Origin", "mutated")
	entry.Body[0] = 'X'

	loaded, ok := second.Load("cache-key")
	if !ok {
		t.Fatal("Load() missed an entry stored through the shared zone")
	}
	if got := loaded.Header.Get("X-Origin"); got != "upstream" {
		t.Fatalf("loaded header = %q, want cloned upstream value", got)
	}
	if got := string(loaded.Body); got != "cached-body" {
		t.Fatalf("loaded body = %q, want cloned cached body", got)
	}
	if loaded.Status != http.StatusCreated || loaded.StoredAt != now || loaded.TTL != time.Minute ||
		loaded.ExpiresAt != now.Add(time.Minute) {
		t.Fatalf("loaded metadata = %#v, want stored metadata", loaded)
	}

	loaded.Header.Set("X-Origin", "changed-after-load")
	loaded.Body[0] = 'Y'
	reloaded, ok := first.Load("cache-key")
	if !ok || reloaded.Header.Get("X-Origin") != "upstream" || string(reloaded.Body) != "cached-body" {
		t.Fatalf("stored entry was aliased by Load(): %#v, ok=%t", reloaded, ok)
	}
	if !second.Delete("cache-key") || second.Delete("cache-key") {
		t.Fatal("Delete() did not report found then missing")
	}
	if _, ok := first.Load("cache-key"); ok {
		t.Fatal("deleted entry remained visible through another reference")
	}

	first.Close()
	first.Close()
	second.Close()
	reopened := AcquireMemoryZoneStore("shared-store-contract")
	t.Cleanup(reopened.Close)
	if _, ok := reopened.Load("cache-key"); ok {
		t.Fatal("last Close() retained the prior memory-zone generation")
	}

	var nilStore *MemoryZoneStore
	nilStore.Store("ignored", SharedCacheEntry{})
	if _, ok := nilStore.Load("ignored"); ok || nilStore.Delete("ignored") {
		t.Fatal("nil memory store reported an entry")
	}
	nilStore.Close()
}

func TestMemoryZoneStoreEnforcesConfiguredCapacity(t *testing.T) {
	setConfiguredZones(t, []appconfig.Zone{{Name: "bounded-memory-store", MemorySize: "320B"}})

	store := AcquireMemoryZoneStore("bounded-memory-store")
	t.Cleanup(store.Close)
	now := time.Now()
	entry := SharedCacheEntry{
		Header:    http.Header{"X-Origin": {"upstream"}},
		Body:      []byte(strings.Repeat("a", 180)),
		Status:    http.StatusOK,
		StoredAt:  now,
		TTL:       time.Minute,
		ExpiresAt: now.Add(time.Minute),
	}
	store.Store("oldest", entry)
	entry.Body = []byte(strings.Repeat("b", 180))
	entry.StoredAt = now.Add(time.Second)
	store.Store("newest", entry)

	if _, ok := store.Load("oldest"); ok {
		t.Fatal("oldest entry remained after configured capacity was exceeded")
	}
	if loaded, ok := store.Load("newest"); !ok || string(loaded.Body) != strings.Repeat("b", 180) {
		t.Fatalf("newest entry = %#v, found %t; want retained newest value", loaded, ok)
	}
	store.zone.lock.RLock()
	usedBeforeOverwrite := store.zone.usedBytes
	store.zone.lock.RUnlock()
	entry.Body = []byte(strings.Repeat("c", 80))
	store.Store("newest", entry)
	store.zone.lock.RLock()
	usedAfterOverwrite := store.zone.usedBytes
	store.zone.lock.RUnlock()
	if usedAfterOverwrite >= usedBeforeOverwrite {
		t.Fatalf("used bytes after smaller overwrite = %d, want less than %d", usedAfterOverwrite, usedBeforeOverwrite)
	}

	entry.Body = []byte(strings.Repeat("x", 512))
	entry.StoredAt = now.Add(2 * time.Second)
	store.Store("oversized", entry)
	if _, ok := store.Load("oversized"); ok {
		t.Fatal("single entry larger than memory_size was retained")
	}
	if loaded, ok := store.Load("newest"); !ok || string(loaded.Body) != strings.Repeat("c", 80) {
		t.Fatalf("newest entry after oversized store = %#v, found %t; want unchanged retained value", loaded, ok)
	}
}

func TestMemoryZoneStoreRejectsOversizedOverwriteWithoutMutatingExistingEntry(t *testing.T) {
	setConfiguredZones(t, []appconfig.Zone{{Name: "bounded-memory-overwrite", MemorySize: "320B"}})

	store := AcquireMemoryZoneStore("bounded-memory-overwrite")
	t.Cleanup(store.Close)
	now := time.Now()
	original := SharedCacheEntry{
		Header:    http.Header{"X-Origin": {"original"}},
		Body:      []byte("original-body"),
		Status:    http.StatusOK,
		StoredAt:  now,
		TTL:       time.Minute,
		ExpiresAt: now.Add(time.Minute),
	}
	store.Store("newest", original)

	oversized := original
	oversized.Body = []byte(strings.Repeat("x", 512))
	oversized.StoredAt = now.Add(time.Second)
	store.Store("newest", oversized)

	loaded, ok := store.Load("newest")
	if !ok || loaded.Header.Get("X-Origin") != "original" || string(loaded.Body) != "original-body" {
		t.Fatalf("oversized overwrite changed existing entry = %#v, found %t", loaded, ok)
	}
}

func TestDiskZoneStoreLifecycleRejectsCorruptAndExpiredEntries(t *testing.T) {
	root := t.TempDir()
	setConfiguredZones(t, []appconfig.Zone{{Name: "shared-disk-contract", DiskPath: root, DiskSize: "1m"}})

	store, configured, err := NewDiskZoneStore("shared-disk-contract")
	if err != nil || !configured {
		t.Fatalf("NewDiskZoneStore() = configured %t, error %v", configured, err)
	}
	now := time.Now().Round(time.Second)
	entry := SharedCacheEntry{
		Header:    http.Header{"X-Origin": {"upstream"}},
		Body:      []byte("disk-body"),
		Status:    http.StatusOK,
		StoredAt:  now,
		TTL:       time.Minute,
		ExpiresAt: now.Add(time.Minute),
	}
	if err := store.Store("cache-key", entry); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	entry.Header.Set("X-Origin", "mutated")
	entry.Body[0] = 'X'
	loaded, found, expired := store.Load("cache-key", now)
	if !found || expired || loaded.Header.Get("X-Origin") != "upstream" || string(loaded.Body) != "disk-body" {
		t.Fatalf("Load() = %#v, found %t, expired %t", loaded, found, expired)
	}
	if loaded.Status != http.StatusOK || !loaded.StoredAt.Equal(now) || loaded.TTL != time.Minute ||
		!loaded.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("loaded metadata = %#v, want stored metadata", loaded)
	}
	if !store.Delete("cache-key") || store.Delete("cache-key") {
		t.Fatal("Delete() did not report found then missing")
	}

	expiredEntry := entry
	expiredEntry.ExpiresAt = now.Add(-time.Second)
	if err := store.Store("expired", expiredEntry); err != nil {
		t.Fatalf("Store(expired) error = %v", err)
	}
	if _, found, expired := store.Load("expired", now); found || !expired {
		t.Fatalf("Load(expired) = found %t, expired %t", found, expired)
	}
	if _, err := os.Stat(store.entryPath("expired")); !os.IsNotExist(err) {
		t.Fatalf("expired entry stat error = %v, want removal", err)
	}

	corruptPath := store.entryPath("corrupt")
	if err := os.WriteFile(corruptPath, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt entry: %v", err)
	}
	if _, found, expired := store.Load("corrupt", now); found || expired {
		t.Fatalf("Load(corrupt) = found %t, expired %t", found, expired)
	}
	if _, err := os.Stat(corruptPath); !os.IsNotExist(err) {
		t.Fatalf("corrupt entry stat error = %v, want removal", err)
	}

	invalidStatusPath := store.entryPath("invalid-status")
	if err := os.WriteFile(invalidStatusPath, []byte(`{"status":99}`), 0o600); err != nil {
		t.Fatalf("write invalid-status entry: %v", err)
	}
	if _, found, expired := store.Load("invalid-status", now); found || expired {
		t.Fatalf("Load(invalid-status) = found %t, expired %t", found, expired)
	}

	cleanupEntry := entry
	cleanupEntry.ExpiresAt = now.Add(-time.Second)
	if err := store.Store("cleanup-expired", cleanupEntry); err != nil {
		t.Fatalf("Store(cleanup-expired) error = %v", err)
	}
	store.Cleanup(now)
	if _, err := os.Stat(store.entryPath("cleanup-expired")); !os.IsNotExist(err) {
		t.Fatalf("Cleanup() stat error = %v, want expired entry removed", err)
	}

	var nilStore *DiskZoneStore
	if err := nilStore.Store("ignored", SharedCacheEntry{}); err == nil {
		t.Fatal("nil DiskZoneStore.Store() error = nil")
	}
	if _, found, expired := nilStore.Load("ignored", now); found || expired || nilStore.Delete("ignored") {
		t.Fatal("nil disk store reported an entry")
	}
	nilStore.Cleanup(now)
}

func TestNewDiskZoneStorePreservesUnconfiguredFallback(t *testing.T) {
	setConfiguredZones(t, nil)

	store, configured, err := NewDiskZoneStore("undeclared")
	if err != nil || configured || store != nil {
		t.Fatalf("NewDiskZoneStore(unconfigured) = %#v, %t, %v", store, configured, err)
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
