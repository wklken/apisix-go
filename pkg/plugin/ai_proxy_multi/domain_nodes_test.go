package ai_proxy_multi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	stdjson "encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"golang.org/x/net/dns/dnsmessage"
)

func TestResolveDomainEndpointBuildsSortedUniqueNodeSnapshot(t *testing.T) {
	p := &Plugin{config: Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "https://llm.internal:8443/base"},
	}}}}
	p.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("192.0.2.2"),
			netip.MustParseAddr("192.0.2.1"),
			netip.MustParseAddr("192.0.2.2"),
		}, nil
	}
	p.initResolverDefaults()
	if err := p.refreshResolvedNodes(context.Background(), true); err != nil {
		t.Fatalf("refreshResolvedNodes() error = %v", err)
	}

	nodes := p.resolvedNodes(0)
	if len(nodes) != 2 {
		t.Fatalf("resolved node count = %d, want 2", len(nodes))
	}
	if nodes[0].ip.String() != "192.0.2.1" || nodes[1].ip.String() != "192.0.2.2" {
		t.Fatalf("resolved node IPs = [%s %s], want sorted unique addresses", nodes[0].ip, nodes[1].ip)
	}
	for _, node := range nodes {
		if node.logicalHost != "llm.internal" {
			t.Fatalf("node logical host = %q, want llm.internal", node.logicalHost)
		}
		if node.client == nil || node.healthClient == nil {
			t.Fatalf(
				"node clients = (%p, %p), want node-owned request and health clients",
				node.client,
				node.healthClient,
			)
		}
	}
	if nodes[0].client == nodes[1].client {
		t.Fatal("resolved nodes share a request client")
	}
}

func TestPostInitDoesNotWaitForDomainResolution(t *testing.T) {
	tasks, owner, _ := newAIHealthTestTasks(t)
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	var started sync.Once
	p := &Plugin{
		config: Config{Instances: []Instance{{
			Name: "domain", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: "https://llm.internal/v1"},
		}}},
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			started.Do(func() { close(lookupStarted) })
			<-releaseLookup
			return nil, errors.New("resolver unavailable")
		},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	postInitDone := make(chan error, 1)
	go func() { postInitDone <- p.PostInit() }()
	t.Cleanup(func() {
		close(releaseLookup)
		stopTestRegistry(t, tasks)
		p.Stop()
	})

	select {
	case err := <-postInitDone:
		if err != nil {
			t.Fatalf("PostInit() error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("PostInit() waited for domain resolution")
	}
	select {
	case <-lookupStarted:
	case <-time.After(time.Second):
		t.Fatal("generation-owned resolver did not start after PostInit")
	}
	snapshot := p.snapshot.Load()
	if snapshot == nil || len(snapshot.nodeRequired) != 1 || !snapshot.nodeRequired[0] || snapshot.healthy[0] {
		t.Fatalf("initial unresolved snapshot = %#v, want required and unavailable", snapshot)
	}
}

func TestUnresolvedDomainIsNotProbedThroughSystemResolver(t *testing.T) {
	tasks, owner, _ := newAIHealthTestTasks(t)
	p := &Plugin{
		config: Config{Instances: []Instance{{
			Name: "domain", Provider: "openai-compatible", Weight: 1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer secret"}},
			Override: Override{Endpoint: "https://llm.internal/v1"},
			Checks:   &HealthChecks{Active: ActiveHealthCheck{Type: "https"}},
		}}},
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, errors.New("configured resolver unavailable")
		},
		healthNow: time.Now,
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	p.initResolverDefaults()
	p.nodeRequired[0] = true
	p.publishResolvedNodeSnapshotLocked()
	p.initHealthStates()
	var probes atomic.Int32
	p.probeForTest = func(context.Context, int) healthProbeResult {
		probes.Add(1)
		return healthProbeResult{status: http.StatusOK}
	}
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})

	if !p.refreshHealthPass(context.Background()) {
		t.Fatal("refreshHealthPass() = false, want completed resolver-only pass")
	}
	if got := probes.Load(); got != 0 {
		t.Fatalf("unresolved domain probe calls = %d, want 0", got)
	}
}

