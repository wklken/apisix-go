package graphql_proxy_cache

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	proxy_cache "github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/resource"
)

type Plugin struct {
	base.BasePlugin
	config Config

	entries map[string]cacheEntry
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
}

// FailRouteOnInitError marks cache configuration errors as route-build
// failures instead of allowing the builder to silently omit the cache plugin.
func (p *Plugin) FailRouteOnInitError() bool {
	return true
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

type graphqlRequest struct {
	Query string `json:"query"`
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func (p *Plugin) Config() interface{} {
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
	p.entries = make(map[string]cacheEntry)
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
		if entry, status := p.lookup(key); status == "HIT" {
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
		query := r.URL.Query().Get("query")
		if query == "" {
			http.Error(w, "invalid graphql request, args[query] is nil", http.StatusBadRequest)
			return nil, "", false
		}
		return []byte(r.URL.RawQuery), query, true
	}

	body, err := readBody(r, p.maxSize)
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
		if req.Query == "" {
			http.Error(w, "invalid graphql request, json body[query] is nil", http.StatusBadRequest)
			return nil, "", false
		}
		return body, req.Query, true
	}

	if strings.HasPrefix(contentType, "application/graphql") {
		return body, string(body), true
	}

	http.Error(w, "invalid graphql request, error content-type: "+contentType, http.StatusBadRequest)
	return nil, "", false
}

func (p *Plugin) fetchAndStore(w http.ResponseWriter, r *http.Request, next http.Handler, key string, status string) {
	recorder := newResponseRecorder()
	next.ServeHTTP(recorder, r)
	if recorder.statusCode == 0 {
		recorder.statusCode = http.StatusOK
	}

	if recorder.statusCode == http.StatusOK &&
		!responseCacheControlSkipsStore(recorder.header) &&
		(p.cacheSetCookieEnabled() || recorder.header.Get("Set-Cookie") == "") {
		p.store(key, recorder)
	}
	recorder.header.Set(cacheStatusHeader, status)
	recorder.header.Set(cacheKeyHeader, key)
	recorder.writeTo(w)
}

func responseCacheControlSkipsStore(header http.Header) bool {
	for _, value := range header.Values("Cache-Control") {
		for _, rawDirective := range strings.Split(value, ",") {
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

func (p *Plugin) lookup(key string) (cacheEntry, string) {
	if p.memoryStore != nil {
		shared, ok := p.memoryStore.Load(key)
		if !ok {
			return cacheEntry{}, "MISS"
		}
		entry := localCacheEntry(shared)
		if p.now().After(entry.expiresAt) {
			p.memoryStore.Delete(key)
			return cacheEntry{}, "EXPIRED"
		}
		return entry, "HIT"
	}
	if p.diskStore != nil {
		shared, found, expired := p.diskStore.Load(key, p.now())
		if expired {
			return cacheEntry{}, "EXPIRED"
		}
		if !found {
			return cacheEntry{}, "MISS"
		}
		return localCacheEntry(shared), "HIT"
	}
	p.lock.RLock()
	entry, ok := p.entries[key]
	p.lock.RUnlock()
	if !ok {
		return cacheEntry{}, "MISS"
	}
	if p.now().After(entry.expiresAt) {
		return cacheEntry{}, "EXPIRED"
	}
	return entry, "HIT"
}

func (p *Plugin) store(key string, recorder *responseRecorder) {
	ttl := time.Duration(p.config.CacheTTL) * time.Second
	if p.diskStore != nil {
		ttl = diskResponseTTL(recorder.header, ttl, p.now())
	}
	entry := cacheEntry{
		header:   cloneHeader(recorder.header),
		body:     append([]byte(nil), recorder.body.Bytes()...),
		status:   recorder.statusCode,
		storedAt: p.now(),
		ttl:      ttl,
	}
	entry.expiresAt = entry.storedAt.Add(entry.ttl)
	entry.header.Del(cacheStatusHeader)
	entry.header.Del(cacheKeyHeader)
	shared := sharedCacheEntry(entry)
	if p.memoryStore != nil {
		p.memoryStore.Store(key, shared)
		return
	}
	if p.diskStore != nil {
		_ = p.diskStore.Store(key, shared)
		return
	}

	p.lock.Lock()
	p.entries[key] = entry
	p.lock.Unlock()
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
		Header:    cloneHeader(entry.header),
		Body:      append([]byte(nil), entry.body...),
		Status:    entry.status,
		StoredAt:  entry.storedAt,
		TTL:       entry.ttl,
		ExpiresAt: entry.expiresAt,
	}
}

func localCacheEntry(entry proxy_cache.SharedCacheEntry) cacheEntry {
	return cacheEntry{
		header:    cloneHeader(entry.Header),
		body:      append([]byte(nil), entry.Body...),
		status:    entry.Status,
		storedAt:  entry.StoredAt,
		ttl:       entry.TTL,
		expiresAt: entry.ExpiresAt,
	}
}

func diskResponseTTL(header http.Header, fallback time.Duration, now time.Time) time.Duration {
	for _, value := range header.Values("Cache-Control") {
		for _, rawDirective := range strings.Split(value, ",") {
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

func readBody(r *http.Request, maxSize int) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxSize)+1))
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err == nil && len(body) > maxSize {
		err = fmt.Errorf("graphql request body exceeds maximum size %d", maxSize)
	}
	return body, err
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
	if p.memoryStore != nil {
		return p.memoryStore.Delete(key)
	}
	if p.diskStore != nil {
		return p.diskStore.Delete(key)
	}
	p.lock.Lock()
	_, found := p.entries[key]
	delete(p.entries, key)
	p.lock.Unlock()
	return found
}

func graphqlHasMutation(query string) (bool, error) {
	tokens, err := tokenize(query)
	if err != nil {
		return false, err
	}
	parser := graphQLParser{tokens: tokens}
	return parser.parseDocument()
}

type graphQLParser struct {
	tokens []string
	pos    int
}

func (p *graphQLParser) parseDocument() (bool, error) {
	if !p.hasNext() {
		return false, fmt.Errorf("empty graphql query")
	}

	hasMutation := false
	for p.hasNext() {
		switch p.peek() {
		case "{":
			if err := p.parseSelectionSet(); err != nil {
				return false, err
			}
		case "query", "mutation", "subscription":
			operation := p.next()
			if operation == "mutation" {
				hasMutation = true
			}
			if err := p.parseOperationDefinition(); err != nil {
				return false, err
			}
		case "fragment":
			p.next()
			if err := p.parseFragmentDefinition(); err != nil {
				return false, err
			}
		default:
			return false, fmt.Errorf("unexpected graphql token %q", p.peek())
		}
	}
	return hasMutation, nil
}

func (p *graphQLParser) parseOperationDefinition() error {
	if p.hasNext() && isGraphQLName(p.peek()) {
		p.next()
	}
	if p.consume("(") {
		if err := p.parseVariableDefinitions(); err != nil {
			return err
		}
	}
	if err := p.parseDirectives(); err != nil {
		return err
	}
	return p.parseSelectionSet()
}

func (p *graphQLParser) parseFragmentDefinition() error {
	name, err := p.requireName("fragment name")
	if err != nil {
		return err
	}
	if name == "on" || !p.consume("on") {
		return fmt.Errorf("fragment %q is missing type condition", name)
	}
	if _, err := p.requireName("fragment type condition"); err != nil {
		return err
	}
	if err := p.parseDirectives(); err != nil {
		return err
	}
	return p.parseSelectionSet()
}

func (p *graphQLParser) parseVariableDefinitions() error {
	count := 0
	for p.hasNext() && p.peek() != ")" {
		if !p.consume("$") {
			return fmt.Errorf("graphql variable definition must start with $")
		}
		if _, err := p.requireName("variable name"); err != nil {
			return err
		}
		if !p.consume(":") {
			return fmt.Errorf("graphql variable definition is missing type")
		}
		if err := p.parseTypeReference(); err != nil {
			return err
		}
		if p.consume("=") {
			if err := p.parseValue(); err != nil {
				return err
			}
		}
		if err := p.parseDirectives(); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("graphql variable definitions cannot be empty")
	}
	if !p.consume(")") {
		return fmt.Errorf("graphql variable definitions are missing closing parenthesis")
	}
	return nil
}

func (p *graphQLParser) parseSelectionSet() error {
	if !p.consume("{") {
		return fmt.Errorf("missing opening selection")
	}
	count := 0
	for p.hasNext() && p.peek() != "}" {
		if err := p.parseSelection(); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("graphql selection set cannot be empty")
	}
	if !p.consume("}") {
		return fmt.Errorf("missing closing selection")
	}
	return nil
}

func (p *graphQLParser) parseSelection() error {
	if p.consume("...") {
		if p.consume("on") {
			if _, err := p.requireName("inline fragment type condition"); err != nil {
				return err
			}
			if err := p.parseDirectives(); err != nil {
				return err
			}
			return p.parseSelectionSet()
		}
		if _, err := p.requireName("fragment spread name"); err != nil {
			return err
		}
		return p.parseDirectives()
	}

	if _, err := p.requireName("field name"); err != nil {
		return err
	}
	if p.consume(":") {
		if _, err := p.requireName("aliased field name"); err != nil {
			return err
		}
	}
	if p.consume("(") {
		if err := p.parseArguments(); err != nil {
			return err
		}
	}
	if err := p.parseDirectives(); err != nil {
		return err
	}
	if p.hasNext() && p.peek() == "{" {
		return p.parseSelectionSet()
	}
	return nil
}

func (p *graphQLParser) parseArguments() error {
	count := 0
	for p.hasNext() && p.peek() != ")" {
		if _, err := p.requireName("argument name"); err != nil {
			return err
		}
		if !p.consume(":") {
			return fmt.Errorf("graphql argument is missing colon")
		}
		if err := p.parseValue(); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("graphql argument list cannot be empty")
	}
	if !p.consume(")") {
		return fmt.Errorf("graphql argument list is missing closing parenthesis")
	}
	return nil
}

func (p *graphQLParser) parseDirectives() error {
	for p.consume("@") {
		if _, err := p.requireName("directive name"); err != nil {
			return err
		}
		if p.consume("(") {
			if err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *graphQLParser) parseTypeReference() error {
	if p.consume("[") {
		if err := p.parseTypeReference(); err != nil {
			return err
		}
		if !p.consume("]") {
			return fmt.Errorf("graphql list type is missing closing bracket")
		}
	} else if _, err := p.requireName("type name"); err != nil {
		return err
	}
	if p.consume("!") {
		if p.hasNext() && p.peek() == "!" {
			return fmt.Errorf("graphql type cannot contain repeated non-null marker")
		}
	}
	return nil
}

func (p *graphQLParser) parseValue() error {
	if !p.hasNext() {
		return fmt.Errorf("graphql value is missing")
	}
	switch p.peek() {
	case "$":
		p.next()
		_, err := p.requireName("variable name")
		return err
	case "[":
		p.next()
		for p.hasNext() && p.peek() != "]" {
			if err := p.parseValue(); err != nil {
				return err
			}
		}
		if !p.consume("]") {
			return fmt.Errorf("graphql list value is missing closing bracket")
		}
		return nil
	case "{":
		p.next()
		for p.hasNext() && p.peek() != "}" {
			if _, err := p.requireName("object field name"); err != nil {
				return err
			}
			if !p.consume(":") {
				return fmt.Errorf("graphql object field is missing colon")
			}
			if err := p.parseValue(); err != nil {
				return err
			}
		}
		if !p.consume("}") {
			return fmt.Errorf("graphql object value is missing closing brace")
		}
		return nil
	default:
		token := p.next()
		if strings.HasPrefix(token, "\"") || isGraphQLName(token) || isGraphQLNumber(token) {
			return nil
		}
		return fmt.Errorf("unexpected graphql value token %q", token)
	}
}

func (p *graphQLParser) requireName(description string) (string, error) {
	if !p.hasNext() || !isGraphQLName(p.peek()) {
		if p.hasNext() {
			return "", fmt.Errorf("graphql %s is invalid near %q", description, p.peek())
		}
		return "", fmt.Errorf("graphql %s is missing", description)
	}
	return p.next(), nil
}

func (p *graphQLParser) consume(token string) bool {
	if !p.hasNext() || p.peek() != token {
		return false
	}
	p.next()
	return true
}

func (p *graphQLParser) peek() string {
	return p.tokens[p.pos]
}

func (p *graphQLParser) next() string {
	token := p.tokens[p.pos]
	p.pos++
	return token
}

func (p *graphQLParser) hasNext() bool {
	return p.pos < len(p.tokens)
}

func tokenize(query string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(query); {
		if strings.HasPrefix(query[i:], "\xEF\xBB\xBF") {
			i += len("\xEF\xBB\xBF")
			continue
		}
		switch ch := query[i]; {
		case ch == '#':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case ch == '"':
			token, next, err := readGraphQLString(query, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token)
			i = next
		case strings.HasPrefix(query[i:], "..."):
			tokens = append(tokens, "...")
			i += 3
		case strings.ContainsRune("!$():=@[]{|}&", rune(ch)):
			tokens = append(tokens, string(ch))
			i++
		case isGraphQLNameStart(ch):
			start := i
			for i < len(query) && isGraphQLNameContinue(query[i]) {
				i++
			}
			tokens = append(tokens, query[start:i])
		case ch == '-' || ch >= '0' && ch <= '9':
			start := i
			next, err := readGraphQLNumber(query, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, query[start:next])
			i = next
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		default:
			return nil, fmt.Errorf("invalid graphql character %q", ch)
		}
	}
	return tokens, nil
}

func readGraphQLString(query string, start int) (string, int, error) {
	if strings.HasPrefix(query[start:], "\"\"\"") {
		for i := start + 3; i < len(query); i++ {
			if query[i] == '\\' && i+1 < len(query) {
				i++
				continue
			}
			if strings.HasPrefix(query[i:], "\"\"\"") {
				end := i + 3
				return query[start:end], end, nil
			}
		}
		return "", 0, fmt.Errorf("unterminated graphql block string")
	}

	for i := start + 1; i < len(query); i++ {
		switch query[i] {
		case '\\':
			if i+1 >= len(query) {
				return "", 0, fmt.Errorf("unterminated graphql string escape")
			}
			i++
		case '"':
			end := i + 1
			return query[start:end], end, nil
		case '\n', '\r':
			return "", 0, fmt.Errorf("graphql string cannot contain an unescaped newline")
		}
	}
	return "", 0, fmt.Errorf("unterminated graphql string")
}

func readGraphQLNumber(query string, start int) (int, error) {
	i := start
	if query[i] == '-' {
		i++
	}
	if i >= len(query) || query[i] < '0' || query[i] > '9' {
		return 0, fmt.Errorf("invalid graphql number")
	}
	if query[i] == '0' {
		i++
		if i < len(query) && query[i] >= '0' && query[i] <= '9' {
			return 0, fmt.Errorf("graphql number cannot have a leading zero")
		}
	} else {
		for i < len(query) && query[i] >= '0' && query[i] <= '9' {
			i++
		}
	}
	if i < len(query) && query[i] == '.' {
		i++
		fractionStart := i
		for i < len(query) && query[i] >= '0' && query[i] <= '9' {
			i++
		}
		if i == fractionStart {
			return 0, fmt.Errorf("graphql number is missing fractional digits")
		}
	}
	if i < len(query) && (query[i] == 'e' || query[i] == 'E') {
		i++
		if i < len(query) && (query[i] == '+' || query[i] == '-') {
			i++
		}
		exponentStart := i
		for i < len(query) && query[i] >= '0' && query[i] <= '9' {
			i++
		}
		if i == exponentStart {
			return 0, fmt.Errorf("graphql number is missing exponent digits")
		}
	}
	if i < len(query) && (isGraphQLNameStart(query[i]) || query[i] == '.') {
		return 0, fmt.Errorf("invalid graphql number near %q", query[start:i+1])
	}
	return i, nil
}

func isGraphQLName(token string) bool {
	if token == "" || !isGraphQLNameStart(token[0]) {
		return false
	}
	for i := 1; i < len(token); i++ {
		if !isGraphQLNameContinue(token[i]) {
			return false
		}
	}
	return true
}

func isGraphQLNameStart(ch byte) bool {
	return ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
}

func isGraphQLNameContinue(ch byte) bool {
	return isGraphQLNameStart(ch) || ch >= '0' && ch <= '9'
}

func isGraphQLNumber(token string) bool {
	if token == "" || (token[0] != '-' && (token[0] < '0' || token[0] > '9')) {
		return false
	}
	return true
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     http.Header{},
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = w.Write(r.body.Bytes())
}

func writeCachedResponse(w http.ResponseWriter, entry cacheEntry, cacheStatus string, cacheKey string) {
	for field, values := range entry.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	age := time.Since(entry.storedAt) / time.Second
	if age < 0 {
		age = 0
	}
	w.Header().Set("Age", fmt.Sprintf("%d", age))
	w.Header().Set(cacheStatusHeader, cacheStatus)
	w.Header().Set(cacheKeyHeader, cacheKey)
	w.WriteHeader(entry.status)
	_, _ = w.Write(entry.body)
}

func cloneHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	for field, values := range header {
		cloned[field] = append([]string(nil), values...)
	}
	return cloned
}
