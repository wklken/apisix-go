package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/smallnest/weighted"
)

// HealthReporter receives passive upstream outcomes from the route/protocol
// owner. It intentionally has no active-probe method: probe scheduling and
// protocol-specific health semantics remain outside this shared abstraction.
type HealthReporter interface {
	ReportHTTP(target string, status int)
	ReportTCPFailure(target string, timeout bool)
}

type healthRequestState struct {
	reporter HealthReporter
	target   string
	mu       sync.RWMutex
}

type healthRequestContextKey struct{}

// WithHealthReporter attaches the selected upstream's passive-health owner to
// a request. The pointer state survives ReverseProxy request cloning and is
// also available to protocol terminals that execute inside the route.
func WithHealthReporter(r *http.Request, reporter HealthReporter) *http.Request {
	if r == nil {
		return r
	}
	state, ok := r.Context().Value(healthRequestContextKey{}).(*healthRequestState)
	if !ok {
		state = &healthRequestState{}
		r = r.WithContext(context.WithValue(r.Context(), healthRequestContextKey{}, state))
	}
	state.mu.Lock()
	state.reporter = reporter
	state.mu.Unlock()
	return r
}

// SetSelectedTarget records the exact selector key returned by LoadBalancer.
// It is deliberately separate from URL parsing so Dubbo terminals can return
// host-only connection targets while health state remains keyed by the URI.
func SetSelectedTarget(r *http.Request, target string) {
	if r == nil {
		return
	}
	state, ok := r.Context().Value(healthRequestContextKey{}).(*healthRequestState)
	if !ok {
		return
	}
	state.mu.Lock()
	state.target = target
	state.mu.Unlock()
}

func ReportHTTPOutcome(r *http.Request, status int) {
	state, ok := healthStateFromRequest(r)
	if !ok || state.reporter == nil || state.target == "" {
		return
	}
	state.reporter.ReportHTTP(state.target, status)
}

func ReportTCPFailureOutcome(r *http.Request, timeout bool) {
	state, ok := healthStateFromRequest(r)
	if !ok || state.reporter == nil || state.target == "" {
		return
	}
	state.reporter.ReportTCPFailure(state.target, timeout)
}

func healthStateFromRequest(r *http.Request) (*healthRequestState, bool) {
	if r == nil {
		return nil, false
	}
	state, ok := r.Context().Value(healthRequestContextKey{}).(*healthRequestState)
	if !ok {
		return nil, false
	}
	state.mu.RLock()
	reporter, target := state.reporter, state.target
	state.mu.RUnlock()
	if reporter == nil || target == "" {
		return nil, false
	}
	return &healthRequestState{reporter: reporter, target: target}, true
}

// NewUpstreamLoadBalance builds the common upstream selector. A passive
// checks block enables local health state; active-only checks retain the
// existing weighted selector until an explicit active-probe owner exists.
func NewUpstreamLoadBalance(servers map[string]int, checks map[string]any) (LoadBalancer, error) {
	if _, hasPassive := checks["passive"]; !hasPassive {
		return NewWeightedRRLoadBalance(servers), nil
	}
	return NewHealthAwareLoadBalance(servers, checks)
}

// PassiveHealthConfig is the bounded subset of APISIX checks.passive used by
// observed HTTP/TCP outcomes. A zero threshold disables that failure class.
type PassiveHealthConfig struct {
	Type              string
	HealthyStatuses   map[int]struct{}
	UnhealthyStatuses map[int]struct{}
	HTTPFailures      int
	TCPFailures       int
	Timeouts          int
}

type healthState struct {
	httpFailures int
	tcpFailures  int
	timeouts     int
	unhealthy    bool
}

// HealthAwareLoadBalance preserves weighted round-robin selection while
// excluding passively unhealthy targets. If every target is unhealthy, it
// deliberately fails open and returns the next configured target, matching
// APISIX's documented availability behavior for an exhausted pool.
type HealthAwareLoadBalance struct {
	selector *RRLoadBalance
	targets  []string
	states   map[string]*healthState
	config   PassiveHealthConfig
	mu       sync.Mutex
}

func NewHealthAwareLoadBalance(servers map[string]int, checks map[string]any) (*HealthAwareLoadBalance, error) {
	config, err := parsePassiveHealthConfig(checks)
	if err != nil {
		return nil, err
	}

	targets := make([]string, 0, len(servers))
	for target := range servers {
		targets = append(targets, target)
	}
	sort.Strings(targets)

	selector := &RRLoadBalance{w: &weighted.SW{}}
	for _, target := range targets {
		selector.w.Add(target, servers[target])
	}

	states := make(map[string]*healthState, len(targets))
	for _, target := range targets {
		states[target] = &healthState{}
	}
	return &HealthAwareLoadBalance{
		selector: selector,
		targets:  targets,
		states:   states,
		config:   config,
	}, nil
}

func (lb *HealthAwareLoadBalance) Next() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if len(lb.targets) == 0 {
		return ""
	}

	for range lb.targets {
		target := lb.selector.Next()
		if target == "" || !lb.states[target].unhealthy {
			return target
		}
	}

	// APISIX keeps forwarding when no healthy node is available. The extra
	// selection is intentional: all prior candidates were quarantined.
	return lb.selector.Next()
}

func (lb *HealthAwareLoadBalance) ReportHTTP(target string, status int) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if lb.config.Type == "tcp" {
		return
	}
	state, ok := lb.states[target]
	if !ok || state.unhealthy {
		return
	}
	if _, unhealthy := lb.config.UnhealthyStatuses[status]; unhealthy {
		state.httpFailures++
		if lb.config.HTTPFailures > 0 && state.httpFailures >= lb.config.HTTPFailures {
			state.unhealthy = true
		}
		return
	}
	if _, healthy := lb.config.HealthyStatuses[status]; healthy {
		state.httpFailures = 0
		state.tcpFailures = 0
		state.timeouts = 0
	}
}