func TestExecutionTargetSkipsUnresolvedHigherPriorityInstance(t *testing.T) {
	tasks, owner, _ := newAIHealthTestTasks(t)
	p := &Plugin{
		config: Config{Instances: []Instance{
			{
				Name: "unresolved", Provider: "openai-compatible", Priority: 10, Weight: 1,
				Override: Override{Endpoint: "https://llm.internal/v1"},
			},
			{
				Name: "fallback", Provider: "openai-compatible", Priority: 0, Weight: 1,
				Override: Override{Endpoint: "https://192.0.2.10/v1"},
			},
		}},
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, errors.New("configured resolver unavailable")
		},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})

	target, ok, err := p.pickExecutionTarget(nil, nil)
	if err != nil || !ok || target.index != 1 || target.node != nil {
		t.Fatalf("pickExecutionTarget() = (%+v, %v, %v), want fallback instance 1", target, ok, err)
	}
}

func TestDomainResolutionRejectsAddressSetAboveBound(t *testing.T) {
	addresses := make([]netip.Addr, 65)
	for index := range addresses {
		addresses[index] = netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, byte(index >> 8), byte(index)})
	}
	p := &Plugin{config: Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "https://llm.internal/v1"},
	}}}}
	p.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return addresses, nil
	}
	p.initResolverDefaults()

	if err := p.refreshResolvedNodes(context.Background(), true); err == nil {
		t.Fatal("refreshResolvedNodes() accepted 65 addresses")
	}
	if nodes := p.resolvedNodes(0); len(nodes) != 0 {
		t.Fatalf("resolved nodes = %d, want fail-closed empty set", len(nodes))
	}
}

func TestDomainResolutionUsesBoundedParallelWorkers(t *testing.T) {
	instances := make([]Instance, 8)
	for index := range instances {
		instances[index] = Instance{
			Name: "domain-" + strconv.Itoa(index), Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: "https://llm-" + strconv.Itoa(index) + ".internal/v1"},
		}
	}
	tasks, owner, _ := newAIHealthTestTasks(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var active, maximum atomic.Int32
	p := &Plugin{config: Config{Instances: instances}}
	p.lookupNetIP = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		select {
		case <-release:
			return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	done := make(chan error, 1)
	finished := false
	go func() { done <- p.refreshResolvedNodes(context.Background(), true) }()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !finished {
			<-done
		}
		stopTestRegistry(t, tasks)
		p.Stop()
	})

	deadline := time.Now().Add(200 * time.Millisecond)
	for maximum.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := maximum.Load(); got != 4 {
		t.Fatalf("concurrent DNS lookups = %d, want fixed worker count 4", got)
	}
	releaseOnce.Do(func() { close(release) })
	err := <-done
	finished = true
	if err != nil {
		t.Fatalf("refreshResolvedNodes() error = %v", err)
	}
}

func TestResolvedHealthPassUsesBoundedWorkerTasks(t *testing.T) {
	addresses := make([]netip.Addr, 64)
	for index := range addresses {
		addresses[index] = netip.MustParseAddr("2001:db8::" + strconv.FormatInt(int64(index+1), 16))
	}
	tasks, owner, _ := newAIHealthTestTasks(t)
	p := &Plugin{
		config: Config{Instances: []Instance{{
			Name: "domain", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: "https://llm.internal/v1"},
			Checks: &HealthChecks{Active: ActiveHealthCheck{
				Concurrency: 64,
				Healthy:     HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
			}},
		}}},
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return addresses, nil
		},
		healthNow: time.Now,
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	p.initResolverDefaults()
	if err := p.refreshResolvedNodes(context.Background(), true); err != nil {
		t.Fatalf("refreshResolvedNodes() error = %v", err)
	}
	p.initHealthStates()
	release := make(chan struct{})
	var releaseOnce sync.Once
	p.probeForTest = func(ctx context.Context, _ int) healthProbeResult {
		select {
		case <-release:
			return healthProbeResult{status: http.StatusOK}
		case <-ctx.Done():
			return healthProbeResult{err: ctx.Err()}
		}
	}
	done := make(chan bool, 1)
	finished := false
	go func() { done <- p.refreshHealthPass(context.Background()) }()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !finished {
			<-done
		}
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	deadline := time.Now().Add(time.Second)
	for len(tasks.Active()) < 32 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active := tasks.Active(); len(active) != 32 {
		t.Fatalf("active health worker tasks = %d (%v), want fixed cap 32", len(active), active)
	}
	releaseOnce.Do(func() { close(release) })
	complete := <-done
	finished = true
	if !complete {
		t.Fatal("refreshHealthPass() = false, want complete bounded pass")
	}
}

