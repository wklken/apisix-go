package metrics

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type metricSeriesEntry struct {
	labels   []string
	lastSeen atomic.Int64
	inFlight atomic.Int64
}

type metricSeriesCandidate struct {
	key      string
	lastSeen int64
}

// metricSeriesTracker owns admission, last-seen state, and deletion for one
// metric family. Vector updates run under its read side so expiration cannot
// detach a child while an observation is still using it.
type metricSeriesTracker struct {
	mu                sync.RWMutex
	limit             int
	expire            time.Duration
	entries           map[string]*metricSeriesEntry
	overflowLabels    []string
	overflowCounter   prometheus.Counter
	deleteLabelValues func(...string) bool
	now               func() time.Time
}

func newMetricSeriesTracker(
	limit int,
	labelCount int,
	expire time.Duration,
	overflow prometheus.Counter,
	deleteLabels func(...string) bool,
) *metricSeriesTracker {
	overflowLabels := make([]string, labelCount)
	for index := range overflowLabels {
		overflowLabels[index] = overflowLabel
	}
	return &metricSeriesTracker{
		limit:             limit,
		expire:            expire,
		entries:           make(map[string]*metricSeriesEntry, limit),
		overflowLabels:    overflowLabels,
		overflowCounter:   overflow,
		deleteLabelValues: deleteLabels,
		now:               time.Now,
	}
}

func (t *metricSeriesTracker) withSeries(labels []string, update func([]string)) {
	if t == nil {
		update(labels)
		return
	}

	key := metricSeriesTupleKey(labels)
	observedAt := t.now().UnixNano()
	if t.updateExisting(key, observedAt, update) {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.entries[key]; ok {
		entry.lastSeen.Store(observedAt)
		update(entry.labels)
		return
	}
	if len(t.entries) < t.limit {
		storedLabels := append([]string(nil), labels...)
		entry := &metricSeriesEntry{labels: storedLabels}
		entry.lastSeen.Store(observedAt)
		t.entries[key] = entry
		update(storedLabels)
		return
	}
	t.recordOverflow(update)
}

func (t *metricSeriesTracker) updateExisting(
	key string,
	observedAt int64,
	update func([]string),
) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	entry, ok := t.entries[key]
	if !ok {
		return false
	}
	entry.lastSeen.Store(observedAt)
	update(entry.labels)
	return true
}

func (t *metricSeriesTracker) acquireSeries(
	labels []string,
	increment func([]string),
	decrement func([]string),
) func() {
	if t == nil {
		increment(labels)
		return func() { decrement(labels) }
	}

	key := metricSeriesTupleKey(labels)
	observedAt := t.now().UnixNano()
	if entry := t.acquireExisting(key, observedAt, increment); entry != nil {
		return t.releaseFunc(entry, decrement)
	}

	t.mu.Lock()
	if entry, ok := t.entries[key]; ok {
		entry.lastSeen.Store(observedAt)
		entry.inFlight.Add(1)
		increment(entry.labels)
		t.mu.Unlock()
		return t.releaseFunc(entry, decrement)
	}
	if len(t.entries) < t.limit {
		storedLabels := append([]string(nil), labels...)
		entry := &metricSeriesEntry{labels: storedLabels}
		entry.lastSeen.Store(observedAt)
		entry.inFlight.Store(1)
		t.entries[key] = entry
		increment(storedLabels)
		t.mu.Unlock()
		return t.releaseFunc(entry, decrement)
	}
	t.recordOverflow(increment)
	t.mu.Unlock()
	return func() { decrement(t.overflowLabels) }
}

func (t *metricSeriesTracker) acquireExisting(
	key string,
	observedAt int64,
	increment func([]string),
) *metricSeriesEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	entry := t.entries[key]
	if entry == nil {
		return nil
	}
	entry.lastSeen.Store(observedAt)
	entry.inFlight.Add(1)
	increment(entry.labels)
	return entry
}

func (t *metricSeriesTracker) releaseFunc(
	entry *metricSeriesEntry,
	decrement func([]string),
) func() {
	return func() {
		t.mu.RLock()
		defer t.mu.RUnlock()
		decrement(entry.labels)
		entry.lastSeen.Store(t.now().UnixNano())
		entry.inFlight.Add(-1)
	}
}

func (t *metricSeriesTracker) recordOverflow(update func([]string)) {
	if t.overflowCounter != nil {
		t.overflowCounter.Inc()
	}
	update(t.overflowLabels)
}

func (t *metricSeriesTracker) expireSeries(now time.Time, maxDeletes int) int {
	candidates := t.expiredCandidates(now, maxDeletes)
	return t.deleteExpired(candidates, now)
}

func (t *metricSeriesTracker) expiredCandidates(now time.Time, max int) []metricSeriesCandidate {
	if t == nil || t.expire <= 0 || max <= 0 {
		return nil
	}
	deadline := now.Add(-t.expire).UnixNano()
	candidates := make([]metricSeriesCandidate, 0, max)
	t.mu.RLock()
	defer t.mu.RUnlock()
	for key, entry := range t.entries {
		lastSeen := entry.lastSeen.Load()
		if entry.inFlight.Load() != 0 || lastSeen > deadline {
			continue
		}
		candidates = append(candidates, metricSeriesCandidate{key: key, lastSeen: lastSeen})
		if len(candidates) == max {
			break
		}
	}
	return candidates
}

func (t *metricSeriesTracker) deleteExpired(candidates []metricSeriesCandidate, now time.Time) int {
	if t == nil || t.expire <= 0 || len(candidates) == 0 {
		return 0
	}
	deadline := now.Add(-t.expire).UnixNano()
	deleted := 0
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, candidate := range candidates {
		entry := t.entries[candidate.key]
		if entry == nil ||
			entry.inFlight.Load() != 0 ||
			entry.lastSeen.Load() != candidate.lastSeen ||
			candidate.lastSeen > deadline {
			continue
		}
		if t.deleteLabelValues != nil {
			t.deleteLabelValues(entry.labels...)
		}
		delete(t.entries, candidate.key)
		deleted++
	}
	return deleted
}

func (t *metricSeriesTracker) entryCount() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}

func metricSeriesTupleKey(values []string) string {
	var builder strings.Builder
	for _, value := range values {
		builder.WriteString(strconv.Itoa(len(value)))
		builder.WriteByte(':')
		builder.WriteString(value)
	}
	return builder.String()
}
