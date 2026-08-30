package secret

import (
	"context"
	"sync"
)

type generationResourceSet struct {
	mu       sync.Mutex
	limiters map[string]*generationLimiterState
	stopping chan struct{}
	stopOnce sync.Once
}

func newGenerationResourceSet() *generationResourceSet {
	return &generationResourceSet{
		limiters: make(map[string]*generationLimiterState),
		stopping: make(chan struct{}),
	}
}

func (resources *generationResourceSet) limiter(name string, capacity int) (GenerationLimiter, error) {
	if resources == nil || name == "" || capacity <= 0 {
		return GenerationLimiter{}, ErrInvalidCapability
	}
	resources.mu.Lock()
	defer resources.mu.Unlock()
	if limiter, ok := resources.limiters[name]; ok {
		if cap(limiter.slots) != capacity {
			return GenerationLimiter{}, ErrInvalidCapability
		}
		return GenerationLimiter{state: limiter, generationStopping: resources.stopping}, nil
	}
	limiter := &generationLimiterState{slots: make(chan struct{}, capacity)}
	resources.limiters[name] = limiter
	return GenerationLimiter{state: limiter, generationStopping: resources.stopping}, nil
}

func (resources *generationResourceSet) stop() {
	if resources == nil {
		return
	}
	resources.stopOnce.Do(func() { close(resources.stopping) })
}

type generationLimiterState struct {
	slots chan struct{}
}

// GenerationLimiter bounds one named operation across every binding that shares
// the same runtime generation. It grants no attempt-close authority.
type GenerationLimiter struct {
	state              *generationLimiterState
	generationStopping <-chan struct{}
}

func (limiter GenerationLimiter) Valid() bool {
	return limiter.state != nil && limiter.state.slots != nil && limiter.generationStopping != nil
}

// Acquire reserves one generation-wide slot. A canceled request never receives a
// slot, including when cancellation and capacity become ready together.
func (limiter GenerationLimiter) Acquire(
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
	case <-limiter.generationStopping:
		return nil, ErrCredentialUnavailable
	default:
	}

	select {
	case limiter.state.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-bindingStopping:
		return nil, ErrCredentialUnavailable
	case <-limiter.generationStopping:
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
	case <-limiter.generationStopping:
		release()
		return nil, ErrCredentialUnavailable
	default:
	}

	var once sync.Once
	return func() { once.Do(release) }, nil
}