func TestDomainDNSReorderPreservesNodeClientsAndHealth(t *testing.T) {
	var reversed atomic.Bool
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			Healthy:   HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
			Unhealthy: UnhealthyCheckPolicy{HTTPStatuses: []int{http.StatusInternalServerError}, HTTPFailures: 1},
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		addresses := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}
		if reversed.Load() {
			slices.Reverse(addresses)
		}
		return addresses, nil
	})
	before := p.resolvedNodes(0)
	p.recordResolvedNodeProbeResult(0, before[0], healthProbeResult{status: http.StatusInternalServerError}, time.Now())
	if before[0].health.healthy {
		t.Fatal("failed node remained healthy before DNS reorder")
	}

	reversed.Store(true)
	if err := p.refreshResolvedNodes(context.Background(), true); err != nil {
		t.Fatalf("refreshResolvedNodes() after reorder error = %v", err)
	}
	after := p.resolvedNodes(0)
	if after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("DNS reorder replaced nodes: before=%p/%p after=%p/%p", before[0], before[1], after[0], after[1])
	}
	if after[0].client != before[0].client || after[0].health.healthy {
		t.Fatal("DNS reorder replaced the node client or reset its health state")
	}
}

func TestDomainNodeSetChangeRetiresRemovedNodeAndIgnoresStaleProbe(t *testing.T) {
	var generation atomic.Int32
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			Healthy:   HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
			Unhealthy: UnhealthyCheckPolicy{HTTPStatuses: []int{http.StatusInternalServerError}, HTTPFailures: 1},
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		if generation.Load() == 0 {
			return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("192.0.2.3")}, nil
	})
	before := p.resolvedNodes(0)
	removed, retained := before[0], before[1]
	p.recordResolvedNodeProbeResult(0, removed, healthProbeResult{status: http.StatusInternalServerError}, time.Now())

	generation.Store(1)
	if err := p.refreshResolvedNodes(context.Background(), true); err != nil {
		t.Fatalf("refreshResolvedNodes() after set change error = %v", err)
	}
	after := p.resolvedNodes(0)
	if after[0] != retained || after[1].ip.String() != "192.0.2.3" {
		t.Fatalf("changed nodes = %p/%s, want retained 192.0.2.2 plus new 192.0.2.3", after[0], after[1].ip)
	}
	if !removed.retired.Load() {
		t.Fatal("removed node was not retired")
	}
	if picked := p.pickResolvedNode(0); picked == removed {
		t.Fatal("request selection still exposed a removed node after the refreshed snapshot was published")
	}
	p.recordResolvedNodeProbeResult(0, removed, healthProbeResult{status: http.StatusOK}, time.Now())
	if removed.health.healthy {
		t.Fatal("stale successful probe completion revived a removed node")
	}
}

func TestDomainAllUnhealthyFallsBackAcrossResolvedNodes(t *testing.T) {
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			Healthy:   HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
			Unhealthy: UnhealthyCheckPolicy{HTTPStatuses: []int{http.StatusInternalServerError}, HTTPFailures: 1},
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, nil
	})
	for _, node := range p.resolvedNodes(0) {
		p.recordResolvedNodeProbeResult(0, node, healthProbeResult{status: http.StatusInternalServerError}, time.Now())
	}
	choices := []int{0, 1}
	p.nodeRandom = func(int) int {
		choice := choices[0]
		choices = choices[1:]
		return choice
	}
	p.publishHealthSnapshot()
	first := p.pickResolvedNode(0)
	second := p.pickResolvedNode(0)
	if first == nil || second == nil || first == second {
		t.Fatalf("all-unhealthy fallback picks = %p/%p, want both resolved nodes available", first, second)
	}
}

func TestDomainRequestPreservesLogicalHostAndTLSServerName(t *testing.T) {
	var gotHost, gotSNI string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotSNI = r.TLS.ServerName
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok","role":"assistant"}}]}`))
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split TLS server address: %v", err)
	}
	verify := false
	p := newResolvedDomainTestPlugin(t, Config{
		SSLVerify: &verify,
		Instances: []Instance{{
			Name: "domain", Provider: "openai-compatible", Weight: 1,
			Override: Override{Endpoint: "https://llm.internal:" + port},
		}},
	}, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	_ = serveChat(t, p, "")
	if gotHost != "llm.internal:"+port {
		t.Fatalf("provider Host = %q, want logical domain authority", gotHost)
	}
	if gotSNI != "llm.internal" {
		t.Fatalf("provider TLS ServerName = %q, want logical domain", gotSNI)
	}
}

