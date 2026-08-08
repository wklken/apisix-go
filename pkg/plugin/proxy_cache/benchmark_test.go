package proxy_cache

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// BenchmarkForgetDiskEntry measures directory-driven disk cleanup forgetting
// the in-memory entry for a disk file. The reverse disk-path index must keep
// the forget independent of the total entry count and allocation-free.
func BenchmarkForgetDiskEntry(b *testing.B) {
	for _, n := range []int{1, 100, 10000} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			benchmarkForgetDiskEntry(b, n)
		})
	}
}

func benchmarkForgetDiskEntry(b *testing.B, n int) {
	b.ReportAllocs()
	p := &Plugin{
		entries:  make(map[string]cacheEntry, n),
		diskRoot: b.TempDir(),
		lock:     &sync.RWMutex{},
	}
	var targetKey string
	for i := range n {
		key := fmt.Sprintf("key-%d", i)
		entry := cacheEntry{
			header:    make(http.Header),
			status:    http.StatusOK,
			storedAt:  time.Now(),
			expiresAt: time.Now().Add(time.Hour),
		}
		p.entries[key] = entry
		if err := p.persistEntry(key, entry); err != nil {
			b.Fatal(err)
		}
		if i == n/2 {
			targetKey = key
		}
	}
	targetPath := p.entryPath(targetKey)
	for b.Loop() {
		p.forgetDiskEntryLocked(targetPath)
	}
	if _, ok := p.entries[targetKey]; ok {
		b.Fatal("target entry was not forgotten")
	}
}
