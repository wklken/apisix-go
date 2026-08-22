package route

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/apache/dubbo-go-hessian2"
	"github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

type recordingDubboHealthReporter struct {
	tcpTargets  []string
	httpTargets []string
}

func (r *recordingDubboHealthReporter) ReportHTTP(target string, _ int) {
	r.httpTargets = append(r.httpTargets, target)
}

func (r *recordingDubboHealthReporter) ReportTCPFailure(target string, _ bool) {
	r.tcpTargets = append(r.tcpTargets, target)
}

func TestServeDubboIfConfiguredUsesRouteUpstreamTarget(t *testing.T) {
	upstream := startRouteHessianDubboTestServer(t)
	lb := pxy.NewWeightedRRLoadBalance(map[string]int{"dubbo://" + upstream: 1})
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = dubbo_proxy.WithConfig(req, dubbo_proxy.Config{
		ServiceName:    "svc",
		ServiceVersion: "1.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()

	if !serveDubboIfConfiguredCompiled(rr, req, lb, nil) {
		t.Fatal("serveDubboIfConfiguredCompiled() = false, want true")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "from hessian upstream" {
		t.Fatalf("response body = %q, want hessian upstream response", rr.Body.String())
	}
}

func TestServeDubboIfConfiguredUsesSafeUpstreamRetries(t *testing.T) {
	upstream := startRouteHessianDubboTestServer(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen retry target: %v", err)
	}
	failedTarget := listener.Addr().String()
	_ = listener.Close()
	lb := &sequenceLoadBalancer{targets: []string{"dubbo://" + failedTarget, "dubbo://" + upstream}}

	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = dubbo_proxy.WithConfig(req, dubbo_proxy.Config{
		ServiceName:    "svc",
		ServiceVersion: "1.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()

	if !serveDubboIfConfiguredCompiled(rr, req, lb, nil, 1) {
		t.Fatal("serveDubboIfConfiguredCompiled() = false, want true")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "from hessian upstream" {
		t.Fatalf("response body = %q, want from hessian upstream", rr.Body.String())
	}
	if lb.index != 2 {
		t.Fatalf("selected targets = %d, want 2", lb.index)
	}
}

func TestServeDubboAndHTTPDubboUseDefaultRetryCountForRouteTerminals(t *testing.T) {
	builder := &Builder{}
	t.Cleanup(builder.Stop)
	upstream := resource.Upstream{
		Scheme: "dubbo",
		Nodes: []resource.Node{
			{Host: "first.example", Port: 20880, Weight: 1},
			{Host: "second.example", Port: 20880, Weight: 1},
		},
	}
	_, terminals, err := builder.buildReverseHandlerWithTerminals(resource.Route{}, resource.Service{}, upstream)
	if err != nil {
		t.Fatalf("buildReverseHandlerWithTerminals() error = %v", err)
	}
	dubboTerminal, ok := terminals.dubbo.(routeDubboTerminal)
	if !ok {
		t.Fatalf("Dubbo terminal = %T, want routeDubboTerminal", terminals.dubbo)
	}
	if dubboTerminal.retries != 1 {
		t.Fatalf("Dubbo terminal retries = %d, want nodes-1 (1)", dubboTerminal.retries)
	}
	httpDubboTerminal, ok := terminals.httpDubbo.(routeHTTPDubboTerminal)
	if !ok {
		t.Fatalf("HTTP-Dubbo terminal = %T, want routeHTTPDubboTerminal", terminals.httpDubbo)
	}
	if httpDubboTerminal.retries != 1 {
		t.Fatalf("HTTP-Dubbo terminal retries = %d, want nodes-1 (1)", httpDubboTerminal.retries)
	}
}

func TestServeDubboIfConfiguredUsesRequestAwarePriorityRetryTargets(t *testing.T) {
	upstream := startRouteHessianDubboTestServer(t)
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
	req = dubbo_proxy.WithConfig(req, dubbo_proxy.Config{
		ServiceName:    "svc",
		ServiceVersion: "1.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()

	if !serveDubboIfConfiguredCompiled(rr, req, lb, targets, 1) {
		t.Fatal("serveDubboIfConfiguredCompiled() = false, want true")
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "from hessian upstream" {
		t.Fatalf("response = %d/%q, want 200/from hessian upstream", rr.Code, rr.Body.String())
	}
}

func TestServeDubboIfConfiguredAdvancesTrafficSplitRetryOverride(t *testing.T) {
	upstream := startRouteHessianDubboTestServer(t)
	failedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed target: %v", err)
	}
	failedTarget := failedListener.Addr().String()
	_ = failedListener.Close()
	firstReporter := &recordingDubboHealthReporter{}
	secondReporter := &recordingDubboHealthReporter{}
	second := &traffic_split.Override{
		Scheme:         "dubbo",
		Host:           upstream,
		HealthReporter: secondReporter,
		HealthTarget:   "dubbo://" + upstream,
	}
	first := &traffic_split.Override{
		Scheme:         "dubbo",
		Host:           failedTarget,
		Retries:        1,
		HealthReporter: firstReporter,
		HealthTarget:   "dubbo://" + failedTarget,
	}
	first.NextRetry = func(*http.Request) *traffic_split.Override { return second }
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = traffic_split.WithOverride(req, first)
	req = dubbo_proxy.WithConfig(req, dubbo_proxy.Config{
		ServiceName:    "svc",
		ServiceVersion: "1.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()

	if !serveDubboIfConfiguredCompiled(rr, req, nil, nil) {
		t.Fatal("serveDubboIfConfiguredCompiled() = false, want true")
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "from hessian upstream" {
		t.Fatalf("response = %d/%q, want 200/from hessian upstream", rr.Code, rr.Body.String())
	}
	if len(firstReporter.tcpTargets) != 1 || firstReporter.tcpTargets[0] != first.HealthTarget {
		t.Fatalf("first retry health targets = %#v, want [%q]", firstReporter.tcpTargets, first.HealthTarget)
	}
	if len(secondReporter.httpTargets) != 1 || secondReporter.httpTargets[0] != second.HealthTarget {
		t.Fatalf("second retry health targets = %#v, want [%q]", secondReporter.httpTargets, second.HealthTarget)
	}
}

func TestRouteDubboTerminalUsesTrafficSplitOverrideWithoutBaseUpstream(t *testing.T) {
	upstream := startRouteHessianDubboTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{
		Scheme: "dubbo",
		Host:   upstream,
	})
	req = dubbo_proxy.WithConfig(req, dubbo_proxy.Config{
		ServiceName:    "svc",
		ServiceVersion: "1.0.0",
		Method:         "hello",
	})
	rr := httptest.NewRecorder()
	fallbackCalled := false

	_, _, _, err := (routeDubboTerminal{}).RunExclusiveProtocol(
		rr,
		req,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fallbackCalled = true }),
	)
	if err != nil {
		t.Fatalf("RunExclusiveProtocol() error = %v", err)
	}
	if fallbackCalled {
		t.Fatal("fallback handler ran despite traffic-split Dubbo override")
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "from hessian upstream" {
		t.Fatalf("response = %d/%q, want 200/from hessian upstream", rr.Code, rr.Body.String())
	}
}

func TestServeDubboIfConfiguredDoesNotReattributeMissingTrafficSplitRetryTarget(t *testing.T) {
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
				Scheme:         "dubbo",
				Host:           failedTarget,
				Retries:        1,
				HealthReporter: reporter,
				HealthTarget:   "dubbo://" + failedTarget,
				NextRetry:      test.nextRetry,
			}
			req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
			req = traffic_split.WithOverride(req, override)
			req = dubbo_proxy.WithConfig(req, dubbo_proxy.Config{
				ServiceName:    "svc",
				ServiceVersion: "1.0.0",
				Method:         "hello",
			})
			rr := httptest.NewRecorder()

			if !serveDubboIfConfiguredCompiled(rr, req, nil, nil) {
				t.Fatal("serveDubboIfConfiguredCompiled() = false, want true")
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

func TestServeDubboIfConfiguredSkipsUnconfiguredRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	rr := httptest.NewRecorder()

	if serveDubboIfConfiguredCompiled(
		rr,
		req,
		pxy.NewWeightedRRLoadBalance(map[string]int{"dubbo://127.0.0.1:20880": 1}),
		nil,
	) {
		t.Fatal("serveDubboIfConfiguredCompiled() = true, want false")
	}
}

func startRouteHessianDubboTestServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if !discardRouteHessianFrame(conn) {
			return
		}
		encoder := hessian.NewEncoder()
		_ = encoder.Encode(int32(1))
		_ = encoder.Encode(map[string]any{"body": "from hessian upstream"})
		frame := make([]byte, 16+len(encoder.Buffer()))
		frame[0], frame[1], frame[2], frame[3] = 0xda, 0xbb, 0x02, 20
		binary.BigEndian.PutUint64(frame[4:12], 1)
		binary.BigEndian.PutUint32(frame[12:16], uint32(len(encoder.Buffer())))
		copy(frame[16:], encoder.Buffer())
		writer := bufio.NewWriter(conn)
		_, _ = writer.Write(frame)
		_ = writer.Flush()
	}()
	return listener.Addr().String()
}

func discardRouteHessianFrame(conn net.Conn) bool {
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		return false
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[12:16]))
	_, err := io.ReadFull(conn, payload)
	return err == nil
}
