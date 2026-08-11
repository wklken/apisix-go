package graphql_proxy_cache

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"github.com/wklken/apisix-go/pkg/plugin/graphql"
	proxy_cache "github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/resource"

	"github.com/vektah/gqlparser/v2/ast"
)

type Plugin struct {
	base.BasePlugin
	config Config

	entries map[string]cacheEntry
	vary    map[string]varyIndex
	lock    sync.RWMutex
	now     func() time.Time

	memoryStore *proxy_cache.MemoryZoneStore
	diskStore   *proxy_cache.DiskZoneStore

	cleanupInterval time.Duration
	cleanupMu       sync.Mutex
	cleanupStop     chan struct{}
	cleanupDone     chan struct{}

	maxSize   int
	routeID   string
	serviceID string

	configFingerprintValue string
}

const (
	priority = 1009
	name     = "graphql-proxy-cache"

	cacheStatusHeader = "Apisix-Cache-Status"
	cacheKeyHeader    = "APISIX-Cache-Key"
	PurgeURI          = "/apisix/plugin/graphql-proxy-cache/*"
	purgePrefix       = "/apisix/plugin/graphql-proxy-cache/"
	defaultMaxSize    = 1048576
)

var routeCaches = struct {
	sync.RWMutex
	plugins map[string]*Plugin
}{plugins: map[string]*Plugin{}}

const schema = `
{
  "type": "object",
  "properties": {
    "cache_zone": {
      "type": "string",
      "minLength": 1,
      "maxLength": 100,
      "default": "disk_cache_one"
    },
    "cache_strategy": {
      "type": "string",
      "enum": ["disk", "memory"],
      "default": "disk"
    },
    "cache_ttl": {
      "type": "integer",
      "minimum": 1,
      "default": 300
    },
    "consumer_isolation": {
      "type": "boolean",
      "default": true
    },
    "cache_set_cookie": {
      "type": "boolean",
      "default": false
    }
  }
}
`

type Config struct {
	CacheZone         string `json:"cache_zone,omitempty"`
	CacheStrategy     string `json:"cache_strategy,omitempty"`
	CacheTTL          int    `json:"cache_ttl,omitempty"`
	ConsumerIsolation *bool  `json:"consumer_isolation,omitempty"`
	CacheSetCookie    bool   `json:"cache_set_cookie,omitempty"`
}

type cacheEntry struct {
	header    http.Header
	body      []byte
	status    int
	storedAt  time.Time
	ttl       time.Duration
	expiresAt time.Time
}

type varyIndex struct {
	headers     []string
	storageKeys map[string]struct{}
}

type graphqlRequest struct {
	Query *string `json:"query"`
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	p.Stop()
	if p.config.CacheZone == "" {
		p.config.CacheZone = "disk_cache_one"
	}
	if p.config.CacheStrategy == "" {
		p.config.CacheStrategy = "disk"
	}
	if p.config.CacheTTL == 0 {
		p.config.CacheTTL = 300
	}
	if p.config.ConsumerIsolation == nil {
		value := true
		p.config.ConsumerIsolation = &value
	}
	if err := proxy_cache.ValidateCacheZoneStrategy(p.config.CacheZone, p.config.CacheStrategy); err != nil {
		return err
	}
	p.configFingerprintValue = p.buildConfigFingerprint()
	p.entries = make(map[string]cacheEntry)
	p.vary = make(map[string]varyIndex)
	if p.now == nil {
		p.now = time.Now
	}
	p.maxSize = defaultMaxSize
	if config.GlobalConfig != nil && config.GlobalConfig.GraphQL.MaxSize > 0 {
		p.maxSize = config.GlobalConfig.GraphQL.MaxSize
	}
	if p.config.CacheStrategy == "memory" && proxy_cache.CacheZoneDeclared(p.config.CacheZone) {
		p.memoryStore = proxy_cache.AcquireMemoryZoneStore(p.config.CacheZone)
	}
	if p.config.CacheStrategy == "disk" {
		store, configured, err := proxy_cache.NewDiskZoneStore(p.config.CacheZone)
		if err != nil {
			return err
		}
		if configured {
			p.diskStore = store
			p.startDiskCleanup()
		}
	}
	if p.routeID != "" {
		routeCaches.Lock()
		routeCaches.plugins[p.routeID] = p
		routeCaches.Unlock()
	}
	return nil
}

func (p *Plugin) SetResourceContext(route resource.Route, service resource.Service) {
	p.routeID = route.ID
	p.serviceID = route.ServiceID
	if p.serviceID == "" {
		p.serviceID = service.ID
	}
}