func TestConfiguredDomainResolverIsGenerationPinned(t *testing.T) {
	first := newDomainDNSServer(t)
	second := newDomainDNSServer(t)
	effective := &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{
		DnsResolver:      []string{first.address, second.address},
		DnsResolverValid: 17,
		ResolverTimeout:  2,
	}}}
	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Config: effective})
	p.initResolverDefaults()

	effective.Config.Apisix.DnsResolver[0] = "127.0.0.1:1"
	effective.Config.Apisix.DnsResolver[1] = "127.0.0.1:2"
	effective.Config.Apisix.DnsResolverValid = 99
	effective.Config.Apisix.ResolverTimeout = 99
	for call := range 2 {
		addresses, err := p.lookupNetIP(context.Background(), "ip4", "llm.internal")
		if err != nil {
			t.Fatalf("LookupNetIP() call %d error = %v", call+1, err)
		}
		if len(addresses) != 1 || addresses[0] != netip.MustParseAddr("192.0.2.10") {
			t.Fatalf("LookupNetIP() call %d addresses = %v, want [192.0.2.10]", call+1, addresses)
		}
	}
	for index, server := range []*domainDNSServer{first, second} {
		question := server.waitForQuestion(t)
		if question.Name.String() != "llm.internal." || question.Type != dnsmessage.TypeA {
			t.Fatalf("DNS server %d question = %#v, want llm.internal. A", index+1, question)
		}
	}
	if p.resolverTimeout != 2*time.Second || p.resolverTTL != 17*time.Second {
		t.Fatalf("pinned resolver timeout/TTL = %s/%s, want 2s/17s", p.resolverTimeout, p.resolverTTL)
	}
}

func TestBuiltinProviderResolutionUsesConfiguredSplitDNS(t *testing.T) {
	dns := newDomainDNSServer(t)
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/ai-multi-split-dns/attempt-1", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	p := &Plugin{config: Config{Instances: []Instance{{
		Name: "openai", Provider: "openai", Weight: 1,
	}}}}
	p.SetDependencies(base.Dependencies{
		Tasks: owner,
		Config: &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{
			DnsResolver: []string{dns.address}, ResolverTimeout: 1, DnsResolverValid: 10,
		}}},
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if err := p.refreshResolvedNodes(context.Background(), true); err != nil {
		t.Fatalf("refreshResolvedNodes() error = %v", err)
	}
	p.publishHealthSnapshot()
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	if nodes := p.resolvedNodes(0); len(nodes) != 1 || nodes[0].ip != netip.MustParseAddr("192.0.2.10") {
		t.Fatalf("split-DNS nodes = %#v, want configured resolver address 192.0.2.10", nodes)
	}
	question := dns.waitForQuestion(t)
	if question.Name.String() != "api.openai.com." {
		t.Fatalf("split-DNS question = %q, want api.openai.com.", question.Name.String())
	}
}

func TestStopClosesResolvedNodeIdleTransports(t *testing.T) {
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
	})
	node := p.resolvedNodes(0)[0]
	requestTransport := &countingRoundTripper{}
	healthTransport := &countingRoundTripper{}
	node.client.Transport = requestTransport
	node.healthClient.Transport = healthTransport

	p.Stop()
	if requestTransport.closed.Load() != 1 || healthTransport.closed.Load() != 1 {
		t.Fatalf(
			"resolved node CloseIdleConnections calls = request:%d health:%d, want 1:1",
			requestTransport.closed.Load(), healthTransport.closed.Load(),
		)
	}
}

