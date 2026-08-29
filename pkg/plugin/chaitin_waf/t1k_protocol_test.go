package chaitin_waf

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const pinnedRejectEventID = "b3c6ce574dc24f09a01f634a39dca83b"

type testT1KFrame struct {
	tag     byte
	payload []byte
}

type legacyT1KServer struct {
	URL      string
	listener net.Listener
	handler  http.Handler
	close    sync.Once
}

func newLegacyT1KServer(t *testing.T, handler http.Handler) *legacyT1KServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for T1K fixture: %v", err)
	}
	server := &legacyT1KServer{
		URL:      "http://" + listener.Addr().String(),
		listener: listener,
		handler:  handler,
	}
	go server.serve()
	return server
}

func (server *legacyT1KServer) serve() {
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			return
		}
		go server.serveConnection(connection)
	}
}

func (server *legacyT1KServer) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	frames, err := readTestT1KFrames(connection)
	if err != nil {
		return
	}
	request, err := testHTTPRequestFromT1KFrames(frames)
	if err != nil {
		return
	}
	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, request)

	var decision wafDecision
	if err := json.Unmarshal(recorder.Body.Bytes(), &decision); err != nil {
		_ = writeTestT1KFrame(connection, testT1KFrame{tag: 0xc1, payload: []byte("invalid")})
		return
	}
	if decision.Status == 0 {
		decision.Status = recorder.Code
	}
	if decision.Status == http.StatusOK {
		_ = writeTestT1KFrame(connection, testT1KFrame{tag: 0x41, payload: []byte(".")})
		_ = writeTestT1KFrame(connection, testT1KFrame{
			tag:     0xa5,
			payload: []byte(`{"event_id":"fixture","request_hit_whitelist":false}`),
		})
		return
	}
	_ = writeTestT1KFrame(connection, testT1KFrame{tag: 0x41, payload: []byte("?")})
	lastStatusTag := byte(0x02)
	if decision.EventID == "" {
		lastStatusTag |= t1kMaskLast
	}
	_ = writeTestT1KFrame(connection, testT1KFrame{tag: lastStatusTag, payload: []byte(strconv.Itoa(decision.Status))})
	if decision.EventID != "" {
		_ = writeTestT1KFrame(connection, testT1KFrame{
			tag:     0xa4,
			payload: []byte("<!-- event_id: " + decision.EventID + " -->"),
		})
	}
}

func (server *legacyT1KServer) Close() {
	server.close.Do(func() { _ = server.listener.Close() })
}

func testHTTPRequestFromT1KFrames(frames []testT1KFrame) (*http.Request, error) {
	if len(frames) < 3 || frames[0].tag != 0x41 {
		return nil, fmt.Errorf("invalid T1K fixture request frames")
	}
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(frames[0].payload)))
	if err != nil {
		return nil, fmt.Errorf("parse T1K HEAD: %w", err)
	}
	var body []byte
	if len(frames) == 4 && frames[1].tag == t1kTagBody {
		body = frames[1].payload
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	return request, nil
}

