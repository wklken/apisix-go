package http_dubbo

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerStoresHTTPDubboConfig(t *testing.T) {
	p := newTestPlugin(t, Config{
		ServiceName:    "org.apache.dubbo.sample.DemoService",
		ServiceVersion: "1.2.3",
		Method:         "sayHello",
		ParamsTypeDesc: "Ljava/lang/String;",
	})

	req := httptest.NewRequest(http.MethodPost, "/dubbo", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg, ok := GetConfig(r)
		if !ok {
			t.Fatal("GetConfig() ok = false, want true")
		}
		if cfg.ServiceName != "org.apache.dubbo.sample.DemoService" {
			t.Fatalf("service name = %q, want configured service", cfg.ServiceName)
		}
		if cfg.ServiceVersion != "1.2.3" {
			t.Fatalf("service version = %q, want 1.2.3", cfg.ServiceVersion)
		}
		if cfg.Method != "sayHello" {
			t.Fatalf("method = %q, want sayHello", cfg.Method)
		}
		if cfg.ParamsTypeDesc != "Ljava/lang/String;" {
			t.Fatalf("params type desc = %q, want Java descriptor", cfg.ParamsTypeDesc)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestBuildDubboRequestSerializesGenericInvocationParams(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/dubbo", strings.NewReader(`["abc\"",123,{"name":"apisix"},null]`))
	cfg := Config{
		ServiceName:    "org.apache.dubbo.sample.DemoService",
		ServiceVersion: "0.0.0",
		Method:         "sayHello",
		ParamsTypeDesc: "Ljava/lang/String;Ljava/lang/Long;Ljava/util/Map;Ljava/lang/Object;",
	}

	frame, err := buildDubboRequest(req, cfg)
	if err != nil {
		t.Fatalf("buildDubboRequest() error = %v", err)
	}

	if !bytes.Equal(frame[:4], []byte{0xda, 0xbb, 0xc6, 0x00}) {
		t.Fatalf("header first bytes = % x, want Dubbo magic/request flags", frame[:4])
	}
	if got := binary.BigEndian.Uint64(frame[4:12]); got != 1 {
		t.Fatalf("request id = %d, want 1", got)
	}
	payload := string(frame[16:])
	wantPayload := "\"2.0.2\"\n" +
		"\"org.apache.dubbo.sample.DemoService\"\n" +
		"\"0.0.0\"\n" +
		"\"sayHello\"\n" +
		"\"Ljava/lang/String;Ljava/lang/Long;Ljava/util/Map;Ljava/lang/Object;\"\n" +
		"\"abc\\\"\"\n" +
		"123\n" +
		"{\"name\":\"apisix\"}\n" +
		"null\n" +
		"{}\n"
	if payload != wantPayload {
		t.Fatalf("payload = %q, want %q", payload, wantPayload)
	}
	if got := binary.BigEndian.Uint32(frame[12:16]); got != uint32(len(payload)) {
		t.Fatalf("payload length = %d, want %d", got, len(payload))
	}
}

func TestBuildDubboRequestUsesSerializedBodyWhenHeaderIsTrue(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/dubbo", strings.NewReader("1\n2"))
	req.Header.Set("X-Dubbo-Serialized", "true")
	cfg := Config{
		ServiceName:            "svc",
		ServiceVersion:         "0.0.0",
		Method:                 "sum",
		SerializationHeaderKey: "X-Dubbo-Serialized",
	}

	frame, err := buildDubboRequest(req, cfg)
	if err != nil {
		t.Fatalf("buildDubboRequest() error = %v", err)
	}

	payload := string(frame[16:])
	if !strings.Contains(payload, "\"sum\"\n\"\"\n1\n2\n{}\n") {
		t.Fatalf("payload = %q, want raw serialized params with trailing newline", payload)
	}
}

func TestServeDubboReturnsBodyForApplicationResponse(t *testing.T) {
	upstream, seenRequest := startDubboTestServer(t, dubboFrame("1\nhello dubbo\n"))
	p := newTestPlugin(t, Config{
		ServiceName:    "svc",
		ServiceVersion: "0.0.0",
		Method:         "hello",
	})

	req := httptest.NewRequest(http.MethodPost, "/dubbo", strings.NewReader(`["alice"]`))
	rr := httptest.NewRecorder()

	p.ServeDubbo(rr, req, upstream)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "hello dubbo" {
		t.Fatalf("response body = %q, want upstream application body", rr.Body.String())
	}
	request := <-seenRequest
	if !bytes.Contains(request, []byte("\"svc\"\n\"0.0.0\"\n\"hello\"\n")) {
		t.Fatalf("dubbo request = %q, want service/version/method payload", request)
	}
}

func TestServeDubboReturnsBadGatewayOnTCPFailure(t *testing.T) {
	p := newTestPlugin(t, Config{ServiceName: "svc", ServiceVersion: "0.0.0", Method: "hello"})
	req := httptest.NewRequest(http.MethodPost, "/dubbo", strings.NewReader(`[]`))
	rr := httptest.NewRecorder()

	p.ServeDubbo(rr, req, "127.0.0.1:1")

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want 502", rr.Code)
	}
}

func startDubboTestServer(t *testing.T, response []byte) (string, <-chan []byte) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	seen := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		request := readDubboFrameForTest(conn)
		seen <- request
		_, _ = conn.Write(response)
	}()

	return ln.Addr().String(), seen
}

func readDubboFrameForTest(conn net.Conn) []byte {
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		return header
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[12:16]))
	_, _ = io.ReadFull(conn, payload)
	return append(header, payload...)
}

func dubboFrame(payload string) []byte {
	frame := make([]byte, 16+len(payload))
	frame[0], frame[1] = 0xda, 0xbb
	frame[3] = 20
	binary.BigEndian.PutUint64(frame[4:12], 1)
	binary.BigEndian.PutUint32(frame[12:16], uint32(len(payload)))
	copy(frame[16:], payload)
	return frame
}
