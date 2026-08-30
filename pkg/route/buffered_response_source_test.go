package route

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestModifyResponseSetsUpstreamBeforeBoundedCapture(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(0, 0),
	)
	response := &http.Response{StatusCode: http.StatusOK, Request: request}

	if err := newModifyResponse(&testEffectiveConfig().Config)(response); err != nil {
		t.Fatalf("newModifyResponse() error = %v", err)
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceUpstream {
		t.Fatalf(
			"response source = %q, want %q before capture",
			got,
			apisixctx.ResponseSourceUpstream,
		)
	}
}

func TestErrorAndRequestBodyHandlersSetAPISIXBeforeJSON(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(0, 0),
	)
	response := &sourceObservingWriter{lifecycle: lifecycle, header: make(http.Header)}

	newErrorHandler(&testEffectiveConfig().Config)(response, request, errors.New("upstream failed"))
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceAPISIX ||
		response.firstSource != apisixctx.ResponseSourceAPISIX {
		t.Fatalf(
			"proxy-error source = %q, first write source = %q, want APISIX before JSON",
			lifecycle.ResponseSource(),
			response.firstSource,
		)
	}

	handler := testPreparedProxyHandler(t,
		resource.Route{
			ID: "oversized-request-source",
			Upstream: resource.Upstream{
				Scheme: "http",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
		resource.Service{}, testEffectiveConfig(),
	)
	bodyRequest, bodyLifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "http://gateway.test/upload", bytes.NewReader(
			bytes.Repeat([]byte("x"), int(proxy_control.DefaultRequestBufferingLimit+1)),
		)),
		time.Unix(0, 0),
	)
	bodyRequest = proxy_control.WithRequestBuffering(bodyRequest, true)
	writer := &sourceObservingWriter{lifecycle: bodyLifecycle, header: make(http.Header)}
	handler.ServeHTTP(writer, bodyRequest)
	if bodyLifecycle.ResponseSource() != apisixctx.ResponseSourceAPISIX ||
		writer.firstSource != apisixctx.ResponseSourceAPISIX {
		t.Fatalf(
			"request-body source = %q, first write source = %q, want APISIX before JSON",
			bodyLifecycle.ResponseSource(),
			writer.firstSource,
		)
	}
}

type sourceObservingWriter struct {
	lifecycle   *apisixctx.RequestLifecycle
	header      http.Header
	firstSource apisixctx.ResponseSource
	status      int
}

func (w *sourceObservingWriter) Header() http.Header { return w.header }
func (w *sourceObservingWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.firstSource = w.lifecycle.ResponseSource()
	w.status = status
}

func (w *sourceObservingWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return len(body), nil
}

func TestGlobalNotFoundSetsEarlyStopBeforeWrite(t *testing.T) {
	handler, err := BuildPreparedNotFoundHandler("", nil)
	if err != nil {
		t.Fatalf("BuildPreparedNotFoundHandler() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/missing", nil),
		time.Unix(0, 0),
	)
	response := &sourceObservingWriter{lifecycle: lifecycle, header: make(http.Header)}
	handler.ServeHTTP(response, request)
	if response.status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.status, http.StatusNotFound)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceEarlyStop ||
		response.firstSource != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf(
			"global-not-found source = %q, first write source = %q, want early_stop before write",
			lifecycle.ResponseSource(),
			response.firstSource,
		)
	}
}