func TestUnresolvedDomainFailsClosedUntilRefreshSucceedsWithoutHealthChecks(t *testing.T) {
	var available atomic.Bool
	p := newResolvedDomainTestPluginWithResolverTTL(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		if !available.Load() {
			return nil, errors.New("split DNS not ready")
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
	}, 10*time.Millisecond)
	var fallbackCalls atomic.Int32
	p.client.Transport = multiStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		fallbackCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"choices":[]}`)),
			Request:    request,
		}, nil
	})

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unresolved response status = %d, want 503", response.Code)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("shared OS-resolving client calls = %d, want 0", fallbackCalls.Load())
	}
	if p.wakeHealth == nil {
		t.Fatal("unresolved domain without checks did not start its refresh owner")
	}

	available.Store(true)
	deadline := time.Now().Add(time.Second)
	for len(p.resolvedNodes(0)) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(p.resolvedNodes(0)) != 1 {
		t.Fatalf("background-resolved nodes after DNS recovery = %d, want 1", len(p.resolvedNodes(0)))
	}
}

func TestResolvedDomainRefreshesAtTTLWithoutRequestWake(t *testing.T) {
	var lookups atomic.Int32
	p := newResolvedDomainTestPluginWithResolverTTL(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
	}, 20*time.Millisecond)

	p.wakeHealthRefresh()
	deadline := time.Now().Add(time.Second)
	for lookups.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := lookups.Load(); got < 2 {
		t.Fatalf("configured resolver lookups after idle TTL = %d, want at least 2", got)
	}
}

func TestBuiltinProviderEndpointUsesGenerationOwnedResolution(t *testing.T) {
	var gotHost string
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "openai", Provider: "openai", Weight: 1,
	}}}, func(_ context.Context, _, host string) ([]netip.Addr, error) {
		gotHost = host
		return []netip.Addr{netip.MustParseAddr("192.0.2.20")}, nil
	})
	if gotHost != "api.openai.com" {
		t.Fatalf("resolved host = %q, want built-in provider host api.openai.com", gotHost)
	}
	if nodes := p.resolvedNodes(0); len(nodes) != 1 || nodes[0].ip != netip.MustParseAddr("192.0.2.20") {
		t.Fatalf("built-in provider nodes = %#v, want generation-owned 192.0.2.20", nodes)
	}
}

func TestGCPTokenExchangeUsesIndependentLogicalHostClient(t *testing.T) {
	var tokenHost, tokenSNI, tokenDial string
	tokenServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHost = r.Host
		tokenSNI = r.TLS.ServerName
		_, _ = w.Write([]byte(`{"access_token":"gcp-token","expires_in":3600}`))
	}))
	tokenServer.StartTLS()
	t.Cleanup(tokenServer.Close)
	_, tokenPort, err := net.SplitHostPort(tokenServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split token server address: %v", err)
	}
	tokenURI := "https://token.internal:" + tokenPort

	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "vertex", Provider: "vertex-ai", Weight: 1,
		Override: Override{Endpoint: "https://provider.internal:8443/v1/chat/completions"},
		Auth: Auth{GCP: &ai_auth.GCPConfig{
			ServiceAccountJSON: domainTestServiceAccount(t, tokenURI),
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.55")}, nil
	})
	tokenTransport := tokenServer.Client().Transport.(*http.Transport).Clone()
	tokenTransport.TLSClientConfig = tokenTransport.TLSClientConfig.Clone()
	tokenTransport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // test server uses an unrelated hostname
	tokenTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		tokenDial = address
		return (&net.Dialer{}).DialContext(ctx, network, tokenServer.Listener.Addr().String())
	}
	p.authClient = &http.Client{Transport: tokenTransport}
	var providerAuthorization string
	providerNode := p.resolvedNodes(0)[0]
	providerNode.client.Transport = multiStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		providerAuthorization = request.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"choices":[{"message":{"content":"ok","role":"assistant"}}]}`,
			)),
			Request: request,
		}, nil
	})

	_ = serveChat(t, p, "")
	if tokenDial != "token.internal:"+tokenPort || tokenHost != "token.internal:"+tokenPort ||
		tokenSNI != "token.internal" {
		t.Fatalf("token dial/Host/SNI = %q/%q/%q, want logical token authority", tokenDial, tokenHost, tokenSNI)
	}
	if providerAuthorization != "Bearer gcp-token" {
		t.Fatalf("provider Authorization = %q, want exchanged GCP token", providerAuthorization)
	}
}

