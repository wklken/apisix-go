package splunk_hec_logging

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/util"
)

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

func TestPostInitSetsSplunkDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   "http://127.0.0.1:8088/services/collector/event",
			Token: "token",
		},
	})

	if p.config.Endpoint.Timeout != 10 {
		t.Fatalf("timeout = %d, want 10", p.config.Endpoint.Timeout)
	}
	if p.config.Endpoint.KeepaliveTimeout != 60000 {
		t.Fatalf("keepalive timeout = %d, want 60000", p.config.Endpoint.KeepaliveTimeout)
	}
	if !p.sslVerify() {
		t.Fatal("sslVerify() = false, want true by default")
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestPostInitRejectsInvalidEncryptedToken(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{Endpoint: Endpoint{Token: "not-a-ciphertext"}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted endpoint.token rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedToken(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{newKey, oldKey})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{Endpoint: Endpoint{Token: encryptSplunkTestValue(t, oldKey, "splunk-token")}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.Endpoint.Token != "splunk-token" {
		t.Fatalf("endpoint.token = %q, want resolved plaintext", p.config.Endpoint.Token)
	}
}

func TestBuildEventUsesSplunkHECShape(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   "http://127.0.0.1:8088/services/collector/event",
			Token: "token",
		},
	})

	event := p.buildEvent(map[string]any{
		"path":   "/orders",
		"status": 201,
	})

	if event.Source != "apache-apisix-splunk-hec-logging" {
		t.Fatalf("source = %q, want apache-apisix-splunk-hec-logging", event.Source)
	}
	if event.SourceType != "_json" {
		t.Fatalf("sourcetype = %q, want _json", event.SourceType)
	}
	if event.Host == "" {
		t.Fatal("host is empty")
	}
	if event.Event["path"] != "/orders" {
		t.Fatalf("event path = %v, want /orders", event.Event["path"])
	}
	if event.Event["status"] != 201 {
		t.Fatalf("event status = %v, want 201", event.Event["status"])
	}
	if event.Time <= 0 {
		t.Fatalf("event time = %v, want positive Unix timestamp", event.Time)
	}
}

func TestMetadataSchemaAcceptsAdditiveLogFormat(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	metadata := map[string]any{
		"log_format_extra":    map[string]any{"upstream_host": "$upstream_unresolved_host"},
		"max_pending_entries": 1,
	}
	if err := util.Validate(metadata, p.GetMetadataSchema()); err != nil {
		t.Fatalf("metadata schema rejected additive log format: %v", err)
	}
	if err := util.Validate(map[string]any{"log_format": "wrong-type"}, p.GetMetadataSchema()); err == nil {
		t.Fatal("metadata schema accepted string log_format")
	}
}

func TestHandlerBuildsDefaultEventAndDoesNotClobberFieldsWithExtraFormat(t *testing.T) {
	p := &Plugin{
		logFormatExtra: map[string]string{
			"response_status": "extra-must-not-clobber",
			"upstream_host":   "$upstream_unresolved_host",
		},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"http://gateway.example:9443/orders?sku=one",
		strings.NewReader("payload"),
	)
	req.RemoteAddr = "192.0.2.44:4567"
	req.Header.Set("X-Request-Marker", "request-value")
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)

	entry := captureHandlerEntry(t, p, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "10.0.0.8")
		apisixctx.RegisterApisixVar(r, "$balancer_port", "9080")
		w.Header().Set("X-Upstream-Marker", "response-value")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	if got := entry["request_url"]; got != "http://gateway.example:9443/orders?sku=one" {
		t.Fatalf("request_url = %#v", got)
	}
	if got := entry["request_method"]; got != http.MethodPost {
		t.Fatalf("request_method = %#v", got)
	}
	requestHeaders, ok := entry["request_headers"].(http.Header)
	if !ok || requestHeaders.Get("X-Request-Marker") != "request-value" {
		t.Fatalf("request_headers = %#v", entry["request_headers"])
	}
	requestQuery, ok := entry["request_query"].(map[string][]string)
	if !ok || len(requestQuery["sku"]) != 1 || requestQuery["sku"][0] != "one" {
		t.Fatalf("request_query = %#v", entry["request_query"])
	}
	if got := entry["request_size"]; got != int64(len("payload")) {
		t.Fatalf("request_size = %#v", got)
	}
	responseHeaders, ok := entry["response_headers"].(http.Header)
	if !ok || responseHeaders.Get("X-Upstream-Marker") != "response-value" {
		t.Fatalf("response_headers = %#v", entry["response_headers"])
	}
	if got := entry["response_status"]; got != http.StatusCreated {
		t.Fatalf("response_status = %#v, want %d", got, http.StatusCreated)
	}
	if got := entry["response_size"]; got != int64(len("ok")) {
		t.Fatalf("response_size = %#v", got)
	}
	if got := entry["upstream"]; got != "10.0.0.8:9080" {
		t.Fatalf("upstream = %#v", got)
	}
	if got := entry["upstream_host"]; got != "10.0.0.8" {
		t.Fatalf("upstream_host = %#v", got)
	}
	if latency, ok := entry["latency"].(int64); !ok || latency < 0 {
		t.Fatalf("latency = %#v", entry["latency"])
	}
}

