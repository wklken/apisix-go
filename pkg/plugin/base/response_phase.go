package base

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

const DefaultBufferedResponseMaxBytes int64 = 4 << 20

// BufferedResponseConfig controls the bounded response capture.
type BufferedResponseConfig struct {
	MaxBytes int64
}

// ResponseState is the canonical response representation passed between
// response callbacks. Keep this deliberately limited to status, headers and
// body; request/lifecycle/cache state belongs to their owning components.
type ResponseState struct {
	Status int
	Header http.Header
	Body   []byte
}

func CloneResponseState(state ResponseState) ResponseState {
	cloned := ResponseState{Status: state.Status}
	if state.Header != nil {
		cloned.Header = state.Header.Clone()
	}
	cloned.Body = slices.Clone(state.Body)
	return cloned
}

type HeaderFilterPlugin interface {
	RunHeaderFilter(*http.Request, *ResponseState) error
}

type BufferedBodyFilterPlugin interface {
	RunBufferedBodyFilter(*http.Request, *ResponseState) error
}

type FinalResponseStorePlugin interface {
	RunFinalResponseStore(*http.Request, ResponseState) error
}

type ResponseEligibility interface {
	AppliesToResponseSource(apisixctx.ResponseSource) bool
}

// CachedResponseState is the immutable representation handed from a cache
// lookup to the response executor. It intentionally mirrors only the
// canonical response fields.
type CachedResponseState struct {
	Status int
	Header http.Header
	Body   []byte
}

var ErrCacheHitResponseAlreadyConsumed = errors.New("cache hit response already consumed")

// CacheHitResponseHolder transports one cache-hit response without making
// writer calls in the request phase. Publication and consumption deep-copy
// all mutable response values and consumption is exactly once.
type CacheHitResponseHolder struct {
	mu        sync.Mutex
	published bool
	consumed  bool
	state     CachedResponseState
}

func NewCacheHitResponseHolder() *CacheHitResponseHolder {
	return &CacheHitResponseHolder{}
}

func (h *CacheHitResponseHolder) Publish(state CachedResponseState) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.published || h.consumed {
		return
	}
	h.state = cloneCachedResponseState(state)
	h.published = true
}

func (h *CacheHitResponseHolder) Consume() (CachedResponseState, error) {
	state, _, err := h.ConsumePublished()
	return state, err
}

// ConsumePublished distinguishes a missing publication from a consumed hit;
// both remain exactly-once operations, while callers can fail closed when the
// lifecycle source claims CacheHit without a published representation.
func (h *CacheHitResponseHolder) ConsumePublished() (CachedResponseState, bool, error) {
	if h == nil {
		return CachedResponseState{}, false, ErrCacheHitResponseAlreadyConsumed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.consumed {
		return CachedResponseState{}, false, ErrCacheHitResponseAlreadyConsumed
	}
	h.consumed = true
	if !h.published {
		return CachedResponseState{}, false, nil
	}
	return cloneCachedResponseState(h.state), true, nil
}

func (h *CacheHitResponseHolder) Published() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.published
}

func cloneCachedResponseState(state CachedResponseState) CachedResponseState {
	return CachedResponseState{
		Status: state.Status,
		Header: func() http.Header {
			if state.Header == nil {
				return nil
			}
			return state.Header.Clone()
		}(),
		Body: slices.Clone(state.Body),
	}
}

type cacheHitHolderKey struct{}

func WithCacheHitResponseHolder(r *http.Request, holder *CacheHitResponseHolder) *http.Request {
	if r == nil {
		return nil
	}
	return r.WithContext(context.WithValue(r.Context(), cacheHitHolderKey{}, holder))
}

func CacheHitResponseHolderFromRequest(r *http.Request) *CacheHitResponseHolder {
	if r == nil {
		return nil
	}
	holder, _ := r.Context().Value(cacheHitHolderKey{}).(*CacheHitResponseHolder)
	return holder
}
