package proxy_cache

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
)

// storeIntent is the request-local, immutable decision made by a cache miss.
// It intentionally contains no request, writer, lifecycle, body, or trailer.
type storeIntent struct {
	key               string
	requestHeader     http.Header
	ttl               time.Duration
	cacheControl      bool
	cacheSetCookie    bool
	hideCacheHeaders  bool
	cacheHTTPStatuses []int
}

type storeIntentHolder struct {
	mu       sync.Mutex
	intents  map[*Plugin]storeIntent
	consumed map[*Plugin]bool
}

type storeIntentHolderKey struct{}

var ErrStoreIntentAlreadyConsumed = errors.New("proxy-cache store intent already consumed")

func ensureStoreIntentHolder(r *http.Request) (*http.Request, *storeIntentHolder) {
	if r == nil {
		return nil, nil
	}
	if holder, ok := r.Context().Value(storeIntentHolderKey{}).(*storeIntentHolder); ok && holder != nil {
		return r, holder
	}
	holder := &storeIntentHolder{
		intents:  make(map[*Plugin]storeIntent),
		consumed: make(map[*Plugin]bool),
	}
	return r.WithContext(context.WithValue(r.Context(), storeIntentHolderKey{}, holder)), holder
}

func storeIntentHolderFromRequest(r *http.Request) *storeIntentHolder {
	if r == nil {
		return nil
	}
	holder, _ := r.Context().Value(storeIntentHolderKey{}).(*storeIntentHolder)
	return holder
}

func (h *storeIntentHolder) publish(plugin *Plugin, intent storeIntent) {
	if h == nil || plugin == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.consumed[plugin] {
		return
	}
	if h.intents == nil {
		h.intents = make(map[*Plugin]storeIntent)
	}
	if _, exists := h.intents[plugin]; exists {
		return
	}
	intent.requestHeader = cacheutil.CloneHeader(intent.requestHeader)
	intent.cacheHTTPStatuses = slices.Clone(intent.cacheHTTPStatuses)
	h.intents[plugin] = intent
}

func (h *storeIntentHolder) consume(plugin *Plugin) (storeIntent, bool, error) {
	if h == nil || plugin == nil {
		return storeIntent{}, false, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.consumed[plugin] {
		return storeIntent{}, false, ErrStoreIntentAlreadyConsumed
	}
	h.consumed[plugin] = true
	intent, ok := h.intents[plugin]
	delete(h.intents, plugin)
	return intent, ok, nil
}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	r, intents := ensureStoreIntentHolder(r)
	if r == nil {
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}

	if r.Method == purgeMethod {
		if p.purgeAll(p.cacheKey(r)) {
			if w != nil {
				w.WriteHeader(http.StatusOK)
			}
		} else if w != nil {
			w.WriteHeader(http.StatusNotFound)
		}
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}

	if !p.cacheableMethod(r.Method) {
		status := "BYPASS"
		if p.config.CacheStrategy == "memory" {
			status = "MISS"
		}
		setCacheHeader(w, cacheStatusHeader, status)
		return base.ContinueRequest(r)
	}

	key := p.cacheKey(r)
	if p.hasTruthyValue(r, p.config.CacheBypass) || p.requestCacheControlBypass(r) {
		setCacheHeader(w, cacheStatusHeader, "BYPASS")
		return base.ContinueRequest(r)
	}

	entry, status := p.lookup(r, key)
	if status == "HIT" && !cacheEntrySatisfiesRequiredVary(r, entry) {
		p.purgeAll(key)
		status = "MISS"
	}
	if status == "HIT" {
		r = publishCacheHit(r, entry, "HIT")
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceCacheHit)
	} else if status == "EXPIRED" || status == "STALE" {
		setCacheHeader(w, cacheStatusHeader, status)
		if r.Method != http.MethodHead && !p.hasTruthyValue(r, p.config.NoCache) {
			intents.publish(p, p.newStoreIntent(key, r))
		}
		return base.ContinueRequest(r)
	} else if p.onlyIfCachedMiss(r) {
		setCacheHeader(w, cacheStatusHeader, "MISS")
		if w != nil {
			w.WriteHeader(http.StatusGatewayTimeout)
		}
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}

	setCacheHeader(w, cacheStatusHeader, "MISS")
	if r.Method != http.MethodHead && !p.hasTruthyValue(r, p.config.NoCache) {
		intents.publish(p, p.newStoreIntent(key, r))
	}
	return base.ContinueRequest(r)
}

func cacheEntrySatisfiesRequiredVary(r *http.Request, entry cacheEntry) bool {
	if !cacheutil.RequiredVary(r, "Accept-Encoding") {
		return true
	}
	varyHeaders, cacheable := cacheutil.ParseVaryHeader(entry.header)
	return cacheable && slices.Contains(varyHeaders, "accept-encoding")
}

func (p *Plugin) newStoreIntent(key string, r *http.Request) storeIntent {
	var requestHeader http.Header
	if r != nil {
		requestHeader = r.Header
	}
	return storeIntent{
		key:               key,
		requestHeader:     requestHeader,
		ttl:               time.Duration(p.config.CacheTTL) * time.Second,
		cacheControl:      p.cacheControlEnabled(),
		cacheSetCookie:    p.cacheSetCookieEnabled(),
		hideCacheHeaders:  p.config.HideCacheHeaders,
		cacheHTTPStatuses: slices.Clone(p.config.CacheHTTPStatus),
	}
}

