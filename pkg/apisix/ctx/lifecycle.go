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
	case ResponseSourceUnknown, ResponseSourceUpstream, ResponseSourceEarlyStop, ResponseSourceCacheHit:
	default:
		source = ResponseSourceUnknown
	}
	l.mu.Lock()
	l.responseSource = source
	l.mu.Unlock()
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
