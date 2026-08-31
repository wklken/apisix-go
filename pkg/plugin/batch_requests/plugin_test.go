package batch_requests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	batchCorePanicHelperEnv = "APISIX_GO_BATCH_OWNER_HELPER"
	batchOwnerRecovered     = "batch-owner-recovered"
	batchWorkerReleased     = "batch-worker-released"
)

var batchCorePanicSentinel = &struct{ message string }{message: "raw core panic"}

func TestMetadataSchemaRejectsNonpositiveLimits(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.GetMetadataSchema() == "" {
		t.Fatal("metadata schema is empty")
	}
	var metadataSchemaDocument struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetMetadataSchema()), &metadataSchemaDocument); err != nil {
		t.Fatalf("decode metadata schema: %v", err)
	}
	if _, ok := metadataSchemaDocument.Properties["max_body_size"]; !ok {
		t.Fatal("metadata schema is missing max_body_size")
	}
	if _, ok := metadataSchemaDocument.Properties["max_pipeline_items"]; !ok {
		t.Fatal("metadata schema is missing max_pipeline_items")
	}
	if got := len(metadataSchemaDocument.Properties); got != 2 {
		t.Fatalf("metadata property count = %d, want only max_body_size and max_pipeline_items", got)
	}

	for _, metadata := range []map[string]any{
		{"max_body_size": 0},
		{"max_pipeline_items": 0},
	} {
		if err := util.Validate(metadata, p.GetMetadataSchema()); err == nil {
			t.Fatalf("metadata %#v validated, want positive-limit rejection", metadata)
		}
	}
	if got := applyLimitDefaults(Limits{}).maxTimeout; got != defaultMaxTimeout {
		t.Fatalf("default max timeout = %d, want %d", got, defaultMaxTimeout)
	}
	if got := applyLimitDefaults(Limits{}).MaxPipelineItems; got != 1000 {
		t.Fatalf("default max pipeline items = %d, want APISIX default 1000", got)
	}
	if got := applyLimitDefaults(Limits{maxTimeout: hardMaxTimeout + 1}).maxTimeout; got != hardMaxTimeout {
		t.Fatalf("capped max timeout = %d, want %d", got, hardMaxTimeout)
	}
}

func TestLegacyMetadataFieldsCannotChangeInternalLimits(t *testing.T) {
	view, err := runtime.NewMetadataView(map[string][]byte{
		name: []byte(`{"max_concurrency":1,"max_response_body_size":1,"max_timeout":1}`),
	})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	var limits Limits
	if _, err := view.Decode(name, &limits); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	limits = applyLimitDefaults(limits)
	if limits.maxConcurrency != 8 || limits.maxResponseBodySize != 4*1024*1024 || limits.maxTimeout != 30000 {
		t.Fatalf(
			"internal limits = concurrency %d, response bytes %d, timeout %d; want fixed defaults",
			limits.maxConcurrency,
			limits.maxResponseBodySize,
			limits.maxTimeout,
		)
	}
}

func TestHandlerRejectsNestedBatchBeforeConcurrencyLease(t *testing.T) {
	mux := http.NewServeMux()
	handler := NewHandlerWithLimits(mux, Limits{maxConcurrency: 1})
	// The handler is intentionally mounted at a non-default URI. The marker
	// must follow the dispatched request rather than rely on DefaultURI.
	mux.Handle("/custom/batch", handler)

	req := httptest.NewRequest(http.MethodPost, "/custom/batch", strings.NewReader(`{
		"timeout": 20,
		"pipeline": [{
			"path": "/custom/batch",
			"body": "{\"pipeline\":[{\"path\":\"/inner\"}]}"
		}]
	}`))
	res := httptest.NewRecorder()
	start := time.Now()
	handler.ServeHTTP(res, req)

	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("nested batch request took %s, want prompt rejection", elapsed)
	}
	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusBadRequest {
		t.Fatalf("outer pipeline status = %d, want 400 nested rejection", responses[0].Status)
	}
	if !strings.Contains(responses[0].Body, "nested batch requests are not allowed") {
		t.Fatalf("nested response body = %q, want deterministic rejection", responses[0].Body)
	}
}

func TestHandlerEnforcesTimeoutBounds(t *testing.T) {
	const maxTimeout = 25
	observed := make(chan time.Duration, 2)
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("pipeline context has no timeout deadline")
		} else {
			observed <- time.Until(deadline)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{maxTimeout: maxTimeout})

	for _, body := range []string{
		`{"pipeline":[{"path":"/omitted"}]}`,
		`{"timeout":25,"pipeline":[{"path":"/exact"}]}`,
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(body)))
		if res.Code != http.StatusOK {
			t.Fatalf("response code = %d, want 200; body=%q", res.Code, res.Body.String())
		}
		if response := decodePipelineResponses(t, res.Body.String())[0]; response.Status != http.StatusNoContent {
			t.Fatalf("pipeline response = %#v, want 204", response)
		}
	}
	for range 2 {
		select {
		case remaining := <-observed:
			if remaining <= 0 || remaining > 100*time.Millisecond {
				t.Fatalf("remaining timeout = %s, want a positive value within configured bound", remaining)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for observed timeout")
		}
	}
}

