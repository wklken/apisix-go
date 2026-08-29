package ai_proxy_multi

import (
	"context"
	"crypto/tls"
	"errors"
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
	healthy        []bool
	allNodes       [][]*resolvedNode
	healthyNodes   [][]*resolvedNode
	nodeRequired   []bool
	nodeResolveErr []error
}

type healthProbeResult struct {
	status  int
	err     error
	timeout bool
}

type healthProbeCompletion struct {
	index     int
	node      *resolvedNode
	result    healthProbeResult
	completed bool
}

const (
	maxHealthProbeWorkers = 32
	healthPassRetryDelay  = time.Second
)

var errHealthProbePanicked = errors.New("health probe panicked")

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
	allNodes := make([][]*resolvedNode, len(p.config.Instances))
	healthyNodes := make([][]*resolvedNode, len(p.config.Instances))
	nodeRequired := make([]bool, len(p.config.Instances))
	nodeResolveErr := make([]error, len(p.config.Instances))
	for index := range healthy {
		healthy[index] = true
	}
	for index, state := range p.health {
		healthy[index] = state.healthy
	}
	if nodes := p.nodeSnapshot.Load(); nodes != nil {
		copy(nodeRequired, nodes.required)
		copy(nodeResolveErr, nodes.resolveErr)
		for index, instanceNodes := range nodes.instances {
			allNodes[index] = slices.Clone(instanceNodes)
			if len(instanceNodes) == 0 {
				if nodeRequired[index] {
					healthy[index] = false
				}
				continue
			}
			if p.config.Instances[index].Checks == nil {
				healthyNodes[index] = slices.Clone(instanceNodes)
				continue
			}
			healthy[index] = false
			for _, node := range instanceNodes {
				if node.health.healthy {
					healthyNodes[index] = append(healthyNodes[index], node)
					healthy[index] = true
				}
			}
		}
	}
	p.snapshot.Store(&healthSnapshot{
		healthy: healthy, allNodes: allNodes, healthyNodes: healthyNodes,
		nodeRequired: nodeRequired, nodeResolveErr: nodeResolveErr,
	})
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
		p.healthStopping = true
		cancelHealth := p.healthCancel
		healthDone := p.healthDone
		p.healthMu.Unlock()
		if cancelHealth != nil {
			cancelHealth()
		}
		if healthDone != nil {
			<-healthDone
		}

		p.healthMu.Lock()
		clients := make([]*http.Client, 0, len(p.healthClients))
		for _, client := range p.healthClients {
			clients = append(clients, client)
		}
		p.healthClients = nil
		p.healthMu.Unlock()
		p.nodeMu.Lock()
		nodes := p.nodeSnapshot.Load()
		p.nodeMu.Unlock()

		for _, client := range clients {
			client.CloseIdleConnections()
		}
		if nodes != nil {
			for _, instanceNodes := range nodes.instances {
				for _, node := range instanceNodes {
					node.retired.Store(true)
					node.closeIdleConnections()
				}
			}
		}
		if p.client != nil {
			p.client.CloseIdleConnections()
		}
		if p.authClient != nil {
			p.authClient.CloseIdleConnections()
		}
	})
	p.stopMultiSecrets()
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
	p.healthMu.Lock()
	p.healthDone = make(chan struct{})
	p.healthStopping = false
	healthDone := p.healthDone
	p.healthMu.Unlock()
	if err := owner.Go("health-refresh", func(ctx context.Context) error {
		loopCtx, cancel := context.WithCancel(ctx)
		p.healthMu.Lock()
		p.healthCancel = cancel
		stopping := p.healthStopping
		p.healthMu.Unlock()
		if stopping {
			cancel()
		}
		defer func() {
			cancel()
			p.healthMu.Lock()
			p.healthCancel = nil
			p.healthMu.Unlock()
			close(healthDone)
		}()
		return p.healthLoop(loopCtx)
	}); err != nil {
		close(healthDone)
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
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	started := false
	retrying := false
	for {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		wake := p.wakeHealth
		var scheduled <-chan time.Time
		if started {
			delay, ok := p.nextHealthRefreshDelay()
			if retrying {
				wake = nil
				if !ok || delay < healthPassRetryDelay {
					delay = healthPassRetryDelay
					ok = true
				}
			}
			if ok {
				timer.Reset(delay)
				scheduled = timer.C
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-scheduled:
		}
		if ctx.Err() != nil {
			return nil
		}
		started = true
		retrying = !p.refreshHealthPass(ctx)
	}
}

func (p *Plugin) nextHealthRefreshDelay() (time.Duration, bool) {
	now := time.Now()
	if p.healthNow != nil {
		now = p.healthNow()
	}
	p.nodeMu.Lock()
	defer p.nodeMu.Unlock()

	var deadline time.Time
	record := func(candidate time.Time) {
		if deadline.IsZero() || candidate.Before(deadline) {
			deadline = candidate
		}
	}
	for index := range p.config.Instances {
		state, checked := p.health[index]
		if p.nodeRequired[index] {
			record(p.nodeExpires[index])
			if checked {
				for _, node := range p.nodeSets[index] {
					record(node.health.nextCheck)
				}
			}
			continue
		}
		if checked {
			record(state.nextCheck)
		}
	}
	if deadline.IsZero() || !deadline.After(now) {
		return 0, !deadline.IsZero()
	}
	return deadline.Sub(now), true
}

// refreshHealthPass probes every instance whose next check is due, records
// the results, and publishes one immutable snapshot.
func (p *Plugin) refreshHealthPass(ctx context.Context) bool {
	_ = p.refreshResolvedNodes(ctx, false)
	now := p.healthNow()
	type dueProbe struct {
		index int
		node  *resolvedNode
	}
	due := make([]dueProbe, 0)
	for index, state := range p.health {
		nodes := p.resolvedNodes(index)
		if len(nodes) == 0 {
			nodeSnapshot := p.nodeSnapshot.Load()
			if nodeSnapshot != nil && index < len(nodeSnapshot.required) && nodeSnapshot.required[index] {
				continue
			}
			if !now.Before(state.nextCheck) {
				due = append(due, dueProbe{index: index})
			}
			continue
		}
		for _, node := range nodes {
			if !now.Before(node.health.nextCheck) {
				due = append(due, dueProbe{index: index, node: node})
			}
		}
	}
	if ctx.Err() != nil {
		return false
	}

	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan healthProbeCompletion, len(due))
	completed := make([]healthProbeCompletion, 0, len(due))
	workerCount := min(len(due), maxHealthProbeWorkers)
	workerDone := make(chan struct{}, workerCount)
	jobs := make(chan dueProbe, len(due))
	for _, probe := range due {
		jobs <- probe
	}
	close(jobs)
	admittedWorkers := 0
	completePass := true
	owner := p.TaskOwner()
	if owner == nil {
		return false
	}
	probeSlots := make(map[int]chan struct{}, len(p.health))
	for index := range p.health {
		limit := p.config.Instances[index].Checks.Active.Concurrency
		if limit <= 0 {
			limit = 1
		}
		probeSlots[index] = make(chan struct{}, limit)
	}
	for workerIndex := range workerCount {
		component := fmt.Sprintf("health-probe-worker-%d", workerIndex)
		err := owner.Go(component, func(context.Context) error {
			defer func() { workerDone <- struct{}{} }()
			for {
				var probe dueProbe
				var ok bool
				select {
				case <-passCtx.Done():
					return nil
				case probe, ok = <-jobs:
					if !ok {
						return nil
					}
				}
				marker := healthProbeCompletion{index: probe.index, node: probe.node}
				select {
				case probeSlots[probe.index] <- struct{}{}:
				case <-passCtx.Done():
					return nil
				}
				func() {
					defer func() { <-probeSlots[probe.index] }()
					defer func() {
						if recover() != nil {
							marker.result = healthProbeResult{err: errHealthProbePanicked}
						}
					}()
					if p.probeForTest != nil {
						marker.result = p.probeForTest(passCtx, probe.index)
					} else if probe.node != nil {
						marker.result = p.probeResolvedNode(passCtx, probe.index, probe.node)
					} else {
						marker.result = p.probeInstance(passCtx, probe.index)
					}
				}()
				if passCtx.Err() != nil {
					return nil
				}
				marker.completed = true
				results <- marker
			}
		})
		if err != nil {
			completePass = false
			cancel()
			break
		}
		admittedWorkers++
	}
	for range admittedWorkers {
		<-workerDone
	}
	close(results)
	for marker := range results {
		completed = append(completed, marker)
	}
	if !completePass || len(completed) != len(due) || ctx.Err() != nil {
		return false
	}
	for _, marker := range completed {
		if marker.node != nil {
			p.recordResolvedNodeProbeResult(marker.index, marker.node, marker.result, p.healthNow())
		} else {
			p.recordProbeResult(marker.index, marker.result, p.healthNow())
		}
	}
	p.publishHealthSnapshot()
	return true
}