func (p *Plugin) Stop() {
	p.stopDiskCleanup()
	if p.memoryStore != nil {
		p.memoryStore.Close()
		p.memoryStore = nil
	}
	p.diskStore = nil
	if p.routeID == "" {
		return
	}
	routeCaches.Lock()
	if routeCaches.plugins[p.routeID] == p {
		delete(routeCaches.plugins, p.routeID)
	}
	routeCaches.Unlock()
}

func (p *Plugin) startDiskCleanup() {
	if p.diskStore == nil {
		return
	}
	p.cleanupMu.Lock()
	if p.cleanupStop != nil {
		p.cleanupMu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	p.cleanupStop = stop
	p.cleanupDone = done
	interval := p.cleanupPeriod()
	p.cleanupMu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case now := <-ticker.C:
				p.diskStore.Cleanup(now)
			case <-stop:
				return
			}
		}
	}()
}

func (p *Plugin) stopDiskCleanup() {
	p.cleanupMu.Lock()
	stop := p.cleanupStop
	done := p.cleanupDone
	p.cleanupStop = nil
	p.cleanupDone = nil
	p.cleanupMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

func (p *Plugin) cleanupPeriod() time.Duration {
	if p.cleanupInterval > 0 {
		return p.cleanupInterval
	}
	return time.Minute
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		body, query, ok := p.graphqlRequest(w, r)
		if !ok {
			return
		}

		isMutation, err := graphqlHasMutation(query)
		if err != nil {
			if errors.Is(err, errEmptyGraphqlQuery) {
				http.Error(w, "Invalid graphql request: empty graphql query", http.StatusBadRequest)
				return
			}
			http.Error(w, "Invalid graphql request: failed to parse graphql query", http.StatusBadRequest)
			return
		}
		if isMutation {
			w.Header().Set(cacheStatusHeader, "BYPASS")
			next.ServeHTTP(w, r)
			return
		}

		key := p.cacheKey(r, body)
		w.Header().Set(cacheKeyHeader, key)
		if entry, status := p.lookup(r, key); status == "HIT" {
			writeCachedResponse(w, entry, status, key)
			return
		} else if status == "EXPIRED" {
			p.fetchAndStore(w, r, next, key, status)
			return
		}

		p.fetchAndStore(w, r, next, key, "MISS")
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) graphqlRequest(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil, "", false
	}

	if r.Method == http.MethodGet {
		if len(r.URL.RawQuery) > p.maxSize {
			http.Error(w, "Invalid graphql request: can't get graphql request body", http.StatusBadRequest)
			return nil, "", false
		}
		if r.URL.RawQuery == "" {
			http.Error(w, "Invalid graphql request: can't get graphql request body", http.StatusBadRequest)
			return nil, "", false
		}
		query := r.URL.Query().Get("query")
		if query == "" {
			http.Error(w, "invalid graphql request, args[query] is nil", http.StatusBadRequest)
			return nil, "", false
		}
		return []byte(r.URL.RawQuery), query, true
	}

	body, err := base.ReadRequestBodyLimited(r, p.maxSize)
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		http.Error(w, "Invalid graphql request: can't get graphql request body", http.StatusBadRequest)
		return nil, "", false
	}

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var req graphqlRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid graphql request, "+err.Error(), http.StatusBadRequest)
			return nil, "", false
		}
		if req.Query == nil {
			http.Error(w, "invalid graphql request, json body[query] is nil", http.StatusBadRequest)
			return nil, "", false
		}
		return body, *req.Query, true
	}

	if strings.HasPrefix(contentType, "application/graphql") {
		return body, string(body), true
	}

	http.Error(w, "invalid graphql request, error content-type: "+contentType, http.StatusBadRequest)
	return nil, "", false
}

func (p *Plugin) fetchAndStore(w http.ResponseWriter, r *http.Request, next http.Handler, key string, status string) {
	recorder := base.GetOrCreateTransformResponseWriter(r)
	next.ServeHTTP(recorder, r)

	if recorder.StatusCode() == http.StatusOK &&
		!responseCacheControlSkipsStore(recorder.Header()) &&
		(p.cacheSetCookieEnabled() || recorder.Header().Get("Set-Cookie") == "") {
		_ = p.store(r, key, recorder)
	}
	recorder.Header().Set(cacheStatusHeader, status)
	recorder.Header().Set(cacheKeyHeader, key)
	recorder.Commit(w)
}