func TestHandlerRejectsTimeoutAboveConfiguredMaximum(t *testing.T) {
	handler := NewHandlerWithLimits(http.NotFoundHandler(), Limits{maxTimeout: 25})
	for _, timeout := range []string{"26", "99999999999999999999999999999999"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, DefaultURI,
			strings.NewReader(fmt.Sprintf(`{"timeout":%s,"pipeline":[{"path":"/inner"}]}`, timeout))))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("timeout %s response code = %d, want 400; body=%q", timeout, res.Code, res.Body.String())
		}
		if !strings.Contains(strings.ToLower(res.Body.String()), "timeout") {
			t.Fatalf("timeout %s response body = %q, want timeout validation", timeout, res.Body.String())
		}
	}
}

func TestHandlerRejectsUnknownPipelineField(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "/inner", "unknown_field": "x"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if strings.HasSuffix(res.Body.String(), "\n") {
		t.Fatalf("response body has trailing newline: %q", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "unknown_field") {
		t.Fatalf("response body = %q, want unknown field error", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "bad request body") {
		t.Fatalf("response body = %q, want schema validation prefix", res.Body.String())
	}
}

func TestHandlerRejectsTrailingJSONValue(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(
		http.MethodPost,
		DefaultURI,
		strings.NewReader(`{"pipeline":[{"path":"/inner"}]} {}`),
	)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsExplicitZeroTimeout(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 0,
		"pipeline": [{"path": "/inner"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "timeout") {
		t.Fatalf("response body = %q, want timeout validation error", res.Body.String())
	}
}

func TestHandlerReportsTimeoutTypeMismatchAsBadRequestBody(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": "200",
		"pipeline": [{"path": "/inner"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "bad request body") {
		t.Fatalf("response body = %q, want schema validation prefix", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "timeout") {
		t.Fatalf("response body = %q, want timeout field name", res.Body.String())
	}
}

func TestHandlerReportsSSLVerifyTypeMismatchAsBadRequestBody(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "/inner", "ssl_verify": 1.2}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "bad request body") {
		t.Fatalf("response body = %q, want schema validation prefix", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "ssl_verify") {
		t.Fatalf("response body = %q, want ssl_verify field name", res.Body.String())
	}
}

func TestHandlerStopsAfterTimedOutPipelineRequest(t *testing.T) {
	var calls atomic.Int32
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 10,
		"pipeline": [
			{"path": "/slow"},
			{"path": "/must-not-run"}
		]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodeAllPipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("pipeline status = %d, want 504", responses[0].Status)
	}
	if len(responses) != 1 {
		t.Fatalf("responses = %#v, want only the timed-out response", responses)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", got)
	}
}

func TestHandlerBoundsCancellationIgnoringWorkers(t *testing.T) {
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	dispatcher := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		<-release
		active.Add(-1)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{maxConcurrency: 2})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 20,
		"pipeline": [{"path":"/1"},{"path":"/2"},{"path":"/3"},{"path":"/4"}]
	}`))
	res := &batchCommitSignalWriter{ResponseRecorder: httptest.NewRecorder(), committed: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		handler.ServeHTTP(res, req)
		close(served)
	}()
	select {
	case <-res.committed:
	case <-time.After(time.Second):
		t.Fatal("batch response was not committed")
	}
	close(release)
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("batch handler did not return after releasing workers")
	}

	responses := decodeAllPipelineResponses(t, res.Body.String())
	if len(responses) != 1 || responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("responses = %#v, want one timeout response", responses)
	}
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak workers = %d, want exactly 1 admitted slot", got)
	}
	deadline := time.Now().Add(time.Second)
	for active.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("active workers = %d after release, want 0", active.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHandlerBoundsPipelineResponseBody(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "123456")
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{maxResponseBodySize: 5})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path":"/large"},{"path":"/large-again"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodeAllPipelineResponses(t, res.Body.String())
	if len(responses) != 2 || responses[0].Status != http.StatusBadGateway ||
		responses[1].Status != http.StatusBadGateway {
		t.Fatalf("responses = %#v, want two bounded 502 responses", responses)
	}
	if responses[0].Body != "" || responses[1].Body != "" {
		t.Fatalf("oversized bodies retained: %#v", responses)
	}
}

func TestHandlerBoundsAggregatePipelineResponseBodies(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "12345")
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{maxResponseBodySize: 5})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, newBatchRequest(t, 21))

	responses := decodeAllPipelineResponses(t, res.Body.String())
	if len(responses) != 21 {
		t.Fatalf("responses = %d, want 21", len(responses))
	}
	for i, response := range responses[:20] {
		if response.Status != http.StatusOK || response.Body != "12345" {
			t.Fatalf("response[%d] = %#v, want retained 200 response", i, response)
		}
	}
	if got := responses[20]; got.Status != http.StatusBadGateway || got.Body != "" {
		t.Fatalf("response[20] = %#v, want aggregate-limit 502 without body", got)
	}
}

func TestHandlerBoundsAggregatePipelineResponseHeaders(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Large", "12345")
		w.WriteHeader(http.StatusOK)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{maxResponseBodySize: 1})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, newBatchRequest(t, 2))

	responses := decodeAllPipelineResponses(t, res.Body.String())
	if len(responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(responses))
	}
	if responses[0].Status != http.StatusOK || responses[0].Headers["X-Large"] != "12345" {
		t.Fatalf("response[0] = %#v, want retained 200 response", responses[0])
	}
	if got := responses[1]; got.Status != http.StatusBadGateway || len(got.Headers) != 0 {
		t.Fatalf("response[1] = %#v, want aggregate-limit 502 without headers", got)
	}
}

func TestHandlerConvertsAbortHandlerToBadGateway(t *testing.T) {
	handler := NewHandlerWithLimits(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), Limits{})
	request := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path":"/abort"}]
	}`))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	responses := decodePipelineResponses(t, response.Body.String())
	if responses[0].Status != http.StatusBadGateway {
		t.Fatalf("pipeline status = %d, want 502", responses[0].Status)
	}
	if responses[0].Reason != http.StatusText(http.StatusBadGateway) {
		t.Fatalf("pipeline reason = %q, want %q", responses[0].Reason, http.StatusText(http.StatusBadGateway))
	}
}

