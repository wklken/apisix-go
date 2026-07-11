package ai_proxy_multi

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
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
	Healthy                HealthyCheckPolicy   `json:"healthy,omitempty"`
	Unhealthy              UnhealthyCheckPolicy `json:"unhealthy,omitempty"`
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
	checking     bool
}

type healthProbeResult struct {
	status  int
	err     error
	timeout bool
}

func (p *Plugin) initHealthStates() {
	p.health = make(map[int]*instanceHealthState)
	for index := range p.config.Instances {
		instance := &p.config.Instances[index]
		if instance.Checks == nil {
			continue
		}
		applyHealthDefaults(&instance.Checks.Active)
		p.health[index] = &instanceHealthState{healthy: true}
	}
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

func (p *Plugin) refreshHealth(ctx context.Context) {
	now := p.healthNow()
	due := make([]int, 0)
	p.healthMu.Lock()
	for index, state := range p.health {
		if !state.checking && !now.Before(state.nextCheck) {
			state.checking = true
			due = append(due, index)
		}
	}
	p.healthMu.Unlock()

	var wait sync.WaitGroup
	for _, index := range due {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result := p.probeInstance(ctx, p.config.Instances[index])
			p.recordProbeResult(index, result, p.healthNow())
		}(index)
	}
	wait.Wait()
}

func (p *Plugin) probeInstance(ctx context.Context, instance Instance) healthProbeResult {
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if check.Type == "https" && check.HTTPSVerifyCertificate != nil && !*check.HTTPSVerifyCertificate {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return healthProbeResult{err: err, timeout: probeContext.Err() == context.DeadlineExceeded}
	}
	defer response.Body.Close()
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
	if check.Type == "tcp" {
		base.Scheme = "tcp"
	} else if check.Type == "http" || check.Type == "https" {
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
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	state := p.health[index]
	if state == nil {
		return
	}
	check := p.config.Instances[index].Checks.Active
	success := result.err == nil && containsStatus(check.Healthy.HTTPStatuses, result.status)
	failure := result.err != nil || containsStatus(check.Unhealthy.HTTPStatuses, result.status)
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
	state.checking = false
}

func (p *Plugin) instanceHealthy(index int) bool {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	state := p.health[index]
	return state == nil || state.healthy
}

func containsStatus(statuses []int, status int) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}