func (p *Plugin) probeResolvedNode(ctx context.Context, index int, node *resolvedNode) healthProbeResult {
	instance := p.config.Instances[index]
	check := instance.Checks.Active
	var result healthProbeResult
	err := p.withInstanceAuth(index, func(auth Auth) error {
		instance.Auth = auth
		result = p.probeResolvedNodeWithAuth(ctx, instance, check, node)
		return nil
	})
	if err != nil {
		return healthProbeResult{err: err}
	}
	return result
}

func (p *Plugin) probeResolvedNodeWithAuth(
	ctx context.Context, instance Instance, check ActiveHealthCheck, node *resolvedNode,
) healthProbeResult {
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
		port := target.Port()
		connection, dialErr := (&net.Dialer{}).DialContext(
			probeContext, "tcp", net.JoinHostPort(node.ip.String(), port),
		)
		if dialErr != nil {
			return healthProbeResult{err: dialErr, timeout: probeContext.Err() == context.DeadlineExceeded}
		}
		_ = connection.Close()
		return healthProbeResult{status: http.StatusOK}
	}
	request, err := http.NewRequestWithContext(probeContext, http.MethodGet, target.String(), nil)
	if err != nil {
		return healthProbeResult{err: err}
	}
	if check.Host != "" {
		request.Host = check.Host
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
	response, err := node.healthClient.Do(request)
	if err != nil {
		node.finalizeIfRetired()
		return healthProbeResult{err: err, timeout: probeContext.Err() == context.DeadlineExceeded}
	}
	defer func() {
		_ = response.Body.Close()
		node.finalizeIfRetired()
	}()
	return healthProbeResult{status: response.StatusCode}
}

