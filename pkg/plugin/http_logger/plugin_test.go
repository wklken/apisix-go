package http_logger

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	brotli "github.com/andybalholm/brotli"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestStopDefersHTTPClientReleaseUntilDeliveryCallbackReturns(t *testing.T) {
	started := make(chan struct{})
	releaseCallback := make(chan struct{})
	clientReleased := make(chan struct{})
	p := &Plugin{}
	p.BatchProcessor = logger_batch.NewWithContext(logger_batch.Config{
		BatchMaxSize:    1,
		ShutdownTimeout: 20 * time.Millisecond,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(ctx context.Context, _ []map[string]any, _ int) (int, error) {
		close(started)
		<-ctx.Done()
		<-releaseCallback
		return 0, ctx.Err()
	})
	p.clientRelease = func() { close(clientReleased) }
	if !p.BatchProcessor.Push(map[string]any{"id": "blocked"}) {
		t.Fatal("push was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Plugin.Stop() exceeded the processor shutdown bound")
	}
	select {
	case <-clientReleased:
		t.Fatal("HTTP client was released before delivery callback exit")
	default:
	}
	close(releaseCallback)
	select {
	case <-clientReleased:
	case <-time.After(time.Second):
		t.Fatal("HTTP client was not released after delivery callback exit")
	}
}

func TestSendBatchCancelsRestyRequestWithContext(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	p := newTestPlugin(t, Config{URI: server.URL, Timeout: 10})
	t.Cleanup(p.BatchProcessor.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(ctx, []map[string]any{{"path": "/cancel"}}, 1)
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HTTP logger request")
	}
	cancel()

	var err error
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("SendBatch() did not return after context cancellation")
	}
	if err == nil {
		t.Fatal("SendBatch() error = nil, want context cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(100 * time.Millisecond):
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SendBatch() error = %v, want context cancellation when backend did not observe it", err)
		}
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}

func TestPostInitDefaultsWithoutMetadataStore(t *testing.T) {
	p := newTestPlugin(t, Config{URI: "http://127.0.0.1/logs"})

	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want official default 3 seconds", p.config.Timeout)
	}
	if p.config.ConcatMethod != "json" {
		t.Fatalf("concat_method = %q, want json", p.config.ConcatMethod)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.MaxRetryCount != 0 {
		t.Fatalf("max_retry_count = %d, want 0", p.config.MaxRetryCount)
	}
}

func TestConfigPreservesExplicitZeroRetryDelay(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"uri":"http://127.0.0.1/logs","retry_delay":0}`), &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	p := newTestPlugin(t, cfg)
	t.Cleanup(p.BatchProcessor.Stop)
	if p.config.RetryDelay != 0 {
		t.Fatalf("retry_delay = %d, want explicit zero", p.config.RetryDelay)
	}
	if !p.config.retryDelaySet {
		t.Fatal("config lost explicit retry_delay presence")
	}
}

func TestPostInitNormalizesOfficialInBodyExpression(t *testing.T) {
	p := newTestPlugin(t, Config{
		URI: "http://127.0.0.1/logs",
		IncludeRespBodyExpr: []any{
			[]any{"http_content_length", "<", float64(1024)},
			[]any{
				"http_content_type",
				"in",
				[]any{"application/xml", "application/json", "text/plain", "text/xml"},
			},
		},
	})
	t.Cleanup(p.BatchProcessor.Stop)

	second := p.config.IncludeRespBodyExpr[1].([]any)
	if second[1] != "~" {
		t.Fatalf("normalized operator = %#v, want regex match", second[1])
	}
	if second[2] != `^(application/xml|application/json|text/plain|text/xml)$` {
		t.Fatalf("normalized expression = %#v", second[2])
	}
}

func TestPostInitRejectsInvalidEncryptedAuthHeader(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	authHeader := "not-a-ciphertext"
	p := &Plugin{config: Config{URI: "http://127.0.0.1/logs", AuthHeader: &authHeader}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted auth_header rejection")
	}
}

func TestPostInitResolvesEncryptedAuthHeader(t *testing.T) {
	key := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{key})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	authHeader := encryptHTTPLoggerTestValue(t, key, "Bearer secret")
	p := &Plugin{config: Config{URI: "http://127.0.0.1/logs", AuthHeader: &authHeader}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.AuthHeader == nil || *p.config.AuthHeader != "Bearer secret" {
		t.Fatalf("auth_header = %v, want decrypted value", p.config.AuthHeader)
	}
}

func TestSendPostsJSONLogWithAuthorizationHeader(t *testing.T) {
	authHeader := "Bearer secret"
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != authHeader {
			t.Fatalf("authorization = %q, want %q", r.Header.Get("Authorization"), authHeader)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:        server.URL + "/logs?source=apisix",
		AuthHeader: &authHeader,
		Timeout:    3,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if body["path"] != "/orders" {
			t.Fatalf("body = %#v, want path /orders", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func encryptHTTPLoggerTestValue(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}

func TestPostInitSetsTextContentTypeForNewLineConcat(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		ConcatMethod: "new_line",
		Timeout:      3,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case got := <-received:
		if got != "text/plain" {
			t.Fatalf("content-type = %q, want text/plain", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerBatchesJSONLogs(t *testing.T) {
	received := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:             server.URL,
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))

	select {
	case body := <-received:
		if len(body) != 2 {
			t.Fatalf("batch length = %d, want 2", len(body))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched http log request")
	}
}

func TestHandlerBatchesNewLineLogs(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received <- string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:             server.URL,
		ConcatMethod:    "new_line",
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))

	select {
	case body := <-received:
		lines := strings.Split(body, "\n")
		if len(lines) != 2 {
			t.Fatalf("body = %q, want two newline-delimited JSON entries", body)
		}
		for _, line := range lines {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("line %q is not JSON: %v", line, err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched http log request")
	}
}

func TestHandlerDropsWhenMaxPendingEntriesExceeded(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:               server.URL,
		BatchMaxSize:      1,
		MaxPendingEntries: 1,
		InactiveTimeout:   60,
		BufferDuration:    60,
	})
	t.Cleanup(func() {
		close(release)
		p.BatchProcessor.Stop()
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/three", nil))

	stats := p.BatchProcessor.Stats()
	if stats.Dropped != 2 {
		t.Fatalf("dropped = %d, want 2", stats.Dropped)
	}
}

func TestBatchProcessorLifecycleStateMatchesStaleAndBufferedCases(t *testing.T) {
	t.Run("completed delivery worker is removed while processor remains usable", func(t *testing.T) {
		received := make(chan struct{}, 2)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			received <- struct{}{}
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(server.Close)

		p := newTestPlugin(t, Config{URI: server.URL, BatchMaxSize: 1})
		t.Cleanup(p.BatchProcessor.Stop)
		handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/first", nil))
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for first delivery")
		}
		deadline := time.Now().Add(time.Second)
		for {
			stats := p.BatchProcessor.Stats()
			if stats.Pending == 0 && stats.Processing == 0 && stats.Buffered == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("completed worker state = %+v, want no pending, processing, or buffered entries", stats)
			}
			time.Sleep(10 * time.Millisecond)
		}

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/second", nil))
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("processor was not usable after completed worker cleanup")
		}
	})

	t.Run("buffered processor remains in use past stale window", func(t *testing.T) {
		received := make(chan struct{}, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			received <- struct{}{}
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(server.Close)

		p := newTestPlugin(t, Config{
			URI:             server.URL,
			BatchMaxSize:    2,
			InactiveTimeout: 5,
		})
		t.Cleanup(p.BatchProcessor.Stop)
		handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/first", nil))
		time.Sleep(1500 * time.Millisecond)
		stats := p.BatchProcessor.Stats()
		if stats.Pending != 1 || stats.Buffered != 1 || stats.Processing != 0 {
			t.Fatalf("buffered state = %+v, want one pending buffered entry and no delivery worker", stats)
		}

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/second", nil))
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for preserved two-entry batch")
		}
	})
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:              server.URL,
		BatchMaxSize:     1,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
	})

	upstreamBody := make(chan string, 1)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream read body: %v", err)
		}
		upstreamBody <- string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Body.String() != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream body preserved", rr.Body.String())
	}
	select {
	case body := <-upstreamBody:
		if body != `{"order":1}` {
			t.Fatalf("upstream request body = %q, want original body", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request body")
	}

	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", body["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", body["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:                 server.URL,
		BatchMaxSize:        1,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", body["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", body["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:                 server.URL,
		BatchMaxSize:        1,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	upstreamBody := make(chan string, 1)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":3}`))
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream read body: %v", err)
		}
		upstreamBody <- string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-upstreamBody:
		if body != `{"order":3}` {
			t.Fatalf("upstream request body = %q, want original body", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request body")
	}
	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want default request object", body["request"])
		}
		if _, ok := request["body"]; ok {
			t.Fatalf("request = %#v, want no logged request body", request)
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want default response object", body["response"])
		}
		if _, ok := response["body"]; ok {
			t.Fatalf("response = %#v, want no logged response body", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerResolvesNestedLogFormatAndTruncatesAtDepthFive(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"nested": map[string]any{"method": "$request_method"},
			"a": map[string]any{
				"b": map[string]any{
					"c": map[string]any{
						"d": map[string]any{
							"e": map[string]any{"f": "too-deep"},
						},
					},
				},
			},
		},
	})
	t.Cleanup(p.BatchProcessor.Stop)

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://example.com/nested", nil))

	select {
	case body := <-received:
		nested, ok := body["nested"].(map[string]any)
		if !ok || nested["method"] != http.MethodPost {
			t.Fatalf("nested = %#v, want resolved request method", body["nested"])
		}
		a := body["a"].(map[string]any)
		b := a["b"].(map[string]any)
		c := b["c"].(map[string]any)
		d := c["d"].(map[string]any)
		e := d["e"].(map[string]any)
		if len(e) != 0 {
			t.Fatalf("depth-five object = %#v, want nested f truncated", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for nested http log")
	}
}

func TestHandlerResolvesFinalStatusWithoutCapturingResponseBody(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"response": map[string]any{"status": "$status"},
		},
	})
	t.Cleanup(p.BatchProcessor.Stop)

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, capturingBody := w.(*base.ResponseRecorder); capturingBody {
			t.Error("handler received a response-body recorder without a body logging requirement")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "created")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/status", nil))

	select {
	case body := <-received:
		response, ok := body["response"].(map[string]any)
		if !ok || response["status"] != float64(http.StatusCreated) {
			t.Fatalf("response = %#v, want final status %d", body["response"], http.StatusCreated)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for status log")
	}
}

