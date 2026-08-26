package ai_proxy_multi

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/httpclient"
	"github.com/wklken/apisix-go/pkg/runtime"
)

type HealthChecks struct {
	Active ActiveHealthCheck `json:"active"`
}

type ActiveHealthCheck struct {
	Type                   string               `json:"type,omitempty"`
	Timeout                float64              `json:"timeout,omitempty"`
	Concurrency            int                  `json:"concurrency,omitempty"`
	Host                   string               `json:"host,omitempty"`
	Port                   int                  `json:"port,omitempty"`
	HTTPPath               string               `json:"http_path,omitempty"`
	HTTPSVerifyCertificate *bool                `json:"https_verify_certificate,omitempty"`
	Healthy                HealthyCheckPolicy   `json:"healthy"`
	Unhealthy              UnhealthyCheckPolicy `json:"unhealthy"`
	ReqHeaders             []string             `json:"req_headers,omitempty"`
}

type HealthyCheckPolicy struct {
	Interval     int   `json:"interval,omitempty"`
	HTTPStatuses []int `json:"http_statuses,omitempty"`
	Successes    int   `json:"successes,omitempty"`
}

type UnhealthyCheckPolicy struct {
	Interval     int   `json:"interval,omitempty"`
	HTTPStatuses []int `json:"http_statuses,omitempty"`
	HTTPFailures int   `json:"http_failures,omitempty"`
	TCPFailures  int   `json:"tcp_failures,omitempty"`
	Timeouts     int   `json:"timeouts,omitempty"`
}

type instanceHealthState struct {
	healthy      bool
	successes    int
	httpFailures int
	tcpFailures  int
	timeouts     int
	nextCheck    time.Time
}

// healthSnapshot is an immutable view of the latest probe results. Requests
// read it without locking and never wait for an in-flight probe.
type healthSnapshot struct {
	healthy []bool
}

type healthProbeResult struct {
	status  int
	err     error
	timeout bool
}

type healthProbeCompletion struct {
	index     int
	result    healthProbeResult
	completed bool
}

func (p *Plugin) initHealthStates() {
	p.health = make(map[int]*instanceHealthState)
	p.healthClients = make(map[int]*http.Client)
	for index := range p.config.Instances {
		instance := &p.config.Instances[index]
		if instance.Checks == nil {
			continue
		}
		applyHealthDefaults(&instance.Checks.Active)
		p.health[index] = &instanceHealthState{healthy: true}
		check := instance.Checks.Active
		if check.Type == "http" || check.Type == "https" {
			p.healthClients[index] = newHealthCheckClient(check)
		}
	}
	p.publishHealthSnapshot()
}

// publishHealthSnapshot publishes the current probe state as an immutable
// snapshot. Instances without health checks are always healthy. It is called
// only by the plugin-owned refresher (and once at initialization), so no lock
// is required.
func (p *Plugin) publishHealthSnapshot() {
	healthy := make([]bool, len(p.config.Instances))
	for index := range healthy {
		healthy[index] = true
	}
	for index, state := range p.health {
		healthy[index] = state.healthy
	}
	p.snapshot.Store(&healthSnapshot{healthy: healthy})
}