func responseCacheControlSkipsStore(header http.Header) bool {
	for _, value := range header.Values("Cache-Control") {
		for rawDirective := range strings.SplitSeq(value, ",") {
			directive := strings.TrimSpace(rawDirective)
			if index := strings.IndexByte(directive, '='); index >= 0 {
				directive = directive[:index]
			}
			switch strings.ToLower(strings.TrimSpace(directive)) {
			case "private", "no-store", "no-cache":
				return true
			}
		}
	}
	return false
}

func (p *Plugin) cacheSetCookieEnabled() bool {
	return p.config.CacheSetCookie && p.diskStore == nil
}

func (p *Plugin) lookup(r *http.Request, key string) (cacheEntry, string) {
	storageKey := p.storageKey(r, key)
	if p.memoryStore != nil {
		shared, ok := p.memoryStore.Load(storageKey)
		if !ok {
			return cacheEntry{}, "MISS"
		}
		entry := localCacheEntry(shared)
		if p.now().After(entry.expiresAt) {
			p.memoryStore.Delete(storageKey)
			return cacheEntry{}, "EXPIRED"
		}
		return entry, "HIT"
	}
	if p.diskStore != nil {
		shared, found, expired := p.diskStore.Load(storageKey, p.now())
		if expired {
			return cacheEntry{}, "EXPIRED"
		}
		if !found {
			return cacheEntry{}, "MISS"
		}
		return localCacheEntry(shared), "HIT"
	}
	p.lock.RLock()
	entry, ok := p.entries[storageKey]
	p.lock.RUnlock()
	if !ok {
		return cacheEntry{}, "MISS"
	}
	if p.now().After(entry.expiresAt) {
		return cacheEntry{}, "EXPIRED"
	}
	return entry, "HIT"
}

func (p *Plugin) store(r *http.Request, key string, recorder *base.BufferedResponseWriter) error {
	return p.storeState(r, key, base.ResponseState{
		Status: recorder.StatusCode(),
		Header: recorder.Header(),
		Body:   recorder.Body(),
	}, time.Duration(p.config.CacheTTL)*time.Second)
}

func (p *Plugin) storageKey(r *http.Request, key string) string {
	p.lock.RLock()
	index, ok := p.vary[key]
	p.lock.RUnlock()
	if !ok || len(index.headers) == 0 {
		return key
	}
	return key + "::" + cacheutil.VarySignature(index.headers, r)
}

func (p *Plugin) prepareStorageKey(requestHeader http.Header, key string, varyHeaders []string) (string, []string) {
	p.lock.Lock()
	defer p.lock.Unlock()

	index, hadIndex := p.vary[key]
	if len(varyHeaders) == 0 {
		if !hadIndex {
			return key, nil
		}
		staleKeys := varyStorageKeys(index)
		for _, staleKey := range staleKeys {
			delete(p.entries, staleKey)
		}
		delete(p.vary, key)
		return key, staleKeys
	}

	var staleKeys []string
	if hadIndex && !slices.Equal(index.headers, varyHeaders) {
		staleKeys = varyStorageKeys(index)
		for _, staleKey := range staleKeys {
			delete(p.entries, staleKey)
		}
		index = varyIndex{}
	}
	if index.storageKeys == nil {
		index = varyIndex{
			headers:     append([]string(nil), varyHeaders...),
			storageKeys: make(map[string]struct{}),
		}
	}
	storageKey := key + "::" + varySignatureFromHeader(varyHeaders, requestHeader)
	index.storageKeys[storageKey] = struct{}{}
	p.vary[key] = index
	delete(p.entries, key)
	return storageKey, append(staleKeys, key)
}

func (p *Plugin) cacheKey(r *http.Request, body []byte) string {
	routeID := apisixVarString(r, "$route_id")
	if routeID == "" {
		routeID = p.routeID
	}
	serviceID := apisixVarString(r, "$service_id")
	if serviceID == "" {
		serviceID = p.serviceID
	}
	parts := []string{
		p.configFingerprint(),
		r.Host,
		routeID,
		serviceID,
		"",
		string(body),
	}
	if p.config.ConsumerIsolation != nil && *p.config.ConsumerIsolation {
		parts[4] = apisixVarString(r, "$consumer_name")
		if parts[4] == "" {
			parts[4] = apisixVarString(r, "$remote_user")
		}
		if parts[4] == "" {
			parts[4] = r.Header.Get("X-Consumer-Username")
		}
	}
	sum := md5.Sum([]byte(strings.Join(parts, "\x01")))
	return hex.EncodeToString(sum[:])
}