func TestHandlerDecodesCompressedResponseBodies(t *testing.T) {
	for _, test := range []struct {
		name     string
		encoding string
		encode   func(*testing.T, string) string
	}{
		{
			name:     "gzip",
			encoding: "gzip",
			encode: func(t *testing.T, value string) string {
				t.Helper()
				var buf bytes.Buffer
				writer := gzip.NewWriter(&buf)
				if _, err := writer.Write([]byte(value)); err != nil {
					t.Fatalf("gzip write: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("gzip close: %v", err)
				}
				return buf.String()
			},
		},
		{
			name:     "brotli",
			encoding: "br",
			encode: func(t *testing.T, value string) string {
				t.Helper()
				var buf bytes.Buffer
				writer := brotli.NewWriter(&buf)
				if _, err := writer.Write([]byte(value)); err != nil {
					t.Fatalf("brotli write: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("brotli close: %v", err)
				}
				return buf.String()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			received := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- body
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(server.Close)

			p := newTestPlugin(t, Config{
				URI:             server.URL,
				BatchMaxSize:    1,
				IncludeRespBody: true,
			})
			t.Cleanup(p.BatchProcessor.Stop)
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", test.encoding)
				_, _ = io.WriteString(w, test.encode(t, "hello world"))
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/compressed", nil))

			select {
			case body := <-received:
				response := body["response"].(map[string]any)
				if response["body"] != "hello world" {
					t.Fatalf("response body = %#v, want decoded body", response["body"])
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for compressed response log")
			}
		})
	}
}

func TestSchemaAcceptsOfficialBodySizeFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":                 "http://127.0.0.1/logs",
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body size fields: %v", err)
	}
}

func TestSchemaAcceptsOfficialBatchFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":                 "http://127.0.0.1/logs",
		"batch_max_size":      10,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     2,
		"inactive_timeout":    1,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official batch fields: %v", err)
	}
}
