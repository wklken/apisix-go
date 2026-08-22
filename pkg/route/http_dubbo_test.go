package route

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
)

func TestServeHTTPDubboIfConfiguredUsesRouteUpstreamTarget(t *testing.T) {
	upstream := startRouteDubboTestServer(t, routeDubboFrame("1\nfrom route upstream\n"))
	lb := pxy.NewWeightedRRLoadBalance(map[string]int{"dubbo://" + upstream: 1})

	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = http_dubbo.WithConfig(req, http_dubbo.Config{
		ServiceName:    "svc",
		ServiceVersion: "0.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()

	if !serveHTTPDubboIfConfiguredCompiled(rr, req, lb, nil) {
		t.Fatal("serveHTTPDubboIfConfiguredCompiled() = false, want true")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "from route upstream" {
		t.Fatalf("response body = %q, want Dubbo upstream response", rr.Body.String())
	}
}

func TestServeHTTPDubboIfConfiguredUsesSafeUpstreamRetries(t *testing.T) {
	upstream := startRouteDubboTestServer(t, routeDubboFrame("1\nretry-success\n"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen retry target: %v", err)
	}
	failedTarget := listener.Addr().String()
	_ = listener.Close()
	lb := &sequenceLoadBalancer{targets: []string{"dubbo://" + failedTarget, "dubbo://" + upstream}}

	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = http_dubbo.WithConfig(req, http_dubbo.Config{
		ServiceName:    "svc",
		ServiceVersion: "0.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()

	if !serveHTTPDubboIfConfiguredCompiled(rr, req, lb, nil, 1) {
		t.Fatal("serveHTTPDubboIfConfiguredCompiled() = false, want true")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "retry-success" {
		t.Fatalf("response body = %q, want retry-success", rr.Body.String())
	}
	if lb.index != 2 {
		t.Fatalf("selected targets = %d, want 2", lb.index)
	}
}

func TestServeHTTPDubboIfConfiguredUsesRequestAwarePriorityRetryTargets(t *testing.T) {
	upstream := startRouteDubboTestServer(t, routeDubboFrame("1\npriority-fallback\n"))
	failedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed target: %v", err)
	}
	failedTarget := failedListener.Addr().String()
	_ = failedListener.Close()
	servers := map[string]int{
		"dubbo://" + failedTarget: 1,
		"dubbo://" + upstream:     1,
	}
	priorities := map[string]int{
		"dubbo://" + failedTarget: 10,
		"dubbo://" + upstream:     0,
	}
	lb, err := pxy.NewUpstreamLoadBalanceWithPriorities(servers, priorities, nil)
	if err != nil {
		t.Fatalf("NewUpstreamLoadBalanceWithPriorities() error = %v", err)
	}
	targets, err := compileUpstreamTargets(servers)
	if err != nil {
		t.Fatalf("compileUpstreamTargets() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = http_dubbo.WithConfig(req, http_dubbo.Config{
		ServiceName:    "svc",
		ServiceVersion: "0.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()

	if !serveHTTPDubboIfConfiguredCompiled(rr, req, lb, targets, 1) {
		t.Fatal("serveHTTPDubboIfConfiguredCompiled() = false, want true")
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "priority-fallback" {
		t.Fatalf("response = %d/%q, want 200/priority-fallback", rr.Code, rr.Body.String())
	}
}

func TestServeHTTPDubboIfConfiguredAdvancesTrafficSplitRetryOverride(t *testing.T) {
	upstream := startRouteDubboTestServer(t, routeDubboFrame("1\ntraffic-split-retry\n"))
	failedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed target: %v", err)
	}
	failedTarget := failedListener.Addr().String()
	_ = failedListener.Close()
	firstReporter := &recordingDubboHealthReporter{}
	secondReporter := &recordingDubboHealthReporter{}
	second := &traffic_split.Override{
		Scheme:         "http",
		Host:           upstream,
		HealthReporter: secondReporter,
		HealthTarget:   "http://" + upstream,
	}
	first := &traffic_split.Override{
		Scheme:         "http",
		Host:           failedTarget,
		Retries:        1,
		HealthReporter: firstReporter,
		HealthTarget:   "http://" + failedTarget,
	}
	first.NextRetry = func(*http.Request) *traffic_split.Override { return second }
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = traffic_split.WithOverride(req, first)
	req = http_dubbo.WithConfig(req, http_dubbo.Config{
		ServiceName:    "svc",
		ServiceVersion: "0.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()

	if !serveHTTPDubboIfConfiguredCompiled(rr, req, nil, nil) {
		t.Fatal("serveHTTPDubboIfConfiguredCompiled() = false, want true")
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "traffic-split-retry" {
		t.Fatalf("response = %d/%q, want 200/traffic-split-retry", rr.Code, rr.Body.String())
	}
	if len(firstReporter.tcpTargets) != 1 || firstReporter.tcpTargets[0] != first.HealthTarget {
		t.Fatalf("first retry health targets = %#v, want [%q]", firstReporter.tcpTargets, first.HealthTarget)
	}
	if len(secondReporter.httpTargets) != 1 || secondReporter.httpTargets[0] != second.HealthTarget {
		t.Fatalf("second retry health targets = %#v, want [%q]", secondReporter.httpTargets, second.HealthTarget)
	}
}

func TestRouteHTTPDubboTerminalUsesTrafficSplitOverrideWithoutBaseUpstream(t *testing.T) {
	upstream := startRouteDubboTestServer(t, routeDubboFrame("1\ntraffic-split-only\n"))
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{
		Scheme: "http",
		Host:   upstream,
	})
	req = http_dubbo.WithConfig(req, http_dubbo.Config{
		ServiceName:    "svc",
		ServiceVersion: "0.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()
	fallbackCalled := false

	_, _, _, err := (routeHTTPDubboTerminal{}).RunExclusiveProtocol(
		rr,
		req,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fallbackCalled = true }),
	)
	if err != nil {
		t.Fatalf("RunExclusiveProtocol() error = %v", err)
	}
	if fallbackCalled {
		t.Fatal("fallback handler ran despite traffic-split HTTP-Dubbo override")
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "traffic-split-only" {
		t.Fatalf("response = %d/%q, want 200/traffic-split-only", rr.Code, rr.Body.String())
	}
}

func TestServeHTTPDubboIfConfiguredDoesNotReattributeMissingTrafficSplitRetryTarget(t *testing.T) {
	tests := []struct {
		name      string
		nextRetry func(*http.Request) *traffic_split.Override
	}{
		{name: "nil callback"},
		{name: "nil result", nextRetry: func(*http.Request) *traffic_split.Override { return nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failedListener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen failed target: %v", err)
			}
			failedTarget := failedListener.Addr().String()
			_ = failedListener.Close()
			reporter := &recordingDubboHealthReporter{}
			override := &traffic_split.Override{
				Scheme:         "http",
				Host:           failedTarget,
				Retries:        1,
				HealthReporter: reporter,
				HealthTarget:   "http://" + failedTarget,
				NextRetry:      test.nextRetry,
			}
			req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
			req = traffic_split.WithOverride(req, override)
			req = http_dubbo.WithConfig(req, http_dubbo.Config{
				ServiceName:    "svc",
				ServiceVersion: "0.0.0",
				Method:         "hello",
			})
			rr := httptest.NewRecorder()

			if !serveHTTPDubboIfConfiguredCompiled(rr, req, nil, nil) {
				t.Fatal("serveHTTPDubboIfConfiguredCompiled() = false, want true")
			}
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("response code = %d, want 502; body=%q", rr.Code, rr.Body.String())
			}
			if len(reporter.tcpTargets) != 1 || reporter.tcpTargets[0] != override.HealthTarget {
				t.Fatalf(
					"reported TCP targets = %#v, want one actual failure for %q",
					reporter.tcpTargets,
					override.HealthTarget,
				)
			}
		})
	}
}

func TestServeHTTPDubboIfConfiguredSkipsUnconfiguredRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	rr := httptest.NewRecorder()

	if serveHTTPDubboIfConfiguredCompiled(
		rr,
		req,
		pxy.NewWeightedRRLoadBalance(map[string]int{"dubbo://127.0.0.1:20880": 1}),
		nil,
	) {
		t.Fatal("serveHTTPDubboIfConfiguredCompiled() = true, want false")
	}
}

func startRouteDubboTestServer(t *testing.T, response []byte) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		discardDubboFrame(conn)
		_, _ = conn.Write(response)
	}()

	return ln.Addr().String()
}

type sequenceLoadBalancer struct {
	targets []string
	index   int
}

func (lb *sequenceLoadBalancer) Next() string {
	if lb.index >= len(lb.targets) {
		return lb.targets[len(lb.targets)-1]
	}
	target := lb.targets[lb.index]
	lb.index++
	return target
}

func discardDubboFrame(conn net.Conn) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[12:16]))
	_, _ = io.ReadFull(conn, payload)
}

func routeDubboFrame(payload string) []byte {
	frame := make([]byte, 16+len(payload))
	frame[0], frame[1] = 0xda, 0xbb
	frame[3] = 20
	binary.BigEndian.PutUint64(frame[4:12], 1)
	binary.BigEndian.PutUint32(frame[12:16], uint32(len(payload)))
	copy(frame[16:], payload)
	return frame
}