func TestBatchCorePanicReturnsToRequestOwnerInSubprocess(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestBatchCorePanicOwnerHelper$")
	command.Env = append(os.Environ(), batchCorePanicHelperEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("detached worker escaped request owner: %v\n%s", err, output)
	}
	for _, marker := range [][]byte{[]byte(batchOwnerRecovered), []byte(batchWorkerReleased)} {
		if !bytes.Contains(output, marker) {
			t.Fatalf("missing %q in %s", marker, output)
		}
	}
	if bytes.Contains(output, []byte("batch-normal-response")) {
		t.Fatalf("normal batch response was written after core panic: %s", output)
	}
}

func TestBatchCorePanicOwnerHelper(t *testing.T) {
	if os.Getenv(batchCorePanicHelperEnv) != "1" {
		return
	}
	handler := NewHandlerWithLimits(http.NotFoundHandler(), Limits{})
	request := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path":"/panic"}]
	}`))
	request = WithDispatchLeaseFactory(request, func() (DispatchLease, bool) {
		return DispatchLease{
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic(batchCorePanicSentinel)
			}),
			Release: func() { _, _ = fmt.Fprintln(os.Stderr, batchWorkerReleased) },
		}, true
	})
	response := httptest.NewRecorder()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(response, request)
	}()
	if recovered != batchCorePanicSentinel {
		t.Fatalf("request owner recovered %#v, want exact sentinel %#v", recovered, batchCorePanicSentinel)
	}
	if response.Body.Len() != 0 {
		_, _ = fmt.Fprintln(os.Stderr, "batch-normal-response")
		t.Fatalf("response body = %q, want no normal batch response", response.Body.String())
	}
	_, _ = fmt.Fprintln(os.Stderr, batchOwnerRecovered)
}

func TestHandlerPreservesRepeatedResponseHeaders(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "session=one")
		w.Header().Add("Set-Cookie", "session=two")
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	request := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path":"/cookies"}]
	}`))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	var responses []struct {
		Status  int            `json:"status"`
		Headers map[string]any `json:"headers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &responses); err != nil {
		t.Fatalf("decode response: %v; body=%q", err, response.Body.String())
	}
	if responses[0].Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", responses[0].Status)
	}
	cookies, ok := responses[0].Headers["Set-Cookie"].([]any)
	if !ok {
		t.Fatalf("Set-Cookie header = %#v, want JSON array", responses[0].Headers["Set-Cookie"])
	}
	if got, want := cookies, []any{"session=one", "session=two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Set-Cookie values = %#v, want %#v", got, want)
	}
}

func TestHandlerRetainsPipelineResponseAtExactLimit(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Exact", "yes")
		_, _ = io.WriteString(w, "12345")
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{maxResponseBodySize: 5})
	request := httptest.NewRequest(
		http.MethodPost,
		DefaultURI,
		strings.NewReader(`{"pipeline":[{"path":"/exact"}]}`),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	item := decodePipelineResponses(t, response.Body.String())[0]
	if item.Status != http.StatusOK || item.Body != "12345" || item.Headers["X-Exact"] != "yes" {
		t.Fatalf("pipeline response = %#v, want exact body and committed header", item)
	}
}

func TestHandlerDefaultsToAPISIXPipelineLimit(t *testing.T) {
	response := httptest.NewRecorder()
	NewHandlerWithLimits(http.NewServeMux(), Limits{}).ServeHTTP(
		response,
		newBatchRequest(t, defaultMaxPipelineItems+1),
	)

	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "1001 exceeds the maximum of 1000") {
		t.Fatalf("status=%d body=%q, want default item-limit rejection", response.Code, response.Body.String())
	}
}

func TestHandlerRejectsInvalidPipelinePathWithoutPanic(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "http://[::1"}]
	}`))
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "pipeline[0].path") {
		t.Fatalf("response body = %q, want invalid pipeline path message", res.Body.String())
	}
}

