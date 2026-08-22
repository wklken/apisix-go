package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
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
	if r == nil || reporter == nil {
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

// NewUpstreamLoadBalance builds the common upstream selector. A passive or
// active checks block enables local health state; active probes can then
// recover or quarantine targets through the health-aware selector. An empty
// node pool is a construction error: the empty round-robin picker used to
// panic on the first Next() type assertion.
func NewUpstreamLoadBalance(servers map[string]int, checks map[string]any) (LoadBalancer, error) {
	return newUpstreamLoadBalanceWithPriorities(servers, nil, checks)
}

// NewUpstreamLoadBalanceWithPriorities builds the common upstream selector
// while preserving APISIX node priority groups.
func NewUpstreamLoadBalanceWithPriorities(
	servers map[string]int,
	priorities map[string]int,
	checks map[string]any,
) (LoadBalancer, error) {
	return newUpstreamLoadBalanceWithPriorities(servers, priorities, checks)
}

func newUpstreamLoadBalanceWithPriorities(
	servers map[string]int,
	priorities map[string]int,
	checks map[string]any,
) (LoadBalancer, error) {
	if len(servers) == 0 {
		return nil, fmt.Errorf("cannot build upstream load balancer without nodes")
	}
	groups := newPriorityGroups(servers, priorities)
	if len(groups) == 0 {
		return nil, fmt.Errorf("cannot build upstream load balancer without positive-weight nodes")
	}
	healthChecksConfigured := false
	if _, hasPassive := checks["passive"]; hasPassive {
		healthChecksConfigured = true
	}
	if _, hasActive := checks["active"]; hasActive {
		healthChecksConfigured = true
	}
	if len(groups) > 1 {
		if !healthChecksConfigured {
			return &priorityLoadBalance{groups: groups}, nil
		}
		return newHealthAwareLoadBalance(servers, priorities, checks)
	}
	if !healthChecksConfigured {
		return &priorityLoadBalance{groups: groups}, nil
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
	groups           []priorityGroup
	healthySelectors []*RRLoadBalance
	targets          []string
	states           map[string]*healthState
	config           PassiveHealthConfig
	name             string
	observer         ClusterObserver
	mu               sync.Mutex
}

func NewHealthAwareLoadBalance(servers map[string]int, checks map[string]any) (*HealthAwareLoadBalance, error) {
	return newHealthAwareLoadBalance(servers, nil, checks)
}

func newHealthAwareLoadBalance(
	servers map[string]int,
	priorities map[string]int,
	checks map[string]any,
) (*HealthAwareLoadBalance, error) {
	config, err := parsePassiveHealthConfig(checks)
	if err != nil {
		return nil, err
	}

	groups := newPriorityGroups(servers, priorities)
	targets := make([]string, 0, len(servers))
	for target := range servers {
		targets = append(targets, target)
	}
	sort.Strings(targets)

	states := make(map[string]*healthState, len(targets))
	for _, target := range targets {
		states[target] = &healthState{}
	}
	lb := &HealthAwareLoadBalance{
		groups:  groups,
		targets: targets,
		states:  states,
		config:  config,
	}
	lb.refreshHealthySelectorsLocked()
	return lb, nil
}

func (lb *HealthAwareLoadBalance) Next() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if len(lb.targets) == 0 {
		return ""
	}

	if target, ok := lb.nextHealthyLocked(); ok {
		return target
	}

	// APISIX keeps forwarding when no healthy node is available. The extra
	// selection is intentional: all prior candidates were quarantined. When
	// priorities are configured, fail-open starts with the highest group.
	if len(lb.groups) == 0 {
		return ""
	}
	return lb.groups[0].selector.Next()
}

func (lb *HealthAwareLoadBalance) NextForRequest(request *http.Request) string {
	state := priorityStateForRequest(request)
	state.finishPreviousAttempt()

	lb.mu.Lock()
	defer lb.mu.Unlock()
	anyHealthy := false
	for _, group := range lb.groups {
		for target := range group.weights {
			if !lb.states[target].unhealthy {
				anyHealthy = true
				break
			}
		}
		if anyHealthy {
			break
		}
	}
	selectable := func(target string) bool {
		return !anyHealthy || !lb.states[target].unhealthy
	}
	next := func() string {
		for _, group := range lb.groups {
			if target := group.nextUntried(state.tried, selectable); target != "" {
				return target
			}
		}
		return ""
	}
	if target := next(); target != "" {
		state.last = target
		return target
	}
	// A request may have more retries than there are eligible healthy nodes.
	// Start another cycle while retaining the healthy-only filter; only the
	// no-healthy-node case above intentionally falls open to all targets.
	clear(state.tried)
	if target := next(); target != "" {
		state.last = target
		return target
	}
	return ""
}

func (lb *HealthAwareLoadBalance) RecordSelectedTarget(request *http.Request, target string) {
	recordPriorityTargetAttempt(request, target)
}

func (lb *HealthAwareLoadBalance) nextHealthyLocked() (string, bool) {
	for _, selector := range lb.healthySelectors {
		if target := selector.Next(); target != "" {
			return target, true
		}
	}
	return "", false
}

func (lb *HealthAwareLoadBalance) refreshHealthySelectorsLocked() {
	selectors := make([]*RRLoadBalance, 0, len(lb.groups))
	for _, group := range lb.groups {
		allHealthy := true
		for target := range group.weights {
			if lb.states[target].unhealthy {
				allHealthy = false
				break
			}
		}
		if allHealthy {
			selectors = append(selectors, group.selector)
			continue
		}
		weights := make(map[string]int, len(group.weights))
		for target, weight := range group.weights {
			if !lb.states[target].unhealthy {
				weights[target] = weight
			}
		}
		selectors = append(selectors, NewWeightedRRLoadBalance(weights))
	}
	lb.healthySelectors = selectors
}

func (lb *HealthAwareLoadBalance) ReportHTTP(target string, status int) {
	lb.mu.Lock()
	if lb.config.Type == "tcp" {
		lb.mu.Unlock()
		return
	}
	state, ok := lb.states[target]
	if !ok || state.unhealthy {
		lb.mu.Unlock()
		return
	}
	becameUnhealthy := false
	if _, unhealthy := lb.config.UnhealthyStatuses[status]; unhealthy {
		state.httpFailures++
		if lb.config.HTTPFailures > 0 && state.httpFailures >= lb.config.HTTPFailures {
			state.unhealthy = true
			lb.refreshHealthySelectorsLocked()
			becameUnhealthy = true
		}
	} else if _, healthy := lb.config.HealthyStatuses[status]; healthy {
		state.httpFailures = 0
		state.tcpFailures = 0
		state.timeouts = 0
	}
	if becameUnhealthy && lb.observer != nil {
		lb.observer.SetHealth(lb.name, target, false)
	}
	lb.mu.Unlock()
}

func (lb *HealthAwareLoadBalance) ReportTCPFailure(target string, timeout bool) {
	lb.mu.Lock()
	state, ok := lb.states[target]
	if !ok || state.unhealthy {
		lb.mu.Unlock()
		return
	}
	becameUnhealthy := false
	if timeout {
		state.timeouts++
		if lb.config.Timeouts > 0 && state.timeouts >= lb.config.Timeouts {
			state.unhealthy = true
			lb.refreshHealthySelectorsLocked()
			becameUnhealthy = true
		}
	} else {
		state.tcpFailures++
		if lb.config.TCPFailures > 0 && state.tcpFailures >= lb.config.TCPFailures {
			state.unhealthy = true
			lb.refreshHealthySelectorsLocked()
			becameUnhealthy = true
		}
	}
	if becameUnhealthy && lb.observer != nil {
		lb.observer.SetHealth(lb.name, target, false)
	}
	lb.mu.Unlock()
}

func (lb *HealthAwareLoadBalance) setObserver(name string, observer ClusterObserver) {
	lb.mu.Lock()
	lb.name = name
	lb.observer = observer
	lb.mu.Unlock()
}

func (lb *HealthAwareLoadBalance) clearObserver() {
	lb.mu.Lock()
	lb.observer = nil
	lb.mu.Unlock()
}

func (lb *HealthAwareLoadBalance) IsHealthy(target string) bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	state, ok := lb.states[target]
	return ok && !state.unhealthy
}

