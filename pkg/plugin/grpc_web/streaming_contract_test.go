package grpc_web_test

import (
	"compress/gzip"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/grpc_web"
	gzipplugin "github.com/wklken/apisix-go/pkg/plugin/gzip"
)

func TestGrpcWebStreamingExecutorReachesUpstreamAndFramesOnce(t *testing.T) {
	p := &grpc_web.Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	binding := bindPluginForTest("grpc-web", p, plugin.ScopeRoute, plugin.ResourceProvenance{
		Kind: plugin.ResourceRoute,
		ID:   "grpc-web",
	})
	executor, err := plugin.NewStreamingResponseExecutor([]plugin.Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/service/Call", strings.NewReader("payload"))
	request.Header.Set("Content-Type", "application/grpc-web")
	request, _ = apisixctx.EnsureRequestLifecycle(request, time.Now())
	response := httptest.NewRecorder()
	called := 0
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.Header().Set("Grpc-Status", "0")
		_, _ = w.Write([]byte("reply"))
	})).ServeHTTP(response, request)
	if called != 1 {
		t.Fatalf("upstream calls = %d, want 1", called)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if response.Body.Len() == 0 {
		t.Fatal("grpc-web response body is empty")
	}
}

func TestGrpcWebResponsePlanCompressesBinaryAndTextAfterFraming(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		requestBody string
		decodeBody  func(*testing.T, []byte) []byte
	}{
		{
			name:        "binary",
			contentType: "application/grpc-web",
			requestBody: "payload",
			decodeBody:  func(_ *testing.T, body []byte) []byte { return body },
		},
		{
			name:        "text",
			contentType: "application/grpc-web-text",
			requestBody: base64.StdEncoding.EncodeToString([]byte("payload")),
			decodeBody: func(t *testing.T, body []byte) []byte {
				t.Helper()
				messageLength := base64.StdEncoding.EncodedLen(len("reply"))
				if len(body) <= messageLength {
					t.Fatalf("grpc-web-text body length = %d, want message and trailer", len(body))
				}
				message, err := base64.StdEncoding.DecodeString(string(body[:messageLength]))
				if err != nil {
					t.Fatalf("decode grpc-web-text message: %v", err)
				}
				trailer, err := base64.StdEncoding.DecodeString(string(body[messageLength:]))
				if err != nil {
					t.Fatalf("decode grpc-web-text trailer: %v", err)
				}
				return append(message, trailer...)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			grpcPlugin := &grpc_web.Plugin{}
			if err := grpcPlugin.Init(); err != nil {
				t.Fatalf("grpc Init() error = %v", err)
			}
			if err := grpcPlugin.PostInit(); err != nil {
				t.Fatalf("grpc PostInit() error = %v", err)
			}
			compressor := &gzipplugin.Plugin{}
			if err := compressor.Init(); err != nil {
				t.Fatalf("gzip Init() error = %v", err)
			}
			minLength := 1
			compressor.Config().(*gzipplugin.Config).Types = []string{test.contentType}
			compressor.Config().(*gzipplugin.Config).MinLength = &minLength
			if err := compressor.PostInit(); err != nil {
				t.Fatalf("gzip PostInit() error = %v", err)
			}

			bindings := []plugin.Binding{
				bindPluginForTest("grpc-web", grpcPlugin, plugin.ScopeRoute, plugin.ResourceProvenance{
					Kind: plugin.ResourceRoute, ID: "grpc-web",
				}),
				bindPluginForTest("gzip", compressor, plugin.ScopeRoute, plugin.ResourceProvenance{
					Kind: plugin.ResourceRoute, ID: "gzip",
				}),
			}
			plan, err := plugin.BuildResponsePlan(bindings)
			if err != nil {
				t.Fatalf("BuildResponsePlan() error = %v", err)
			}
			request := httptest.NewRequest(
				http.MethodPost,
				"/service/Call",
				strings.NewReader(test.requestBody),
			)
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Accept-Encoding", "gzip")
			request, _ = apisixctx.EnsureRequestLifecycle(request, time.Now())
			response := httptest.NewRecorder()
			upstreamCalls := 0
			plan.Install(plugin.NewRequestPipeline(bindings, nil), http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					upstreamCalls++
					w.Header().Set("Grpc-Status", "0")
					_, _ = w.Write([]byte("reply"))
				},
			)).ServeHTTP(response, request)

			if upstreamCalls != 1 {
				t.Fatalf("upstream calls = %d, want 1", upstreamCalls)
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if got := response.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			reader, err := gzip.NewReader(response.Body)
			if err != nil {
				t.Fatalf("gzip.NewReader() error = %v", err)
			}
			compressedBody, err := io.ReadAll(reader)
			if closeErr := reader.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatalf("read gzip response: %v", err)
			}
			framedBody := test.decodeBody(t, compressedBody)
			if !strings.Contains(string(framedBody), "reply") {
				t.Fatalf("framed response %q does not contain upstream payload", framedBody)
			}
		})
	}
}