func TestHandlerUsesT1KV2AndAppliesPinnedReject(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverErr := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverErr <- fmt.Errorf("accept: %w", err)
			return
		}
		defer func() { _ = connection.Close() }()
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))

		frames, err := readTestT1KFrames(connection)
		if err != nil {
			serverErr <- err
			return
		}
		if err := assertPinnedRequestFrames(frames); err != nil {
			serverErr <- err
			return
		}

		responses := []testT1KFrame{
			{tag: 0x41, payload: []byte("?")},
			{tag: 0x02, payload: []byte("403")},
			{tag: 0x25, payload: []byte(`{"event_id":"` + pinnedRejectEventID + `","request_hit_whitelist":false}`)},
			{
				tag: 0x23,
				payload: []byte(
					"Set-Cookie:sl-session=ulgbPfMSuWRNsi/u7Aj9aA==; Domain=; Path=/; Max-Age=86400\n" +
						"X-SafeLine-Test:blocked\n",
				),
			},
			{tag: 0xa4, payload: []byte("<!-- event_id: " + pinnedRejectEventID + " -->")},
		}
		for _, frame := range responses {
			if err := writeTestT1KFrame(connection, frame); err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}()

	address := listener.Addr().(*net.TCPAddr)
	p := newTestPlugin(t, Config{
		Mode:                 "block",
		AppendWAFRespHeader:  new(true),
		AppendWAFDebugHeader: new(false),
		Nodes:                []Node{{Host: address.IP.String(), Port: address.Port}},
	})

	request := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders?debug=1",
		strings.NewReader("a=1 and 1=1"),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Test", "value")
	request.RemoteAddr = "198.51.100.2:12345"
	request = request.WithContext(withTestLocalAddr(request, "192.0.2.10:9080"))

	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("T1K rejection reached downstream")
	})).ServeHTTP(response, request)

	if err := <-serverErr; err != nil {
		t.Fatalf("T1K fixture: %v", err)
	}
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", response.Code, response.Body.String())
	}
	wantBody := `{"code": 403, "success":false, "message": "blocked by Chaitin SafeLine Web Application Firewall", "event_id": "` + pinnedRejectEventID + `"}` + "\n"
	if response.Body.String() != wantBody {
		t.Fatalf("body = %q, want %q", response.Body.String(), wantBody)
	}
	wantCookie := "sl-session=ulgbPfMSuWRNsi/u7Aj9aA==; Domain=; Path=/; Max-Age=86400"
	if got := response.Header().Get("Set-Cookie"); got != wantCookie {
		t.Fatalf("Set-Cookie = %q", got)
	}
	if got := response.Header().Get("X-SafeLine-Test"); got != "blocked" {
		t.Fatalf("X-SafeLine-Test = %q, want blocked", got)
	}
	if response.Header().Get(HeaderChaitinWAF) != "yes" ||
		response.Header().Get(HeaderChaitinWAFStatus) != "403" ||
		response.Header().Get(HeaderChaitinWAFAction) != "reject" {
		t.Fatalf("WAF headers = %#v", response.Header())
	}
}

