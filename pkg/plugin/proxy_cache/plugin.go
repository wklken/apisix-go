package proxy_cache

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	entries map[string]cacheEntry
	vary    map[string]varyIndex
	loaded  map[string]bool
	lock    *sync.RWMutex

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

type cacheEntry struct {
	header    http.Header
	body      []byte
	status    int
	storedAt  time.Time
	ttl       time.Duration
	expiresAt time.Time
}

type memoryZone struct {
	lock        sync.RWMutex
	entries     map[string]cacheEntry
	vary        map[string]varyIndex
	loaded      map[string]bool
	refs        int
	fingerprint string
}

// SharedCacheEntry is the cache envelope exchanged by plugins that use the
// configured proxy-cache zones. The fields intentionally mirror the persisted
// disk envelope so memory and disk strategies keep the same expiry contract.
type SharedCacheEntry struct {
	Header    http.Header
	Body      []byte
	Status    int
	StoredAt  time.Time
	TTL       time.Duration
	ExpiresAt time.Time
}

// MemoryZoneStore provides a reference-counted view of a configured memory
// zone. It is used by proxy-cache and graphql-proxy-cache to share the same
// entries without exposing the plugin's internal vary index.
type MemoryZoneStore struct {
	zone *memoryZone
	name string
	once sync.Once
}

// CacheZoneDeclared reports whether a zone is present in the configured
// proxy-cache registry. An empty registry intentionally retains compatibility
// with local, process-only cache fallbacks.
func CacheZoneDeclared(name string) bool {
	return declaredCacheZone(name)
}

// ValidateCacheZone validates a plugin cache_zone against the configured
// registry when one is present.
func ValidateCacheZone(name string) error {
	return validateCacheZoneRegistry(name)
}

// ValidateConfiguredZones validates the complete static proxy-cache zone
// registry before a route replacement starts. An empty registry preserves the
// compatibility fallback used when no zones are declared.
func ValidateConfiguredZones() error {
	return validateCacheZoneRegistry("")
}

// RefreshConfiguredZones validates and atomically publishes a complete
// proxy-cache zone snapshot. An invalid snapshot leaves the last valid
// configuration untouched; existing plugin instances keep their current
// memory-zone generation until they stop.
func RefreshConfiguredZones(zones []appconfig.Zone) error {
	cloned := append([]appconfig.Zone(nil), zones...)
	if _, err := validateZoneDefinitions(cloned); err != nil {
		return err
	}

	configuredZoneRefreshMu.Lock()
	defer configuredZoneRefreshMu.Unlock()

	var next appconfig.Config
	if appconfig.GlobalConfig != nil {
		next = *appconfig.GlobalConfig
	}
	next.Apisix.ProxyCache.Zones = cloned
	appconfig.GlobalConfig = &next
	return nil
}

// ValidateCacheZoneStrategy validates a plugin cache_zone against the
// configured zone's storage strategy. A configured disk_path makes a zone
// disk-backed; a zone without one is memory-backed.
func ValidateCacheZoneStrategy(name, strategy string) error {
	zones := configuredZones()
	seen, err := validateZoneDefinitions(zones)
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return nil
	}
	if _, ok := seen[name]; !ok {
		return fmt.Errorf("proxy-cache cache_zone %q is not declared", name)
	}
	for _, zone := range zones {
		if zone.Name != name {
			continue
		}
		diskConfigured := strings.TrimSpace(zone.DiskPath) != ""
		if (strategy == "memory" && diskConfigured) || (strategy == "disk" && !diskConfigured) {
			return fmt.Errorf("invalid or empty cache_zone for cache_strategy: %s", strategy)
		}
		return nil
	}
	return nil
}

// AcquireMemoryZoneStore acquires a reference to a named shared memory zone.
// Call Close when the owning plugin instance stops.
func AcquireMemoryZoneStore(name string) *MemoryZoneStore {
	if name == "" {
		return nil
	}
	return &MemoryZoneStore{zone: acquireMemoryZone(name), name: name}
}

func (s *MemoryZoneStore) Load(key string) (SharedCacheEntry, bool) {
	if s == nil || s.zone == nil {
		return SharedCacheEntry{}, false
	}
	s.zone.lock.RLock()
	entry, ok := s.zone.entries[key]
	s.zone.lock.RUnlock()
	if !ok {
		return SharedCacheEntry{}, false
	}
	return sharedCacheEntry(entry), true
}

