package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ActiveHealthConfig is the bounded subset of APISIX checks.active used by the
// Go-native active probe owner. Zero thresholds select the official defaults.
type ActiveHealthConfig struct {
	Type               string
	HTTPPath           string
	Host               string
	Timeout            time.Duration
	Concurrency        int
	HealthyInterval    time.Duration
	UnhealthyInterval  time.Duration
	HealthySuccesses   int
	UnhealthyHTTPFails int
	UnhealthyTCPFails  int
	UnhealthyTimeouts  int
	HealthyStatuses    map[int]struct{}
	UnhealthyStatuses  map[int]struct{}
}

type activeProbeResult uint8

const (
	activeProbeSuccess activeProbeResult = iota
	activeProbeHTTPFailure
	activeProbeTCPFailure
	activeProbeTimeout
	activeProbeCanceled
)

// ParseActiveHealthConfig parses the supported APISIX active HTTP/HTTPS probe
// subset. The enabled result is false when checks.active is absent, so callers
// can skip probe scheduling without treating a missing block as an error.
func ParseActiveHealthConfig(checks map[string]any) (ActiveHealthConfig, bool, error) {
	if checks == nil {
		return ActiveHealthConfig{}, false, nil
	}
	rawActive, exists := checks["active"]
	if !exists || rawActive == nil {
		return ActiveHealthConfig{}, false, nil
	}
	active, err := healthMap(rawActive, "checks.active")
	if err != nil {
		return ActiveHealthConfig{}, false, err
	}

	config := ActiveHealthConfig{
		Type:               "http",
		HTTPPath:           "/",
		Timeout:            time.Second,
		Concurrency:        10,
		HealthyInterval:    time.Second,
		UnhealthyInterval:  time.Second,
		HealthySuccesses:   2,
		UnhealthyHTTPFails: 5,
		UnhealthyTCPFails:  2,
		UnhealthyTimeouts:  3,
		HealthyStatuses:    defaultActiveHealthyStatuses(),
		UnhealthyStatuses:  map[int]struct{}{429: {}, 500: {}, 503: {}},
	}

	if rawType, exists := active["type"]; exists {
		value, ok := rawType.(string)
		if !ok {
			return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.type must be a string")
		}
		config.Type = strings.ToLower(value)
	}
	if config.Type != "http" && config.Type != "https" {
		return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.type %q is unsupported", config.Type)
	}

	for _, item := range []struct {
		key  string
		dest *string
	}{
		{key: "http_path", dest: &config.HTTPPath},
		{key: "host", dest: &config.Host},
	} {
		if rawValue, exists := active[item.key]; exists {
			value, ok := rawValue.(string)
			if !ok {
				return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.%s must be a string", item.key)
			}
			*item.dest = value
		}
	}
	if config.HTTPPath == "" {
		return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.http_path must not be empty")
	}

	if rawTimeout, exists := active["timeout"]; exists {
		seconds, err := nonNegativeInt(rawTimeout, "checks.active.timeout")
		if err != nil {
			return ActiveHealthConfig{}, false, err
		}
		config.Timeout = time.Duration(seconds) * time.Second
	}
	if config.Timeout <= 0 {
		return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.timeout must be positive")
	}

	if rawConcurrency, exists := active["concurrency"]; exists {
		concurrency, err := nonNegativeInt(rawConcurrency, "checks.active.concurrency")
		if err != nil {
			return ActiveHealthConfig{}, false, err
		}
		config.Concurrency = concurrency
	}
	if config.Concurrency <= 0 {
		return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.concurrency must be positive")
	}

	if rawHealthy, exists := active["healthy"]; exists && rawHealthy != nil {
		healthy, err := healthMap(rawHealthy, "checks.active.healthy")
		if err != nil {
			return ActiveHealthConfig{}, false, err
		}
		if rawInterval, exists := healthy["interval"]; exists {
			interval, err := nonNegativeInt(rawInterval, "checks.active.healthy.interval")
			if err != nil {
				return ActiveHealthConfig{}, false, err
			}
			config.HealthyInterval = time.Duration(interval) * time.Second
		}
		if rawSuccesses, exists := healthy["successes"]; exists {
			successes, err := nonNegativeInt(rawSuccesses, "checks.active.healthy.successes")
			if err != nil {
				return ActiveHealthConfig{}, false, err
			}
			config.HealthySuccesses = successes
		}
		if rawStatuses, exists := healthy["http_statuses"]; exists {
			statuses, err := parseStatusSet(rawStatuses, "checks.active.healthy.http_statuses")
			if err != nil {
				return ActiveHealthConfig{}, false, err
			}
			config.HealthyStatuses = statuses
		}
	}
	if config.HealthyInterval <= 0 {
		return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.healthy.interval must be positive")
	}

	if rawUnhealthy, exists := active["unhealthy"]; exists && rawUnhealthy != nil {
		unhealthy, err := healthMap(rawUnhealthy, "checks.active.unhealthy")
		if err != nil {
			return ActiveHealthConfig{}, false, err
		}
		if rawInterval, exists := unhealthy["interval"]; exists {
			interval, err := nonNegativeInt(rawInterval, "checks.active.unhealthy.interval")
			if err != nil {
				return ActiveHealthConfig{}, false, err
			}
			config.UnhealthyInterval = time.Duration(interval) * time.Second
		}
		for _, item := range []struct {
			key  string
			dest *int
		}{
			{key: "http_failures", dest: &config.UnhealthyHTTPFails},
			{key: "tcp_failures", dest: &config.UnhealthyTCPFails},
			{key: "timeouts", dest: &config.UnhealthyTimeouts},
		} {
			if rawValue, exists := unhealthy[item.key]; exists {
				value, err := nonNegativeInt(rawValue, "checks.active.unhealthy."+item.key)
				if err != nil {
					return ActiveHealthConfig{}, false, err
				}
				*item.dest = value
			}
		}
		if rawStatuses, exists := unhealthy["http_statuses"]; exists {
			statuses, err := parseStatusSet(rawStatuses, "checks.active.unhealthy.http_statuses")
			if err != nil {
				return ActiveHealthConfig{}, false, err
			}
			config.UnhealthyStatuses = statuses
		}
	}
	if config.UnhealthyInterval <= 0 {
		return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.unhealthy.interval must be positive")
	}
	if config.HealthySuccesses <= 0 {
		return ActiveHealthConfig{}, false, fmt.Errorf("checks.active.healthy.successes must be positive")
	}

	return config, true, nil
}

