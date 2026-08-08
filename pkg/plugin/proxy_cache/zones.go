package proxy_cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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
		Header:    cacheutil.CloneHeader(entry.header),
		Body:      append([]byte(nil), entry.body...),
		Status:    entry.status,
		StoredAt:  entry.storedAt,
		TTL:       entry.ttl,
		ExpiresAt: entry.expiresAt,
	}
}

func localCacheEntry(entry SharedCacheEntry) cacheEntry {
	return cacheEntry{
		header:    cacheutil.CloneHeader(entry.Header),
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
		Header:    cacheutil.CloneHeader(persisted.Header),
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
		Header:    cacheutil.CloneHeader(entry.Header),
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