func setCacheHeader(w http.ResponseWriter, field, value string) {
	if w != nil {
		w.Header().Set(field, value)
	}
}

func publishCacheHit(r *http.Request, entry cacheEntry, status string) *http.Request {
	holder := base.CacheHitResponseHolderFromRequest(r)
	if holder == nil {
		holder = base.NewCacheHitResponseHolder()
		r = base.WithCacheHitResponseHolder(r, holder)
	}
	header := cacheutil.CloneHeader(entry.header)
	removeDerivedCacheHeaders(header)
	age := max(time.Since(entry.storedAt)/time.Second, 0)
	header.Set("Age", strconv.FormatInt(int64(age), 10))
	header.Set(cacheStatusHeader, status)
	holder.Publish(base.CachedResponseState{
		Status: entry.status,
		Header: header,
		Body:   slices.Clone(entry.body),
	})
	return r
}

func (p *Plugin) RunFinalResponseStore(r *http.Request, state base.ResponseState) error {
	holder := storeIntentHolderFromRequest(r)
	if holder == nil {
		return nil
	}
	intent, ok, err := holder.consume(p)
	if err != nil || !ok {
		return err
	}
	if len(state.Trailer) > 0 {
		return nil
	}
	if p.hasTruthyValue(r, p.config.NoCache) {
		return nil
	}

	canonical := base.CloneResponseState(state)
	removeDerivedCacheHeaders(canonical.Header)
	if !slices.Contains(intent.cacheHTTPStatuses, canonical.Status) ||
		responseCacheControlSkipsStore(canonical.Header) ||
		(!intent.cacheSetCookie && headerHasField(canonical.Header, "Set-Cookie")) {
		return nil
	}
	ttl := intent.ttl
	if intent.cacheControl {
		var cacheable bool
		ttl, cacheable = responseCacheControlTTL(canonical.Header)
		if !cacheable {
			return nil
		}
	}
	return p.storeStateWithHeader(intent.requestHeader, intent.key, canonical, ttl, intent.hideCacheHeaders)
}

func removeDerivedCacheHeaders(header http.Header) {
	for field := range header {
		if strings.EqualFold(field, "Age") ||
			strings.EqualFold(field, cacheStatusHeader) ||
			strings.EqualFold(field, "APISIX-Cache-Key") {
			delete(header, field)
		}
	}
}

func headerHasField(header http.Header, wanted string) bool {
	for field := range header {
		if strings.EqualFold(field, wanted) {
			return len(header[field]) > 0
		}
	}
	return false
}

func (p *Plugin) storeState(
	r *http.Request,
	key string,
	state base.ResponseState,
	ttl time.Duration,
	hideCacheHeaders bool,
) error {
	var requestHeader http.Header
	if r != nil {
		requestHeader = r.Header
	}
	return p.storeStateWithHeader(requestHeader, key, state, ttl, hideCacheHeaders)
}

func (p *Plugin) storeStateWithHeader(
	requestHeader http.Header,
	key string,
	state base.ResponseState,
	ttl time.Duration,
	hideCacheHeaders bool,
) error {
	varyHeaders, cacheable := cacheutil.ParseVaryHeader(state.Header)
	if !cacheable {
		return nil
	}
	now := time.Now()
	entry := cacheEntry{
		header:    cacheutil.CloneHeader(state.Header),
		body:      slices.Clone(state.Body),
		status:    state.Status,
		storedAt:  now,
		ttl:       ttl,
		expiresAt: now.Add(ttl),
	}
	removeDerivedCacheHeaders(entry.header)
	if hideCacheHeaders {
		deleteHeaderFold(entry.header, "Expires")
		deleteHeaderFold(entry.header, "Cache-Control")
	}

	p.lock.Lock()
	defer p.lock.Unlock()
	storageKey := key
	var storageSignature string
	if len(varyHeaders) > 0 {
		storageSignature = varySignatureFromHeader(varyHeaders, requestHeader)
		storageKey = key + "::" + storageSignature
	}
	if !canStoreMemoryEntryWithVary(
		p.memoryZone,
		key,
		storageKey,
		entry,
		varyHeaders,
		storageSignature,
	) {
		return nil
	}
	if p.diskEnabled {
		p.loadVaryIndexLocked(key)
	}
	if len(varyHeaders) > 0 {
		p.updateVaryIndexLocked(key, varyHeaders, storageSignature, entry.expiresAt)
		delete(p.entries, key)
	} else if _, ok := p.vary[key]; ok {
		p.purgeAllLocked(key)
	}
	p.entries[storageKey] = entry
	if p.memoryZone != nil {
		p.memoryZone.recalculateUsedBytesLocked()
		p.memoryZone.enforceCapacityLocked()
	}
	index, hasIndex := p.vary[key]
	if p.diskEnabled {
		if err := p.persistEntryLocked(storageKey, entry); err != nil {
			return err
		}
		if hasIndex {
			if err := p.persistVaryIndex(key, index); err != nil {
				return err
			}
		}
		p.cleanupDiskLocked(now)
	}
	return nil
}

func varySignatureFromHeader(headers []string, requestHeader http.Header) string {
	return cacheutil.VarySignature(headers, &http.Request{Header: requestHeader})
}

func deleteHeaderFold(header http.Header, wanted string) {
	for field := range header {
		if strings.EqualFold(field, wanted) {
			delete(header, field)
		}
	}
}
