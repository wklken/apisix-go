package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/file_logger"
)

func BenchmarkFileLoggerLogExecutor(b *testing.B) {
	b.Run("default", func(b *testing.B) {
		benchmarkFileLoggerLogExecutor(b, file_logger.Config{}, defaultLogExecutorRequest)
	})
	b.Run("custom", func(b *testing.B) {
		benchmarkFileLoggerLogExecutor(b, file_logger.Config{
			LogFormat: map[string]any{
				"request": map[string]any{
					"line":    "$request",
					"uri":     "$uri",
					"headers": map[string]any{"trace": "$http_x_trace_id"},
				},
				"response": map[string]any{
					"status": "$status",
				},
				"route_id":   "$route_id",
				"service_id": "$service_id",
				"variables": map[string]any{
					"balancer": "$balancer_ip",
				},
			},
		}, customLogExecutorRequest)
	})
	b.Run("bodies", func(b *testing.B) {
		benchmarkFileLoggerLogExecutor(b, file_logger.Config{
			IncludeReqBody:   true,
			IncludeRespBody:  true,
			MaxReqBodyBytes:  64,
			MaxRespBodyBytes: 128,
		}, bodyLogExecutorRequest)
	})
}

type logExecutorBenchmarkRequest func(int) (*http.Request, *benchmarkResponse)

type benchmarkResponse struct {
	body []byte
}

func benchmarkFileLoggerLogExecutor(
	b *testing.B,
	cfg file_logger.Config,
	buildRequest logExecutorBenchmarkRequest,
) {
	b.Helper()
	path := filepath.Join(b.TempDir(), "access.log")
	cfg.Path = path

	loggerPlugin := &file_logger.Plugin{}
	if err := loggerPlugin.Init(); err != nil {
		b.Fatalf("file logger Init() error = %v", err)
	}
	*loggerPlugin.Config().(*file_logger.Config) = cfg
	if err := loggerPlugin.PostInit(); err != nil {
		b.Fatalf("file logger PostInit() error = %v", err)
	}
	executor, err := NewLogExecutor([]LogBinding{
		{
			Plugin: loggerPlugin,
			Scope:  ScopeRoute,
			Policy: loggerPlugin.LogCapturePolicy(),
		},
	})
	if err != nil {
		loggerPlugin.Stop()
		b.Fatalf("NewLogExecutor() error = %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		request, response := buildRequest(i)
		started := time.Unix(1_700_000_000, int64(i))
		request, lifecycle := apisixctx.EnsureRequestLifecycle(request, started)
		configureLogExecutorRequest(request, i)
		wrapped, capture := base.CaptureResponseOutcomeController(httptest.NewRecorder())
		request = base.WithResponseCapture(request, capture)
		request, err = executor.Prepare(request)
		if err != nil {
			b.Fatalf("Prepare() error = %v", err)
		}
		wrapped.Header().Set("Content-Type", "application/json")
		if len(response.body) > 0 {
			if _, err := wrapped.Write(response.body); err != nil {
				b.Fatalf("response Write() error = %v", err)
			}
		} else {
			if _, err := io.WriteString(wrapped, strings.Repeat("r", 512)); err != nil {
				b.Fatalf("response WriteString() error = %v", err)
			}
		}
		if err := executor.SealAndRegister(request); err != nil {
			b.Fatalf("SealAndRegister() error = %v", err)
		}
		lifecycle.Complete(capture.Outcome(), started.Add(time.Millisecond))
		if failures := lifecycle.Finalize(); len(failures) != 0 {
			b.Fatalf("Finalize() failures = %#v", failures)
		}
	}
	b.StopTimer()

	loggerPlugin.Stop()
	validateFileLoggerBenchmarkOutput(b, path, b.N)
}

func defaultLogExecutorRequest(_ int) (*http.Request, *benchmarkResponse) {
	request := newLogExecutorRequest(http.MethodGet, "/orders/123?include=summary", "")
	return request, &benchmarkResponse{}
}

func customLogExecutorRequest(i int) (*http.Request, *benchmarkResponse) {
	request, response := defaultLogExecutorRequest(i)
	return request, response
}

func bodyLogExecutorRequest(_ int) (*http.Request, *benchmarkResponse) {
	request := newLogExecutorRequest(http.MethodPost, "/orders/123", strings.Repeat("q", 64))
	return request, &benchmarkResponse{body: []byte(strings.Repeat("r", 128))}
}

func newLogExecutorRequest(method, target, body string) *http.Request {
	request, err := http.NewRequest(method, "http://gateway.example"+target, strings.NewReader(body))
	if err != nil {
		panic(fmt.Sprintf("construct benchmark request: %v", err))
	}
	request.Host = "gateway.example"
	request.RemoteAddr = "192.0.2.10:43120"
	request.Header.Set("User-Agent", "apisix-go-benchmark")
	request.Header.Set("X-Trace-Id", "trace-123")
	request.Header.Set("Accept", "application/json")
	return request
}

func configureLogExecutorRequest(request *http.Request, i int) {
	apisixctx.RegisterApisixVar(request, "$route_id", "route-orders")
	apisixctx.RegisterApisixVar(request, "$service_id", "service-orders")
	apisixctx.RegisterApisixVar(request, "$balancer_ip", "192.0.2.20")
	apisixctx.RegisterApisixVar(request, "$balancer_port", "8080")
	apisixctx.RegisterRequestVar(request, "$request_id", fmt.Sprintf("request-%d", i))
	apisixctx.RegisterRequestVar(request, "$upstream_status", "200")
}

func validateFileLoggerBenchmarkOutput(b *testing.B, path string, want int) {
	b.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		b.Fatalf("read benchmark log %q: %v", path, err)
	}
	lines := bytes.Split(content, []byte{'\n'})
	count := 0
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		count++
		var object map[string]any
		if err := json.Unmarshal(line, &object); err != nil {
			b.Fatalf("unmarshal benchmark log line %d: %v; line=%q", count, err, line)
		}
	}
	if count != want {
		b.Fatalf("benchmark log entries = %d, want %d", count, want)
	}
}
