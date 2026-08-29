package secret

import (
	"context"
	"sync"
)

type attemptResourceSet struct {
	mu       sync.Mutex
	limiters map[string]*attemptLimiterState
	stopping chan struct{}
	stopOnce sync.Once
}

func newAttemptResourceSet() *attemptResourceSet {
	return &attemptResourceSet{
		limiters: make(map[string]*attemptLimiterState),
		stopping: make(chan struct{}),
	}
}

func (resources *attemptResourceSet) limiter(name string, capacity int) (AttemptLimiter, error) {
	if resources == nil || name == "" || capacity <= 0 {
		return AttemptLimiter{}, ErrInvalidCapability
	}
	resources.mu.Lock()
	defer resources.mu.Unlock()
	if limiter, ok := resources.limiters[name]; ok {
		if cap(limiter.slots) != capacity {
			return AttemptLimiter{}, ErrInvalidCapability
		}
		return AttemptLimiter{state: limiter, attemptStopping: resources.stopping}, nil
	}
	limiter := &attemptLimiterState{slots: make(chan struct{}, capacity)}
	resources.limiters[name] = limiter
	return AttemptLimiter{state: limiter, attemptStopping: resources.stopping}, nil
}

func (resources *attemptResourceSet) stop() {
	if resources == nil {
		return
	}
	resources.stopOnce.Do(func() { close(resources.stopping) })
}

type attemptLimiterState struct {
	slots chan struct{}
}

// AttemptLimiter bounds one named operation across every binding that shares
// the same generation attempt. It grants no attempt-close authority.
type AttemptLimiter struct {
	state           *attemptLimiterState
	attemptStopping <-chan struct{}
}

func (limiter AttemptLimiter) Valid() bool {
	return limiter.state != nil && limiter.state.slots != nil && limiter.attemptStopping != nil
}

// Acquire reserves one attempt-wide slot. A canceled request never receives a
// slot, including when cancellation and capacity become ready together.
func (limiter AttemptLimiter) Acquire(
	ctx context.Context,
	bindingStopping <-chan struct{},
) (func(), error) {
	if ctx == nil || !limiter.Valid() {
		return nil, ErrInvalidCapability
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-bindingStopping:
		return nil, ErrCredentialUnavailable
	default:
	}
	select {
	case <-limiter.attemptStopping:
		return nil, ErrCredentialUnavailable
	default:
	}

	select {
	case limiter.state.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-bindingStopping:
		return nil, ErrCredentialUnavailable
	case <-limiter.attemptStopping:
		return nil, ErrCredentialUnavailable
	}

	release := func() { <-limiter.state.slots }
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	select {
	case <-bindingStopping:
		release()
		return nil, ErrCredentialUnavailable
	default:
	}
	select {
	case <-limiter.attemptStopping:
		release()
		return nil, ErrCredentialUnavailable
	default:
	}

	var once sync.Once
	return func() { once.Do(release) }, nil
}