func TestHandlerContinuesAfterIntentionalGatewayTimeout(t *testing.T) {
	var calls atomic.Int32
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/gateway-timeout":
			w.WriteHeader(http.StatusGatewayTimeout)
		case "/after":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected dispatch path %q", r.URL.Path)
		}
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [
			{"path": "/gateway-timeout"},
			{"path": "/after"}
		]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	var responses []PipelineResponse
	if err := json.Unmarshal(res.Body.Bytes(), &responses); err != nil {
		t.Fatalf("decode responses: %v, body=%q", err, res.Body.String())
	}
	if len(responses) != 2 {
		t.Fatalf("responses length = %d, want 2: %#v", len(responses), responses)
	}
	if responses[0].Status != http.StatusGatewayTimeout || responses[0].Reason != "Gateway Timeout" {
		t.Fatalf("first response = %#v, want intentional 504", responses[0])
	}
	if responses[1].Status != http.StatusNoContent {
		t.Fatalf("second response = %#v, want 204", responses[1])
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2", got)
	}
}

func TestHandlerAllowsUnknownTopLevelFields(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inner" {
			t.Fatalf("dispatch path = %q, want /inner", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"extension": {"trace": true},
		"pipeline": [{"path": "/inner"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusNoContent {
		t.Fatalf("pipeline status = %d, want 204", responses[0].Status)
	}
}

func TestHandlerMatchesAPISIXMissingPipelineError(t *testing.T) {
	var dispatchCalls atomic.Int32
	handler := NewHandlerWithLimits(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatchCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}), Limits{})
	response := httptest.NewRecorder()

	handler.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodPost,
			DefaultURI,
			strings.NewReader(`{"pipeline1":[{"path":"/inner"}]}`),
		),
	)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	const wantBody = `{"error_msg":"bad request body: object matches none of the required: [\"pipeline\"]"}` + "\n"
	if got := response.Body.String(); got != wantBody {
		t.Fatalf("response body = %q, want %q", got, wantBody)
	}
	if got := dispatchCalls.Load(); got != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", got)
	}
}

