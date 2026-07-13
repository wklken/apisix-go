package stream

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestRuntimeServesConfiguredListenerAndReloadsRoutes(t *testing.T) {
	firstUpstream, firstAddr := startStreamUpstream(t, []byte("first-response"))
	defer func() { _ = firstUpstream.Close() }()
	secondUpstream, secondAddr := startStreamUpstream(t, []byte("second-response"))
	defer func() { _ = secondUpstream.Close() }()

	ctx := t.Context()
	results := make(chan Result, 2)
	runtime, err := NewRuntime(
		ctx,
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "first", firstAddr)},
		nil,
		func(result Result) { results <- result },
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	firstResponse := runtimeRoundTrip(t, runtime.Addresses()[0], []byte("stream-request"), len("first-response"))
	if string(firstResponse) != "first-response" {
		t.Fatalf("first response = %q, want first-response", firstResponse)
	}
	if err := runtime.Reload([]resource.StreamRoute{runtimeTestRoute(t, "second", secondAddr)}); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	secondResponse := runtimeRoundTrip(t, runtime.Addresses()[0], []byte("stream-request"), len("second-response"))
	if string(secondResponse) != "second-response" {
		t.Fatalf("second response = %q, want second-response", secondResponse)
	}

	for range 2 {
		select {
		case result := <-results:
			if result.Err != nil {
				t.Fatalf("stream result error = %v", result.Err)
			}
		case <-time.After(time.Second):
			t.Fatal("missing runtime stream result")
		}
	}
}

func TestRuntimeCloseCancelsActiveStream(t *testing.T) {
	upstream, upstreamAddr := startBlockingStreamUpstream(t)
	defer func() { _ = upstream.Close() }()

	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "blocking", upstreamAddr)},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	client, err := net.Dial("tcp", runtime.Addresses()[0])
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("stream-request")); err != nil {
		t.Fatalf("write runtime request: %v", err)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client read succeeded after runtime close")
	}
}

func TestRuntimeCancellationBoundsBackpressure(t *testing.T) {
	upstream, upstreamAddr, accepted, release := startNonReadingStreamUpstream(t)
	defer func() { _ = upstream.Close() }()
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	runtime, err := NewRuntime(
		ctx,
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "backpressure", upstreamAddr)},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	client, err := net.Dial("tcp", runtime.Addresses()[0])
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	<-accepted

	writeDone := make(chan error, 1)
	go func() {
		payload := make([]byte, 64*1024)
		for range 256 {
			if _, writeErr := client.Write(payload); writeErr != nil {
				writeDone <- writeErr
				return
			}
		}
		writeDone <- nil
	}()
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("large client write completed while upstream was not reading")
		}
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := runtime.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("large client write completed without cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("large client write remained blocked after cancellation")
	}
}

func TestNewRuntimeRejectsTLSAndInvalidAddress(t *testing.T) {
	if _, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0", Tls: true}},
		nil,
		nil,
		nil,
	); err == nil {
		t.Fatal("NewRuntime() accepted unsupported TLS listener")
	}
	if _, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "not-an-address"}},
		nil,
		nil,
		nil,
	); err == nil {
		t.Fatal("NewRuntime() accepted invalid listener address")
	}
}

func runtimeTestRoute(t *testing.T, id, upstreamAddr string) resource.StreamRoute {
	t.Helper()
	host, portText, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		t.Fatalf("split upstream address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	return resource.StreamRoute{
		ID: id,
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: host, Port: port, Weight: 1}},
		},
	}
}

func runtimeRoundTrip(t *testing.T, address string, request []byte, responseSize int) []byte {
	t.Helper()
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	if _, err := client.Write(request); err != nil {
		_ = client.Close()
		t.Fatalf("write runtime request: %v", err)
	}
	response := make([]byte, responseSize)
	if _, err := io.ReadFull(client, response); err != nil {
		_ = client.Close()
		t.Fatalf("read runtime response: %v", err)
	}
	_ = client.Close()
	return response
}

func startBlockingStreamUpstream(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen blocking upstream: %v", err)
	}
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(io.Discard, conn)
	}()
	return listener, listener.Addr().String()
}

func startNonReadingStreamUpstream(t *testing.T) (net.Listener, string, <-chan struct{}, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen non-reading upstream: %v", err)
	}
	accepted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		close(accepted)
		<-release
	}()
	return listener, listener.Addr().String(), accepted, func() { close(release) }
}
