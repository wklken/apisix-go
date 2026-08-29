package chaitin_waf

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandlerFailsClosedOnMalformedWAFResponse(t *testing.T) {
	waf := newLegacyT1KServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":`))
	}))
	t.Cleanup(waf.Close)
	p := newTestPlugin(t, Config{
		Mode: "block", AppendWAFRespHeader: new(true), Nodes: []Node{nodeFromURL(t, waf.URL)},
	})

	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("malformed WAF response reached downstream")
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("malformed WAF status = %d, want 500", response.Code)
	}
	if response.Header().Get(HeaderChaitinWAF) != "waf-err" {
		t.Fatalf("malformed WAF header = %q, want waf-err", response.Header().Get(HeaderChaitinWAF))
	}
}

func TestHandlerClosesT1KConnectionAfterResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = connection.Close() }()
		if _, err := readTestT1KFrames(connection); err != nil {
			serverDone <- err
			return
		}
		if err := writeTestT1KFrame(connection, testT1KFrame{tag: 0x41, payload: []byte(".")}); err != nil {
			serverDone <- err
			return
		}
		passMetadata := testT1KFrame{
			tag:     0xa5,
			payload: []byte(`{"event_id":"fixture","request_hit_whitelist":false}`),
		}
		if err := writeTestT1KFrame(connection, passMetadata); err != nil {
			serverDone <- err
			return
		}
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		var one [1]byte
		_, err = connection.Read(one[:])
		if err != io.EOF {
			serverDone <- fmt.Errorf("read after response = %v, want EOF from client close", err)
			return
		}
		serverDone <- nil
	}()

	address := listener.Addr().(*net.TCPAddr)
	p := newTestPlugin(t, Config{Mode: "monitor", Nodes: []Node{{Host: address.IP.String(), Port: address.Port}}})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("T1K client did not close the per-request connection")
	}
}

func TestStopDoesNotInterruptInFlightWAFRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		if _, readErr := readTestT1KFrames(connection); readErr != nil {
			return
		}
		close(entered)
		<-release
		_ = writeTestT1KFrame(connection, testT1KFrame{tag: 0x41, payload: []byte(".")})
		_ = writeTestT1KFrame(connection, testT1KFrame{
			tag:     0xa5,
			payload: []byte(`{"event_id":"fixture","request_hit_whitelist":false}`),
		})
	}()
	address := listener.Addr().(*net.TCPAddr)
	p := newTestPlugin(t, Config{Mode: "monitor", Nodes: []Node{{Host: address.IP.String(), Port: address.Port}}})

	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("WAF request did not reach fixture")
	}
	stopper, ok := any(p).(interface{ Stop() })
	if !ok {
		close(release)
		<-done
		t.Fatal("chaitin-waf has no Stop lifecycle hook")
	}
	stopper.Stop()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("in-flight WAF request did not complete after Stop")
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("in-flight status = %d, want 204", response.Code)
	}
}