// newHealthCheckClient builds one HTTP client for an immutable health-check
// configuration so repeated probes reuse the transport instead of cloning a
// transport per probe.
func newHealthCheckClient(check ActiveHealthCheck) *http.Client {
	transport := httpclient.NewTransport()
	if check.Type == "https" && check.HTTPSVerifyCertificate != nil && !*check.HTTPSVerifyCertificate {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	timeout := time.Duration(check.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = time.Second
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func (p *Plugin) healthClient(index int) *http.Client {
	return p.healthClients[index]
}

func (p *Plugin) Stop() {
	p.healthCloseOnce.Do(func() {
		p.stoppedHealth.Store(true)
		p.healthMu.Lock()
		clients := make([]*http.Client, 0, len(p.healthClients))
		for _, client := range p.healthClients {
			clients = append(clients, client)
		}
		p.healthClients = nil
		p.healthMu.Unlock()

		for _, client := range clients {
			client.CloseIdleConnections()
		}
	})
}

func applyHealthDefaults(check *ActiveHealthCheck) {
	if check.Type == "" {
		check.Type = "http"
	}
	if check.Timeout == 0 {
		check.Timeout = 1
	}
	if check.Concurrency == 0 {
		check.Concurrency = 10
	}
	if check.HTTPPath == "" {
		check.HTTPPath = "/"
	}
	if check.HTTPSVerifyCertificate == nil {
		verify := true
		check.HTTPSVerifyCertificate = &verify
	}
	if check.Healthy.Interval == 0 {
		check.Healthy.Interval = 1
	}
	if len(check.Healthy.HTTPStatuses) == 0 {
		check.Healthy.HTTPStatuses = []int{http.StatusOK, http.StatusFound}
	}
	if check.Healthy.Successes == 0 {
		check.Healthy.Successes = 2
	}
	if check.Unhealthy.Interval == 0 {
		check.Unhealthy.Interval = 1
	}
	if len(check.Unhealthy.HTTPStatuses) == 0 {
		check.Unhealthy.HTTPStatuses = []int{429, 404, 500, 501, 502, 503, 504, 505}
	}
	if check.Unhealthy.HTTPFailures == 0 {
		check.Unhealthy.HTTPFailures = 5
	}
	if check.Unhealthy.TCPFailures == 0 {
		check.Unhealthy.TCPFailures = 2
	}
	if check.Unhealthy.Timeouts == 0 {
		check.Unhealthy.Timeouts = 3
	}
}

// refreshHealth wakes the plugin-owned refresher and returns immediately.
// Probes never run on the request goroutine; selection reads the last
// published snapshot. The request context is intentionally not used for
// probes: they are bound to the plugin lifecycle instead.
func (p *Plugin) refreshHealth(_ context.Context) {
	p.wakeHealthRefresh()
}

// startHealthLoop publishes the plugin-owned refresher lifecycle before any
// request can arrive. It runs once from PostInit when at least one instance
// has active checks, so the generation task owner controls its lifetime.
func (p *Plugin) startHealthLoop() error {
	owner := p.TaskOwner()
	if owner == nil {
		return runtime.ErrTaskOwnerRequired
	}
	p.wakeHealth = make(chan struct{}, 1)
	if err := owner.Go("health-refresh", p.healthLoop); err != nil {
		p.wakeHealth = nil
		return err
	}
	return nil
}

// wakeHealthRefresh wakes the plugin-owned refresher for one refresh pass.
// Wakes coalesce: concurrent requests never start more than one probe cycle.
func (p *Plugin) wakeHealthRefresh() {
	if p.stoppedHealth.Load() || p.wakeHealth == nil {
		return
	}
	select {
	case p.wakeHealth <- struct{}{}:
	default:
	}
}

func (p *Plugin) healthLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.wakeHealth:
		}
		if ctx.Err() != nil {
			return nil
		}
		if !p.refreshHealthPass(ctx) {
			return nil
		}
	}
}

// refreshHealthPass probes every instance whose next check is due, records
// the results, and publishes one immutable snapshot.
func (p *Plugin) refreshHealthPass(ctx context.Context) bool {
	now := p.healthNow()
	due := make([]int, 0)
	for index, state := range p.health {
		if !now.Before(state.nextCheck) {
			due = append(due, index)
		}
	}
	if ctx.Err() != nil {
		return false
	}

	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan chan healthProbeCompletion, len(due))
	completed := make([]healthProbeCompletion, 0, len(due))
	admitted := 0
	completePass := true
	owner := p.TaskOwner()
	if owner == nil {
		return false
	}
	for _, index := range due {
		completion := make(chan healthProbeCompletion, 1)
		err := owner.Go("health-probe", func(context.Context) error {
			marker := healthProbeCompletion{index: index}
			defer func() {
				completion <- marker
				ready <- completion
			}()
			if p.probeForTest != nil {
				marker.result = p.probeForTest(passCtx, index)
			} else {
				marker.result = p.probeInstance(passCtx, index)
			}
			if passCtx.Err() != nil {
				return nil
			}
			marker.completed = true
			return nil
		})
		if err != nil {
			completePass = false
			cancel()
			break
		}
		admitted++
	}
	for range admitted {
		completion := <-ready
		marker := <-completion
		if !marker.completed {
			completePass = false
			cancel()
		}
		completed = append(completed, marker)
	}
	if !completePass || admitted != len(due) || ctx.Err() != nil {
		return false
	}
	for _, marker := range completed {
		p.recordProbeResult(marker.index, marker.result, p.healthNow())
	}
	p.publishHealthSnapshot()
	return true
}

func (p *Plugin) probeInstance(ctx context.Context, index int) healthProbeResult {
	instance := p.config.Instances[index]
	check := instance.Checks.Active
	timeout := time.Duration(check.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = time.Second
	}
	probeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target, err := healthTarget(instance, check)
	if err != nil {
		return healthProbeResult{err: err}
	}
	if check.Type == "tcp" {
		dialer := &net.Dialer{}
		connection, err := dialer.DialContext(probeContext, "tcp", target.Host)
		if err != nil {
			return healthProbeResult{err: err, timeout: probeContext.Err() == context.DeadlineExceeded}
		}
		_ = connection.Close()
		return healthProbeResult{status: http.StatusOK}
	}

	request, err := http.NewRequestWithContext(probeContext, http.MethodGet, target.String(), nil)
	if err != nil {
		return healthProbeResult{err: err}
	}
	for name, value := range instance.Auth.Header {
		request.Header.Set(name, value)
	}
	for _, rawHeader := range check.ReqHeaders {
		name, value, ok := strings.Cut(rawHeader, ":")
		if ok && request.Header.Get(strings.TrimSpace(name)) == "" {
			request.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
		}
	}
	response, err := p.healthClient(index).Do(request)
	if err != nil {
		return healthProbeResult{err: err, timeout: probeContext.Err() == context.DeadlineExceeded}
	}
	defer func() { _ = response.Body.Close() }()
	return healthProbeResult{status: response.StatusCode}
}