type healthChecker interface {
	Close()
}

// activeHealthChecker schedules one goroutine per target and bounds concurrent
// network probes with a semaphore. Probes use their own request context and
// the cluster's verified base transport, never the retry wrapper, so a failing
// probe cannot trigger retries or consume in-flight tokens.
type activeHealthChecker struct {
	config     ActiveHealthConfig
	targets    []string
	lb         *HealthAwareLoadBalance
	observer   ClusterObserver
	name       string
	transport  http.RoundTripper
	semaphore  chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	closeOnce  sync.Once
	wg         sync.WaitGroup
	httpClient *http.Client
}

func newActiveHealthChecker(
	config ActiveHealthConfig,
	lb *HealthAwareLoadBalance,
	targets map[string]int,
	name string,
	observer ClusterObserver,
	base http.RoundTripper,
) *activeHealthChecker {
	if config.Concurrency <= 0 {
		config.Concurrency = 10
	}
	ctx, cancel := context.WithCancel(context.Background())
	targetList := make([]string, 0, len(targets))
	for target := range targets {
		targetList = append(targetList, target)
	}
	return &activeHealthChecker{
		config:    config,
		targets:   targetList,
		lb:        lb,
		name:      name,
		observer:  observer,
		transport: base,
		semaphore: make(chan struct{}, config.Concurrency),
		ctx:       ctx,
		cancel:    cancel,
		httpClient: &http.Client{
			Transport: base,
			Timeout:   0, // per-request context bounds each probe
		},
	}
}

// Start launches one probe goroutine per target.
func (c *activeHealthChecker) Start() {
	for _, target := range c.targets {
		c.wg.Add(1)
		go c.probeTarget(target)
	}
}

// Close cancels the root context and waits for every probe goroutine to exit.
func (c *activeHealthChecker) Close() {
	c.closeOnce.Do(func() {
		c.cancel()
		c.wg.Wait()
	})
}