func (s *MemoryZoneStore) Store(key string, entry SharedCacheEntry) {
	if s == nil || s.zone == nil {
		return
	}
	s.zone.lock.Lock()
	s.zone.entries[key] = localCacheEntry(entry)
	s.zone.lock.Unlock()
}

func (s *MemoryZoneStore) Delete(key string) bool {
	if s == nil || s.zone == nil {
		return false
	}
	s.zone.lock.Lock()
	_, found := s.zone.entries[key]
	delete(s.zone.entries, key)
	s.zone.lock.Unlock()
	return found
}

func (s *MemoryZoneStore) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		releaseMemoryZoneRef(s.name, s.zone)
		s.zone = nil
	})
}

func sharedCacheEntry(entry cacheEntry) SharedCacheEntry {
	return SharedCacheEntry{
		Header:    cloneHeader(entry.header),
		Body:      append([]byte(nil), entry.body...),
		Status:    entry.status,
		StoredAt:  entry.storedAt,
		TTL:       entry.ttl,
		ExpiresAt: entry.expiresAt,
	}
}

func localCacheEntry(entry SharedCacheEntry) cacheEntry {
	return cacheEntry{
		header:    cloneHeader(entry.Header),
		body:      append([]byte(nil), entry.Body...),
		status:    entry.Status,
		storedAt:  entry.StoredAt,
		ttl:       entry.TTL,
		expiresAt: entry.ExpiresAt,
	}
}

var memoryZoneRegistry = struct {
	sync.Mutex
	zones map[string]*memoryZone
}{
	zones: make(map[string]*memoryZone),
}

var configuredZoneRefreshMu sync.RWMutex

type varyIndex struct {
	headers    []string
	signatures []string
	expiresAt  time.Time
}

type diskCacheEntry struct {
	Header    http.Header `json:"header"`
	Body      []byte      `json:"body"`
	Status    int         `json:"status"`
	StoredAt  time.Time   `json:"stored_at"`
	TTL       int64       `json:"ttl"`
	ExpiresAt time.Time   `json:"expires_at"`
}

// DiskZoneStore exposes the common versioned disk envelope to plugins that
// share a configured proxy-cache zone. It deliberately leaves cache-key and
// vary-index policy to the owning plugin.
type DiskZoneStore struct {
	root     string
	diskSize int64
}

// NewDiskZoneStore resolves and prepares a configured disk zone. configured is
// false when the process has no proxy-cache zone registry, preserving the
// compatibility in-memory fallback used by local tests and development.
func NewDiskZoneStore(name string) (*DiskZoneStore, bool, error) {
	root, diskSize, configured, err := diskZonePath(name)
	if err != nil || !configured {
		return nil, configured, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, true, fmt.Errorf("create proxy-cache disk zone %q: %w", name, err)
	}
	return &DiskZoneStore{root: root, diskSize: diskSize}, true, nil
}

func (s *DiskZoneStore) entryPath(storageKey string) string {
	digest := sha256.Sum256([]byte(storageKey))
	return filepath.Join(s.root, hex.EncodeToString(digest[:])+".entry")
}

// Load returns expired=true when an existing entry was removed because its TTL
// elapsed. This lets the caller preserve APISIX's visible EXPIRED status.
func (s *DiskZoneStore) Load(storageKey string, now time.Time) (SharedCacheEntry, bool, bool) {
	if s == nil {
		return SharedCacheEntry{}, false, false
	}
	path := s.entryPath(storageKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return SharedCacheEntry{}, false, false
	}
	var persisted diskCacheEntry
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.Status < 100 || persisted.Status > 599 {
		_ = os.Remove(path)
		return SharedCacheEntry{}, false, false
	}
	if !persisted.ExpiresAt.IsZero() && now.After(persisted.ExpiresAt) {
		_ = os.Remove(path)
		return SharedCacheEntry{}, false, true
	}
	return SharedCacheEntry{
		Header:    cloneHeader(persisted.Header),
		Body:      append([]byte(nil), persisted.Body...),
		Status:    persisted.Status,
		StoredAt:  persisted.StoredAt,
		TTL:       time.Duration(persisted.TTL),
		ExpiresAt: persisted.ExpiresAt,
	}, true, false
}