func (lb *HealthAwareLoadBalance) ReportTCPFailure(target string, timeout bool) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	state, ok := lb.states[target]
	if !ok || state.unhealthy {
		return
	}
	if timeout {
		state.timeouts++
		if lb.config.Timeouts > 0 && state.timeouts >= lb.config.Timeouts {
			state.unhealthy = true
		}
		return
	}
	state.tcpFailures++
	if lb.config.TCPFailures > 0 && state.tcpFailures >= lb.config.TCPFailures {
		state.unhealthy = true
	}
}

func (lb *HealthAwareLoadBalance) IsHealthy(target string) bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	state, ok := lb.states[target]
	return ok && !state.unhealthy
}

func parsePassiveHealthConfig(checks map[string]any) (PassiveHealthConfig, error) {
	config := PassiveHealthConfig{
		Type:              "http",
		HealthyStatuses:   defaultHealthyStatuses(),
		UnhealthyStatuses: map[int]struct{}{429: {}, 500: {}, 503: {}},
		HTTPFailures:      5,
		TCPFailures:       2,
		Timeouts:          7,
	}
	if checks == nil {
		return config, nil
	}
	passive, ok := checks["passive"]
	if !ok || passive == nil {
		return config, nil
	}
	passiveMap, err := healthMap(passive, "checks.passive")
	if err != nil {
		return config, err
	}
	if rawType, exists := passiveMap["type"]; exists {
		value, ok := rawType.(string)
		if !ok {
			return config, fmt.Errorf("checks.passive.type must be a string")
		}
		config.Type = strings.ToLower(value)
	}
	if config.Type != "http" && config.Type != "https" && config.Type != "tcp" {
		return config, fmt.Errorf("checks.passive.type %q is unsupported", config.Type)
	}

	if rawHealthy, exists := passiveMap["healthy"]; exists && rawHealthy != nil {
		healthy, err := healthMap(rawHealthy, "checks.passive.healthy")
		if err != nil {
			return config, err
		}
		if rawStatuses, exists := healthy["http_statuses"]; exists {
			config.HealthyStatuses, err = parseStatusSet(rawStatuses, "checks.passive.healthy.http_statuses")
			if err != nil {
				return config, err
			}
		}
	}
	if rawUnhealthy, exists := passiveMap["unhealthy"]; exists && rawUnhealthy != nil {
		unhealthy, err := healthMap(rawUnhealthy, "checks.passive.unhealthy")
		if err != nil {
			return config, err
		}
		if rawStatuses, exists := unhealthy["http_statuses"]; exists {
			config.UnhealthyStatuses, err = parseStatusSet(rawStatuses, "checks.passive.unhealthy.http_statuses")
			if err != nil {
				return config, err
			}
		}
		for key, destination := range map[string]*int{
			"http_failures": &config.HTTPFailures,
			"tcp_failures":  &config.TCPFailures,
			"timeouts":      &config.Timeouts,
		} {
			if rawValue, exists := unhealthy[key]; exists {
				value, err := nonNegativeInt(rawValue, "checks.passive.unhealthy."+key)
				if err != nil {
					return config, err
				}
				*destination = value
			}
		}
	}
	return config, nil
}

func healthMap(value any, field string) (map[string]any, error) {
	if result, ok := value.(map[string]any); ok {
		return result, nil
	}
	return nil, fmt.Errorf("%s must be an object", field)
}

func parseStatusSet(value any, field string) (map[int]struct{}, error) {
	values, ok := value.([]any)
	if !ok {
		if ints, ok := value.([]int); ok {
			values = make([]any, len(ints))
			for index, item := range ints {
				values[index] = item
			}
		} else {
			return nil, fmt.Errorf("%s must be an array", field)
		}
	}
	result := make(map[int]struct{}, len(values))
	for _, item := range values {
		status, err := nonNegativeInt(item, field)
		if err != nil {
			return nil, err
		}
		if status < 100 || status > 599 {
			return nil, fmt.Errorf("%s value %d must be between 100 and 599", field, status)
		}
		result[status] = struct{}{}
	}
	return result, nil
}

func nonNegativeInt(value any, field string) (int, error) {
	var result int
	switch typed := value.(type) {
	case int:
		result = typed
	case int8:
		result = int(typed)
	case int16:
		result = int(typed)
	case int32:
		result = int(typed)
	case int64:
		result = int(typed)
	case uint:
		result = int(typed)
	case uint8:
		result = int(typed)
	case uint16:
		result = int(typed)
	case uint32:
		result = int(typed)
	case uint64:
		if uint64(int(typed)) != typed {
			return 0, fmt.Errorf("%s is out of range", field)
		}
		result = int(typed)
	case float64:
		result = int(typed)
		if float64(result) != typed {
			return 0, fmt.Errorf("%s must be an integer", field)
		}
	default:
		return 0, fmt.Errorf("%s must be an integer", field)
	}
	if result < 0 {
		return 0, fmt.Errorf("%s must be non-negative", field)
	}
	return result, nil
}

func defaultHealthyStatuses() map[int]struct{} {
	statuses := make(map[int]struct{}, 17)
	for status := 200; status <= 208; status++ {
		statuses[status] = struct{}{}
	}
	statuses[226] = struct{}{}
	for status := 300; status <= 308; status++ {
		statuses[status] = struct{}{}
	}
	return statuses
}
