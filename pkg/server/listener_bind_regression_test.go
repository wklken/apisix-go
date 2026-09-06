package server

import (
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
)

func TestListenerBindFailureNeverServesHTTP(t *testing.T) {
	bindError := errors.New("control bind failed")
	var served atomic.Int64
	var dataAddress string
	server := &Server{
		staticConfig: &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{
			EnableControl: true,
			Control:       config.Control{Ip: "127.0.0.1", Port: 9090},
			Status:        config.Status{IP: "127.0.0.1", Port: 0},
		}}},
		addrs: []string{"127.0.0.1:0"},
		server: newConfiguredHTTPServer(
			http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) { served.Add(1); w.WriteHeader(http.StatusTeapot) },
			),
			nil,
		),
		statusServer:  newConfiguredHTTPServer(http.NotFoundHandler(), nil),
		controlServer: newConfiguredHTTPServer(http.NotFoundHandler(), nil),
	}
	defer func() { _ = server.server.Close(); _ = server.statusServer.Close() }()
	server.runtimeFactories.listenHTTP = func(network, address string) (net.Listener, error) {
		if address == "127.0.0.1:9090" {
			transport := &http.Transport{DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 200 * time.Millisecond}
			response, err := client.Get("http://" + dataAddress + "/bind-window")
			if err == nil {
				_ = response.Body.Close()
			}
			return nil, bindError
		}
		listener, err := net.Listen(network, address)
		if err == nil && dataAddress == "" {
			dataAddress = listener.Addr().String()
		}
		return listener, err
	}
	if _, err := server.serveHTTPListenerRuntime(nil, nil); !errors.Is(err, bindError) {
		t.Fatalf("listen error=%v", err)
	}
	if served.Load() != 0 {
		t.Fatalf("served %d requests before a later bind failure", served.Load())
	}
	if len(server.listeners) != 0 {
		t.Fatal("failed startup retained listeners")
	}
}