func healthTarget(instance Instance, check ActiveHealthCheck) (*url.URL, error) {
	if check.Host != "" {
		scheme := check.Type
		if scheme == "tcp" {
			scheme = "tcp"
		} else if scheme != "https" {
			scheme = "http"
		}
		host := check.Host
		if check.Port > 0 {
			host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(check.Port))
		}
		return healthURL(scheme, host, check.HTTPPath, instance.Auth.Query), nil
	}
	base, err := instanceHealthBaseURL(instance)
	if err != nil {
		return nil, err
	}
	switch check.Type {
	case "tcp":
		base.Scheme = "tcp"
	case "http", "https":
		base.Scheme = check.Type
	}
	if check.Port > 0 {
		base.Host = net.JoinHostPort(base.Hostname(), strconv.Itoa(check.Port))
	}
	base.Path = check.HTTPPath
	query := base.Query()
	for name, value := range instance.Auth.Query {
		query.Set(name, value)
	}
	base.RawQuery = query.Encode()
	return base, nil
}

func healthURL(scheme, host, path string, authQuery map[string]string) *url.URL {
	target := &url.URL{Scheme: scheme, Host: host, Path: path}
	query := target.Query()
	for name, value := range authQuery {
		query.Set(name, value)
	}
	target.RawQuery = query.Encode()
	return target
}

func instanceHealthBaseURL(instance Instance) (*url.URL, error) {
	if instance.Override.Endpoint != "" {
		return url.Parse(instance.Override.Endpoint)
	}
	var raw string
	switch instance.Provider {
	case "openai":
		raw = "https://api.openai.com"
	case "deepseek":
		raw = "https://api.deepseek.com"
	case "aimlapi":
		raw = "https://api.aimlapi.com"
	case "openrouter":
		raw = "https://openrouter.ai"
	case "gemini":
		raw = "https://generativelanguage.googleapis.com"
	case "anthropic":
		raw = "https://api.anthropic.com"
	case "vertex-ai":
		region, _ := instance.ProviderConf["region"].(string)
		raw = "https://" + region + "-aiplatform.googleapis.com"
	case "bedrock":
		region, _ := instance.ProviderConf["region"].(string)
		raw = "https://bedrock-runtime." + region + ".amazonaws.com"
	default:
		return nil, fmt.Errorf("instance %q requires override.endpoint for health checks", instance.Name)
	}
	return url.Parse(raw)
}

func (p *Plugin) recordProbeResult(index int, result healthProbeResult, now time.Time) {
	state := p.health[index]
	if state == nil {
		return
	}
	check := p.config.Instances[index].Checks.Active
	success := result.err == nil && slices.Contains(check.Healthy.HTTPStatuses, result.status)
	failure := result.err != nil || slices.Contains(check.Unhealthy.HTTPStatuses, result.status)
	if success {
		state.successes++
		state.httpFailures, state.tcpFailures, state.timeouts = 0, 0, 0
		if state.successes >= check.Healthy.Successes {
			state.healthy = true
		}
	} else if failure {
		state.successes = 0
		if result.timeout {
			state.timeouts++
		} else if check.Type == "tcp" || result.err != nil {
			state.tcpFailures++
		} else {
			state.httpFailures++
		}
		if state.httpFailures >= check.Unhealthy.HTTPFailures ||
			state.tcpFailures >= check.Unhealthy.TCPFailures || state.timeouts >= check.Unhealthy.Timeouts {
			state.healthy = false
		}
	}
	interval := check.Healthy.Interval
	if !state.healthy {
		interval = check.Unhealthy.Interval
	}
	state.nextCheck = now.Add(time.Duration(interval) * time.Second)
}

// instanceHealthy reads the last published snapshot; stale-but-valid health
// stays readable while a refresh is in flight.
func (p *Plugin) instanceHealthy(index int) bool {
	snapshot := p.snapshot.Load()
	if snapshot == nil {
		return true
	}
	if index < 0 || index >= len(snapshot.healthy) {
		return true
	}
	return snapshot.healthy[index]
}
