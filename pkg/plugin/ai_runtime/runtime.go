package ai_runtime

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

type ExecuteFunc func(http.ResponseWriter, *http.Request)

type State struct {
	mu                     sync.RWMutex
	instanceName           string
	execute                ExecuteFunc
	streamingIntent        bool
	rateLimitFallback      bool
	advanceRateLimitTarget func() bool
	consumed               atomic.Bool
}

type (
	stateKey           struct{}
	terminalEnabledKey struct{}
)

func WithExecution(r *http.Request, instanceName string, execute ExecuteFunc) *http.Request {
	state := &State{instanceName: instanceName, execute: execute}
	return r.WithContext(context.WithValue(r.Context(), stateKey{}, state))
}

func WithSelectedInstanceName(r *http.Request, instanceName string) *http.Request {
	if state := FromRequest(r); state != nil {
		state.SetInstanceName(instanceName)
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), stateKey{}, &State{instanceName: instanceName}))
}

func FromRequest(r *http.Request) *State {
	state, _ := r.Context().Value(stateKey{}).(*State)
	return state
}

func SelectedInstanceName(r *http.Request) (string, bool) {
	state := FromRequest(r)
	if state == nil {
		return "", false
	}
	name := state.InstanceName()
	return name, name != ""
}

func (s *State) InstanceName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.instanceName
}

func (s *State) SetInstanceName(instanceName string) {
	s.mu.Lock()
	s.instanceName = instanceName
	s.mu.Unlock()
}

func (s *State) SetStreamingIntent(streaming bool) {
	s.mu.Lock()
	s.streamingIntent = streaming
	s.mu.Unlock()
}

func (s *State) StreamingIntent() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streamingIntent
}

func (s *State) Execute(w http.ResponseWriter, r *http.Request) bool {
	s.mu.RLock()
	execute := s.execute
	s.mu.RUnlock()
	if execute == nil {
		return false
	}
	execute(w, r)
	return true
}

// Consume executes the request-local AI operation at most once. The route
// response plan owns protocol execution; this guard keeps direct compatibility
// handlers from forwarding the same request twice when both layers observe it.
func (s *State) Consume(w http.ResponseWriter, r *http.Request) bool {
	if s == nil || !s.consumed.CompareAndSwap(false, true) {
		return false
	}
	return s.Execute(w, r)
}

func (s *State) ConfigureRateLimitFallback(enabled bool, advance func() bool) {
	s.mu.Lock()
	s.rateLimitFallback = enabled
	s.advanceRateLimitTarget = advance
	s.mu.Unlock()
}

func (s *State) RateLimitFallbackEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rateLimitFallback
}

func (s *State) AdvanceRateLimitTarget() bool {
	s.mu.RLock()
	advance := s.advanceRateLimitTarget
	s.mu.RUnlock()
	return advance != nil && advance()
}

func EnableTerminal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), terminalEnabledKey{}, true))
		next.ServeHTTP(w, r)
	})
}

func TerminalEnabled(r *http.Request) bool {
	enabled, _ := r.Context().Value(terminalEnabledKey{}).(bool)
	return enabled
}

func TerminalHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state := FromRequest(r); state != nil && state.Consume(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}
