package proxy_cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
)

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
	return diskZonePathIn(configuredZones(), name)
}

func diskZonePathIn(zones []appconfig.Zone, name string) (string, int64, bool, error) {
	for _, zone := range zones {
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
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.persistEntryLocked(storageKey, entry)
}

// persistEntryLocked persists an entry to disk and records its storage key in
// the reverse path index so directory-driven cleanup can forget it without
// scanning the whole entries map. Callers must hold p.lock.
func (p *Plugin) persistEntryLocked(storageKey string, entry cacheEntry) error {
	if err := writeDiskJSON(p.diskRoot, p.entryPath(storageKey), diskCacheEntry{
		Header:    cacheutil.CloneHeader(entry.header),
		Body:      append([]byte(nil), entry.body...),
		Status:    entry.status,
		StoredAt:  entry.storedAt,
		TTL:       int64(entry.ttl),
		ExpiresAt: entry.expiresAt,
	}); err != nil {
		return err
	}
	if p.diskEntryKeys == nil {
		p.diskEntryKeys = map[string]string{}
	}
	p.diskEntryKeys[p.entryPath(storageKey)] = storageKey
	return nil
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
	path := p.entryPath(storageKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return cacheEntry{}, false
	}
	var persisted diskCacheEntry
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.Status < 100 || persisted.Status > 599 {
		_ = os.Remove(path)
		delete(p.diskEntryKeys, path)
		return cacheEntry{}, false
	}
	p.diskEntryKeys[path] = storageKey
	return cacheEntry{
		header:    cacheutil.CloneHeader(persisted.Header),
		body:      append([]byte(nil), persisted.Body...),
		status:    persisted.Status,
		storedAt:  persisted.StoredAt,
		ttl:       time.Duration(persisted.TTL),
		expiresAt: persisted.ExpiresAt,
	}, true
}

func (p *Plugin) removeEntryLocked(storageKey string) {
	path := p.entryPath(storageKey)
	delete(p.diskEntryKeys, path)
	if p.diskEnabled {
		_ = os.Remove(path)
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
	slices.SortFunc(files, func(a, b diskCacheFile) int {
		if a.vary != b.vary {
			if !a.vary {
				return -1
			}
			return 1
		}
		return a.modTime.Compare(b.modTime)
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
	key, ok := p.diskEntryKeys[path]
	if !ok {
		return
	}
	delete(p.entries, key)
	delete(p.diskEntryKeys, path)
}

func (p *Plugin) updateVaryIndexLocked(key string, headers []string, signature string, expiresAt time.Time) {
	index, ok := p.vary[key]
	if ok && slices.Equal(index.headers, headers) {
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