// MarkHealthy recovers a target after a successful active probe. It reports
// whether the target actually changed from unhealthy to healthy so the caller
// can notify observers only on a state transition.
func (lb *HealthAwareLoadBalance) MarkHealthy(target string) bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	state, ok := lb.states[target]
	if !ok || !state.unhealthy {
		return false
	}
	state.unhealthy = false
	state.httpFailures = 0
	state.tcpFailures = 0
	state.timeouts = 0
	lb.refreshHealthySelectorsLocked()
	return true
}

// MarkUnhealthy quarantines a target after a failed active probe. It reports
// whether the target actually changed from healthy to unhealthy so the caller
// can notify observers only on a state transition.
func (lb *HealthAwareLoadBalance) MarkUnhealthy(target string) bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	state, ok := lb.states[target]
	if !ok || state.unhealthy {
		return false
	}
	state.unhealthy = true
	lb.refreshHealthySelectorsLocked()
	return true
}

// HealthSnapshot returns a copy of the current healthy state for every target.
// It is used by active probes to select the healthy/unhealthy probe interval
// and to decide which targets need recovery.
func (lb *HealthAwareLoadBalance) HealthSnapshot() map[string]bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	snapshot := make(map[string]bool, len(lb.states))
	for target, state := range lb.states {
		snapshot[target] = !state.unhealthy
	}
	return snapshot
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
		for _, item := range []struct {
			key  string
			dest *int
		}{
			{key: "http_failures", dest: &config.HTTPFailures},
			{key: "tcp_failures", dest: &config.TCPFailures},
			{key: "timeouts", dest: &config.Timeouts},
		} {
			if rawValue, exists := unhealthy[item.key]; exists {
				value, err := nonNegativeInt(rawValue, "checks.passive.unhealthy."+item.key)
				if err != nil {
					return config, err
				}
				*item.dest = value
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
		if typed > uint64(^uint(0)>>1) {
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
