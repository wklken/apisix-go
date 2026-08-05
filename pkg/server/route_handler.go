package server

import (
	"net/http"
	"sync"
	"sync/atomic"
)

const routeSetRetired = uint64(1) << 63

type routeSet struct {
	handler     http.Handler
	stop        func()
	state       atomic.Uint64 // high bit is retired; remaining bits are active requests
	drained     chan struct{}
	drainedOnce sync.Once
}

type routeHandler struct {
	mu      sync.Mutex
	current atomic.Pointer[routeSet]
	closed  bool
}

func newRouteHandler(handler http.Handler, stop func()) *routeHandler {
	routes := &routeHandler{}
	routes.current.Store(newRouteSet(handler, stop))
	return routes
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var current *routeSet
	for {
		current = h.current.Load()
		if current == nil || current.handler == nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		state := current.state.Load()
		if state&routeSetRetired != 0 {
			continue
		}
		if current.state.CompareAndSwap(state, state+1) {
			break
		}
	}

	defer h.finishRequest(current)
	current.handler.ServeHTTP(w, r)
}

func (h *routeHandler) Replace(handler http.Handler, stop func()) {
	next := newRouteSet(handler, stop)
	h.mu.Lock()
	if h.closed {
		retireRouteSet(next)
		h.mu.Unlock()
		stopRouteSet(next)
		return
	}
	previous := h.current.Swap(next)
	retireRouteSet(previous)
	h.mu.Unlock()

	stopRouteSet(previous)
}

func (h *routeHandler) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	previous := h.current.Swap(nil)
	retireRouteSet(previous)
	h.mu.Unlock()

	stopRouteSet(previous)
}

func newRouteSet(handler http.Handler, stop func()) *routeSet {
	return &routeSet{handler: handler, stop: stop, drained: make(chan struct{})}
}

func (h *routeHandler) finishRequest(current *routeSet) {
	state := current.state.Add(^uint64(0))
	if state == routeSetRetired {
		current.closeDrained()
	}
}

func retireRouteSet(current *routeSet) {
	if current == nil {
		return
	}
	for {
		state := current.state.Load()
		if state&routeSetRetired != 0 {
			return
		}
		retired := state | routeSetRetired
		if current.state.CompareAndSwap(state, retired) {
			if retired == routeSetRetired {
				current.closeDrained()
			}
			return
		}
	}
}

func (r *routeSet) closeDrained() {
	r.drainedOnce.Do(func() { close(r.drained) })
}

func stopRouteSet(current *routeSet) {
	if current == nil {
		return
	}
	<-current.drained
	if current.stop != nil {
		current.stop()
	}
}