func TestHandlerDistinguishesMissingPipelineFromEmptyPipeline(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})

	missing := httptest.NewRecorder()
	handler.ServeHTTP(
		missing,
		httptest.NewRequest(
			http.MethodPost,
			DefaultURI,
			strings.NewReader(`{"pipeline1":[{"path":"/inner"}]}`),
		),
	)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing pipeline response code = %d, want 400; body=%q",
			missing.Code, missing.Body.String())
	}
	if !strings.Contains(missing.Body.String(), "object matches none of the required") {
		t.Fatalf("missing pipeline body = %q, want APISIX required-schema reason", missing.Body.String())
	}

	empty := httptest.NewRecorder()
	handler.ServeHTTP(
		empty,
		httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{"pipeline":[]}`)),
	)
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty pipeline response code = %d, want 400; body=%q",
			empty.Code, empty.Body.String())
	}
	if !strings.Contains(empty.Body.String(), "at least one request") {
		t.Fatalf("empty pipeline body = %q, want minItems reason", empty.Body.String())
	}
}

func TestNewHandlerFromMetadataDecodesFinalLimitsOnce(t *testing.T) {
	source := []byte(`{"max_pipeline_items":1}`)
	metadata := map[string][]byte{name: source}
	view, err := runtime.NewMetadataView(metadata)
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	handler, err := NewHandlerFromMetadata(http.NotFoundHandler(), view)
	if err != nil {
		t.Fatalf("NewHandlerFromMetadata() error = %v", err)
	}

	// Neither mutation of the original byte slice nor replacement of its map
	// entry may change the fixed handler after construction.
	source[len(source)-2] = '2'
	metadata[name] = []byte(`{"max_pipeline_items":20}`)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, newBatchRequest(t, 2))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for the construction-time limit; body=%q", res.Code, res.Body.String())
	}
}

func TestPreparedGenerationsRetainBatchRequestLimits(t *testing.T) {
	var calls atomic.Int32
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	viewN, err := runtime.NewMetadataView(map[string][]byte{
		name: []byte(`{"max_pipeline_items":1}`),
	})
	if err != nil {
		t.Fatalf("NewMetadataView(N) error = %v", err)
	}
	viewN1, err := runtime.NewMetadataView(map[string][]byte{
		name: []byte(`{"max_pipeline_items":2}`),
	})
	if err != nil {
		t.Fatalf("NewMetadataView(N+1) error = %v", err)
	}
	handlerN, err := NewHandlerFromMetadata(dispatcher, viewN)
	if err != nil {
		t.Fatalf("NewHandlerFromMetadata(N) error = %v", err)
	}
	handlerN1, err := NewHandlerFromMetadata(dispatcher, viewN1)
	if err != nil {
		t.Fatalf("NewHandlerFromMetadata(N+1) error = %v", err)
	}

	resN := httptest.NewRecorder()
	handlerN.ServeHTTP(resN, newBatchRequest(t, 2))
	if resN.Code != http.StatusBadRequest {
		t.Fatalf("N response code = %d, want 400; body=%q", resN.Code, resN.Body.String())
	}
	resN1 := httptest.NewRecorder()
	handlerN1.ServeHTTP(resN1, newBatchRequest(t, 2))
	if resN1.Code != http.StatusOK {
		t.Fatalf("N+1 response code = %d, want 200; body=%q", resN1.Code, resN1.Body.String())
	}
	responsesN1 := decodeAllPipelineResponses(t, resN1.Body.String())
	if len(responsesN1) != 2 {
		t.Fatalf("N+1 responses length = %d, want exactly 2", len(responsesN1))
	}
	for i, response := range responsesN1 {
		if response.Status != http.StatusNoContent {
			t.Fatalf("N+1 response[%d] status = %d, want 204", i, response.Status)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("dispatcher calls after N+1 two-item request = %d, want 2", got)
	}

	resN1TooMany := httptest.NewRecorder()
	handlerN1.ServeHTTP(resN1TooMany, newBatchRequest(t, 3))
	if resN1TooMany.Code != http.StatusBadRequest {
		t.Fatalf("N+1 three-item response code = %d, want 400; body=%q", resN1TooMany.Code, resN1TooMany.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("dispatcher calls after N+1 three-item request = %d, want unchanged at 2", got)
	}
}

func TestNewHandlerFromMetadataUsesDefaultsWhenDocumentIsAbsent(t *testing.T) {
	view, err := runtime.NewMetadataView(nil)
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	handler, err := NewHandlerFromMetadata(http.NotFoundHandler(), view)
	if err != nil {
		t.Fatalf("NewHandlerFromMetadata() error = %v", err)
	}

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, newBatchRequest(t, defaultMaxPipelineItems+1))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want default max-pipeline rejection; body=%q", res.Code, res.Body.String())
	}
}

func TestNewHandlerFromMetadataRejectsInvalidLimitsSynchronously(t *testing.T) {
	view, err := runtime.NewMetadataView(map[string][]byte{
		name: []byte(`{"max_pipeline_items":"two"}`),
	})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	if handler, err := NewHandlerFromMetadata(http.NotFoundHandler(), view); err == nil || handler != nil {
		t.Fatalf("NewHandlerFromMetadata() = (%v, %v), want nil handler and synchronous error", handler, err)
	}
}

func TestHandlerRejectsBodyAboveConfiguredLimit(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{
		MaxBodySize:      20,
		MaxPipelineItems: defaultMaxPipelineItems,
	})

	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{"pipeline":[{"path":"/ok"}]}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(res.Body.String(), "http: request body too large") {
		t.Fatalf("response body = %q, want body size error", res.Body.String())
	}
}

func TestHandlerRejectsPipelineAboveConfiguredLimit(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{
		MaxBodySize:      defaultMaxBodySize,
		MaxPipelineItems: 1,
	})

	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [
			{"path": "/one"},
			{"path": "/two"}
		]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if !strings.Contains(res.Body.String(), "2 exceeds the maximum of 1") {
		t.Fatalf("response body = %q, want pipeline limit error", res.Body.String())
	}
}

func TestHandlerInjectsRealIPHeaderIntoPipelineRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/inner", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Real-IP", r.Header.Get("X-Real-IP"))
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(mux, Limits{
		MaxBodySize:      defaultMaxBodySize,
		MaxPipelineItems: defaultMaxPipelineItems,
	})

	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"headers": {"X-Real-IP": "198.51.100.1"},
		"pipeline": [
			{"path": "/inner", "headers": {"X-Real-IP": "198.51.100.2"}}
		]
	}`))
	req.RemoteAddr = "203.0.113.10:54321"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d, body=%q", res.Code, http.StatusOK, res.Body.String())
	}
	responses := decodePipelineResponses(t, res.Body.String())
	if got := responses[0].Headers["X-Got-Real-Ip"]; got != "203.0.113.10" {
		t.Fatalf("X-Got-Real-IP = %q, want outer remote IP", got)
	}
}

