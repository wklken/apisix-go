package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPUpstreamDoesNotNegotiateHTTP2(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	transport := NewTransport((&TransportOptionBuilder{}).WithInsecureSkipVerify(true).Build())
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "HTTP/1.1" {
		t.Fatalf("upstream protocol=%s want HTTP/1.1", body)
	}
}

func TestTLSHandshakeUsesConfiguredConnectTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	stop := make(chan struct{})
	done := make(chan struct{})
	defer func() { close(stop); <-done }()
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-stop
	}()
	transport := NewTransport(
		(&TransportOptionBuilder{}).WithDialTimeout(40 * time.Millisecond).WithInsecureSkipVerify(true).Build(),
	)
	defer transport.CloseIdleConnections()
	start := time.Now()
	response, err := (&http.Client{Transport: transport, Timeout: 750 * time.Millisecond}).Get(
		"https://" + listener.Addr().String(),
	)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("stalled TLS handshake succeeded")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("handshake elapsed=%s ignored 40ms connect timeout", elapsed)
	}
}

func TestClusterConfigKeySeparatesHTTP1AndTLSHTTP2(t *testing.T) {
	a := testClusterConfig()
	b := testClusterConfig()
	b.Transport = a.Transport
	b.Transport.http2 = true
	keyA, err := a.Key()
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := b.Key()
	if err != nil {
		t.Fatal(err)
	}
	if keyA == keyB {
		t.Fatal("TLS HTTP2 reused HTTP1 cluster identity")
	}
}
