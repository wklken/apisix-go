package server

import (
	"net/http"
	"sync"
)

type routeSet struct {
	handler http.Handler
	stop    func()
	active  int
	retired bool
	drained chan struct{}
}

type routeHandler struct {
	mu      sync.Mutex
	current *routeSet
	closed  bool
}

func newRouteHandler(handler http.Handler, stop func()) *routeHandler {
	return &routeHandler{current: newRouteSet(handler, stop)}
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	current := h.current
	if current == nil || current.handler == nil {
		h.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	current.active++
	h.mu.Unlock()

	defer h.finishRequest(current)
	current.handler.ServeHTTP(w, r)
}

func (h *routeHandler) Replace(handler http.Handler, stop func()) {
	next := newRouteSet(handler, stop)
	h.mu.Lock()
	if h.closed {
		h.retireLocked(next)
		h.mu.Unlock()
		stopRouteSet(next)
		return
	}
	previous := h.current
	h.current = next
	h.retireLocked(previous)
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
	previous := h.current
	h.current = nil
	h.retireLocked(previous)
	h.mu.Unlock()

	stopRouteSet(previous)
}

func newRouteSet(handler http.Handler, stop func()) *routeSet {
	return &routeSet{handler: handler, stop: stop, drained: make(chan struct{})}
}

func (h *routeHandler) finishRequest(current *routeSet) {
	h.mu.Lock()
	current.active--
	if current.retired && current.active == 0 {
		close(current.drained)
	}
	h.mu.Unlock()
}

func (h *routeHandler) retireLocked(current *routeSet) {
	if current == nil || current.retired {
		return
	}
	current.retired = true
	if current.active == 0 {
		close(current.drained)
	}
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
