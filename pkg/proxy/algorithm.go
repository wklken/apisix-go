package proxy

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/chash"
)

var (
	upstreamHashVarsSchema = regexp.MustCompile(
		`^((uri|server_name|server_addr|request_uri|remote_port|remote_addr|query_string|host|hostname|mqtt_client_id)|arg_[0-9a-zA-z_-]+)$`,
	)
	upstreamHashHeaderSchema = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

// ValidateAlgorithm checks the APISIX HTTP upstream selection configuration.
func ValidateAlgorithm(algorithm, hashOn, key string, keyPresent ...bool) error {
	switch algorithm {
	case "", "roundrobin", "least_conn", "ewma":
		return nil
	case "chash":
		switch hashOn {
		case "", "vars", "header", "cookie", "consumer", "vars_combinations":
		default:
			return fmt.Errorf("unsupported upstream hash_on %q", hashOn)
		}
		if hashOn != "consumer" && key == "" && (len(keyPresent) == 0 || !keyPresent[0]) {
			return fmt.Errorf("chash upstream requires key")
		}
		switch hashOn {
		case "", "vars":
			if !upstreamHashVarsSchema.MatchString(key) {
				return fmt.Errorf("invalid upstream chash vars key %q", key)
			}
		case "header", "cookie":
			if !upstreamHashHeaderSchema.MatchString(key) {
				return fmt.Errorf("invalid upstream chash %s key %q", hashOn, key)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported upstream type %q", algorithm)
	}
}

type algorithmStats struct {
	inFlight int
	ewma     float64
	touched  time.Time
}

type algorithmLoadBalance struct {
	*HealthAwareLoadBalance
	algorithm, hashOn, hashKey string
	rings                      []*chash.Ring
	stats                      map[string]*algorithmStats // protected by the health balancer's mutex
}

func newClusterLoadBalancer(config ClusterConfig) (LoadBalancer, error) {
	if err := ValidateAlgorithm(config.Type, config.HashOn, config.HashKey, config.HashKeyConfigured); err != nil {
		return nil, err
	}
	if config.Type == "" || config.Type == "roundrobin" {
		return newUpstreamLoadBalanceWithPriorities(config.Targets, config.Priorities, config.Checks)
	}
	health, err := newHealthAwareLoadBalance(config.Targets, config.Priorities, config.Checks)
	if err != nil {
		return nil, err
	}
	lb := &algorithmLoadBalance{
		HealthAwareLoadBalance: health,
		algorithm:              config.Type,
		hashOn:                 config.HashOn,
		hashKey:                config.HashKey,
		stats:                  make(map[string]*algorithmStats),
	}
	for target := range config.Targets {
		lb.stats[target] = &algorithmStats{}
	}
	for _, group := range health.groups {
		var ring *chash.Ring
		if config.Type == "chash" {
			nodes := make([]chash.Node, 0, len(group.targets))
			for _, target := range group.targets {
				parsed, err := url.Parse(target)
				if err != nil || parsed.Host == "" {
					return nil, fmt.Errorf("invalid hash upstream target")
				}
				nodes = append(nodes, chash.Node{ID: parsed.Host, Target: target, Weight: group.weights[target]})
			}
			ring, err = chash.New(nodes)
			if err != nil {
				return nil, err
			}
		}
		lb.rings = append(lb.rings, ring)
	}
	return lb, nil
}

func clusterHealthBalancer(lb LoadBalancer) *HealthAwareLoadBalance {
	switch selected := lb.(type) {
	case *HealthAwareLoadBalance:
		return selected
	case *algorithmLoadBalance:
		return selected.HealthAwareLoadBalance
	}
	return nil
}

func (lb *algorithmLoadBalance) Next() string { return lb.selectTarget(nil, nil) }

func (lb *algorithmLoadBalance) NextForRequest(r *http.Request) string {
	if r == nil {
		return lb.Next()
	}
	scope := algorithmScope(r)
	if scope == nil {
		var enriched *http.Request
		enriched, scope = withAlgorithmRequestScope(r)
		*r = *enriched
	}
	scope.finish()
	state := priorityStateForRequest(r)
	state.finishPreviousAttempt()
	target := lb.selectTarget(r, state.tried)
	state.last = target
	return target
}

func (lb *algorithmLoadBalance) selectTarget(r *http.Request, tried map[string]struct{}) string {
	hashKey := ""
	if lb.algorithm == "chash" && r != nil {
		scope := algorithmScope(r)
		var exists bool
		hashKey, exists = scope.hashes[lb]
		if !exists {
			hashKey = upstreamHashValue(r, lb.hashOn, lb.hashKey)
			if scope.hashes == nil {
				scope.hashes = make(map[*algorithmLoadBalance]string)
			}
			scope.hashes[lb] = hashKey
		}
	}
	lb.mu.Lock()
	defer lb.mu.Unlock()
	anyHealthy := false
	for _, group := range lb.groups {
		for _, target := range group.targets {
			if !lb.states[target].unhealthy {
				anyHealthy = true
			}
		}
	}
	for index, group := range lb.groups {
		candidates := make([]string, 0, len(group.targets))
		ordered := group.targets
		if lb.algorithm == "chash" {
			ordered = lb.rings[index].Candidates(hashKey)
		}
		for _, target := range ordered {
			if len(lb.targets) != 1 {
				if _, used := tried[target]; used {
					continue
				}
				if anyHealthy && lb.states[target].unhealthy {
					continue
				}
			}
			candidates = append(candidates, target)
		}
		if len(candidates) == 0 {
			continue
		}
		selected := candidates[0]
		switch lb.algorithm {
		case "least_conn":
			for _, target := range candidates[1:] {
				if float64(
					lb.stats[target].inFlight+1,
				)/float64(
					group.weights[target],
				) < float64(
					lb.stats[selected].inFlight+1,
				)/float64(
					group.weights[selected],
				) {
					selected = target
				}
			}
		case "ewma":
			if len(candidates) > 1 {
				a, b := rand.IntN(len(candidates)), rand.IntN(len(candidates)-1)
				if b >= a {
					b++
				}
				selected = candidates[a]
				now := time.Now()
				if lb.stats[candidates[b]].score(now) < lb.stats[selected].score(now) {
					selected = candidates[b]
				}
			}
		}
		if r != nil {
			lb.stats[selected].inFlight++
			algorithmScope(r).current = &algorithmAttempt{lb: lb, target: selected, started: time.Now()}
		}
		return selected
	}
	return ""
}

func (stats *algorithmStats) score(now time.Time) float64 {
	if stats.touched.IsZero() || now.Sub(stats.touched) >= 60*time.Second {
		return 0
	}
	return stats.ewma * math.Exp(-max(now.Sub(stats.touched).Seconds(), 0)/10)
}

type (
	algorithmScopeKey     struct{}
	algorithmRequestScope struct {
		current *algorithmAttempt
		hashes  map[*algorithmLoadBalance]string
	}
)

func withAlgorithmRequestScope(r *http.Request) (*http.Request, *algorithmRequestScope) {
	scope := &algorithmRequestScope{}
	return r.WithContext(context.WithValue(r.Context(), algorithmScopeKey{}, scope)), scope
}

// WithProtocolBalancing owns selections for synchronous protocol terminals that
// do not use an HTTP RoundTripper. Finalization releases even a selection whose
// request could not be encoded; only an actual response records latency.
func WithProtocolBalancing(r *http.Request) (*http.Request, func()) {
	request, scope := withAlgorithmRequestScope(r)
	return request, scope.finish
}

// BeginProtocolAttempt starts timing after local admission and framing. The
// caller reports whether it decoded a response, then releases the selection.
func BeginProtocolAttempt(ctx context.Context) func(bool) {
	scope, _ := ctx.Value(algorithmScopeKey{}).(*algorithmRequestScope)
	if scope == nil || scope.current == nil {
		return func(bool) {}
	}
	attempt := scope.current
	attempt.started = time.Now()
	return func(response bool) {
		attempt.response = response
		attempt.finish()
	}
}

func algorithmScope(r *http.Request) *algorithmRequestScope {
	scope, _ := r.Context().Value(algorithmScopeKey{}).(*algorithmRequestScope)
	return scope
}

func (scope *algorithmRequestScope) finish() {
	if scope.current != nil {
		scope.current.finish()
		scope.current = nil
	}
}

type algorithmAttempt struct {
	once           sync.Once
	lb             *algorithmLoadBalance
	target         string
	started        time.Time
	connectStarted time.Time
	connect        time.Duration
	response       bool
}

func (attempt *algorithmAttempt) finish() {
	attempt.once.Do(func() {
		attempt.lb.mu.Lock()
		defer attempt.lb.mu.Unlock()
		stats := attempt.lb.stats[attempt.target]
		stats.inFlight--
		if attempt.lb.algorithm == "ewma" && attempt.response && !attempt.started.IsZero() {
			now := time.Now()
			weight := 0.0
			if !stats.touched.IsZero() && now.Sub(stats.touched) < 60*time.Second {
				weight = math.Exp(-max(now.Sub(stats.touched).Seconds(), 0) / 10)
			}
			stats.ewma = stats.ewma*weight + (now.Sub(attempt.started)+attempt.connect).Seconds()*(1-weight)
			stats.touched = now
		}
	})
}

type algorithmTransport struct {
	base http.RoundTripper
	lb   *algorithmLoadBalance
}

func (transport *algorithmTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	scope := algorithmScope(r)
	if scope == nil || scope.current == nil || scope.current.lb != transport.lb {
		return transport.base.RoundTrip(r)
	}
	attempt := scope.current
	attempt.started = time.Now()
	// GetConn/GotConn include connection establishment and pooled acquisition.
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { attempt.connectStarted = time.Now() },
		GotConn: func(httptrace.GotConnInfo) { attempt.connect = time.Since(attempt.connectStarted) },
	}
	response, err := transport.base.RoundTrip(r.WithContext(httptrace.WithClientTrace(r.Context(), trace)))
	if err != nil {
		attempt.finish()
		return response, err
	}
	attempt.response = true
	if response == nil || response.Body == nil {
		attempt.finish()
	} else {
		response.Body = wrapReleaseBody(response.Body, attempt.finish)
	}
	return response, nil
}
