package api_breaker

import (
	"container/list"
	"context"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type finalizerRegistrationsKey struct{}

type Plugin struct {
	base.BasePlugin
	config Config

	state *State
	now   func() time.Time
}

type breakerKey struct {
	host string
	uri  string
}

type breakerEntry struct {
	unhealthyCount    int
	healthyCount      int
	lastUnhealthyTime time.Time
	lastTimeExpiresAt time.Time
	lruElement        *list.Element
	accountedBytes    int
}

// State owns the APISIX process-local breaker counters shared by every route
// and plugin instance in overlapping HTTP generations.
type State struct {
	mu        sync.Mutex
	entries   map[breakerKey]*breakerEntry
	lru       *list.List
	maxBytes  int
	usedBytes int
	closed    bool
}

const (
	defaultStateBudgetBytes = 10 << 20
	breakerEntryOverhead    = 256
)

func NewState() *State {
	return newStateWithBudget(defaultStateBudgetBytes)
}

func newStateWithBudget(maxBytes int) *State {
	return &State{
		entries:  make(map[breakerKey]*breakerEntry),
		lru:      list.New(),
		maxBytes: max(maxBytes, 1),
	}
}

// Close releases all counters after the final generation resource lease has
// drained. Closed state fails open and cannot be reused.
func (state *State) Close() {
	if state == nil {
		return
	}
	state.mu.Lock()
	clear(state.entries)
	state.entries = nil
	if state.lru != nil {
		state.lru.Init()
	}
	state.usedBytes = 0
	state.closed = true
	state.mu.Unlock()
}

const (
	// version  = "0.1"
	priority = 1005
	name     = "api-breaker"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "break_response_code": {
		"type": "integer",
		"minimum": 200,
		"maximum": 599
	  },
	  "break_response_body": {
		"type": "string"
	  },
	  "break_response_headers": {
		"type": "array",
		"items": {
		  "type": "object",
		  "properties": {
			"key": {
			  "type": "string",
			  "minLength": 1
			},
			"value": {
			  "type": "string",
			  "minLength": 1
			}
		  },
		  "required": ["key", "value"]
		}
	  },
	  "max_breaker_sec": {
		"type": "integer",
		"minimum": 3,
		"default": 300
	  },
	  "unhealthy": {
		"type": "object",
		"properties": {
		  "http_statuses": {
			"type": "array",
			"minItems": 1,
			"items": {
			  "type": "integer",
			  "minimum": 500,
			  "maximum": 599
			},
			"uniqueItems": true,
			"default": [500]
		  },
		  "failures": {
			"type": "integer",
			"minimum": 1,
			"default": 3
		  }
		},
		"default": {
		  "http_statuses": [500],
		  "failures": 3
		}
	  },
	  "healthy": {
		"type": "object",
		"properties": {
		  "http_statuses": {
			"type": "array",
			"minItems": 1,
			"items": {
			  "type": "integer",
			  "minimum": 200,
			  "maximum": 499
			},
			"uniqueItems": true,
			"default": [200]
		  },
		  "successes": {
			"type": "integer",
			"minimum": 1,
			"default": 3
		  }
		},
		"default": {
		  "http_statuses": [200],
		  "successes": 3
		}
	  }
	},
	"required": ["break_response_code"]
}`

type Header struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type UnHealthCheck struct {
	HTTPStatuses []int `json:"http_statuses"`
	Failures     *int  `json:"failures,omitempty"`
}

type HealthCheck struct {
	HTTPStatuses []int `json:"http_statuses"`
	Successes    *int  `json:"successes,omitempty"`
}

type Config struct {
	BreakResponseCode    int           `json:"break_response_code"`
	BreakResponseBody    *string       `json:"break_response_body,omitempty"`
	BreakResponseHeaders []Header      `json:"break_response_headers,omitempty"`
	MaxBreakerSec        int           `json:"max_breaker_sec"`
	Unhealthy            UnHealthCheck `json:"unhealthy"`
	Healthy              HealthCheck   `json:"healthy"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.now == nil {
		p.now = time.Now
	}
	if p.state == nil {
		p.state = NewState()
	}
	if p.config.MaxBreakerSec == 0 {
		p.config.MaxBreakerSec = 300
	}
	if p.config.Unhealthy.HTTPStatuses == nil {
		p.config.Unhealthy.HTTPStatuses = []int{500}
	}
	if p.config.Unhealthy.Failures == nil {
		defaultFailures := 3
		p.config.Unhealthy.Failures = &defaultFailures
	}

	if p.config.Healthy.HTTPStatuses == nil {
		p.config.Healthy.HTTPStatuses = []int{200}
	}
	if p.config.Healthy.Successes == nil {
		defaultSuccesses := 3
		p.config.Healthy.Successes = &defaultSuccesses
	}

	return nil
}

