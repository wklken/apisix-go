package ctx

import (
	"context"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"time"
)

type RequestOutcomeKind string

const (
	RequestOutcomeCompleted      RequestOutcomeKind = "completed"
	RequestOutcomeRecoveredPanic RequestOutcomeKind = "recovered_panic"
	RequestOutcomeAbortedPanic   RequestOutcomeKind = "aborted_panic"
	RequestOutcomeHandlerAbort   RequestOutcomeKind = "handler_abort"
)

type ResponseOutcome struct {
	Kind      RequestOutcomeKind
	Status    int
	Bytes     int64
	Committed bool
	Flushed   bool
	Hijacked  bool
}

type ResponseSource string

const (
	ResponseSourceUnknown   ResponseSource = "unknown"
	ResponseSourceUpstream  ResponseSource = "upstream"
	ResponseSourceAPISIX    ResponseSource = "apisix"
	ResponseSourceEarlyStop ResponseSource = "early_stop"
	ResponseSourceCacheHit  ResponseSource = "cache_hit"
)

type RequestFinalizer func() error

type FinalizerFailure struct {
	Owner      string
	Err        error
	PanicValue any
	Stack      []byte
}

type registeredFinalizer struct {
	owner string
	fn    RequestFinalizer
}

type RequestLifecycle struct {
	mu             sync.RWMutex
	once           sync.Once
	startedAt      time.Time
	finishedAt     time.Time
	finalizing     bool
	finalizers     []registeredFinalizer
	outcome        ResponseOutcome
	failures       []FinalizerFailure
	finalRequest   *http.Request
	responseSource ResponseSource
}

type requestLifecycleKey struct{}

func NewRequestLifecycle(startedAt time.Time) *RequestLifecycle {
	return &RequestLifecycle{startedAt: startedAt, responseSource: ResponseSourceUnknown}
}

func WithRequestLifecycle(r *http.Request, lifecycle *RequestLifecycle) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestLifecycleKey{}, lifecycle))
}

func GetRequestLifecycle(r *http.Request) *RequestLifecycle {
	lifecycle, _ := r.Context().Value(requestLifecycleKey{}).(*RequestLifecycle)
	return lifecycle
}

func EnsureRequestLifecycle(r *http.Request, startedAt time.Time) (*http.Request, *RequestLifecycle) {
	lifecycle := GetRequestLifecycle(r)
	if lifecycle == nil {
		lifecycle = NewRequestLifecycle(startedAt)
		r = WithRequestLifecycle(r, lifecycle)
	}
	r = WithApisixVars(r, nil)
	r = WithRequestVars(r)
	lifecycle.SetFinalRequest(r)
	return r, lifecycle
}

func (l *RequestLifecycle) SetFinalRequest(r *http.Request) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.finalRequest = r
	l.mu.Unlock()
}

func (l *RequestLifecycle) FinalRequest() *http.Request {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.finalRequest
}

func (l *RequestLifecycle) SetResponseSource(source ResponseSource) {
	if l == nil {
		return
	}
	switch source {
	case ResponseSourceUnknown,
		ResponseSourceUpstream,
		ResponseSourceAPISIX,
		ResponseSourceEarlyStop,
		ResponseSourceCacheHit:
	default:
		source = ResponseSourceUnknown
	}
	l.mu.Lock()
	l.responseSource = source
	l.mu.Unlock()
}

// SetRequestResponseSource is the request-aware source setter. Lifecycle is
// authoritative; the request variable is only a synchronized compatibility
// mirror for existing observability/variable consumers.
func SetRequestResponseSource(r *http.Request, source ResponseSource) *http.Request {
	if r == nil {
		return nil
	}
	if lifecycle := GetRequestLifecycle(r); lifecycle != nil {
		lifecycle.SetResponseSource(source)
		source = lifecycle.ResponseSource()
	} else {
		source = normalizeResponseSource(source)
	}
	RegisterRequestVar(r, "$response_source", string(source))
	RegisterApisixVar(r, "$response_source", string(source))
	return r
}

// SetResponseSource is kept as the concise request-aware spelling for new
// phase owners; RequestLifecycle.SetResponseSource remains the value-only API.
func SetResponseSource(r *http.Request, source ResponseSource) *http.Request {
	return SetRequestResponseSource(r, source)
}

func normalizeResponseSource(source ResponseSource) ResponseSource {
	switch source {
	case ResponseSourceUnknown,
		ResponseSourceUpstream,
		ResponseSourceAPISIX,
		ResponseSourceEarlyStop,
		ResponseSourceCacheHit:
		return source
	default:
		return ResponseSourceUnknown
	}
}

func (l *RequestLifecycle) ResponseSource() ResponseSource {
	if l == nil {
		return ResponseSourceUnknown
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.responseSource == "" {
		return ResponseSourceUnknown
	}
	return l.responseSource
}

func (l *RequestLifecycle) AddFinalizer(owner string, finalizer RequestFinalizer) bool {
	if l == nil || finalizer == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finalizing {
		return false
	}
	l.finalizers = append(l.finalizers, registeredFinalizer{owner: owner, fn: finalizer})
	return true
}

func (l *RequestLifecycle) SetOutcome(outcome ResponseOutcome) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.outcome = outcome
	l.mu.Unlock()
}

// Complete atomically publishes the final response outcome and completion
// timestamp.  Keeping the pair under one lock prevents finalizers from
// observing an outcome from one request boundary with a timestamp from
// another.
func (l *RequestLifecycle) Complete(outcome ResponseOutcome, finishedAt time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.outcome = outcome
	l.finishedAt = finishedAt
	l.mu.Unlock()
}

func (l *RequestLifecycle) Outcome() ResponseOutcome {
	if l == nil {
		return ResponseOutcome{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.outcome
}

func (l *RequestLifecycle) StartedAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.startedAt
}

// FinishedAt returns the timestamp supplied to Complete.  A zero value means
// that completion has not yet been published.
func (l *RequestLifecycle) FinishedAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.finishedAt
}

func (l *RequestLifecycle) Finalize() []FinalizerFailure {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.mu.Lock()
		l.finalizing = true
		finalizers := append([]registeredFinalizer(nil), l.finalizers...)
		l.mu.Unlock()

		failures := make([]FinalizerFailure, 0)
		for _, finalizer := range slices.Backward(finalizers) {
			if failure, failed := runFinalizer(finalizer); failed {
				failures = append(failures, failure)
			}
		}
		l.mu.Lock()
		l.failures = failures
		l.mu.Unlock()
	})
	l.mu.RLock()
	defer l.mu.RUnlock()
	return cloneFinalizerFailures(l.failures)
}

func runFinalizer(finalizer registeredFinalizer) (failure FinalizerFailure, failed bool) {
	failure.Owner = finalizer.owner
	defer func() {
		if recovered := recover(); recovered != nil {
			failure.PanicValue = recovered
			failure.Stack = debug.Stack()
			failed = true
		}
	}()
	if err := finalizer.fn(); err != nil {
		failure.Err = err
		return failure, true
	}
	return FinalizerFailure{}, false
}

func cloneFinalizerFailures(failures []FinalizerFailure) []FinalizerFailure {
	cloned := make([]FinalizerFailure, len(failures))
	copy(cloned, failures)
	for i := range cloned {
		cloned[i].Stack = append([]byte(nil), cloned[i].Stack...)
	}
	return cloned
}