func TestReadT1KDecisionRejectsMalformedFrames(t *testing.T) {
	frame := func(tag byte, payload string) []byte {
		var header [5]byte
		header[0] = tag
		binary.LittleEndian.PutUint32(header[1:], uint32(len(payload)))
		return append(header[:], payload...)
	}
	oversizedHeader := []byte{0xc1, 1, 0, 0x10, 0}
	tests := []struct {
		name string
		wire []byte
		want string
	}{
		{name: "truncated header", wire: []byte{0x41, 1}, want: "response header"},
		{name: "truncated payload", wire: []byte{0xc1, 2, 0, 0, 0, '.'}, want: "payload"},
		{name: "oversized payload", wire: oversizedHeader, want: "exceeds"},
		{name: "unknown tag", wire: frame(0xc6, "x"), want: "unknown tag"},
		{
			name: "out of order",
			wire: append(
				append(frame(0x41, "?"), frame(0x23, "X-Test:one\n")...),
				frame(0x82, "403")...,
			),
			want: "out of order",
		},
		{name: "missing first mask", wire: frame(0x81, "."), want: "lacks FIRST"},
		{name: "unknown action", wire: frame(0xc1, "!"), want: "unknown action"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := readT1KDecision(bytes.NewReader(test.wire))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("readT1KDecision() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestReadT1KDecisionPreservesRepeatedResponseHeaders(t *testing.T) {
	var wire bytes.Buffer
	for _, frame := range []testT1KFrame{
		{tag: 0x41, payload: []byte("?")},
		{tag: 0x02, payload: []byte("403")},
		{tag: 0x23, payload: []byte("Set-Cookie:first=1\nSet-Cookie:second=2\n")},
		{tag: 0xa4, payload: []byte("<!-- event_id: " + pinnedRejectEventID + " -->")},
	} {
		if err := writeTestT1KFrame(&wire, frame); err != nil {
			t.Fatal(err)
		}
	}
	decision, err := readT1KDecision(&wire)
	if err != nil {
		t.Fatalf("readT1KDecision() error = %v", err)
	}
	got := decision.ResponseHeaders.Values("Set-Cookie")
	if len(got) != 2 || got[0] != "first=1" || got[1] != "second=2" {
		t.Fatalf("Set-Cookie values = %#v, want both values in wire order", got)
	}
}

func withTestLocalAddr(request *http.Request, address string) context.Context {
	return context.WithValue(request.Context(), http.LocalAddrContextKey, testNetAddr(address))
}

type testNetAddr string

func (address testNetAddr) Network() string { return "tcp" }
func (address testNetAddr) String() string  { return string(address) }

func readTestT1KFrames(reader io.Reader) ([]testT1KFrame, error) {
	var frames []testT1KFrame
	for {
		var header [5]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return nil, fmt.Errorf("read T1K header: %w", err)
		}
		length := binary.LittleEndian.Uint32(header[1:])
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, fmt.Errorf("read T1K payload: %w", err)
		}
		frames = append(frames, testT1KFrame{tag: header[0], payload: payload})
		if header[0]&0x80 != 0 {
			return frames, nil
		}
	}
}

func writeTestT1KFrame(writer io.Writer, frame testT1KFrame) error {
	var header [5]byte
	header[0] = frame.tag
	binary.LittleEndian.PutUint32(header[1:], uint32(len(frame.payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return fmt.Errorf("write T1K header: %w", err)
	}
	if _, err := writer.Write(frame.payload); err != nil {
		return fmt.Errorf("write T1K payload: %w", err)
	}
	return nil
}

func assertPinnedRequestFrames(frames []testT1KFrame) error {
	if len(frames) != 4 {
		return fmt.Errorf("request frame count = %d, want 4: %#v", len(frames), frames)
	}
	wantTags := []byte{0x41, 0x02, 0x20, 0x83}
	for index, want := range wantTags {
		if frames[index].tag != want {
			return fmt.Errorf("frame %d tag = 0x%02x, want 0x%02x", index, frames[index].tag, want)
		}
	}
	wantHeader := "POST /orders?debug=1 HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Content-Length: 11\r\n" +
		"Content-Type: application/x-www-form-urlencoded\r\n" +
		"X-Test: value\r\n\r\n"
	if string(frames[0].payload) != wantHeader {
		return fmt.Errorf("HEAD payload = %q, want %q", frames[0].payload, wantHeader)
	}
	if string(frames[1].payload) != "a=1 and 1=1" {
		return fmt.Errorf("BODY payload = %q", frames[1].payload)
	}
	if string(frames[2].payload) != "Proto:2\n" {
		return fmt.Errorf("VERSION payload = %q", frames[2].payload)
	}

	extra := string(frames[3].payload)
	for _, line := range []string{
		"RemoteAddr:198.51.100.2\n",
		"RemotePort:12345\n",
		"LocalAddr:192.0.2.10\n",
		"LocalPort:9080\n",
		"Scheme:http\n",
		"ServerName:example.com\n",
		"HasRspIfOK:n\n",
		"HasRspIfBlock:n\n",
	} {
		if !strings.Contains(extra, line) {
			return fmt.Errorf("EXTRA payload missing %q: %q", line, extra)
		}
	}
	values := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSuffix(extra, "\n"), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key == "" || value == "" {
			return fmt.Errorf("malformed EXTRA line %q", line)
		}
		values[key] = value
	}
	if len(values["UUID"]) != 36 {
		return fmt.Errorf("UUID = %q, want UUID-shaped value", values["UUID"])
	}
	if _, err := strconv.ParseInt(values["ReqBeginTime"], 10, 64); err != nil {
		return fmt.Errorf("ReqBeginTime = %q: %w", values["ReqBeginTime"], err)
	}
	if values["ProxyName"] == "" {
		return fmt.Errorf("ProxyName is empty")
	}
	return nil
}
