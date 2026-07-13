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

	if !serveHTTPDubboIfConfigured(rr, req, lb) {
		t.Fatal("serveHTTPDubboIfConfigured() = false, want true")
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

	if !serveHTTPDubboIfConfigured(rr, req, lb, 1) {
		t.Fatal("serveHTTPDubboIfConfigured() = false, want true")
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

func TestServeHTTPDubboIfConfiguredSkipsUnconfiguredRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	rr := httptest.NewRecorder()

	if serveHTTPDubboIfConfigured(rr, req, pxy.NewWeightedRRLoadBalance(map[string]int{"dubbo://127.0.0.1:20880": 1})) {
		t.Fatal("serveHTTPDubboIfConfigured() = true, want false")
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
