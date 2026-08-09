package proxy_cache

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
)

type Plugin struct {
	base.BasePlugin
	config Config

	entries map[string]cacheEntry
	vary    map[string]varyIndex
	loaded  map[string]bool
	lock    *sync.RWMutex

	// diskEntryKeys maps a disk entry file path back to its storage key so
	// directory-driven cleanup can forget the matching in-memory entry without
	// scanning the whole entries map.
	diskEntryKeys map[string]string

	memoryZone *memoryZone

	diskRoot    string
	diskEnabled bool
	diskSize    int64
	lastCleanup time.Time

	cleanupInterval time.Duration
	cleanupMu       sync.Mutex
	cleanupStop     chan struct{}
	cleanupDone     chan struct{}
}

const (
	priority          = 1085
	name              = "proxy-cache"
	cacheStatusHeader = "Apisix-Cache-Status"
	purgeMethod       = "PURGE"
	maxVaryVariants   = 64
	diskCleanupPeriod = time.Minute
)

var registerPurgeMethodOnce sync.Once

var identityVars = map[string]struct{}{
	"$consumer_name":      {},
	"$consumer_group_id":  {},
	"$remote_user":        {},
	"$http_authorization": {},
}

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
    "cache_key": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "pattern": "(^[^$].+$|^[$][0-9a-zA-Z_]+$)"
      },
      "default": ["$host", "$request_uri"]
    },
    "cache_bypass": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "pattern": "(^[^$].+$|^[$][0-9a-zA-Z_]+$)"
      }
    },
    "cache_method": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "enum": ["GET", "POST", "HEAD"]
      },
      "uniqueItems": true,
      "default": ["GET", "HEAD"]
    },
    "cache_http_status": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "integer",
        "minimum": 200,
        "maximum": 599
      },
      "uniqueItems": true,
      "default": [200, 301, 404]
    },
    "hide_cache_headers": {
      "type": "boolean",
      "default": false
    },
    "cache_control": {
      "type": "boolean",
      "default": false
    },
    "no_cache": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "pattern": "(^[^$].+$|^[$][0-9a-zA-Z_]+$)"
      }
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
	CacheZone            string   `json:"cache_zone,omitempty"`
	CacheStrategy        string   `json:"cache_strategy,omitempty"`
	CacheKey             []string `json:"cache_key,omitempty"`
	CacheBypass          []string `json:"cache_bypass,omitempty"`
	CacheMethod          []string `json:"cache_method,omitempty"`
	CacheHTTPStatus      []int    `json:"cache_http_status,omitempty"`
	HideCacheHeaders     bool     `json:"hide_cache_headers,omitempty"`
	CacheControl         bool     `json:"cache_control,omitempty"`
	NoCache              []string `json:"no_cache,omitempty"`
	CacheTTL             int      `json:"cache_ttl,omitempty"`
	ConsumerIsolation    bool     `json:"consumer_isolation,omitempty"`
	CacheSetCookie       bool     `json:"cache_set_cookie,omitempty"`
	consumerIsolationSet bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type configJSON struct {
		CacheZone         string   `json:"cache_zone,omitempty"`
		CacheStrategy     string   `json:"cache_strategy,omitempty"`
		CacheKey          []string `json:"cache_key,omitempty"`
		CacheBypass       []string `json:"cache_bypass,omitempty"`
		CacheMethod       []string `json:"cache_method,omitempty"`
		CacheHTTPStatus   []int    `json:"cache_http_status,omitempty"`
		HideCacheHeaders  bool     `json:"hide_cache_headers,omitempty"`
		CacheControl      bool     `json:"cache_control,omitempty"`
		NoCache           []string `json:"no_cache,omitempty"`
		CacheTTL          int      `json:"cache_ttl,omitempty"`
		ConsumerIsolation *bool    `json:"consumer_isolation,omitempty"`
		CacheSetCookie    bool     `json:"cache_set_cookie,omitempty"`
	}

	var decoded configJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	c.CacheZone = decoded.CacheZone
	c.CacheStrategy = decoded.CacheStrategy
	c.CacheKey = decoded.CacheKey
	c.CacheBypass = decoded.CacheBypass
	c.CacheMethod = decoded.CacheMethod
	c.CacheHTTPStatus = decoded.CacheHTTPStatus
	c.HideCacheHeaders = decoded.HideCacheHeaders
	c.CacheControl = decoded.CacheControl
	c.NoCache = decoded.NoCache
	c.CacheTTL = decoded.CacheTTL
	if decoded.ConsumerIsolation != nil {
		c.ConsumerIsolation = *decoded.ConsumerIsolation
		c.consumerIsolationSet = true
	}
	c.CacheSetCookie = decoded.CacheSetCookie
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	registerPurgeMethodOnce.Do(func() {
		chi.RegisterMethod(purgeMethod)
	})
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	p.Stop()
	if p.config.CacheStrategy == "" {
		p.config.CacheStrategy = "disk"
	}
	if p.config.CacheZone == "" {
		p.config.CacheZone = "disk_cache_one"
	}
	if len(p.config.CacheKey) == 0 {
		p.config.CacheKey = []string{"$host", "$request_uri"}
	}
	if len(p.config.CacheMethod) == 0 {
		p.config.CacheMethod = []string{http.MethodGet, http.MethodHead}
	}
	if len(p.config.CacheHTTPStatus) == 0 {
		p.config.CacheHTTPStatus = []int{http.StatusOK, http.StatusMovedPermanently, http.StatusNotFound}
	}
	if p.config.CacheTTL == 0 {
		p.config.CacheTTL = 300
	}
	if !p.config.consumerIsolationSet {
		p.config.ConsumerIsolation = true
	}
	if err := validateCacheMethods(p.config.CacheMethod); err != nil {
		return err
	}
	if err := validateCacheStatuses(p.config.CacheHTTPStatus); err != nil {
		return err
	}
	if err := ValidateCacheZoneStrategy(p.config.CacheZone, p.config.CacheStrategy); err != nil {
		return err
	}
	if err := validateCacheKey(p.config.CacheKey); err != nil {
		return err
	}
	if err := validateCacheVariables("cache_bypass", p.config.CacheBypass); err != nil {
		return err
	}
	if err := validateCacheVariables("no_cache", p.config.NoCache); err != nil {
		return err
	}
	p.entries = map[string]cacheEntry{}
	p.vary = map[string]varyIndex{}
	p.loaded = map[string]bool{}
	p.diskEntryKeys = map[string]string{}
	p.lock = &sync.RWMutex{}
	p.diskRoot = ""
	p.diskEnabled = false
	p.diskSize = 0
	p.lastCleanup = time.Time{}
	if p.config.CacheStrategy == "memory" && declaredCacheZone(p.config.CacheZone) {
		p.memoryZone = acquireMemoryZone(p.config.CacheZone)
		p.lock = &p.memoryZone.lock
		p.entries = p.memoryZone.entries
		p.vary = p.memoryZone.vary
		p.loaded = p.memoryZone.loaded
	}
	if p.config.CacheStrategy == "disk" {
		root, diskSize, configured, err := diskZonePath(p.config.CacheZone)
		if err != nil {
			return err
		}
		if configured {
			if err := os.MkdirAll(root, 0o700); err != nil {
				return fmt.Errorf("create proxy-cache disk zone %q: %w", p.config.CacheZone, err)
			}
			p.diskRoot = root
			p.diskEnabled = true
			p.diskSize = diskSize
			p.startDiskCleanup()
		}
	}
	return nil
}