func TestRequestExecutionKeepsInstanceAndNodeFromOneHealthSnapshot(t *testing.T) {
	var generation atomic.Int32
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			Healthy:   HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
			Unhealthy: UnhealthyCheckPolicy{HTTPStatuses: []int{http.StatusInternalServerError}, HTTPFailures: 1},
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		if generation.Load() == 0 {
			return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.2")}, nil
	})
	oldNode := p.resolvedNodes(0)[0]
	// Keep this test deterministic: it drives DNS and probe publication itself.
	p.stoppedHealth.Store(true)
	var oldCalls, newCalls atomic.Int32
	oldTransport := &countingRoundTripper{base: domainResponseTransport(&oldCalls, "old-snapshot")}
	oldNode.client.Transport = oldTransport

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	phaseResponse := httptest.NewRecorder()
	result := p.RunRequestPhase(phaseResponse, request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, body = %q", result.Decision, phaseResponse.Body.String())
	}

	generation.Store(1)
	if err := p.refreshResolvedNodes(context.Background(), true); err != nil {
		t.Fatalf("refreshResolvedNodes() error = %v", err)
	}
	newNode := p.resolvedNodes(0)[0]
	newNode.client.Transport = domainResponseTransport(&newCalls, "new-all-unhealthy")
	p.recordResolvedNodeProbeResult(0, newNode, healthProbeResult{status: http.StatusInternalServerError}, time.Now())
	p.publishHealthSnapshot()

	response := httptest.NewRecorder()
	_, _, _, _ = p.RunExclusiveProtocol(response, result.Request, http.NotFoundHandler())
	if !strings.Contains(response.Body.String(), "old-snapshot") {
		t.Fatalf("provider response = %q, want node pinned during RunRequestPhase", response.Body.String())
	}
	if oldCalls.Load() != 1 || newCalls.Load() != 0 {
		t.Fatalf("provider calls = old:%d new:%d, want 1:0", oldCalls.Load(), newCalls.Load())
	}
	if got := oldTransport.closed.Load(); got != 2 {
		t.Fatalf("retired transport closes after old request = %d, want 2", got)
	}
	p.Stop()
	if got := oldTransport.closed.Load(); got != 2 {
		t.Fatalf("retired transport closes after Stop = %d, want 2", got)
	}
}

func TestResolvedHealthProbesRespectInstanceConcurrency(t *testing.T) {
	addresses := make([]netip.Addr, 20)
	for index := range addresses {
		addresses[index] = netip.MustParseAddr("192.0.2." + strconv.Itoa(index+1))
	}
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			Concurrency: 3,
			Healthy:     HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		return addresses, nil
	})
	var active, maximum atomic.Int32
	release := make(chan struct{})
	p.probeForTest = func(ctx context.Context, _ int) healthProbeResult {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		select {
		case <-release:
			return healthProbeResult{status: http.StatusOK}
		case <-ctx.Done():
			return healthProbeResult{err: ctx.Err()}
		}
	}
	done := make(chan bool, 1)
	go func() { done <- p.refreshHealthPass(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for maximum.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	if complete := <-done; !complete {
		t.Fatal("refreshHealthPass() = false, want complete bounded pass")
	}
	if got := maximum.Load(); got != 3 {
		t.Fatalf("maximum concurrent node probes = %d, want checks.active.concurrency 3", got)
	}
}

func TestResolvedHealthProbePanicReleasesInstanceSlotAndNextPassContinues(t *testing.T) {
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			Concurrency: 1,
			Healthy:     HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("192.0.2.1"),
			netip.MustParseAddr("192.0.2.2"),
		}, nil
	})
	var calls atomic.Int32
	secondStarted := make(chan struct{})
	p.probeForTest = func(context.Context, int) healthProbeResult {
		call := calls.Add(1)
		if call == 1 {
			panic("test probe panic")
		}
		if call == 2 {
			close(secondStarted)
		}
		return healthProbeResult{status: http.StatusOK}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- p.refreshHealthPass(ctx) }()
	select {
	case <-secondStarted:
	case <-time.After(300 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("same-instance probe did not acquire the slot after its peer panicked")
	}
	if complete := <-done; !complete {
		t.Fatal("refreshHealthPass() = false after recovered probe panic, want complete pass")
	}
	cancel()

	for _, node := range p.resolvedNodes(0) {
		node.health.nextCheck = time.Now().Add(-time.Second)
	}
	if !p.refreshHealthPass(context.Background()) {
		t.Fatal("next refreshHealthPass() = false after recovered probe panic")
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("probe calls across recovered and next pass = %d, want 4", got)
	}
}