func TestHandlerRemovesConsumerIdentityFromPipelineRequests(t *testing.T) {
	tests := []struct {
		name  string
		batch Request
	}{
		{
			name: "common headers",
			batch: Request{
				Headers:  map[string]string{"X-Consumer-Username": "attacker-common"},
				Pipeline: []PipelineRequest{{Path: "/inner"}},
			},
		},
		{
			name: "item headers",
			batch: Request{
				Pipeline: []PipelineRequest{{
					Path:    "/inner",
					Headers: map[string]string{"X-Consumer-Username": "attacker-item"},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.batch)
			if err != nil {
				t.Fatalf("marshal batch request: %v", err)
			}

			var got string
			mux := http.NewServeMux()
			mux.HandleFunc("/inner", func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Consumer-Username")
				w.WriteHeader(http.StatusNoContent)
			})
			handler := NewHandlerWithLimits(mux, Limits{})
			response := httptest.NewRecorder()
			handler.ServeHTTP(
				response,
				httptest.NewRequest(http.MethodPost, DefaultURI, bytes.NewReader(body)),
			)

			if response.Code != http.StatusOK {
				t.Fatalf("response code = %d, want 200; body=%q", response.Code, response.Body.String())
			}
			if got != "" {
				t.Fatalf("pipeline endpoint saw X-Consumer-Username=%q, want header removed before dispatch", got)
			}
		})
	}
}

func TestHandlerRejectsUnsupportedHTTPVersion(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "/inner", "version": 2.0}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if !strings.Contains(res.Body.String(), "pipeline[0].version is invalid") {
		t.Fatalf("response body = %q, want invalid version error", res.Body.String())
	}
}

func TestHandlerReturnsGatewayTimeoutForTimedOutPipelineRequest(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 10,
		"pipeline": [{"path": "/slow"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusGatewayTimeout || responses[0].Reason != "upstream timeout" {
		t.Fatalf("pipeline response = %#v, want 504 upstream timeout", responses[0])
	}
}

func TestHandlerPinsPipelineHostToOuterRequest(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Host", r.Host)
		w.Header().Set("X-Got-Forwarded-Host", r.Header.Get("X-Forwarded-Host"))
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"headers": {"Host": "common.example.com", "X-Forwarded-Host": "common-forwarded.example.com"},
		"pipeline": [{"path": "/inner", "headers": {"Host": "item.example.com", "X-Forwarded-Host": "item-forwarded.example.com"}}]
	}`))
	req.Host = "outer.example.com"
	req.Header.Set("X-Forwarded-Host", "outer-forwarded.example.com")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodePipelineResponses(t, res.Body.String())
	if got := responses[0].Headers["X-Got-Host"]; got != "outer.example.com" {
		t.Fatalf("pipeline host = %q, want outer.example.com", got)
	}
	if got := responses[0].Headers["X-Got-Forwarded-Host"]; got != "outer-forwarded.example.com" {
		t.Fatalf("pipeline forwarded host = %q, want outer-forwarded.example.com", got)
	}
}

func TestHandlerDoesNotAllowPipelineHeadersToOverrideOuterTrustAndCredentials(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Authorization", r.Header.Get("Authorization"))
		w.Header().Set("X-Got-Cookie", r.Header.Get("Cookie"))
		w.Header().Set("X-Got-Forwarded-For", r.Header.Get("X-Forwarded-For"))
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"headers": {
			"Authorization": "Bearer common-attacker",
			"Cookie": "session=common-attacker",
			"X-Forwarded-For": "198.51.100.1"
		},
		"pipeline": [{
			"path": "/inner",
			"headers": {
				"Authorization": "Bearer item-attacker",
				"Cookie": "session=item-attacker",
				"X-Forwarded-For": "198.51.100.2"
			}
		}]
	}`))
	req.Header.Set("Authorization", "Bearer outer")
	req.Header.Set("Cookie", "session=outer")
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodePipelineResponses(t, res.Body.String())
	for header, want := range map[string]string{
		"X-Got-Authorization": "Bearer outer",
		"X-Got-Cookie":        "session=outer",
		"X-Got-Forwarded-For": "203.0.113.10",
	} {
		if got := responses[0].Headers[header]; got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestHandlerPreservesTrustedCredentialHeaderProvenance(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, header := range []string{"apikey", "X-Custom-Key", "X-Custom-JWT", "X-Rbac-Token", "X-Access-Token"} {
			trusted := apisixctx.RestoreTrustedRequestHeader(r, header)
			w.Header().Set("X-Got-"+header, trusted)
			if got := r.Header.Get(header); got != trusted {
				t.Errorf("restored %s = %q, want %q", header, got, trusted)
			}
		}
		w.Header().Set("X-Got-Backend-Header", r.Header.Get("X-Backend-Header"))
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"headers": {
			"apikey": "common-attacker",
			"X-Custom-Key": "common-attacker",
			"X-Custom-JWT": "common-attacker",
			"X-Rbac-Token": "common-attacker",
			"X-Access-Token": "common-attacker"
		},
		"pipeline": [{
			"path": "/inner",
			"headers": {
				"apikey": "item-attacker",
				"X-Custom-Key": "item-attacker",
				"X-Custom-JWT": "item-attacker",
				"X-Rbac-Token": "item-attacker",
				"X-Access-Token": "item-attacker",
				"X-Backend-Header": "item-value"
			}
		}]
	}`))
	req.Header.Set("apikey", "outer-key")
	req.Header.Set("X-Custom-Key", "outer-custom-key")
	req.Header.Set("X-Custom-JWT", "outer-custom-jwt")
	req.Header.Set("X-Rbac-Token", "outer-rbac")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodePipelineResponses(t, res.Body.String())
	for header, want := range map[string]string{
		"X-Got-Apikey":         "outer-key",
		"X-Got-X-Custom-Key":   "outer-custom-key",
		"X-Got-X-Custom-Jwt":   "outer-custom-jwt",
		"X-Got-X-Rbac-Token":   "outer-rbac",
		"X-Got-X-Access-Token": "",
		"X-Got-Backend-Header": "item-value",
	} {
		if got := responses[0].Headers[header]; got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestHandlerRejectsAbsolutePipelineTarget(t *testing.T) {
	handler := NewHandlerWithLimits(http.NotFoundHandler(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "http://internal.example/secret"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "path must start with /") {
		t.Fatalf("response = %d %q, want path-only rejection", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsSchemeRelativePipelineTarget(t *testing.T) {
	handler := NewHandlerWithLimits(http.NotFoundHandler(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "//internal.example/secret"}]
	}`))
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "path is invalid") {
		t.Fatalf("response = %d %q, want path-only rejection", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsPipelineQueryEmbeddedInPath(t *testing.T) {
	handler := NewHandlerWithLimits(http.NotFoundHandler(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "/inner?admin=true"}]
	}`))
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "path is invalid") {
		t.Fatalf("response = %d %q, want query field requirement", res.Code, res.Body.String())
	}
}

func TestHandlerTimeoutJoinsCancellationAwareDispatcher(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(exited)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 10,
		"pipeline": [{"path": "/slow"}]
	}`))
	res := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(res, req)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("handler elapsed = %s, want bounded timeout", elapsed)
	}
	select {
	case <-entered:
	default:
		t.Fatal("dispatcher never entered")
	}
	select {
	case <-exited:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dispatcher worker was not joined after the timeout canceled its context")
	}
	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("pipeline status = %d, want 504", responses[0].Status)
	}
	if responses[0].Body != "" {
		t.Fatalf("pipeline body = %q, want internal error text discarded", responses[0].Body)
	}
}