func (p *Plugin) Stop() {
	p.cleanupMu.Lock()
	stop := p.cleanupStop
	done := p.cleanupDone
	p.cleanupStop = nil
	p.cleanupDone = nil
	p.cleanupMu.Unlock()

	if stop == nil {
		p.releaseMemoryZone()
		return
	}
	close(stop)
	<-done
	p.releaseMemoryZone()
}

func (p *Plugin) releaseMemoryZone() {
	zone := p.memoryZone
	p.memoryZone = nil
	releaseMemoryZoneRef(p.config.CacheZone, zone)
}

func validateCacheKey(keys []string) error {
	for _, key := range keys {
		if key == "" {
			return fmt.Errorf("cache_key entries must not be empty")
		}
		if key == "$request_method" {
			return fmt.Errorf("cache_key variable %q unsupported", key)
		}
		if strings.HasPrefix(key, "$") && !validCacheVariableName(key[1:]) {
			return fmt.Errorf("cache_key variable %q has invalid name", key)
		}
	}
	return nil
}

func validateCacheVariables(field string, values []string) error {
	for _, value := range values {
		if value == "" {
			return fmt.Errorf("%s entries must not be empty", field)
		}
		if strings.HasPrefix(value, "$") && !validCacheVariableName(value[1:]) {
			return fmt.Errorf("%s variable %q has invalid name", field, value)
		}
	}
	return nil
}