func sharedCacheEntry(entry cacheEntry) proxy_cache.SharedCacheEntry {
	return proxy_cache.SharedCacheEntry{
		Header:    cacheutil.CloneHeader(entry.header),
		Body:      append([]byte(nil), entry.body...),
		Status:    entry.status,
		StoredAt:  entry.storedAt,
		TTL:       entry.ttl,
		ExpiresAt: entry.expiresAt,
	}
}

func localCacheEntry(entry proxy_cache.SharedCacheEntry) cacheEntry {
	return cacheEntry{
		header:    cacheutil.CloneHeader(entry.Header),
		body:      append([]byte(nil), entry.Body...),
		status:    entry.Status,
		storedAt:  entry.StoredAt,
		ttl:       entry.TTL,
		expiresAt: entry.ExpiresAt,
	}
}

func diskResponseTTL(header http.Header, fallback time.Duration, now time.Time) time.Duration {
	for _, value := range header.Values("Cache-Control") {
		for rawDirective := range strings.SplitSeq(value, ",") {
			parts := strings.SplitN(strings.TrimSpace(rawDirective), "=", 2)
			if len(parts) != 2 {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(parts[0]))
			if name != "s-maxage" && name != "max-age" {
				continue
			}
			seconds, err := strconv.Atoi(strings.Trim(strings.TrimSpace(parts[1]), `"`))
			if err == nil && seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}

	values := header.Values("Expires")
	if len(values) > 0 {
		if expires, err := http.ParseTime(values[len(values)-1]); err == nil {
			if ttl := expires.Sub(now); ttl > 0 {
				return ttl
			}
		}
	}
	return fallback
}

func (p *Plugin) configFingerprint() string {
	return p.configFingerprintValue
}

func (p *Plugin) buildConfigFingerprint() string {
	return fmt.Sprintf(
		"%s:%s:%d:%t:%t",
		p.config.CacheStrategy,
		p.config.CacheZone,
		p.config.CacheTTL,
		p.config.CacheSetCookie,
		p.config.ConsumerIsolation != nil && *p.config.ConsumerIsolation,
	)
}

func apisixVarString(r *http.Request, name string) string {
	value, _ := apisixctx.GetApisixVar(r, name).(string)
	return value
}

func PurgeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PURGE" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, purgePrefix) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, purgePrefix), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	strategy, routeID, cacheKey := parts[0], parts[1], parts[2]
	if strategy != "disk" && strategy != "memory" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	routeCaches.RLock()
	plugin := routeCaches.plugins[routeID]
	routeCaches.RUnlock()
	if plugin == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if plugin.config.CacheStrategy != strategy {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	found := plugin.purge(cacheKey)
	if strategy == "disk" && !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (p *Plugin) purge(key string) bool {
	p.lock.Lock()
	storageKeys := []string{key}
	if index, ok := p.vary[key]; ok {
		storageKeys = append(storageKeys, varyStorageKeys(index)...)
		delete(p.vary, key)
	}
	found := false
	for _, storageKey := range storageKeys {
		if _, ok := p.entries[storageKey]; ok {
			found = true
		}
		delete(p.entries, storageKey)
	}
	p.lock.Unlock()

	for _, storageKey := range storageKeys {
		if p.deleteStorageKey(storageKey) {
			found = true
		}
	}
	return found
}

func (p *Plugin) deleteStorageKey(storageKey string) bool {
	if p.memoryStore != nil {
		return p.memoryStore.Delete(storageKey)
	}
	if p.diskStore != nil {
		return p.diskStore.Delete(storageKey)
	}
	return false
}

func varyStorageKeys(index varyIndex) []string {
	keys := make([]string, 0, len(index.storageKeys))
	for key := range index.storageKeys {
		keys = append(keys, key)
	}
	return keys
}

var errEmptyGraphqlQuery = errors.New("empty graphql query")

func graphqlHasMutation(query string) (bool, error) {
	if strings.TrimSpace(query) == "" {
		return false, errEmptyGraphqlQuery
	}
	doc, err := graphql.Parse(query)
	if err != nil {
		return false, err
	}
	for _, operation := range doc.Operations {
		if operation.Operation == ast.Mutation {
			return true, nil
		}
	}
	return false, nil
}

func writeCachedResponse(w http.ResponseWriter, entry cacheEntry, cacheStatus string, cacheKey string) {
	for field, values := range entry.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	age := max(time.Since(entry.storedAt)/time.Second, 0)
	w.Header().Set("Age", strconv.FormatInt(int64(age), 10))
	w.Header().Set(cacheStatusHeader, cacheStatus)
	w.Header().Set(cacheKeyHeader, cacheKey)
	w.WriteHeader(entry.status)
	_, _ = w.Write(entry.body)
}