func TestResolvedHealthProbeWaitersExitOnContextCancellation(t *testing.T) {
	addresses := make([]netip.Addr, 20)
	for index := range addresses {
		addresses[index] = netip.MustParseAddr("192.0.2." + strconv.Itoa(index+1))
	}
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			Concurrency: 1,
			Healthy:     HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		return addresses, nil
	})
	var started sync.Once
	probeStarted := make(chan struct{})
	p.probeForTest = func(ctx context.Context, _ int) healthProbeResult {
		started.Do(func() { close(probeStarted) })
		<-ctx.Done()
		return healthProbeResult{err: ctx.Err()}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- p.refreshHealthPass(ctx) }()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("first resolved-node probe did not start")
	}
	cancel()
	select {
	case complete := <-done:
		if complete {
			t.Fatal("refreshHealthPass() = true after cancellation, want incomplete pass")
		}
	case <-time.After(time.Second):
		t.Fatal("probe waiters did not exit after owner context cancellation")
	}
}

func TestResolvedNodeSelectionUsesInjectableRandomChoice(t *testing.T) {
	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:8080"},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("192.0.2.1"),
			netip.MustParseAddr("192.0.2.2"),
			netip.MustParseAddr("192.0.2.3"),
		}, nil
	})
	choices := []int{2, 2, 0}
	p.nodeRandom = func(limit int) int {
		choice := choices[0]
		choices = choices[1:]
		if choice >= limit {
			t.Fatalf("random choice = %d, limit = %d", choice, limit)
		}
		return choice
	}
	var got []string
	for range 3 {
		got = append(got, p.pickResolvedNode(0).ip.String())
	}
	want := []string{"192.0.2.3", "192.0.2.3", "192.0.2.1"}
	if !slices.Equal(got, want) {
		t.Fatalf("random node choices = %v, want %v", got, want)
	}
}

func TestHealthTargetNormalizesHTTPDefaultPortsAndPreservesTCPDialPorts(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		kind     string
		wantHost string
	}{
		{name: "raw HTTP probe", endpoint: "http://127.0.0.1:80", kind: "http", wantHost: "127.0.0.1"},
		{name: "raw HTTPS probe", endpoint: "https://127.0.0.1:443", kind: "https", wantHost: "127.0.0.1"},
		{name: "raw HTTP non-default", endpoint: "http://127.0.0.1:8080", kind: "http", wantHost: "127.0.0.1:8080"},
		{name: "raw HTTPS non-default", endpoint: "https://127.0.0.1:8443", kind: "https", wantHost: "127.0.0.1:8443"},
		{name: "raw TCP over HTTP endpoint", endpoint: "http://127.0.0.1", kind: "tcp", wantHost: "127.0.0.1:80"},
		{name: "raw TCP over HTTPS endpoint", endpoint: "https://127.0.0.1", kind: "tcp", wantHost: "127.0.0.1:443"},
		{name: "resolved HTTP probe", endpoint: "http://llm.internal:80", kind: "http", wantHost: "llm.internal"},
		{name: "resolved HTTPS probe", endpoint: "https://llm.internal:443", kind: "https", wantHost: "llm.internal"},
		{name: "resolved TCP over HTTP endpoint", endpoint: "http://llm.internal", kind: "tcp", wantHost: "llm.internal:80"},
		{name: "resolved TCP over HTTPS endpoint", endpoint: "https://llm.internal", kind: "tcp", wantHost: "llm.internal:443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, err := healthTarget(
				Instance{Override: Override{Endpoint: test.endpoint}},
				ActiveHealthCheck{Type: test.kind},
			)
			if err != nil {
				t.Fatalf("healthTarget() error = %v", err)
			}
			if target.Host != test.wantHost {
				t.Fatalf("health target Host = %q, want %q", target.Host, test.wantHost)
			}
		})
	}
}