func (s *DiskZoneStore) Store(storageKey string, entry SharedCacheEntry) error {
	if s == nil {
		return fmt.Errorf("proxy-cache disk store is nil")
	}
	return writeDiskJSON(s.root, s.entryPath(storageKey), diskCacheEntry{
		Header:    cloneHeader(entry.Header),
		Body:      append([]byte(nil), entry.Body...),
		Status:    entry.Status,
		StoredAt:  entry.StoredAt,
		TTL:       int64(entry.TTL),
		ExpiresAt: entry.ExpiresAt,
	})
}

func (s *DiskZoneStore) Delete(storageKey string) bool {
	if s == nil {
		return false
	}
	path := s.entryPath(storageKey)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return os.Remove(path) == nil
}

func (s *DiskZoneStore) Cleanup(now time.Time) {
	if s == nil {
		return
	}
	// Reuse the proxy-cache cleanup rules, including malformed-file removal and
	// oldest-file eviction, without sharing a plugin's in-memory index.
	cleanup := &Plugin{
		diskRoot:    s.root,
		diskEnabled: true,
		diskSize:    s.diskSize,
		entries:     make(map[string]cacheEntry),
	}
	cleanup.cleanupDiskLocked(now)
}

type diskVaryIndex struct {
	Headers    []string  `json:"headers"`
	Signatures []string  `json:"signatures"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
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

func releaseMemoryZoneRef(name string, zone *memoryZone) {
	if zone == nil {
		return
	}
	memoryZoneRegistry.Lock()
	zone.refs--
	if zone.refs <= 0 && memoryZoneRegistry.zones[name] == zone {
		delete(memoryZoneRegistry.zones, name)
	}
	memoryZoneRegistry.Unlock()
}

func declaredCacheZone(name string) bool {
	for _, zone := range configuredZones() {
		if zone.Name == name {
			return true
		}
	}
	return false
}

func acquireMemoryZone(name string) *memoryZone {
	memoryZoneRegistry.Lock()
	defer memoryZoneRegistry.Unlock()
	fingerprint := cacheZoneFingerprint(name)
	zone := memoryZoneRegistry.zones[name]
	if zone == nil || zone.fingerprint != fingerprint {
		zone = &memoryZone{
			entries:     make(map[string]cacheEntry),
			vary:        make(map[string]varyIndex),
			loaded:      make(map[string]bool),
			fingerprint: fingerprint,
		}
		memoryZoneRegistry.zones[name] = zone
	}
	zone.refs++
	return zone
}

func cacheZoneFingerprint(name string) string {
	for _, zone := range configuredZones() {
		if zone.Name != name {
			continue
		}
		return strings.Join([]string{
			zone.Name,
			zone.MemorySize,
			zone.DiskSize,
			zone.DiskPath,
			zone.CacheLevels,
		}, "\x00")
	}
	return ""
}

func validateCacheZoneRegistry(cacheZone string) error {
	seen, err := validateZoneDefinitions(configuredZones())
	if err != nil {
		return err
	}
	if len(seen) == 0 {
		return nil
	}
	if cacheZone != "" {
		if _, ok := seen[cacheZone]; !ok {
			return fmt.Errorf("proxy-cache cache_zone %q is not declared", cacheZone)
		}
	}
	return nil
}

func configuredZones() []appconfig.Zone {
	configuredZoneRefreshMu.RLock()
	defer configuredZoneRefreshMu.RUnlock()
	if appconfig.GlobalConfig == nil {
		return nil
	}
	return append([]appconfig.Zone(nil), appconfig.GlobalConfig.Apisix.ProxyCache.Zones...)
}

func validateZoneDefinitions(zones []appconfig.Zone) (map[string]struct{}, error) {
	seen := make(map[string]struct{}, len(zones))
	for _, zone := range zones {
		if zone.Name == "" {
			return nil, fmt.Errorf("proxy-cache zone name must not be empty")
		}
		if _, ok := seen[zone.Name]; ok {
			return nil, fmt.Errorf("proxy-cache zone %q is declared more than once", zone.Name)
		}
		seen[zone.Name] = struct{}{}
		if zone.MemorySize != "" {
			if _, err := parseDiskSize(zone.MemorySize); err != nil {
				return nil, fmt.Errorf("proxy-cache zone %q memory_size: %w", zone.Name, err)
			}
		}
		if zone.DiskSize != "" {
			if _, err := parseDiskSize(zone.DiskSize); err != nil {
				return nil, fmt.Errorf("proxy-cache zone %q disk_size: %w", zone.Name, err)
			}
		}
		if zone.DiskPath != "" && !filepath.IsAbs(filepath.Clean(zone.DiskPath)) {
			return nil, fmt.Errorf("proxy-cache zone %q disk_path must be absolute", zone.Name)
		}
		if err := validateCacheLevels(zone.CacheLevels); err != nil {
			return nil, fmt.Errorf("proxy-cache zone %q cache_levels: %w", zone.Name, err)
		}
	}
	return seen, nil
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

func (p *Plugin) startDiskCleanup() {
	if !p.diskEnabled {
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
				p.lock.Lock()
				p.cleanupDiskLocked(now)
				p.lock.Unlock()
			case <-stop:
				return
			}
		}
	}()
}

func (p *Plugin) cleanupPeriod() time.Duration {
	if p.cleanupInterval > 0 {
		return p.cleanupInterval
	}
	return diskCleanupPeriod
}

func diskZonePath(name string) (string, int64, bool, error) {
	for _, zone := range configuredZones() {
		if zone.Name != name {
			continue
		}
		if zone.DiskPath == "" {
			return "", 0, false, fmt.Errorf("proxy-cache disk zone %q has no disk_path", name)
		}
		root := filepath.Clean(zone.DiskPath)
		if !filepath.IsAbs(root) {
			return "", 0, false, fmt.Errorf("proxy-cache disk zone %q disk_path must be absolute", name)
		}
		diskSize, err := parseDiskSize(zone.DiskSize)
		if err != nil {
			return "", 0, false, fmt.Errorf("proxy-cache disk zone %q: %w", name, err)
		}
		return root, diskSize, true, nil
	}
	return "", 0, false, nil
}

func parseDiskSize(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToUpper(value))
	if value == "" {
		return 0, nil
	}
	original := value

	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		value  int64
	}{
		{suffix: "TB", value: 1 << 40},
		{suffix: "T", value: 1 << 40},
		{suffix: "GB", value: 1 << 30},
		{suffix: "G", value: 1 << 30},
		{suffix: "MB", value: 1 << 20},
		{suffix: "M", value: 1 << 20},
		{suffix: "KB", value: 1 << 10},
		{suffix: "K", value: 1 << 10},
		{suffix: "B", value: 1},
	} {
		if before, ok := strings.CutSuffix(value, unit.suffix); ok {
			value = strings.TrimSpace(before)
			multiplier = unit.value
			break
		}
	}
	if value == "" {
		return 0, fmt.Errorf("disk_size must contain a positive integer")
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("disk_size %q must contain a positive integer with an optional B/K/M/G/T unit", original)
	}
	if size > int64(^uint64(0)>>1)/multiplier {
		return 0, fmt.Errorf("disk_size %q overflows int64", original)
	}
	return size * multiplier, nil
}

func (p *Plugin) entryPath(storageKey string) string {
	digest := sha256.Sum256([]byte(storageKey))
	return filepath.Join(p.diskRoot, hex.EncodeToString(digest[:])+".entry")
}

func (p *Plugin) varyIndexPath(key string) string {
	digest := sha256.Sum256([]byte(key))
	return filepath.Join(p.diskRoot, hex.EncodeToString(digest[:])+".vary")
}

func (p *Plugin) persistEntry(storageKey string, entry cacheEntry) error {
	return writeDiskJSON(p.diskRoot, p.entryPath(storageKey), diskCacheEntry{
		Header:    cloneHeader(entry.header),
		Body:      append([]byte(nil), entry.body...),
		Status:    entry.status,
		StoredAt:  entry.storedAt,
		TTL:       int64(entry.ttl),
		ExpiresAt: entry.expiresAt,
	})
}

func (p *Plugin) persistVaryIndex(key string, index varyIndex) error {
	return writeDiskJSON(p.diskRoot, p.varyIndexPath(key), diskVaryIndex{
		Headers:    append([]string(nil), index.headers...),
		Signatures: append([]string(nil), index.signatures...),
		ExpiresAt:  index.expiresAt,
	})
}

func (p *Plugin) loadVaryIndexLocked(key string) {
	if !p.diskEnabled || p.loaded[key] {
		return
	}
	p.loaded[key] = true
	data, err := os.ReadFile(p.varyIndexPath(key))
	if err != nil {
		return
	}
	var persisted diskVaryIndex
	if err := json.Unmarshal(data, &persisted); err != nil {
		_ = os.Remove(p.varyIndexPath(key))
		return
	}
	p.vary[key] = varyIndex{
		headers:    append([]string(nil), persisted.Headers...),
		signatures: append([]string(nil), persisted.Signatures...),
		expiresAt:  persisted.ExpiresAt,
	}
}

func (p *Plugin) loadEntryLocked(storageKey string) (cacheEntry, bool) {
	data, err := os.ReadFile(p.entryPath(storageKey))
	if err != nil {
		return cacheEntry{}, false
	}
	var persisted diskCacheEntry
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.Status < 100 || persisted.Status > 599 {
		_ = os.Remove(p.entryPath(storageKey))
		return cacheEntry{}, false
	}
	return cacheEntry{
		header:    cloneHeader(persisted.Header),
		body:      append([]byte(nil), persisted.Body...),
		status:    persisted.Status,
		storedAt:  persisted.StoredAt,
		ttl:       time.Duration(persisted.TTL),
		expiresAt: persisted.ExpiresAt,
	}, true
}

func (p *Plugin) removeEntryLocked(storageKey string) {
	if p.diskEnabled {
		_ = os.Remove(p.entryPath(storageKey))
	}
}

func (p *Plugin) removeVaryIndexLocked(key string) {
	if p.diskEnabled {
		_ = os.Remove(p.varyIndexPath(key))
	}
}

func writeDiskJSON(root string, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(root, ".proxy-cache-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
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
			p.fetchAndMaybeStore(w, r, next, key, status, !p.hasTruthyValue(r, p.config.NoCache))
			return
		} else if status == "STALE" {
			p.fetchAndMaybeStore(w, r, next, key, status, !p.hasTruthyValue(r, p.config.NoCache))
			return
		} else if p.onlyIfCachedMiss(r) {
			w.Header().Set(cacheStatusHeader, "MISS")
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}

		p.fetchAndMaybeStore(w, r, next, key, "MISS", !p.hasTruthyValue(r, p.config.NoCache))
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
	recorder := newResponseRecorder()
	next.ServeHTTP(recorder, r)
	if recorder.statusCode == 0 {
		recorder.statusCode = http.StatusOK
	}

	if p.hasTruthyValue(r, p.config.NoCache) {
		shouldStore = false
		cacheStatus = "EXPIRED"
	}
	if responseCacheControlSkipsStore(recorder.header) {
		shouldStore = false
	}
	cacheTTL := time.Duration(p.config.CacheTTL) * time.Second
	if shouldStore && p.cacheControlEnabled() {
		var ok bool
		cacheTTL, ok = responseCacheControlTTL(recorder.header)
		if !ok {
			shouldStore = false
		}
	}
	if shouldStore && p.cacheableStatus(recorder.statusCode) &&
		(p.cacheSetCookieEnabled() || recorder.header.Get("Set-Cookie") == "") {
		p.store(r, key, recorder, cacheTTL)
	}
	recorder.header.Set(cacheStatusHeader, cacheStatus)
	recorder.writeTo(w)
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
	return key + "::" + varySignature(index.headers, r)
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

func (p *Plugin) store(r *http.Request, key string, recorder *responseRecorder, ttl time.Duration) {
	varyHeaders, cacheable := parseVaryHeader(recorder.header)
	if !cacheable {
		return
	}

	now := time.Now()
	entry := cacheEntry{
		header:    cloneHeader(recorder.header),
		body:      append([]byte(nil), recorder.body.Bytes()...),
		status:    recorder.statusCode,
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
		signature := varySignature(varyHeaders, r)
		storageKey = key + "::" + signature
		p.updateVaryIndexLocked(key, varyHeaders, signature, entry.expiresAt)
		delete(p.entries, key)
	} else if _, ok := p.vary[key]; ok {
		p.purgeAllLocked(key)
	}
	p.entries[storageKey] = entry
	index, hasIndex := p.vary[key]
	if p.diskEnabled {
		_ = p.persistEntry(storageKey, entry)
		if hasIndex {
			_ = p.persistVaryIndex(key, index)
		}
		p.cleanupDiskLocked(now)
	}
	p.lock.Unlock()
}

type diskCacheFile struct {
	path    string
	size    int64
	modTime time.Time
	vary    bool
}

func (p *Plugin) maybeCleanupDiskLocked(now time.Time) {
	if !p.diskEnabled || (!p.lastCleanup.IsZero() && now.Before(p.lastCleanup.Add(p.cleanupPeriod()))) {
		return
	}
	p.cleanupDiskLocked(now)
}

func (p *Plugin) cleanupDiskLocked(now time.Time) {
	if !p.diskEnabled {
		return
	}
	p.lastCleanup = now

	directory, err := os.ReadDir(p.diskRoot)
	if err != nil {
		return
	}

	files := make([]diskCacheFile, 0, len(directory))
	var total int64
	for _, item := range directory {
		if item.IsDir() || (!strings.HasSuffix(item.Name(), ".entry") && !strings.HasSuffix(item.Name(), ".vary")) {
			continue
		}
		path := filepath.Join(p.diskRoot, item.Name())
		info, err := item.Info()
		if err != nil {
			continue
		}
		if strings.HasSuffix(item.Name(), ".entry") && diskEntryExpired(path, now) {
			_ = os.Remove(path)
			p.forgetDiskEntryLocked(path)
			continue
		}
		files = append(files, diskCacheFile{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
			vary:    strings.HasSuffix(item.Name(), ".vary"),
		})
		total += info.Size()
	}

	if p.diskSize <= 0 || total <= p.diskSize {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].vary != files[j].vary {
			return !files[i].vary
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	for _, file := range files {
		if total <= p.diskSize {
			break
		}
		if err := os.Remove(file.path); err != nil {
			continue
		}
		total -= file.size
		p.forgetDiskEntryLocked(file.path)
	}
}

func diskEntryExpired(path string, now time.Time) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var persisted diskCacheEntry
	if err := json.Unmarshal(data, &persisted); err != nil {
		return true
	}
	return !persisted.ExpiresAt.IsZero() && now.After(persisted.ExpiresAt)
}

func (p *Plugin) forgetDiskEntryLocked(path string) {
	for key := range p.entries {
		if p.entryPath(key) == path {
			delete(p.entries, key)
		}
	}
}

func (p *Plugin) updateVaryIndexLocked(key string, headers []string, signature string, expiresAt time.Time) {
	index, ok := p.vary[key]
	if ok && sameStringSlice(index.headers, headers) {
		found := slices.Contains(index.signatures, signature)
		if !found {
			for len(index.signatures) >= maxVaryVariants {
				evicted := index.signatures[0]
				index.signatures = index.signatures[1:]
				delete(p.entries, key+"::"+evicted)
				p.removeEntryLocked(key + "::" + evicted)
			}
			index.signatures = append(index.signatures, signature)
		}
		index.expiresAt = expiresAt
		p.vary[key] = index
		return
	}

	if ok {
		for _, existing := range index.signatures {
			delete(p.entries, key+"::"+existing)
			p.removeEntryLocked(key + "::" + existing)
		}
	}
	p.loaded[key] = true
	p.vary[key] = varyIndex{
		headers:    append([]string(nil), headers...),
		signatures: []string{signature},
		expiresAt:  expiresAt,
	}
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
			}
		}
	}
	return found, ok
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

func cloneHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	for field, values := range header {
		cloned[field] = append([]string(nil), values...)
	}
	return cloned
}

func parseVaryHeader(header http.Header) ([]string, bool) {
	values := header.Values("Vary")
	if len(values) == 0 {
		return nil, true
	}

	seen := map[string]struct{}{}
	var headers []string
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if name == "*" {
				return nil, false
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			headers = append(headers, name)
		}
	}
	sort.Strings(headers)
	return headers, true
}

func varySignature(headers []string, r *http.Request) string {
	values := make([]string, 0, len(headers))
	for _, header := range headers {
		values = append(values, r.Header.Get(header))
	}
	sum := md5.Sum([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])
}

func sameStringSlice(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
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