func TestHandlerTreatsExplicitEmptyLogFormatAsCustomAndSuppressesExtras(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   "http://127.0.0.1:8088/services/collector/event",
			Token: "token",
		},
		LogFormat:      map[string]string{},
		LogFormatExtra: map[string]string{"upstream_host": "$upstream_unresolved_host"},
	})
	p.BatchProcessor.Stop()
	p.BatchProcessor = nil

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example:9443/empty", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	entry := captureHandlerEntry(t, p, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "10.0.0.8")
		w.WriteHeader(http.StatusNoContent)
	}))

	if len(entry) != 0 {
		t.Fatalf("entry = %#v, want explicit empty custom event", entry)
	}
}

func TestHandlerResolvesCustomVariablesAfterUpstreamWithoutPorts(t *testing.T) {
	p := &Plugin{
		logFormatExtra: map[string]string{"ignored_extra": "must-not-appear"},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.LogFormat = map[string]string{
		"client_ip":     "$remote_addr",
		"host":          "$host",
		"upstream_host": "$upstream_unresolved_host",
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example:9443/delayed", nil)
	req.RemoteAddr = "192.0.2.44:4567"
	req = apisixctx.WithApisixVars(req, map[string]string{})
	entry := captureHandlerEntry(t, p, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "10.0.0.8")
		r.Host = "upstream.internal:9080"
		w.WriteHeader(http.StatusNoContent)
	}))

	want := map[string]any{
		"client_ip":     "192.0.2.44",
		"host":          "gateway.example",
		"upstream_host": "10.0.0.8",
	}
	if len(entry) != len(want) {
		t.Fatalf("entry = %#v, want %#v", entry, want)
	}
	for key, expected := range want {
		if got := entry[key]; got != expected {
			t.Fatalf("entry[%q] = %#v, want %#v", key, got, expected)
		}
	}
}

func captureHandlerEntry(
	t *testing.T,
	p *Plugin,
	req *http.Request,
	next http.Handler,
) map[string]any {
	t.Helper()

	p.Handler(next).ServeHTTP(httptest.NewRecorder(), req)
	select {
	case entry := <-p.FireChan:
		return entry
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk handler entry")
		return nil
	}
}

func TestSendPostsSplunkHECEvent(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:     server.URL,
			Token:   "secret-token",
			Channel: "channel-a",
			Timeout: 1,
		},
		SSLVerify: &sslVerify,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if got := req.Header.Get("Authorization"); got != "Splunk secret-token" {
			t.Fatalf("Authorization = %q, want Splunk secret-token", got)
		}
		if got := req.Header.Get("X-Splunk-Request-Channel"); got != "channel-a" {
			t.Fatalf("X-Splunk-Request-Channel = %q, want channel-a", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC request")
	}

	select {
	case body := <-bodies:
		event, ok := body["event"].(map[string]any)
		if !ok {
			t.Fatalf("body event = %#v, want object", body["event"])
		}
		if event["path"] != "/orders" {
			t.Fatalf("event path = %v, want /orders", event["path"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC body")
	}
}

func TestSendBatchPostsConcatenatedSplunkHECEvents(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   server.URL,
			Token: "secret-token",
		},
		BatchMaxSize: 2,
	})

	if _, err := p.SendBatch([]map[string]any{{"path": "/a"}, {"path": "/b"}}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-bodies:
		if !strings.Contains(body, `"path":"/a"`) || !strings.Contains(body, `"path":"/b"`) {
			t.Fatalf("body = %q, want both Splunk events", body)
		}
		if strings.Contains(body, "\n") || strings.HasPrefix(body, "[") {
			t.Fatalf("body = %q, want concatenated JSON event objects", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC batch request")
	}
}

func encryptSplunkTestValue(t *testing.T, key string, value string) string {
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
