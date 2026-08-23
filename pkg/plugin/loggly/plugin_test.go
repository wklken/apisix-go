package loggly

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestResolveLogglySnapshotFormatUsesRequestStartTime(t *testing.T) {
	started := time.Date(2026, time.August, 12, 8, 30, 0, 0, time.UTC)
	fields := resolveLogglySnapshotFormat(base.LogSnapshot{
		Started:  started,
		Finished: started.Add(5 * time.Second),
	}, map[string]string{"timestamp": "$time_iso8601"})
	if fields["timestamp"] != started.Format(time.RFC3339) {
		t.Fatalf("timestamp = %#v, want request start", fields["timestamp"])
	}
}

func TestSendBatchCancelsLogglyHTTPBulkWithContext(t *testing.T) {
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

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          server.URL,
		Protocol:      "http",
		Timeout:       10000,
	})
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
		t.Fatal("timed out waiting for Loggly bulk request")
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
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitRejectsMissingDataEncryptionResolver(t *testing.T) {
	p := &Plugin{}
	if err := p.PostInit(); err == nil || err.Error() != "data-encryption resolver is required" {
		t.Fatalf("PostInit() error = %v, want missing resolver error", err)
	}
}

func TestPostInitSetsDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{CustomerToken: "token"})

	if p.config.Severity != "INFO" {
		t.Fatalf("severity = %q, want INFO", p.config.Severity)
	}
	if len(p.config.Tags) != 1 || p.config.Tags[0] != "apisix" {
		t.Fatalf("tags = %v, want [apisix]", p.config.Tags)
	}
	if p.config.Host != "logs-01.loggly.com" {
		t.Fatalf("host = %q, want logs-01.loggly.com", p.config.Host)
	}
	if p.config.Port != 514 {
		t.Fatalf("port = %d, want 514", p.config.Port)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
}

func TestPostInitRejectsInvalidEncryptedCustomerToken(t *testing.T) {
	p := &Plugin{config: Config{CustomerToken: "not-a-ciphertext"}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted customer_token rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedCustomerToken(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	p := &Plugin{config: Config{CustomerToken: encryptLogglyTestValue(t, oldKey, "loggly-token")}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.CustomerToken != "loggly-token" {
		t.Fatalf("customer_token = %q, want resolved plaintext", p.config.CustomerToken)
	}
}

func TestBuildMessageUsesRFC5424ShapeAndTags(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Severity:      "INFO",
		Tags:          []string{"apisix", "route-a"},
	})

	message := p.buildMessage(map[string]any{
		"status": 200,
		"path":   "/get",
	})

	if !strings.HasPrefix(message, "<14>1 ") {
		t.Fatalf("message = %q, want INFO priority prefix <14>1", message)
	}
	if !strings.Contains(message, `[token@41058 tag="apisix" tag="route-a"]`) {
		t.Fatalf("message = %q, want Loggly structured data with tags", message)
	}
	if !strings.Contains(message, `"path":"/get"`) {
		t.Fatalf("message = %q, want JSON log payload", message)
	}
}

func TestBuildMessageUsesSeverityMap(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Severity:      "INFO",
		SeverityMap:   map[string]string{"503": "ERR"},
	})

	message := p.buildMessage(map[string]any{"status": 503})
	if !strings.HasPrefix(message, "<11>1 ") {
		t.Fatalf("message = %q, want ERR priority prefix <11>1", message)
	}
}

func TestHandlerBuildsDefaultAccessLogAndAddsRouteIDToCustomFormat(t *testing.T) {
	tests := []struct {
		name      string
		logFormat map[string]string
		assert    func(*testing.T, map[string]any)
	}{
		{
			name: "default access log",
			assert: func(t *testing.T, payload map[string]any) {
				t.Helper()
				request, ok := payload["request"].(map[string]any)
				if !ok || request["method"] != http.MethodGet || request["uri"] != "/orders?item=1" {
					t.Fatalf("request = %#v, want captured GET request", payload["request"])
				}
				response, ok := payload["response"].(map[string]any)
				if !ok || response["status"] != float64(http.StatusCreated) {
					t.Fatalf("response = %#v, want status 201", payload["response"])
				}
				if payload["route_id"] != "route-1" {
					t.Fatalf("route_id = %#v, want route-1", payload["route_id"])
				}
				if payload["client_ip"] == "" || payload["server"] == nil {
					t.Fatalf("payload = %#v, want client and server fields", payload)
				}
			},
		},
		{
			name:      "custom format",
			logFormat: map[string]string{"method": "$request_method"},
			assert: func(t *testing.T, payload map[string]any) {
				t.Helper()
				if payload["method"] != http.MethodGet {
					t.Fatalf("method = %#v, want GET", payload["method"])
				}
				if payload["route_id"] != "route-1" {
					t.Fatalf("route_id = %#v, want route-1", payload["route_id"])
				}
				if _, ok := payload["request"]; ok {
					t.Fatalf("payload = %#v, custom format must replace default fields", payload)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				received <- body
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)

			p := newTestPlugin(t, Config{
				CustomerToken: "token",
				Host:          server.URL,
				Protocol:      "http",
				Timeout:       1000,
				BatchMaxSize:  1,
				LogFormat:     tt.logFormat,
			})
			p.RouteID = "route-1"
			p.ServerAddr = "127.0.0.1:8080"

			req := httptest.NewRequest(http.MethodGet, "http://localhost/orders?item=1", nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("created"))
			})).ServeHTTP(rr, req)

			select {
			case payload := <-received:
				tt.assert(t, payload)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for Loggly payload")
			}
		})
	}
}