// SetState injects the generation-owned process-local counter store before
// PostInit. Direct package users retain an isolated fallback state.
func (p *Plugin) SetState(state *State) {
	if state != nil {
		p.state = state
	}
}

func (p *Plugin) Config() any {
	return &p.config
}

// RunRequestPhase performs the breaker decision and, on continuation,
// registers one request-local upstream-status observer.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	key := breakerKeyForRequest(r)
	if p.shouldBreak(key) {
		if p.config.BreakResponseBody != nil && p.config.BreakResponseHeaders != nil {
			for _, header := range p.config.BreakResponseHeaders {
				w.Header().Set(header.Key, resolveHeaderValue(r, header.Value))
			}
		}
		w.WriteHeader(p.config.BreakResponseCode)
		if p.config.BreakResponseBody != nil {
			_, _ = w.Write([]byte(*p.config.BreakResponseBody))
		}
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}

	lifecycle := apisixctx.GetRequestLifecycle(r)
	if lifecycle == nil {
		return base.ContinueRequest(r)
	}
	registrations, _ := r.Context().Value(finalizerRegistrationsKey{}).(map[*Plugin]struct{})
	if _, registered := registrations[p]; registered {
		return base.ContinueRequest(r)
	}
	if registrations == nil {
		registrations = make(map[*Plugin]struct{})
	}
	registrations = cloneFinalizerRegistrations(registrations)
	registrations[p] = struct{}{}
	if lifecycle.AddFinalizer(name, func() error {
		if status, ok := lastUpstreamStatus(apisixctx.GetRequestVar(r, "$upstream_status")); ok {
			p.observeStatus(key, status)
		}
		return nil
	}) {
		r = r.WithContext(context.WithValue(r.Context(), finalizerRegistrationsKey{}, registrations))
	}
	return base.ContinueRequest(r)
}

func cloneFinalizerRegistrations(registrations map[*Plugin]struct{}) map[*Plugin]struct{} {
	cloned := make(map[*Plugin]struct{}, len(registrations)+1)
	for plugin := range registrations {
		cloned[plugin] = struct{}{}
	}
	return cloned
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			base.AdaptRequestPhase(p, next).ServeHTTP(w, r)
			return
		}
		key := breakerKeyForRequest(r)
		if p.shouldBreak(key) {
			if p.config.BreakResponseBody != nil && p.config.BreakResponseHeaders != nil {
				for _, header := range p.config.BreakResponseHeaders {
					w.Header().Set(header.Key, resolveHeaderValue(r, header.Value))
				}
			}
			w.WriteHeader(p.config.BreakResponseCode)
			if p.config.BreakResponseBody != nil {
				_, _ = w.Write([]byte(*p.config.BreakResponseBody))
			}
			return
		}

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)
		if status, ok := lastUpstreamStatus(apisixctx.GetRequestVar(r, "$upstream_status")); ok {
			p.observeStatus(key, status)
			return
		}
		// Without the production request lifecycle, next is the direct upstream
		// seam and its response status is the only available upstream outcome.
		p.observeStatus(key, ww.Status())
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) shouldBreak(key breakerKey) bool {
	if p.state == nil {
		return false
	}
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	entry := p.state.entries[key]
	if p.state.closed || entry == nil || entry.unhealthyCount == 0 || entry.lastUnhealthyTime.IsZero() {
		return false
	}
	p.state.touchLocked(entry)
	now := p.now()
	if !entry.lastTimeExpiresAt.IsZero() && !now.Before(entry.lastTimeExpiresAt) {
		entry.lastUnhealthyTime = time.Time{}
		entry.lastTimeExpiresAt = time.Time{}
		return false
	}
	seconds := breakerSeconds(entry.unhealthyCount, *p.config.Unhealthy.Failures, p.config.MaxBreakerSec)
	logger.Debugf("breaker_time: %d", seconds)
	return !now.After(entry.lastUnhealthyTime.Add(time.Duration(seconds) * time.Second))
}

