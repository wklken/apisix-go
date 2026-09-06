package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestActiveHealthAdmitsTCPProbe(t *testing.T) {
	config, enabled, err := ParseActiveHealthConfig(map[string]any{"active": map[string]any{"type": "tcp"}})
	if err != nil || !enabled || config.Type != "tcp" {
		t.Fatalf("config=%+v enabled=%t error=%v", config, enabled, err)
	}
}

func TestActiveHealthTCPProbeConnectsWithoutSendingHTTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	checker := &activeHealthChecker{
		config:     ActiveHealthConfig{Type: "tcp", Timeout: time.Second},
		transport:  http.DefaultTransport,
		httpClient: &http.Client{},
	}
	if result := checker.probeResult(
		context.Background(),
		"http://"+listener.Addr().String(),
	); result != activeProbeSuccess {
		t.Fatalf("probe=%v want success", result)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	data, err := io.ReadAll(conn)
	if err != nil || len(data) != 0 {
		t.Fatalf("TCP probe sent %q, error=%v", data, err)
	}
}

func TestActiveHealthTCPProbeClassifiesCancellationAndFailure(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel bool
		want   activeProbeResult
	}{{"timeout", false, activeProbeTimeout}, {"canceled", true, activeProbeCanceled}, {"refused", false, activeProbeTCPFailure}} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				cancel()
			}
			called := false
			checker := &activeHealthChecker{
				config: ActiveHealthConfig{Type: "tcp", Timeout: time.Millisecond},
				transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					called = true
					if test.name == "refused" {
						return nil, errors.New("refused")
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}},
			}
			if got := checker.probeResult(ctx, "http://127.0.0.1:1"); got != test.want {
				t.Fatalf("result=%v want=%v", got, test.want)
			}
			if test.cancel && called {
				t.Fatal("canceled owner started a dial")
			}
		})
	}
}