func TestHandlerUsesRequestHostInRFC5424Envelope(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
		BatchMaxSize:  1,
		LogFormat:     map[string]string{"marker": "request-host"},
	})
	p.RouteID = "route-1"

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/orders", nil)
	req.Host = "127.0.0.1"
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)

	select {
	case message := <-received:
		if !strings.Contains(message, " 127.0.0.1 apisix ") {
			t.Fatalf("message = %q, want request host in RFC5424 envelope", message)
		}
		if !strings.HasSuffix(message, ` {"marker":"request-host","route_id":"route-1"}`) {
			t.Fatalf("message = %q, want internal host field omitted from payload", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UDP log message")
	}
}

func TestSendWritesUDPMessage(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})

	p.Send(map[string]any{"status": 200, "path": "/get"})

	select {
	case message := <-received:
		if !strings.Contains(message, `[token@41058 tag="apisix"]`) {
			t.Fatalf("message = %q, want Loggly token and default tag", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UDP log message")
	}
}

func TestSendWritesHTTPBulkMessage(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bulk/token/tag/bulk" {
			t.Fatalf("path = %q, want /bulk/token/tag/bulk", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("X-LOGGLY-TAG") != "apisix,route-a" {
			t.Fatalf("X-LOGGLY-TAG = %q, want apisix,route-a", r.Header.Get("X-LOGGLY-TAG"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body["path"].(string)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          server.URL,
		Protocol:      "http",
		Tags:          []string{"apisix", "route-a"},
		Timeout:       1000,
	})

	p.Send(map[string]any{"status": 200, "path": "/bulk"})

	select {
	case path := <-received:
		if path != "/bulk" {
			t.Fatalf("path = %q, want /bulk", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
	}
}

func TestHandlerBatchesHTTPBulkMessages(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          server.URL,
		Protocol:      "http",
		Timeout:       1000,
		BatchMaxSize:  2,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	select {
	case body := <-received:
		lines := strings.Split(body, "\n")
		if len(lines) != 2 {
			t.Fatalf("bulk body = %q, want two newline-delimited entries", body)
		}
		for _, line := range lines {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("unmarshal bulk line %q: %v", line, err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loggly HTTP bulk body")
	}
}

func TestSendBatchWritesUDPMessagesIndividually(t *testing.T) {
	addr, received := startUDPServerN(t, 2)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})

	firstFail, err := p.SendBatch(context.Background(), []map[string]any{
		{"status": 200, "path": "/first"},
		{"status": 201, "path": "/second"},
	}, 2)
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	if firstFail != 0 {
		t.Fatalf("firstFail = %d, want 0", firstFail)
	}

	for _, want := range []string{"/first", "/second"} {
		select {
		case message := <-received:
			if !strings.Contains(message, want) {
				t.Fatalf("message = %q, want path %s", message, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for UDP log message %s", want)
		}
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:    "token",
		Host:             server.URL,
		Protocol:         "http",
		Timeout:          1000,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		BatchMaxSize:     1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if body := rr.Body.String(); body != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream response body", body)
	}

	select {
	case payload := <-received:
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("payload request body = %#v, want original request body", request["body"])
		}

		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("payload response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
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
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:       "token",
		Host:                server.URL,
		Protocol:            "http",
		Timeout:             1000,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case payload := <-received:
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("payload request body = %#v, want captured request body", request["body"])
		}

		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("payload response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
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
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:       "token",
		Host:                server.URL,
		Protocol:            "http",
		Timeout:             1000,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":3}`))
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":3}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)

	select {
	case payload := <-received:
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want default request fields", payload["request"])
		}
		if _, ok := request["body"]; ok {
			t.Fatalf("payload request = %#v, want no request body", request)
		}
		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want default response fields", payload["response"])
		}
		if _, ok := response["body"]; ok {
			t.Fatalf("payload response = %#v, want no response body", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":         "token",
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsOfficialBodySizeAndSSLFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":      "token",
		"include_req_body":    true,
		"include_resp_body":   true,
		"ssl_verify":          false,
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official config fields: %v", err)
	}
}

func TestSchemaAcceptsBatchAndMaxPendingFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":      "token",
		"batch_max_size":      2,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     60,
		"inactive_timeout":    5,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected batch and max pending fields: %v", err)
	}
}

func encryptLogglyTestValue(t *testing.T, key string, value string) string {
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

func startUDPServer(t *testing.T) (string, <-chan string) {
	return startUDPServerN(t, 1)
}

func startUDPServerN(t *testing.T, count int) (string, <-chan string) {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	received := make(chan string, count)
	go func() {
		buf := make([]byte, 4096)
		for range count {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			received <- string(buf[:n])
		}
	}()

	return conn.LocalAddr().String(), received
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var n int
	for _, r := range value {
		if r < '0' || r > '9' {
			t.Fatalf("invalid integer %q", value)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

type countingHTTPListener struct {
	net.Listener
	accepts atomic.Int64
}

func (l *countingHTTPListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return conn, err
}

func TestSendBatchReusesLogglyUDPSocket(t *testing.T) {
	addr, received := startUDPServerN(t, 2)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})
	var dials atomic.Int64
	p.dialFunc = func() (net.Conn, error) {
		dials.Add(1)
		return net.Dial("udp", addr)
	}

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch #1 error = %v", err)
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r2"}}, 1); err != nil {
		t.Fatalf("SendBatch #2 error = %v", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1 reused socket", got)
	}
	for range 2 {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for loggly UDP message")
		}
	}
}

func TestSendBatchLogglyUDPUnblocksOnContextCancellation(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
	})
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          "unused",
		Port:          1,
		Timeout:       1000,
	})
	p.conn = client
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(ctx, []map[string]any{{"route_id": "blocked"}}, 1)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SendBatch() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SendBatch() remained blocked after context cancellation")
	}
}

type cancelAfterWriteConn struct {
	cancel context.CancelFunc
	closed atomic.Bool
}

func (c *cancelAfterWriteConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *cancelAfterWriteConn) Write(payload []byte) (int, error) {
	c.cancel()
	return len(payload), nil
}

func (c *cancelAfterWriteConn) Close() error {
	c.closed.Store(true)
	return nil
}
func (*cancelAfterWriteConn) LocalAddr() net.Addr              { return nil }
func (*cancelAfterWriteConn) RemoteAddr() net.Addr             { return nil }
func (*cancelAfterWriteConn) SetDeadline(time.Time) error      { return nil }
func (*cancelAfterWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*cancelAfterWriteConn) SetWriteDeadline(time.Time) error { return nil }

func TestSendBatchLogglyUDPDiscardsConnectionCanceledAfterWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &cancelAfterWriteConn{cancel: cancel}
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          "unused",
		Port:          1,
		Timeout:       1000,
	})
	p.conn = conn
	_, err := p.SendBatch(ctx, []map[string]any{{"route_id": "deadline-race"}}, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SendBatch() error = %v, want context canceled", err)
	}
	if !conn.closed.Load() {
		t.Fatal("canceled connection was not closed")
	}
	p.connMu.Lock()
	retained := p.conn
	p.connMu.Unlock()
	if retained != nil {
		t.Fatal("canceled connection remained available for reuse")
	}
}

