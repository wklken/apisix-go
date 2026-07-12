package dubbo_proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apache/dubbo-go-hessian2"
	appconfig "github.com/wklken/apisix-go/pkg/config"
)

func TestBuildDubboRequestEncodesHTTPContextMap(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/demo", strings.NewReader("request body"))
	req.Host = "example.org"
	req.Header.Add("User", "apisix")
	req.Header.Add("X-Multi", "first")
	req.Header.Add("X-Multi", "second")

	frame, err := buildDubboRequest(req, Config{
		ServiceName:    "com.example.DemoService",
		ServiceVersion: "1.2.3",
		Method:         "sayHello",
	})
	if err != nil {
		t.Fatalf("buildDubboRequest() error = %v", err)
	}
	if got := frame[:2]; !bytes.Equal(got, []byte{0xda, 0xbb}) {
		t.Fatalf("magic = %x, want da bb", got)
	}
	if frame[2] != 0xc2 {
		t.Fatalf("message flag = %#x, want Hessian2 two-way request %#x", frame[2], byte(0xc2))
	}
	if got := binary.BigEndian.Uint32(frame[12:16]); got != uint32(len(frame)-16) {
		t.Fatalf("payload length = %d, want %d", got, len(frame)-16)
	}

	decoder := hessian.NewDecoder(frame[16:])
	for _, want := range []string{"2.0.2", "com.example.DemoService", "1.2.3", "sayHello", "Ljava/util/Map;"} {
		value, err := decoder.Decode()
		if err != nil {
			t.Fatalf("decode request metadata %q: %v", want, err)
		}
		if got, ok := value.(string); !ok || got != want {
			t.Fatalf("request metadata = %#v, want %q", value, want)
		}
	}

	contextValue, err := decoder.Decode()
	if err != nil {
		t.Fatalf("decode HTTP context: %v", err)
	}
	contextMap := hessianMapForTest(t, contextValue)
	if got := contextMap["user"]; got != "apisix" {
		t.Fatalf("context user = %#v, want apisix", got)
	}
	if got := contextMap["host"]; got != "example.org" {
		t.Fatalf("context host = %#v, want example.org", got)
	}
	if got := contextMap["body"]; !bytes.Equal(got.([]byte), []byte("request body")) {
		t.Fatalf("context body = %#v, want request body", got)
	}
	if values, ok := contextMap["x-multi"].([]string); !ok || len(values) != 2 || values[0] != "first" ||
		values[1] != "second" {
		t.Fatalf("context x-multi = %#v, want two values", contextMap["x-multi"])
	}
}

func TestServeDubboDecodesMapResponseIntoHTTP(t *testing.T) {
	requests := make(chan []byte, 1)
	target, _ := startHessianDubboServer(t, func(conn net.Conn) {
		request, err := readDubboFrameForTest(conn)
		if err != nil {
			return
		}
		requests <- request
		encoder := hessian.NewEncoder()
		_ = encoder.Encode(int32(1))
		_ = encoder.Encode(map[string]interface{}{
			"status":   201,
			"x-result": "ok",
			"body":     []byte("hessian response"),
		})
		_ = writeDubboFrameForTest(conn, 20, encoder.Buffer())
	})

	p := newTestPlugin(t, Config{
		ServiceName:    "com.example.DemoService",
		ServiceVersion: "1.2.3",
		Method:         "sayHello",
	})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/demo", strings.NewReader("body"))
	req.Header.Set("User", "apisix")
	rr := httptest.NewRecorder()
	p.ServeDubbo(rr, req, target)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want 201; body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Result"); got != "ok" {
		t.Fatalf("X-Result = %q, want ok", got)
	}
	if got := rr.Body.String(); got != "hessian response" {
		t.Fatalf("response body = %q, want hessian response", got)
	}
	select {
	case request := <-requests:
		if len(request) < 16 {
			t.Fatalf("captured request length = %d, want Dubbo frame", len(request))
		}
	case <-time.After(time.Second):
		t.Fatal("Dubbo server did not receive request")
	}
}