func domainResponseTransport(calls *atomic.Int32, label string) http.RoundTripper {
	return multiStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"choices":[{"message":{"content":"` + label + `","role":"assistant"}}]}`,
			)),
			Request: request,
		}, nil
	})
}

func domainTestServiceAccount(t *testing.T, tokenURI string) string {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	encoded, err := stdjson.Marshal(map[string]any{
		"type":           "service_account",
		"client_email":   "multi@example.test",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
		"private_key_id": "key-id",
		"token_uri":      tokenURI,
	})
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}
	return string(encoded)
}

type domainDNSServer struct {
	address   string
	questions chan dnsmessage.Question
	errors    chan error
}

func newDomainDNSServer(t *testing.T) *domainDNSServer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen for test DNS: %v", err)
	}
	server := &domainDNSServer{
		address: conn.LocalAddr().String(), questions: make(chan dnsmessage.Question, 4), errors: make(chan error, 1),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		for {
			n, peer, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(buffer[:n]); err != nil {
				server.errors <- fmt.Errorf("unpack DNS query: %w", err)
				continue
			}
			question := query.Questions[0]
			server.questions <- question
			response := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 query.ID,
					Response:           true,
					Authoritative:      true,
					RecursionAvailable: true,
				},
				Questions: query.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60,
					},
					Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}},
				}},
			}
			wire, err := response.Pack()
			if err != nil {
				server.errors <- fmt.Errorf("pack DNS response: %w", err)
				continue
			}
			if _, err := conn.WriteToUDP(wire, peer); err != nil {
				server.errors <- fmt.Errorf("write DNS response: %w", err)
			}
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	return server
}

func (s *domainDNSServer) waitForQuestion(t *testing.T) dnsmessage.Question {
	t.Helper()
	select {
	case question := <-s.questions:
		return question
	case err := <-s.errors:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DNS question")
	}
	return dnsmessage.Question{}
}

func TestDomainHealthSelectsHealthyResolvedNodeAtSamePort(t *testing.T) {
	goodListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen healthy node: %v", err)
	}
	port := goodListener.Addr().(*net.TCPAddr).Port
	badListener, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		_ = goodListener.Close()
		t.Fatalf("listen unhealthy node: %v", err)
	}

	var goodRequests, badRequests atomic.Int32
	var goodHealthHost atomic.Value
	startNode := func(listener net.Listener, healthStatus int, requests *atomic.Int32) *httptest.Server {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				if healthStatus == http.StatusOK {
					goodHealthHost.Store(r.Host)
				}
				w.WriteHeader(healthStatus)
				return
			}
			requests.Add(1)
			_, _ = fmt.Fprintf(w, `{"node":%q}`, listener.Addr().(*net.TCPAddr).IP.String())
		}))
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
		return server
	}
	startNode(goodListener, http.StatusOK, &goodRequests)
	startNode(badListener, http.StatusInternalServerError, &badRequests)

	p := newResolvedDomainTestPlugin(t, Config{Instances: []Instance{{
		Name: "domain", Provider: "openai-compatible", Weight: 1,
		Override: Override{Endpoint: "http://llm.internal:" + strconv.Itoa(port)},
		Checks: &HealthChecks{Active: ActiveHealthCheck{
			HTTPPath: "/health", Host: "health.authority.internal",
			Healthy: HealthyCheckPolicy{HTTPStatuses: []int{http.StatusOK}, Successes: 1},
			Unhealthy: UnhealthyCheckPolicy{
				HTTPStatuses: []int{http.StatusInternalServerError}, HTTPFailures: 1,
			},
		}},
	}}}, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1")}, nil
	})
	if !p.refreshHealthPass(context.Background()) {
		t.Fatal("refreshHealthPass() = false, want completed node probes")
	}

	body := serveChat(t, p, "")
	if !strings.Contains(body, `"node":"127.0.0.1"`) {
		t.Fatalf("provider response = %q, want healthy resolved node", body)
	}
	if goodRequests.Load() != 1 || badRequests.Load() != 0 {
		t.Fatalf("provider request counts = healthy:%d unhealthy:%d, want 1:0", goodRequests.Load(), badRequests.Load())
	}
	if got, _ := goodHealthHost.Load().(string); got != "health.authority.internal" {
		t.Fatalf("healthy node probe Host = %q, want checks.active.host authority", got)
	}
}

func newResolvedDomainTestPlugin(t *testing.T, cfg Config, lookup lookupNetIPFunc) *Plugin {
	return newResolvedDomainTestPluginWithResolverTTL(t, cfg, lookup, 0)
}

func newResolvedDomainTestPluginWithResolverTTL(
	t *testing.T, cfg Config, lookup lookupNetIPFunc, resolverTTL time.Duration,
) *Plugin {
	t.Helper()
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/ai-multi-domain/attempt-1", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	p := &Plugin{config: cfg, lookupNetIP: lookup, healthNow: time.Now, resolverTTL: resolverTTL}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	p.stoppedHealth.Store(true)
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	_ = p.refreshResolvedNodes(context.Background(), true)
	p.publishHealthSnapshot()
	p.stoppedHealth.Store(false)
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	return p
}