func TestSendBatchRedialsLogglyUDPSocketAfterFailure(t *testing.T) {
	addr, received := startUDPServerN(t, 2)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})
	var dials atomic.Int64
	p.dialFunc = func() (net.Conn, error) {
		dials.Add(1)
		return net.Dial("udp", addr)
	}

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch #1 error = %v", err)
	}
	p.connMu.Lock()
	_ = p.conn.Close()
	p.connMu.Unlock()

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r2"}}, 1); err == nil {
		t.Fatal("SendBatch #2 error = nil on a closed socket")
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r3"}}, 1); err != nil {
		t.Fatalf("SendBatch #3 error = %v, want redial delivery", err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dial count after redial = %d, want 2", got)
	}
	for range 2 {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for loggly UDP message")
		}
	}
}

func TestSendBatchReusesLogglyHTTPTransport(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	counting := &countingHTTPListener{Listener: ln}
	server.Listener = counting
	server.Start()
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "http",
		Host:          host,
		Port:          80,
		Timeout:       1000,
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch #1 error = %v", err)
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r2"}}, 1); err != nil {
		t.Fatalf("SendBatch #2 error = %v", err)
	}
	if got := counting.accepts.Load(); got != 1 {
		t.Fatalf("HTTP connections = %d, want 1 reused transport connection", got)
	}
}

func TestStopClosesLogglyUDPSocket(t *testing.T) {
	addr, received := startUDPServerN(t, 1)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})
	p.dialFunc = func() (net.Conn, error) {
		return net.Dial("udp", addr)
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for loggly UDP message")
	}

	p.Stop()
	p.connMu.Lock()
	conn := p.conn
	p.connMu.Unlock()
	if conn != nil {
		t.Fatal("Stop() left the loggly UDP socket open")
	}
}