func TestServeDubboMapsHessianExceptionToBadGateway(t *testing.T) {
	target, _ := startHessianDubboServer(t, func(conn net.Conn) {
		if _, err := readDubboFrameForTest(conn); err != nil {
			return
		}
		encoder := hessian.NewEncoder()
		_ = encoder.Encode(int32(0))
		_ = encoder.Encode("provider failed")
		_ = writeDubboFrameForTest(conn, 20, encoder.Buffer())
	})

	p := newTestPlugin(t, Config{ServiceName: "svc", ServiceVersion: "1.0.0", Method: "call"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/demo", nil)
	rr := httptest.NewRecorder()
	p.ServeDubbo(rr, req, target)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want 502; body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "provider failed") {
		t.Fatalf("response body = %q, want provider error", rr.Body.String())
	}
}

func TestServeDubboWithRetriesRetriesConnectFailure(t *testing.T) {
	target, _ := startHessianDubboServer(t, func(conn net.Conn) {
		if _, err := readDubboFrameForTest(conn); err != nil {
			return
		}
		encoder := hessian.NewEncoder()
		_ = encoder.Encode(int32(1))
		_ = encoder.Encode(map[string]interface{}{"body": "retry-success"})
		_ = writeDubboFrameForTest(conn, 20, encoder.Buffer())
	})
	p := newTestPlugin(t, Config{ServiceName: "svc", ServiceVersion: "1.0.0", Method: "call"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/demo", nil)
	rr := httptest.NewRecorder()
	targets := []string{"127.0.0.1:1", target}
	index := 0

	ServeDubboWithRetries(rr, req, func() (string, error) {
		selected := targets[index]
		index++
		return selected, nil
	}, p.config, 1)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "retry-success" {
		t.Fatalf("response body = %q, want retry-success", rr.Body.String())
	}
	if index != 2 {
		t.Fatalf("target attempts = %d, want 2", index)
	}
}

func TestServeDubboWithRetriesDoesNotRetryAfterRequestWrite(t *testing.T) {
	target, _ := startHessianDubboServer(t, func(conn net.Conn) {
		_, _ = readDubboFrameForTest(conn)
	})
	p := newTestPlugin(t, Config{ServiceName: "svc", ServiceVersion: "1.0.0", Method: "call"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/demo", nil)
	rr := httptest.NewRecorder()
	attempts := 0

	ServeDubboWithRetries(rr, req, func() (string, error) {
		attempts++
		return target, nil
	}, p.config, 1)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want 502; body=%q", rr.Code, rr.Body.String())
	}
	if attempts != 1 {
		t.Fatalf("target attempts = %d, want 1 after request write", attempts)
	}
}

func TestPostInitLoadsDubboMultiplexLimit(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{
		PluginAttr: map[string]map[string]interface{}{
			"dubbo-proxy": {"upstream_multiplex_count": 3},
		},
	}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{ServiceName: "svc", ServiceVersion: "1.0.0"})
	if got := p.config.MultiplexCount; got != 3 {
		t.Fatalf("multiplex count = %d, want 3", got)
	}
}

func TestTargetLimiterHonorsContextCancellation(t *testing.T) {
	first, release := acquireTargetSlot(context.Background(), "127.0.0.1:1", 1)
	if !first {
		t.Fatal("first target slot acquisition failed")
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if acquired, _ := acquireTargetSlot(ctx, "127.0.0.1:1", 1); acquired {
		t.Fatal("second target slot acquisition succeeded while limit was full")
	}
}

func hessianMapForTest(t *testing.T, value interface{}) map[interface{}]interface{} {
	t.Helper()
	result, ok := value.(map[interface{}]interface{})
	if !ok {
		t.Fatalf("decoded map type = %T, want map[interface{}]interface{}", value)
	}
	return result
}

func startHessianDubboServer(t *testing.T, handler func(net.Conn)) (string, chan []byte) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requests := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	return listener.Addr().String(), requests
}

func readDubboFrameForTest(conn net.Conn) ([]byte, error) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	body := make([]byte, binary.BigEndian.Uint32(header[12:16]))
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	return append(header, body...), nil
}

func writeDubboFrameForTest(conn net.Conn, status byte, payload []byte) error {
	frame := make([]byte, 16+len(payload))
	frame[0], frame[1], frame[2], frame[3] = 0xda, 0xbb, 0x02, status
	binary.BigEndian.PutUint64(frame[4:12], 1)
	binary.BigEndian.PutUint32(frame[12:16], uint32(len(payload)))
	copy(frame[16:], payload)
	writer := bufio.NewWriter(conn)
	if _, err := writer.Write(frame); err != nil {
		return err
	}
	return writer.Flush()
}
