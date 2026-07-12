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
	pxy "github.com/wklken/apisix-go/pkg/proxy"
)

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

	if !serveDubboIfConfigured(rr, req, lb) {
		t.Fatal("serveDubboIfConfigured() = false, want true")
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

	if !serveDubboIfConfigured(rr, req, lb, 1) {
		t.Fatal("serveDubboIfConfigured() = false, want true")
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

func TestServeDubboIfConfiguredSkipsUnconfiguredRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	rr := httptest.NewRecorder()

	if serveDubboIfConfigured(rr, req, pxy.NewWeightedRRLoadBalance(map[string]int{"dubbo://127.0.0.1:20880": 1})) {
		t.Fatal("serveDubboIfConfigured() = true, want false")
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
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if !discardRouteHessianFrame(conn) {
			return
		}
		encoder := hessian.NewEncoder()
		_ = encoder.Encode(int32(1))
		_ = encoder.Encode(map[string]interface{}{"body": "from hessian upstream"})
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