func (p *Plugin) observeStatus(key breakerKey, status int) {
	if p.state == nil {
		return
	}
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	if p.state.closed {
		return
	}
	entry := p.state.entries[key]
	switch {
	case slices.Contains(p.config.Unhealthy.HTTPStatuses, status):
		if entry == nil {
			entry = p.state.insertLocked(key)
			if entry == nil {
				return
			}
		} else {
			p.state.touchLocked(entry)
		}
		entry.unhealthyCount++
		entry.healthyCount = 0
		if entry.unhealthyCount%*p.config.Unhealthy.Failures == 0 {
			now := p.now()
			entry.lastUnhealthyTime = now
			entry.lastTimeExpiresAt = now.Add(time.Duration(p.config.MaxBreakerSec) * time.Second)
		}
	case slices.Contains(p.config.Healthy.HTTPStatuses, status):
		if entry == nil || entry.unhealthyCount == 0 {
			return
		}
		p.state.touchLocked(entry)
		entry.healthyCount++
		if entry.healthyCount >= *p.config.Healthy.Successes {
			p.state.removeLocked(key, entry)
		}
	}
}

func (state *State) insertLocked(key breakerKey) *breakerEntry {
	accountedBytes := breakerEntryOverhead + len(key.host) + len(key.uri)
	if accountedBytes > state.maxBytes {
		return nil
	}
	for state.usedBytes+accountedBytes > state.maxBytes {
		oldest := state.lru.Back()
		if oldest == nil {
			return nil
		}
		oldestKey := oldest.Value.(breakerKey)
		state.removeLocked(oldestKey, state.entries[oldestKey])
	}
	key = breakerKey{
		host: strings.Clone(key.host),
		uri:  strings.Clone(key.uri),
	}
	entry := &breakerEntry{accountedBytes: accountedBytes}
	entry.lruElement = state.lru.PushFront(key)
	state.entries[key] = entry
	state.usedBytes += accountedBytes
	return entry
}

func (state *State) touchLocked(entry *breakerEntry) {
	if entry != nil && entry.lruElement != nil {
		state.lru.MoveToFront(entry.lruElement)
	}
}

func (state *State) removeLocked(key breakerKey, entry *breakerEntry) {
	if entry == nil {
		return
	}
	delete(state.entries, key)
	if entry.lruElement != nil {
		state.lru.Remove(entry.lruElement)
	}
	state.usedBytes -= entry.accountedBytes
}

func breakerKeyForRequest(r *http.Request) breakerKey {
	if r == nil || r.URL == nil {
		return breakerKey{}
	}
	host := strings.TrimSpace(r.Host)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	uri := r.URL.Path
	if uri == "" {
		uri = "/"
	}
	return breakerKey{host: host, uri: uri}
}

func lastUpstreamStatus(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return validUpstreamStatus(value)
	case string:
		if separator := strings.LastIndexByte(value, ','); separator >= 0 {
			value = value[separator+1:]
		}
		status, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, false
		}
		return validUpstreamStatus(status)
	default:
		return 0, false
	}
}

func validUpstreamStatus(status int) (int, bool) {
	return status, status >= 100 && status <= 999
}

func breakerSeconds(unhealthyCount, failures, maximum int) int {
	failureTimes := max(unhealthyCount/failures, 1)
	seconds := 2
	for range failureTimes - 1 {
		if seconds >= maximum || seconds > maximum/2 {
			return maximum
		}
		seconds *= 2
	}
	if seconds > maximum {
		return maximum
	}
	return seconds
}

func resolveHeaderValue(r *http.Request, value string) string {
	return base.ResolveRequestVariables(value, func(name string) string {
		return base.RequestVar(r, name, 0)
	})
}