func TestBatchTimeoutWritesBeforeJoiningWorker(t *testing.T) {
	entered := make(chan struct{})
	released := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorker := func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(releaseWorker)
	dispatcher := newBatchDispatcher(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-released
	}), 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveBatchRequest(dispatcher, applyLimitDefaults(Limits{}), w, r)
	})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 10,
		"pipeline": [{"path": "/slow"}]
	}`))
	var childReleased atomic.Int32
	req = WithDispatchLeaseFactory(req, func() (DispatchLease, bool) {
		return DispatchLease{
			Handler: dispatcher.handler,
			Release: func() { childReleased.Add(1) },
		}, true
	})
	res := &batchCommitSignalWriter{ResponseRecorder: httptest.NewRecorder(), committed: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		handler.ServeHTTP(res, req)
		close(served)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stuck worker never started")
	}
	select {
	case <-res.committed:
	case <-time.After(time.Second):
		t.Fatal("timeout response was not written")
	}
	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("pipeline status = %d, want 504", responses[0].Status)
	}
	select {
	case <-served:
		t.Fatal("batch handler returned before the timed-out worker exited")
	default:
	}
	dispatcher.mu.Lock()
	active := dispatcher.active
	dispatcher.mu.Unlock()
	if active != 1 {
		t.Fatalf("dispatcher active = %d after timeout response, want 1", active)
	}
	if got := childReleased.Load(); got != 0 {
		t.Fatalf("child releases after timeout response = %d, want 0", got)
	}
	releaseWorker()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("batch handler did not return after releasing the worker")
	}
	dispatcher.mu.Lock()
	active = dispatcher.active
	dispatcher.mu.Unlock()
	if active != 0 {
		t.Fatalf("dispatcher active after worker release = %d, want 0", active)
	}
	if got := childReleased.Load(); got != 1 {
		t.Fatalf("child releases after worker release = %d, want 1", got)
	}
}

type batchCommitSignalWriter struct {
	*httptest.ResponseRecorder
	committed chan struct{}
	once      sync.Once
}

func (w *batchCommitSignalWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(body)
	w.once.Do(func() { close(w.committed) })
	return n, err
}

func TestHandlerParentCancellationCancelsSubrequestsAndJoinsWorkers(t *testing.T) {
	var started atomic.Int32
	var exited atomic.Int32
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		<-r.Context().Done()
		exited.Add(1)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [
			{"path": "/slow-1"},
			{"path": "/slow-2"},
			{"path": "/slow-3"}
		]
	}`))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	res := httptest.NewRecorder()

	served := make(chan struct{})
	go func() {
		handler.ServeHTTP(res, req)
		close(served)
	}()

	// Subrequests run one at a time; cancel while the first is in flight.
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("first subrequest never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("batch handler did not return after parent cancellation")
	}

	// The handler no longer blocks on the worker, so the cancellation-aware
	// in-flight worker exits asynchronously once its context is canceled.
	exitedDeadline := time.Now().Add(2 * time.Second)
	for exited.Load() != 1 {
		if time.Now().After(exitedDeadline) {
			t.Fatalf("in-flight subrequest worker did not exit after cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := started.Load(); got != 1 {
		t.Fatalf("started subrequests = %d, want the pipeline to stop after cancellation", got)
	}
	var responses []PipelineResponse
	if err := json.Unmarshal(res.Body.Bytes(), &responses); err != nil {
		t.Fatalf("decode batch response: %v, body=%q", err, res.Body.String())
	}
	if len(responses) != 1 {
		t.Fatalf("responses length = %d, want 1", len(responses))
	}
	if responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("response status = %d, want 504", responses[0].Status)
	}
	if responses[0].Body != "" {
		t.Fatalf("response body = %q, want internal error text discarded", responses[0].Body)
	}
}

