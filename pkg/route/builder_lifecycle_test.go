package route

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/error_log_logger"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestBuilderStopFlushesLoggerBatches(t *testing.T) {
	delivered := make(chan struct{}, 1)
	logServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logServer.Close)

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080")
	plugins := builder.initPlugins(
		map[string]resource.PluginConfig{
			"http-logger": map[string]any{
				"uri":              logServer.URL,
				"batch_max_size":   10,
				"buffer_duration":  60,
				"inactive_timeout": 60,
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "route-a"}),
	)
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	httpLogger, ok := plugins[0].(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", plugins[0])
	}
	if err := httpLogger.Fire(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("Fire() error = %v", err)
	}

	builder.Stop()

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Builder.Stop to flush logger batch")
	}
}

func TestBuilderStopFlushesErrorLogLoggerBatch(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	received := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
	}()

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	builder := NewBuilder(nil)
	plugins := builder.initPlugins(
		map[string]resource.PluginConfig{
			"error-log-logger": map[string]any{
				"tcp": map[string]any{
					"host": host,
					"port": port,
				},
				"level":            "INFO",
				"batch_max_size":   10,
				"buffer_duration":  60,
				"inactive_timeout": 60,
			},
		},
		pluginRouteContext{},
	)
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	errorLogger, ok := plugins[0].(*error_log_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *error_log_logger.Plugin", plugins[0])
	}
	errorLogger.Send(map[string]any{"message": "shutdown error"})
	builder.Stop()

	select {
	case payload := <-received:
		if !strings.Contains(payload, "shutdown error") {
			t.Fatalf("payload = %q, want shutdown error", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Builder.Stop to flush error-log-logger")
	}
}