func (c *activeHealthChecker) probeTarget(target string) {
	defer c.wg.Done()
	var consecutiveSuccesses int
	var consecutiveHTTPFailures int
	var consecutiveTCPFailures int
	var consecutiveTimeouts int
	for {
		healthy := c.lb.IsHealthy(target)
		interval := c.config.UnhealthyInterval
		if healthy {
			interval = c.config.HealthyInterval
		}
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(interval):
		}
		if !c.acquireSemaphore() {
			return
		}
		result := c.probeResult(target)
		c.releaseSemaphore()
		if result == activeProbeCanceled || c.ctx.Err() != nil {
			return
		}
		if result == activeProbeSuccess {
			consecutiveSuccesses++
			consecutiveHTTPFailures = 0
			consecutiveTCPFailures = 0
			consecutiveTimeouts = 0
			if !healthy && consecutiveSuccesses >= c.config.HealthySuccesses {
				if c.lb.MarkHealthy(target) {
					c.observer.SetHealth(c.name, target, true)
				}
			}
			continue
		}
		consecutiveSuccesses = 0
		threshold := 0
		failures := 0
		switch result {
		case activeProbeHTTPFailure:
			consecutiveHTTPFailures++
			threshold = c.config.UnhealthyHTTPFails
			failures = consecutiveHTTPFailures
		case activeProbeTCPFailure:
			consecutiveTCPFailures++
			threshold = c.config.UnhealthyTCPFails
			failures = consecutiveTCPFailures
		case activeProbeTimeout:
			consecutiveTimeouts++
			threshold = c.config.UnhealthyTimeouts
			failures = consecutiveTimeouts
		}
		if healthy && threshold > 0 && failures >= threshold {
			if c.lb.MarkUnhealthy(target) {
				c.observer.SetHealth(c.name, target, false)
			}
		}
	}
}

func (c *activeHealthChecker) acquireSemaphore() bool {
	select {
	case c.semaphore <- struct{}{}:
		return true
	case <-c.ctx.Done():
		return false
	}
}

func (c *activeHealthChecker) releaseSemaphore() {
	<-c.semaphore
}

func (c *activeHealthChecker) probeOnce(target string) bool {
	return c.probeResult(target) == activeProbeSuccess
}

func (c *activeHealthChecker) probeResult(target string) activeProbeResult {
	if c.ctx.Err() != nil {
		return activeProbeCanceled
	}
	return c.probeHTTP(target)
}

func (c *activeHealthChecker) probeHTTP(target string) activeProbeResult {
	targetURL, err := url.Parse(target)
	if err != nil {
		return activeProbeTCPFailure
	}
	probePath, err := url.Parse(c.config.HTTPPath)
	if err != nil {
		return activeProbeTCPFailure
	}
	targetURL.Scheme = c.config.Type
	targetURL.Path = probePath.Path
	targetURL.RawPath = probePath.RawPath
	if probePath.RawQuery != "" {
		targetURL.RawQuery = probePath.RawQuery
		targetURL.ForceQuery = probePath.ForceQuery
	}
	targetURL.Fragment = ""

	request, err := http.NewRequestWithContext(c.ctx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return activeProbeTCPFailure
	}
	if c.config.Host != "" {
		request.Host = c.config.Host
	}
	probeCtx, cancel := context.WithTimeout(c.ctx, c.config.Timeout)
	defer cancel()
	request = request.WithContext(probeCtx)

	response, err := c.httpClient.Do(request)
	if err != nil {
		if c.ctx.Err() != nil {
			return activeProbeCanceled
		}
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return activeProbeTimeout
		}
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
			return activeProbeTimeout
		}
		return activeProbeTCPFailure
	}
	defer func() { _ = response.Body.Close() }()
	if _, unhealthy := c.config.UnhealthyStatuses[response.StatusCode]; unhealthy {
		return activeProbeHTTPFailure
	}
	if _, healthy := c.config.HealthyStatuses[response.StatusCode]; !healthy {
		return activeProbeHTTPFailure
	}
	// Drain at most 4 KiB so the connection is reusable without buffering an
	// unbounded upstream response.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4*1024))
	return activeProbeSuccess
}

func defaultActiveHealthyStatuses() map[int]struct{} {
	return map[int]struct{}{http.StatusOK: {}, http.StatusFound: {}}
}