func TestDispatchPipelineRequestInvalidTargetReturnsBadRequest(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	outer := httptest.NewRequest(http.MethodGet, "http://example.com/batch", nil)
	item := PipelineRequest{Path: "/%zz"}

	limits := applyLimitDefaults(Limits{})
	tasks := runtime.NewRequestTaskGroup(outer.Context(), "request/batch-requests/test")
	result, timedOut, err := newBatchDispatcher(dispatcher, limits.maxConcurrency).dispatch(
		outer, Request{}, item, time.Second, limits.maxResponseBodySize, tasks,
	)
	if err != nil {
		t.Fatal(err)
	}
	if waitErr := tasks.Wait(); waitErr != nil {
		t.Fatal(waitErr)
	}

	if timedOut {
		t.Fatal("invalid target reported a timeout")
	}
	if result.response.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", result.response.Status, http.StatusBadRequest)
	}
	if result.response.Reason != http.StatusText(http.StatusBadRequest) {
		t.Fatalf("reason = %q, want %q", result.response.Reason, http.StatusText(http.StatusBadRequest))
	}
}

func TestDispatchPipelineRequestTimeoutWhenHandlerIgnoringCancellation(t *testing.T) {
	release := make(chan struct{})
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	outer := httptest.NewRequest(http.MethodGet, "http://example.com/batch", nil)
	item := PipelineRequest{Path: "/slow"}

	start := time.Now()
	limits := applyLimitDefaults(Limits{})
	tasks := runtime.NewRequestTaskGroup(outer.Context(), "request/batch-requests/test")
	result, timedOut, err := newBatchDispatcher(dispatcher, limits.maxConcurrency).dispatch(
		outer, Request{}, item, 20*time.Millisecond, limits.maxResponseBodySize, tasks,
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}

	if !timedOut {
		t.Fatal("timedOut = false, want true")
	}
	if result.response.Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", result.response.Status, http.StatusGatewayTimeout)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("dispatch took %s, want return before 200ms", elapsed)
	}
	close(release)
	if waitErr := tasks.Wait(); waitErr != nil {
		t.Fatal(waitErr)
	}
}

func newBatchRequest(t *testing.T, pipelineItems int) *http.Request {
	t.Helper()
	items := make([]PipelineRequest, pipelineItems)
	for i := range items {
		items[i].Path = "/pipeline"
	}
	body, err := json.Marshal(Request{Pipeline: items})
	if err != nil {
		t.Fatalf("marshal batch request: %v", err)
	}
	return httptest.NewRequest(http.MethodPost, DefaultURI, bytes.NewReader(body))
}

func decodePipelineResponses(t *testing.T, body string) []PipelineResponse {
	t.Helper()
	responses := decodeAllPipelineResponses(t, body)
	if len(responses) != 1 {
		t.Fatalf("responses length = %d, want 1", len(responses))
	}
	return responses
}

func decodeAllPipelineResponses(t *testing.T, body string) []PipelineResponse {
	t.Helper()

	var responses []PipelineResponse
	if err := json.Unmarshal([]byte(body), &responses); err != nil {
		t.Fatalf("decode batch response: %v, body=%q", err, body)
	}
	return responses
}
