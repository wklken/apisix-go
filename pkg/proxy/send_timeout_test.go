package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendTimeoutCoversHeaderWrite(t *testing.T) {
	for _, name := range []string{"bodyless", "buffered-body", "large-headers"} {
		t.Run(name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer func() { _ = peer.Close() }()
			base := &http.Transport{
				DialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil },
			}
			defer base.CloseIdleConnections()
			transport := NewProgressTimeoutTransport(base, 40*time.Millisecond, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var body io.Reader
			if name == "buffered-body" {
				body = strings.NewReader("payload")
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.test/", body)
			if err != nil {
				t.Fatal(err)
			}
			if name == "large-headers" {
				request.Header.Set("X-Large", strings.Repeat("a", 8192))
			}
			started := time.Now()
			response, err := transport.RoundTrip(request)
			if response != nil {
				_ = response.Body.Close()
			}
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || time.Since(started) > 500*time.Millisecond {
				t.Fatalf("stalled send error/elapsed = %v/%s", err, time.Since(started))
			}
		})
	}
}

func TestSendTimeoutExcludesConnectResponseAndIdleTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	base := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		time.Sleep(100 * time.Millisecond)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}, ResponseHeaderTimeout: time.Second}
	defer base.CloseIdleConnections()
	client := &http.Client{
		Transport: NewProgressTimeoutTransport(base, 40*time.Millisecond, time.Second),
		Timeout:   2 * time.Second,
	}
	defer client.CloseIdleConnections()
	var reused atomic.Int32
	for range 3 {
		request, err := http.NewRequest(http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				reused.Add(1)
			}
		}}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(data) != "ok" {
			t.Fatalf("response = %q, %v", data, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if reused.Load() != 2 {
		t.Fatalf("reused connections = %d, want 2", reused.Load())
	}
}

func TestSendTimeoutAllowsContinuousUpload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count, err := io.Copy(io.Discard, r.Body)
		if err != nil || count != 12 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	base := &http.Transport{}
	defer base.CloseIdleConnections()
	client := &http.Client{
		Transport: NewProgressTimeoutTransport(base, 80*time.Millisecond, time.Second),
		Timeout:   2 * time.Second,
	}
	defer client.CloseIdleConnections()
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() { _ = writer.Close() }()
		for range 12 {
			if _, err := writer.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	response, err := client.Post(server.URL, "text/plain", reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	<-finished
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestSendTimeoutDoesNotApplyConnectionDeadlineToHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := server.Client()
	client.Transport = NewProgressTimeoutTransport(client.Transport, 40*time.Millisecond, time.Second)
	defer client.CloseIdleConnections()
	for range 2 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(data) != "ok" || response.ProtoMajor != 2 {
			t.Fatalf("response: protocol=%s body=%q error=%v", response.Proto, data, err)
		}
	}
}

// failNextWriteConn deterministically models a stale idle connection whose
// first reuse fails before any bytes reach the server.
type failNextWriteConn struct {
	net.Conn
	fail *atomic.Bool
}

func (conn *failNextWriteConn) Write(p []byte) (int, error) {
	if conn.fail.Swap(false) {
		return 0, io.ErrClosedPipe
	}
	return conn.Conn.Write(p)
}

type slowReplayBody struct{ remaining int }

func (body *slowReplayBody) Read(p []byte) (int, error) {
	if body.remaining == 0 {
		return 0, io.EOF
	}
	time.Sleep(20 * time.Millisecond)
	p[0] = 'x'
	body.remaining--
	return 1, nil
}

func (*slowReplayBody) Close() error { return nil }

func TestSendTimeoutTracksBodyProgressAfterInternalRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	var fail atomic.Bool
	var dials atomic.Int32
	base := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		dials.Add(1)
		return &failNextWriteConn{Conn: conn, fail: &fail}, nil
	}}
	defer base.CloseIdleConnections()
	client := &http.Client{
		Transport: NewProgressTimeoutTransport(base, 80*time.Millisecond, time.Second),
		Timeout:   2 * time.Second,
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(strings.Repeat("x", 12)))
	if err != nil {
		t.Fatal(err)
	}
	var replays atomic.Int32
	request.GetBody = func() (io.ReadCloser, error) {
		replays.Add(1)
		return &slowReplayBody{remaining: 12}, nil
	}
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(data) != "ok" || replays.Load() != 1 || dials.Load() != 2 {
		t.Fatalf("response=%q err=%v replays=%d dials=%d", data, err, replays.Load(), dials.Load())
	}
}

func TestSendTimeoutCoversFlushAfterInformationalResponse(t *testing.T) {
	client, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	base := &http.Transport{
		DialContext:           func(context.Context, string, string) (net.Conn, error) { return client, nil },
		ExpectContinueTimeout: time.Second,
	}
	defer base.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	informed := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(peer)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				informed <- err
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, err := io.WriteString(peer, "HTTP/1.1 103 Early Hints\r\n\r\nHTTP/1.1 100 Continue\r\n\r\n")
		informed <- err
	}()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"http://upstream.test/",
		strings.NewReader("payload"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Expect", "100-continue")
	transport := NewProgressTimeoutTransport(base, 40*time.Millisecond, time.Second)
	started := time.Now()
	response, err := transport.RoundTrip(request)
	if response != nil {
		_ = response.Body.Close()
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("stalled send after informational response: %v/%s", err, time.Since(started))
	}
	if err := <-informed; err != nil {
		t.Fatal(err)
	}
}

type closeTrackingSendConn struct {
	net.Conn
	closed *atomic.Int32
	once   sync.Once
}

func (conn *closeTrackingSendConn) Close() error {
	conn.once.Do(func() { conn.closed.Add(1) })
	return conn.Conn.Close()
}

func TestSendTimeoutClusterClosesRequestAndProbePools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	var dials, closed atomic.Int32
	base := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		dials.Add(1)
		return &closeTrackingSendConn{Conn: conn, closed: &closed}, nil
	}}
	config := testClusterConfig()
	config.SendTimeout = time.Second
	config.ReadTimeout = time.Second
	cluster, err := newTestClusterWithTransport(config, NopClusterObserver{}, base, base.CloseIdleConnections)
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	// HTTP health probes retain base, while request sends use the private pool.
	for _, transport := range []http.RoundTripper{base, cluster.RoundTripper()} {
		client := &http.Client{Transport: transport}
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != 2 || closed.Load() != 0 {
		t.Fatalf("pool separation: dials=%d closed=%d", dials.Load(), closed.Load())
	}
	cluster.Close()
	if closed.Load() != 2 {
		t.Fatalf("closed %d connections, want both idle pools", closed.Load())
	}
}