func validateCacheMethods(methods []string) error {
	seen := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodHead:
		default:
			return fmt.Errorf("cache_method contains unsupported method %q", method)
		}
		if _, ok := seen[method]; ok {
			return fmt.Errorf("cache_method contains duplicate method %q", method)
		}
		seen[method] = struct{}{}
	}
	return nil
}

func validateCacheStatuses(statuses []int) error {
	seen := make(map[int]struct{}, len(statuses))
	for _, status := range statuses {
		if status < 200 || status > 599 {
			return fmt.Errorf("cache_http_status %d is outside 200..599", status)
		}
		if _, ok := seen[status]; ok {
			return fmt.Errorf("cache_http_status contains duplicate status %d", status)
		}
		seen[status] = struct{}{}
	}
	return nil
}

func validCacheVariableName(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') &&
			(char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func validateCacheLevels(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	levels := strings.Split(value, ":")
	if len(levels) > 3 {
		return fmt.Errorf("must contain at most three levels")
	}
	for _, level := range levels {
		if level != "1" && level != "2" {
			return fmt.Errorf("each level must be 1 or 2")
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == purgeMethod {
			if p.purgeAll(p.cacheKey(r)) {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if !p.cacheableMethod(r.Method) {
			if p.config.CacheStrategy == "memory" {
				w.Header().Set(cacheStatusHeader, "MISS")
			} else {
				w.Header().Set(cacheStatusHeader, "BYPASS")
			}
			next.ServeHTTP(w, r)
			return
		}

		key := p.cacheKey(r)
		if p.hasTruthyValue(r, p.config.CacheBypass) {
			p.fetchAndMaybeStore(w, r, next, key, "BYPASS", false)
			return
		}
		if p.requestCacheControlBypass(r) {
			p.fetchAndMaybeStore(w, r, next, key, "BYPASS", false)
			return
		}

		if entry, status := p.lookup(r, key); status == "HIT" {
			writeCachedResponse(w, entry, status)
			return
		} else if status == "EXPIRED" {
			shouldStore := r.Method != http.MethodHead && !p.hasTruthyValue(r, p.config.NoCache)
			p.fetchAndMaybeStore(w, r, next, key, status, shouldStore)
			return
		} else if status == "STALE" {
			shouldStore := r.Method != http.MethodHead && !p.hasTruthyValue(r, p.config.NoCache)
			p.fetchAndMaybeStore(w, r, next, key, status, shouldStore)
			return
		} else if p.onlyIfCachedMiss(r) {
			w.Header().Set(cacheStatusHeader, "MISS")
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}

		shouldStore := r.Method != http.MethodHead && !p.hasTruthyValue(r, p.config.NoCache)
		p.fetchAndMaybeStore(w, r, next, key, "MISS", shouldStore)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) fetchAndMaybeStore(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
	key string,
	cacheStatus string,
	shouldStore bool,
) {
	recorder := base.GetOrCreateTransformResponseWriter(r)
	next.ServeHTTP(recorder, r)

	if p.hasTruthyValue(r, p.config.NoCache) {
		shouldStore = false
	}
	if responseCacheControlSkipsStore(recorder.Header()) {
		shouldStore = false
	}
	cacheTTL := time.Duration(p.config.CacheTTL) * time.Second
	if shouldStore && p.cacheControlEnabled() {
		var ok bool
		cacheTTL, ok = responseCacheControlTTL(recorder.Header())
		if !ok {
			shouldStore = false
		}
	}
	if shouldStore && p.cacheableStatus(recorder.StatusCode()) &&
		(p.cacheSetCookieEnabled() || recorder.Header().Get("Set-Cookie") == "") {
		p.store(r, key, recorder, cacheTTL)
	}
	recorder.Header().Set(cacheStatusHeader, cacheStatus)
	recorder.Commit(w)
}

func (p *Plugin) lookup(r *http.Request, key string) (cacheEntry, string) {
	now := time.Now()
	p.lock.Lock()
	if p.diskEnabled {
		p.loadVaryIndexLocked(key)
	}
	storageKey := p.storageKeyLocked(r, key)
	entry, ok := p.entries[storageKey]
	if !ok && p.diskEnabled {
		if loaded, found := p.loadEntryLocked(storageKey); found {
			entry = loaded
			p.entries[storageKey] = loaded
			ok = true
		}
	}
	if p.diskEnabled {
		p.maybeCleanupDiskLocked(now)
	}
	if !ok {
		p.lock.Unlock()
		return cacheEntry{}, "MISS"
	}
	if now.After(entry.expiresAt) {
		delete(p.entries, storageKey)
		p.removeEntryLocked(storageKey)
		p.lock.Unlock()
		return cacheEntry{}, "EXPIRED"
	}
	if p.requestCacheControlStale(r, entry) {
		p.lock.Unlock()
		return cacheEntry{}, "STALE"
	}
	p.lock.Unlock()
	return entry, "HIT"
}

func (p *Plugin) storageKeyLocked(r *http.Request, key string) string {
	index, ok := p.vary[key]
	if !ok || time.Now().After(index.expiresAt) || len(index.headers) == 0 {
		return key
	}
	return key + "::" + cacheutil.VarySignature(index.headers, r)
}

func (p *Plugin) purgeAll(key string) bool {
	p.lock.Lock()
	if p.diskEnabled {
		p.loadVaryIndexLocked(key)
	}
	ok := p.purgeAllLocked(key)
	p.lock.Unlock()
	return ok
}

func (p *Plugin) purgeAllLocked(key string) bool {
	_, baseOK := p.entries[key]
	index, indexOK := p.vary[key]
	for _, signature := range index.signatures {
		delete(p.entries, key+"::"+signature)
		p.removeEntryLocked(key + "::" + signature)
	}
	delete(p.vary, key)
	delete(p.loaded, key)
	p.removeVaryIndexLocked(key)
	delete(p.entries, key)
	p.removeEntryLocked(key)
	return baseOK || indexOK
}

func (p *Plugin) store(r *http.Request, key string, recorder *base.BufferedResponseWriter, ttl time.Duration) {
	varyHeaders, cacheable := cacheutil.ParseVaryHeader(recorder.Header())
	if !cacheable {
		return
	}

	now := time.Now()
	entry := cacheEntry{
		header:    cacheutil.CloneHeader(recorder.Header()),
		body:      append([]byte(nil), recorder.Body()...),
		status:    recorder.StatusCode(),
		storedAt:  now,
		ttl:       ttl,
		expiresAt: now.Add(ttl),
	}
	entry.header.Del(cacheStatusHeader)
	if p.config.HideCacheHeaders {
		entry.header.Del("Expires")
		entry.header.Del("Cache-Control")
	}

	storageKey := key
	p.lock.Lock()
	if p.diskEnabled {
		p.loadVaryIndexLocked(key)
	}
	if len(varyHeaders) > 0 {
		signature := cacheutil.VarySignature(varyHeaders, r)
		storageKey = key + "::" + signature
		p.updateVaryIndexLocked(key, varyHeaders, signature, entry.expiresAt)
		delete(p.entries, key)
	} else if _, ok := p.vary[key]; ok {
		p.purgeAllLocked(key)
	}
	p.entries[storageKey] = entry
	index, hasIndex := p.vary[key]
	if p.diskEnabled {
		_ = p.persistEntryLocked(storageKey, entry)
		if hasIndex {
			_ = p.persistVaryIndex(key, index)
		}
		p.cleanupDiskLocked(now)
	}
	p.lock.Unlock()
}

func (p *Plugin) cacheKey(r *http.Request) string {
	var b strings.Builder
	for _, part := range p.config.CacheKey {
		if after, ok := strings.CutPrefix(part, "$"); ok {
			b.WriteString(requestVar(r, after))
			continue
		}
		b.WriteString(part)
	}
	key := b.String()
	if p.config.ConsumerIsolation && !cacheKeyHasIdentity(p.config.CacheKey) {
		if identity := consumerIdentity(r); identity != "" {
			return identity + "\x01" + key
		}
	}
	return key
}

func (p *Plugin) cacheableMethod(method string) bool {
	return slices.Contains(p.config.CacheMethod, method)
}

func (p *Plugin) cacheableStatus(status int) bool {
	return slices.Contains(p.config.CacheHTTPStatus, status)
}

func (p *Plugin) hasTruthyValue(r *http.Request, values []string) bool {
	for _, value := range values {
		resolved := value
		if after, ok := strings.CutPrefix(value, "$"); ok {
			resolved = requestVar(r, after)
		}
		if resolved != "" && resolved != "0" {
			return true
		}
	}
	return false
}

func (p *Plugin) cacheControlEnabled() bool {
	return p.config.CacheControl && !p.diskEnabled && !cacheKeyHasIdentity(p.config.CacheKey)
}

func (p *Plugin) cacheSetCookieEnabled() bool {
	return p.config.CacheSetCookie && !p.diskEnabled
}

func (p *Plugin) requestCacheControlBypass(r *http.Request) bool {
	return p.cacheControlEnabled() && headerHasCacheControlDirective(r.Header, "no-cache", "no-store")
}

func (p *Plugin) onlyIfCachedMiss(r *http.Request) bool {
	return p.cacheControlEnabled() && headerHasCacheControlDirective(r.Header, "only-if-cached")
}

func (p *Plugin) requestCacheControlStale(r *http.Request, entry cacheEntry) bool {
	if !p.cacheControlEnabled() {
		return false
	}
	age := time.Since(entry.storedAt)
	if value, ok := headerCacheControlDirectiveValue(r.Header, "max-age"); ok {
		seconds, err := strconv.Atoi(value)
		if err == nil && age > time.Duration(seconds)*time.Second {
			return true
		}
	}
	if value, ok := headerCacheControlDirectiveValue(r.Header, "max-stale"); ok {
		seconds, err := strconv.Atoi(value)
		if err == nil && age-entry.ttl > time.Duration(seconds)*time.Second {
			return true
		}
	}
	if value, ok := headerCacheControlDirectiveValue(r.Header, "min-fresh"); ok {
		seconds, err := strconv.Atoi(value)
		if err == nil && entry.ttl-age < time.Duration(seconds)*time.Second {
			return true
		}
	}
	return false
}

func responseCacheControlSkipsStore(header http.Header) bool {
	return headerHasCacheControlDirective(header, "private", "no-store", "no-cache")
}

func responseCacheControlTTL(header http.Header) (time.Duration, bool) {
	if value, ok := headerCacheControlDirectiveValue(header, "s-maxage", "max-age"); ok {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds <= 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}

	values := header.Values("Expires")
	if len(values) == 0 {
		return 0, false
	}
	expires, err := http.ParseTime(values[len(values)-1])
	if err != nil {
		return 0, false
	}
	ttl := time.Until(expires)
	return ttl, ttl > 0
}

func headerHasCacheControlDirective(header http.Header, names ...string) bool {
	for _, value := range header.Values("Cache-Control") {
		if cacheControlValueHasDirective(value, names...) {
			return true
		}
	}
	return false
}

func headerCacheControlDirectiveValue(header http.Header, names ...string) (string, bool) {
	var found string
	ok := false
	for _, value := range header.Values("Cache-Control") {
		if directiveValue, foundInValue := cacheControlValueDirective(value, names...); foundInValue {
			found = directiveValue
			ok = true
		}
	}
	return found, ok
}

func cacheControlValueHasDirective(value string, names ...string) bool {
	_, ok := cacheControlValueDirective(value, names...)
	return ok
}

func cacheControlValueDirective(value string, names ...string) (string, bool) {
	var found string
	ok := false
	for part := range strings.SplitSeq(value, ",") {
		directive := strings.ToLower(strings.TrimSpace(part))
		if directive == "" {
			continue
		}
		directiveValue := ""
		if index := strings.IndexByte(directive, '='); index >= 0 {
			directiveValue = strings.Trim(strings.TrimSpace(directive[index+1:]), `"`)
			directive = strings.TrimSpace(directive[:index])
		}
		for _, name := range names {
			if directive == name {
				found = directiveValue
				ok = true
				break
			}
		}
	}
	return found, ok
}

func writeCachedResponse(w http.ResponseWriter, entry cacheEntry, cacheStatus string) {
	for field, values := range entry.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	age := max(time.Since(entry.storedAt)/time.Second, 0)
	w.Header().Set("Age", strconv.FormatInt(int64(age), 10))
	w.Header().Set(cacheStatusHeader, cacheStatus)
	w.WriteHeader(entry.status)
	_, _ = w.Write(entry.body)
}

func cacheKeyHasIdentity(cacheKey []string) bool {
	for _, part := range cacheKey {
		if _, ok := identityVars[part]; ok {
			return true
		}
	}
	return false
}

func consumerIdentity(r *http.Request) string {
	if consumerName := requestVar(r, "consumer_name"); consumerName != "" {
		return consumerName
	}
	return requestVar(r, "remote_user")
}

func requestVar(r *http.Request, name string) string {
	switch {
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "host":
		return r.Host
	case name == "request_method":
		return r.Method
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		return base.RemoteIP(r.RemoteAddr)
	case name == "consumer_name":
		if value, ok := apisixctx.GetApisixVar(r, "$consumer_name").(string); ok {
			return value
		}
		return ""
	case name == "consumer_group_id":
		if value, ok := apisixctx.GetApisixVar(r, "$consumer_group_id").(string); ok {
			return value
		}
		return ""
	case name == "remote_user":
		user, _, ok := r.BasicAuth()
		if ok {
			return user
		}
		return ""
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
}
