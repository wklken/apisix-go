package tencent_cloud_cls

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
	"google.golang.org/protobuf/encoding/protowire"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	if len(cfg.LogFormat) == 0 {
		cfg.LogFormat = map[string]string{"request_id": "$request_id"}
	}

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
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

func TestEffectiveLogFormatRouteWins(t *testing.T) {
	route := map[string]string{"route": "$request_id"}
	metadata := map[string]string{"metadata": "$route_id"}
	putPluginMetadata(t, metadata)

	p := newRawTestPlugin(t, Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
		LogFormat: route,
	})
	if len(p.LogFormat) != 1 || p.LogFormat["route"] != route["route"] {
		t.Fatalf("effective format = %#v, want route format over metadata %#v", p.LogFormat, metadata)
	}
	route["route"] = "mutated"
	if p.LogFormat["route"] == "mutated" {
		t.Fatal("effective route format was not cloned")
	}
}

func TestEffectiveLogFormatUsesMetadataFallback(t *testing.T) {
	metadata := map[string]string{"route": "$route_id"}
	putPluginMetadata(t, metadata)

	p := newRawTestPlugin(t, Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
	})
	if len(p.LogFormat) != 1 || p.LogFormat["route"] != metadata["route"] {
		t.Fatalf("effective format = %#v, want metadata format %#v", p.LogFormat, metadata)
	}
	metadata["route"] = "mutated"
	if p.LogFormat["route"] == "mutated" {
		t.Fatal("effective metadata format was not cloned")
	}
}

func TestEffectiveLogFormatRejectsEmptyBeforeSideEffects(t *testing.T) {
	p := &Plugin{config: Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
	}}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "log_format") {
		t.Fatalf("PostInit() error = %v, want %s log_format rejection", err, name)
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"PostInit() side effects = client=%v release=%v batch=%v, want none",
			p.client != nil,
			p.clientRelease != nil,
			p.BatchProcessor != nil,
		)
	}
}

func newRawTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func putPluginMetadata(t *testing.T, logFormat map[string]string) {
	t.Helper()

	events := make(chan *store.Event, 1)
	storage, err := store.Open(t.TempDir()+"/store.db", events, testutil.DataEncryptionService(false, nil))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	previous := store.ReplaceGlobalStoreForTest(storage)
	storage.Start()
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previous)
		if err := storage.Stop(); err != nil {
			t.Errorf("Store.Stop() error = %v", err)
		}
	})

	value, err := json.Marshal(map[string]any{"log_format": logFormat})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	events <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/plugin_metadata/" + name),
		Value: value,
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Store.Sync() error = %v", err)
	}
	var metadata pluginMetadata
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		t.Fatalf("GetPluginMetadata() error = %v", err)
	}
	if len(metadata.LogFormat) != len(logFormat) {
		t.Fatalf("stored log format = %#v, want %#v", metadata.LogFormat, logFormat)
	}
}

func TestPostInitAppliesCLSDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
	})

	if p.config.Scheme != "https" {
		t.Fatalf("scheme = %q, want https", p.config.Scheme)
	}
	if !p.sslVerify() {
		t.Fatal("ssl_verify = false, want true by default")
	}
	if p.config.SampleRatio != 1 {
		t.Fatalf("sample_ratio = %v, want 1", p.config.SampleRatio)
	}
	if p.config.MaxReqBodyBytes != 524288 {
		t.Fatalf("max_req_body_bytes = %d, want 524288", p.config.MaxReqBodyBytes)
	}
	if p.config.MaxRespBodyBytes != 524288 {
		t.Fatalf("max_resp_body_bytes = %d, want 524288", p.config.MaxRespBodyBytes)
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

func TestRunLogPhasePreservesSampleRatio(t *testing.T) {
	p := &Plugin{config: Config{SampleRatio: 0.5}, sample: func() float64 { return 0.75 }}
	if err := p.RunLogPhase(base.LogSnapshot{}); err != nil {
		t.Fatalf("sampled-out RunLogPhase() error = %v", err)
	}
	p.sample = func() float64 { return 0.25 }
	if err := p.RunLogPhase(base.LogSnapshot{}); err == nil {
		t.Fatal("sampled-in RunLogPhase() error = nil, want unavailable processor")
	}
}

func TestPostInitRejectsInvalidEncryptedSecretKey(t *testing.T) {
	p := &Plugin{config: Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "not-a-ciphertext",
	}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted secret_key rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedSecretKey(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	p := &Plugin{config: Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: encryptTencentCLSTestValue(t, oldKey, "cls-secret"),
		LogFormat: map[string]string{"request_id": "$request_id"},
	}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.SecretKey != "cls-secret" {
		t.Fatalf("secret_key = %q, want resolved plaintext", p.config.SecretKey)
	}
}

func TestSendPostsCLSProtobufPayload(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:    "http",
		CLSHost:   strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:  "topic-a",
		SecretID:  "secret-id",
		SecretKey: "secret-key",
		GlobalTag: map[string]string{"env": "test"},
		Timeout:   1000,
	})

	p.Send(map[string]any{
		"route_id": "r1",
		"status":   200,
		"nested":   map[string]any{"ok": true},
	})

	req := waitRequest(t, requests)
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if req.URL.Path != "/structuredlog" {
		t.Fatalf("path = %q, want /structuredlog", req.URL.Path)
	}
	if got := req.URL.Query().Get("topic_id"); got != "topic-a" {
		t.Fatalf("topic_id = %q, want topic-a", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/x-protobuf" {
		t.Fatalf("Content-Type = %q, want application/x-protobuf", got)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "q-sign-algorithm=sha1") || !strings.Contains(auth, "q-ak=secret-id") ||
		!strings.Contains(auth, "q-signature=") {
		t.Fatalf("Authorization = %q, want Tencent CLS signature", auth)
	}

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if logs[0]["route_id"] != "r1" {
		t.Fatalf("route_id = %q, want r1", logs[0]["route_id"])
	}
	if logs[0]["status"] != "200" {
		t.Fatalf("status = %q, want 200", logs[0]["status"])
	}
	if logs[0]["nested"] != `{"ok":true}` {
		t.Fatalf("nested = %q, want JSON object string", logs[0]["nested"])
	}
	if logs[0]["env"] != "test" {
		t.Fatalf("env = %q, want global tag", logs[0]["env"])
	}
}

func TestHandlerSendsFormattedRequestLog(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:    "http",
		CLSHost:   strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:  "topic-a",
		SecretID:  "secret-id",
		SecretKey: "secret-key",
		LogFormat: map[string]string{
			"method": "$request_method",
			"path":   "$request_uri",
			"plugin": "tencent-cloud-cls",
		},
		Timeout:      1000,
		BatchMaxSize: 1,
	})

	req := httptest.NewRequest(http.MethodPatch, "http://example.com/orders/1?debug=true", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if logs[0]["method"] != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", logs[0]["method"])
	}
	if logs[0]["path"] != "/orders/1?debug=true" {
		t.Fatalf("path = %q, want request URI", logs[0]["path"])
	}
	if logs[0]["plugin"] != "tencent-cloud-cls" {
		t.Fatalf("plugin = %q, want tencent-cloud-cls", logs[0]["plugin"])
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:           "http",
		CLSHost:          strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:         "topic-a",
		SecretID:         "secret-id",
		SecretKey:        "secret-key",
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		Timeout:          1000,
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

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}

	request := decodeJSONStringField(t, logs[0]["request"])
	if request["body"] != `{"order":1}` {
		t.Fatalf("request body = %#v, want original request body", request["body"])
	}

	response := decodeJSONStringField(t, logs[0]["response"])
	if response["body"] != `{"ok":true}` {
		t.Fatalf("response body = %#v, want upstream response body", response["body"])
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:              "http",
		CLSHost:             strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:            "topic-a",
		SecretID:            "secret-id",
		SecretKey:           "secret-key",
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		Timeout:             1000,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}

	request := decodeJSONStringField(t, logs[0]["request"])
	if request["body"] != `{"order":2}` {
		t.Fatalf("request body = %#v, want captured request body", request["body"])
	}

	response := decodeJSONStringField(t, logs[0]["response"])
	if response["body"] != `{"created":true}` {
		t.Fatalf("response body = %#v, want captured response body", response["body"])
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:              "http",
		CLSHost:             strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:            "topic-a",
		SecretID:            "secret-id",
		SecretKey:           "secret-key",
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		Timeout:             1000,
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

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if _, ok := logs[0]["request"]; ok {
		t.Fatalf("request field = %q, want no request body", logs[0]["request"])
	}
	if _, ok := logs[0]["response"]; ok {
		t.Fatalf("response field = %q, want no response body", logs[0]["response"])
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"cls_host":               "cls.example.com",
		"cls_topic":              "topic-a",
		"secret_id":              "secret-id",
		"secret_key":             "secret-key",
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsBatchAndMaxPendingFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"cls_host":            "cls.example.com",
		"cls_topic":           "topic-a",
		"secret_id":           "secret-id",
		"secret_key":          "secret-key",
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

func TestHandlerBatchesCLSLogs(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:       "http",
		CLSHost:      strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:     "topic-a",
		SecretID:     "secret-id",
		SecretKey:    "secret-key",
		Timeout:      1000,
		BatchMaxSize: 2,
		LogFormat: map[string]string{
			"path": "$uri",
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 2 {
		t.Fatalf("logs = %d, want 2", len(logs))
	}
	if logs[0]["path"] != "/first" || logs[1]["path"] != "/second" {
		t.Fatalf("paths = %q, %q; want /first, /second", logs[0]["path"], logs[1]["path"])
	}
}

func decodeJSONStringField(t *testing.T, value string) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatalf("unmarshal JSON string field %q: %v", value, err)
	}
	return out
}

func waitRequest(t *testing.T, requests <-chan *http.Request) *http.Request {
	t.Helper()

	select {
	case req := <-requests:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CLS request")
		return nil
	}
}

func waitBody(t *testing.T, bodies <-chan []byte) []byte {
	t.Helper()

	select {
	case body := <-bodies:
		return body
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CLS body")
		return nil
	}
}

func decodeCLSBody(t *testing.T, body []byte) []map[string]string {
	t.Helper()

	var logs []map[string]string
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			t.Fatalf("consume LogGroupList tag: %v", protowire.ParseError(n))
		}
		body = body[n:]
		if num != 1 || typ != protowire.BytesType {
			t.Fatalf("LogGroupList field = %d/%v, want 1/bytes", num, typ)
		}
		group, n := protowire.ConsumeBytes(body)
		if n < 0 {
			t.Fatalf("consume LogGroup bytes: %v", protowire.ParseError(n))
		}
		body = body[n:]
		logs = append(logs, decodeLogGroup(t, group)...)
	}
	return logs
}

