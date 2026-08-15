package graphql_proxy_cache

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

// storeIntent is the request-local, immutable decision made by a GraphQL
// cache miss. It intentionally contains no request, writer, lifecycle, body,
// or trailer.
type storeIntent struct {
	key            string
	requestHeader  http.Header
	ttl            time.Duration
	cacheSetCookie bool
}

type storeIntentHolder struct {
	mu       sync.Mutex
	intents  map[*Plugin]storeIntent
	consumed map[*Plugin]bool
}

type storeIntentHolderKey struct{}

var ErrStoreIntentAlreadyConsumed = errors.New("graphql-proxy-cache store intent already consumed")

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
	body, query, ok := p.graphqlRequest(w, r)
	if !ok {
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}

	isMutation, err := graphqlHasMutation(query)
	if err != nil {
		if errors.Is(err, errEmptyGraphqlQuery) {
			if w != nil {
				http.Error(w, "Invalid graphql request: empty graphql query", http.StatusBadRequest)
			}
		} else if w != nil {
			http.Error(w, "Invalid graphql request: failed to parse graphql query", http.StatusBadRequest)
		}
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	if isMutation {
		setGraphQLCacheHeader(w, cacheStatusHeader, "BYPASS")
		return base.ContinueRequest(r)
	}

	key := p.cacheKey(r, body)
	if entry, status := p.lookup(r, key); status == "HIT" {
		r = publishGraphQLCacheHit(r, entry, "HIT", key, p.now())
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceCacheHit)
	} else if status == "EXPIRED" {
		setGraphQLCacheHeader(w, cacheStatusHeader, status)
		setGraphQLCacheHeader(w, cacheKeyHeader, key)
		intents.publish(p, p.newStoreIntent(key, r))
		return base.ContinueRequest(r)
	}

	setGraphQLCacheHeader(w, cacheStatusHeader, "MISS")
	setGraphQLCacheHeader(w, cacheKeyHeader, key)
	intents.publish(p, p.newStoreIntent(key, r))
	return base.ContinueRequest(r)
}

func (p *Plugin) newStoreIntent(key string, r *http.Request) storeIntent {
	var requestHeader http.Header
	if r != nil {
		requestHeader = r.Header
	}
	return storeIntent{
		key:            key,
		requestHeader:  requestHeader,
		ttl:            time.Duration(p.config.CacheTTL) * time.Second,
		cacheSetCookie: p.cacheSetCookieEnabled(),
	}
}

func setGraphQLCacheHeader(w http.ResponseWriter, field, value string) {
	if w != nil {
		w.Header().Set(field, value)
	}
}

func publishGraphQLCacheHit(
	r *http.Request,
	entry cacheEntry,
	status string,
	key string,
	now time.Time,
) *http.Request {
	holder := base.CacheHitResponseHolderFromRequest(r)
	if holder == nil {
		holder = base.NewCacheHitResponseHolder()
		r = base.WithCacheHitResponseHolder(r, holder)
	}
	header := cacheutil.CloneHeader(entry.header)
	removeDerivedGraphQLCacheHeaders(header)
	age := max(now.Sub(entry.storedAt)/time.Second, 0)
	header.Set("Age", strconv.FormatInt(int64(age), 10))
	header.Set(cacheStatusHeader, status)
	header.Set(cacheKeyHeader, key)
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

	canonical := base.CloneResponseState(state)
	removeDerivedGraphQLCacheHeaders(canonical.Header)
	if canonical.Status != http.StatusOK || responseCacheControlSkipsStore(canonical.Header) ||
		(!intent.cacheSetCookie && headerHasField(canonical.Header, "Set-Cookie")) {
		return nil
	}
	return p.storeStateWithHeader(intent.requestHeader, intent.key, canonical, intent.ttl)
}

func removeDerivedGraphQLCacheHeaders(header http.Header) {
	for field := range header {
		if strings.EqualFold(field, "Age") || strings.EqualFold(field, cacheStatusHeader) ||
			strings.EqualFold(field, cacheKeyHeader) {
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

func (p *Plugin) storeState(r *http.Request, key string, state base.ResponseState, fallbackTTL time.Duration) error {
	var requestHeader http.Header
	if r != nil {
		requestHeader = r.Header
	}
	return p.storeStateWithHeader(requestHeader, key, state, fallbackTTL)
}

func (p *Plugin) storeStateWithHeader(
	requestHeader http.Header,
	key string,
	state base.ResponseState,
	fallbackTTL time.Duration,
) error {
	varyHeaders, cacheable := cacheutil.ParseVaryHeader(state.Header)
	if !cacheable {
		return nil
	}
	ttl := fallbackTTL
	if p.diskStore != nil {
		ttl = diskResponseTTL(state.Header, ttl, p.now())
	}
	entry := cacheEntry{
		header:   cacheutil.CloneHeader(state.Header),
		body:     slices.Clone(state.Body),
		status:   state.Status,
		storedAt: p.now(),
		ttl:      ttl,
	}
	entry.expiresAt = entry.storedAt.Add(entry.ttl)
	removeDerivedGraphQLCacheHeaders(entry.header)
	storageKey, staleKeys := p.prepareStorageKey(requestHeader, key, varyHeaders)
	for _, staleKey := range staleKeys {
		p.deleteStorageKey(staleKey)
	}
	shared := sharedCacheEntry(entry)
	if p.memoryStore != nil {
		p.memoryStore.Store(storageKey, shared)
		return nil
	}
	if p.diskStore != nil {
		return p.diskStore.Store(storageKey, shared)
	}

	p.lock.Lock()
	p.entries[storageKey] = entry
	p.lock.Unlock()
	return nil
}

func varySignatureFromHeader(headers []string, requestHeader http.Header) string {
	return cacheutil.VarySignature(headers, &http.Request{Header: requestHeader})
}