func (p *Plugin) probeInstance(ctx context.Context, index int) healthProbeResult {
	instance := p.config.Instances[index]
	check := instance.Checks.Active
	var result healthProbeResult
	err := p.withInstanceAuth(index, func(auth Auth) error {
		instance.Auth = auth
		result = p.probeInstanceWithAuth(ctx, index, instance, check)
		return nil
	})
	if err != nil {
		return healthProbeResult{err: err}
	}
	return result
}

func (p *Plugin) probeInstanceWithAuth(
	ctx context.Context, index int, instance Instance, check ActiveHealthCheck,
) healthProbeResult {
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
	if check.Host != "" {
		request.Host = check.Host
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
	base, err := instanceHealthBaseURL(instance)
	if err != nil {
		return nil, err
	}
	if base.Port() == "" {
		port := "80"
		if base.Scheme == "https" {
			port = "443"
		}
		base.Host = net.JoinHostPort(base.Hostname(), port)
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
	normalizeURLDefaultPort(base)
	base.Path = check.HTTPPath
	query := base.Query()
	for name, value := range instance.Auth.Query {
		query.Set(name, value)
	}
	base.RawQuery = query.Encode()
	return base, nil
}

func normalizeURLDefaultPort(target *url.URL) {
	port := target.Port()
	if (target.Scheme == "http" && port == "80") || (target.Scheme == "https" && port == "443") {
		target.Host = strings.TrimSuffix(target.Host, ":"+port)
	}
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

func (p *Plugin) recordResolvedNodeProbeResult(
	index int, node *resolvedNode, result healthProbeResult, now time.Time,
) {
	if node.retired.Load() || !p.currentResolvedNode(index, node) {
		return
	}
	recordHealthState(&node.health, p.config.Instances[index].Checks.Active, result, now)
}

func recordHealthState(
	state *instanceHealthState, check ActiveHealthCheck, result healthProbeResult, now time.Time,
) {
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