func decodeLogGroup(t *testing.T, group []byte) []map[string]string {
	t.Helper()

	var logs []map[string]string
	for len(group) > 0 {
		num, typ, n := protowire.ConsumeTag(group)
		if n < 0 {
			t.Fatalf("consume LogGroup tag: %v", protowire.ParseError(n))
		}
		group = group[n:]
		if typ != protowire.BytesType {
			t.Fatalf("LogGroup field %d type = %v, want bytes", num, typ)
		}
		value, n := protowire.ConsumeBytes(group)
		if n < 0 {
			t.Fatalf("consume LogGroup value: %v", protowire.ParseError(n))
		}
		group = group[n:]
		if num == 1 {
			logs = append(logs, decodeLog(t, value))
		}
	}
	return logs
}

func decodeLog(t *testing.T, logBody []byte) map[string]string {
	t.Helper()

	out := map[string]string{}
	for len(logBody) > 0 {
		num, typ, n := protowire.ConsumeTag(logBody)
		if n < 0 {
			t.Fatalf("consume Log tag: %v", protowire.ParseError(n))
		}
		logBody = logBody[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				t.Fatalf("Log.time type = %v, want varint", typ)
			}
			_, n := protowire.ConsumeVarint(logBody)
			if n < 0 {
				t.Fatalf("consume Log.time: %v", protowire.ParseError(n))
			}
			logBody = logBody[n:]
		case 2:
			if typ != protowire.BytesType {
				t.Fatalf("Log.contents type = %v, want bytes", typ)
			}
			content, n := protowire.ConsumeBytes(logBody)
			if n < 0 {
				t.Fatalf("consume Log.contents: %v", protowire.ParseError(n))
			}
			logBody = logBody[n:]
			key, value := decodeContent(t, content)
			out[key] = value
		default:
			t.Fatalf("unexpected Log field %d", num)
		}
	}
	return out
}

func decodeContent(t *testing.T, content []byte) (string, string) {
	t.Helper()

	var key, value string
	for len(content) > 0 {
		num, typ, n := protowire.ConsumeTag(content)
		if n < 0 {
			t.Fatalf("consume Content tag: %v", protowire.ParseError(n))
		}
		content = content[n:]
		if typ != protowire.BytesType {
			t.Fatalf("Content field %d type = %v, want bytes", num, typ)
		}
		raw, n := protowire.ConsumeBytes(content)
		if n < 0 {
			t.Fatalf("consume Content value: %v", protowire.ParseError(n))
		}
		content = content[n:]
		switch num {
		case 1:
			key = string(raw)
		case 2:
			value = string(raw)
		default:
			t.Fatalf("unexpected Content field %d", num)
		}
	}
	return key, value
}

func encryptTencentCLSTestValue(t *testing.T, key string, value string) string {
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

func waitCLSEntry(t *testing.T, entries <-chan logger.Entry, substring string) logger.Entry {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case entry := <-entries:
			if strings.Contains(entry.Message, substring) {
				return entry
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("timed out waiting for cls diagnostic containing %q", substring)
	return logger.Entry{}
}

func TestBuildBatchPayloadReportsTruncatedFieldCount(t *testing.T) {
	entries := make(chan logger.Entry, 2)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		entries <- entry
	})
	t.Cleanup(stop)

	p := &Plugin{}
	p.applyDefaults()

	big := strings.Repeat("v", maxSingleValueSize+10)
	payload := p.buildBatchPayload([]map[string]any{{"big": big}})
	if len(payload) == 0 {
		t.Fatal("buildBatchPayload() = nil, want a payload despite truncation")
	}

	entry := waitCLSEntry(t, entries, "truncated")
	if !strings.Contains(entry.Message, "1") {
		t.Fatalf("truncation diagnostic = %q, want the truncated field count", entry.Message)
	}
}

func TestBuildBatchPayloadReportsOverLimitEntryDrops(t *testing.T) {
	entries := make(chan logger.Entry, 2)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		entries <- entry
	})
	t.Cleanup(stop)

	p := &Plugin{}
	p.applyDefaults()

	// Six 1MB values in one entry exceed the 5MB group limit and must be
	// reported as a dropped entry rather than sent.
	huge := map[string]any{}
	for i := range 6 {
		huge["f"+string(rune('a'+i))] = strings.Repeat("v", maxSingleValueSize)
	}
	payload := p.buildBatchPayload([]map[string]any{huge})
	if len(payload) != 0 {
		t.Fatalf("buildBatchPayload() = %d bytes, want empty payload for an over-limit entry", len(payload))
	}

	entry := waitCLSEntry(t, entries, "dropped")
	if !strings.Contains(entry.Message, "1") {
		t.Fatalf("drop diagnostic = %q, want the dropped entry count", entry.Message)
	}
}

func TestBuildBatchPayloadReportsDroppedBatchRemainder(t *testing.T) {
	entries := make(chan logger.Entry, 2)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		entries <- entry
	})
	t.Cleanup(stop)

	p := &Plugin{}
	p.applyDefaults()

	// Six 1MB entries exceed the 5MB group limit; the last two are dropped
	// and the remaining batch is still sent.
	big := strings.Repeat("v", maxSingleValueSize)
	logs := make([]map[string]any, 0, 6)
	for range 6 {
		logs = append(logs, map[string]any{"v": big})
	}
	payload := p.buildBatchPayload(logs)
	if len(payload) == 0 {
		t.Fatal("buildBatchPayload() = nil, want the accepted entries' payload")
	}

	entry := waitCLSEntry(t, entries, "dropped")
	if !strings.Contains(entry.Message, "2") {
		t.Fatalf("drop diagnostic = %q, want the dropped remainder count", entry.Message)
	}
}

func TestAuthorizationSignTimeUsesSingleTimestamp(t *testing.T) {
	p := &Plugin{config: Config{SecretID: "secret-id", SecretKey: "secret-key"}}
	calls := 0
	p.now = func() time.Time {
		calls++
		if calls > 1 {
			return time.Unix(1710000001, 0)
		}
		return time.Unix(1710000000, 0)
	}

	auth := p.authorization()

	start, end, ok := signTimeWindow(auth)
	if !ok {
		t.Fatalf("authorization = %q, want q-sign-time=start;end", auth)
	}
	if end-start != authExpireSeconds {
		t.Fatalf("sign time window = %d seconds, want exactly %d", end-start, authExpireSeconds)
	}
	if calls != 1 {
		t.Fatalf("now() called %d times, want exactly once per signature", calls)
	}
}

func signTimeWindow(auth string) (int64, int64, bool) {
	for part := range strings.SplitSeq(auth, "&") {
		value, ok := strings.CutPrefix(part, "q-sign-time=")
		if !ok {
			continue
		}
		start, end, ok := strings.Cut(value, ";")
		if !ok {
			return 0, 0, false
		}
		startTime, err := strconv.ParseInt(start, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		endTime, err := strconv.ParseInt(end, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		return startTime, endTime, true
	}
	return 0, 0, false
}
