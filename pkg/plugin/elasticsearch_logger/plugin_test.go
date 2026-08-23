package elasticsearch_logger

import (
	"bytes"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v8/esapi"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestNewElasticsearchClientUsesOfficialBulkTransport(t *testing.T) {
	const (
		username = "elastic"
		password = "secret"
	)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/_bulk" {
			t.Errorf("path = %q, want /_bulk", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-ndjson" {
			t.Errorf("Content-Type = %q, want application/x-ndjson", got)
		}
		if got := r.Header.Get("X-Cluster"); got != "logs" {
			t.Errorf("X-Cluster = %q, want logs", got)
		}
		gotUsername, gotPassword, ok := r.BasicAuth()
		if !ok || gotUsername != username || gotPassword != password {
			t.Errorf("BasicAuth() = %q/%q/%v, want %q/%q/true", gotUsername, gotPassword, ok, username, password)
		}
		gotTimeout := r.URL.Query().Get("timeout")
		parsedTimeout, err := time.ParseDuration(gotTimeout)
		if err != nil || parsedTimeout != 10*time.Second {
			t.Errorf("timeout = %q, want 10s", gotTimeout)
		}

		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	client, err := newElasticsearchClient(
		server.URL, username, password,
		map[string]string{"X-Cluster": "logs"},
		10*time.Second,
		true,
	)
	if err != nil {
		t.Fatalf("newElasticsearchClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	resp, err := (esapi.BulkRequest{
		Body:    strings.NewReader("{}\n"),
		Header:  http.Header{"Content-Type": []string{"application/x-ndjson"}},
		Timeout: 10 * time.Second,
	}).Do(context.Background(), client)
	if err != nil {
		t.Fatalf("BulkRequest.Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.IsError() {
		t.Fatalf("BulkRequest.Do() status = %s, want success", resp.Status())
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("bulk attempts = %d, want retry after one 503", got)
	}
}

func TestRunLogPhasePreservesIndexAndDetachedHostFields(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{
		config: Config{Field: FieldConfig{Index: "apisix-$route_id"}},
		BaseLoggerPlugin: base.BaseLoggerPlugin{LogFormat: map[string]string{
			"host": "$host", "remote": "$remote_addr",
		}},
	}
	p.BatchProcessor = logger_batch.NewWithContext(logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	t.Cleanup(p.Stop)
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		Host: "gateway.example", RemoteAddr: "192.0.2.10:8443",
		APISIXVars: map[string]any{"$route_id": "r-17"},
	}}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case entry := <-delivered:
		if entry[elasticsearchIndexField] != "apisix-r-17" {
			t.Fatalf("index = %#v, want resolved route index", entry[elasticsearchIndexField])
		}
		if entry["host"] != "gateway.example" || entry["remote"] != "192.0.2.10" {
			t.Fatalf("detached host fields = %#v/%#v", entry["host"], entry["remote"])
		}
	case <-time.After(time.Second):
		t.Fatal("detached Elasticsearch entry was not delivered")
	}
}

func TestSendBatchCancelsElasticsearchBulkWithContext(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
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
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
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
		t.Fatal("timed out waiting for Elasticsearch bulk request")
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
	if len(cfg.LogFormat) == 0 {
		cfg.LogFormat = map[string]string{"request_id": "$request_id"}
	}

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{DataEncryption: data_encryption.NewService(false, nil).Resolver()})
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

func TestEffectiveLogFormatRouteWins(t *testing.T) {
	route := map[string]string{"route": "$request_id"}
	metadata := map[string]string{"metadata": "$route_id"}
	putPluginMetadata(t, metadata)

	p := newRawTestPlugin(t, Config{
		Field:     FieldConfig{Index: "apisix"},
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

	p := newRawTestPlugin(t, Config{Field: FieldConfig{Index: "apisix"}})
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
		EndpointAddrs: []string{"http://127.0.0.1:9200"},
		Field:         FieldConfig{Index: "apisix"},
	}}
	p.SetDependencies(base.Dependencies{DataEncryption: data_encryption.NewService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "log_format") {
		t.Fatalf("PostInit() error = %v, want %s log_format rejection", err, name)
	}
	if p.BatchProcessor != nil || len(p.clients) != 0 {
		t.Fatalf("PostInit() side effects = batch=%v clients=%d, want none", p.BatchProcessor, len(p.clients))
	}
}

func newRawTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{DataEncryption: data_encryption.NewService(false, nil).Resolver()})
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
	storage, err := store.Open(t.TempDir()+"/store.db", events, data_encryption.NewService(false, nil))
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

func TestPostInitDefaultsWithoutMetadataStore(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:9200"},
		Field:         FieldConfig{Index: "apisix"},
	})

	if p.config.Timeout != 10 {
		t.Fatalf("timeout = %d, want official default 10 seconds", p.config.Timeout)
	}
	if p.config.SslVerify == nil || !*p.config.SslVerify {
		t.Fatalf("ssl_verify = %v, want true", p.config.SslVerify)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestPostInitRejectsInvalidEncryptedAuthPassword(t *testing.T) {
	p := &Plugin{config: Config{
		EndpointAddrs: []string{"http://127.0.0.1:9200"},
		Field:         FieldConfig{Index: "apisix"},
		Auth:          &AuthConfig{Username: "elastic", Password: "not-a-ciphertext"},
	}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: data_encryption.NewService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted auth.password rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedAuthPassword(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	p := &Plugin{config: Config{
		EndpointAddrs: []string{"http://127.0.0.1:9200"},
		Field:         FieldConfig{Index: "apisix"},
		LogFormat:     map[string]string{"request_id": "$request_id"},
		Auth: &AuthConfig{
			Username: "elastic",
			Password: encryptElasticsearchTestValue(t, oldKey, "elasticsearch-secret"),
		},
	}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: data_encryption.NewService(true, []string{newKey, oldKey}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.Auth.Password != "elasticsearch-secret" {
		t.Fatalf("auth.password = %q, want resolved plaintext", p.config.Auth.Password)
	}
}

func TestSendWritesBulkNDJSONWithHeadersAndAuth(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/x-ndjson" {
			t.Fatalf("Content-Type = %q, want application/x-ndjson", contentType)
		}
		if r.Header.Get("X-Cluster") != "logs" {
			t.Fatalf("X-Cluster = %q, want logs", r.Header.Get("X-Cluster"))
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("elastic:secret"))
		if r.Header.Get("Authorization") != wantAuth {
			t.Fatalf("Authorization = %q, want %q", r.Header.Get("Authorization"), wantAuth)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Auth:          &AuthConfig{Username: "elastic", Password: "secret"},
		Headers:       map[string]string{"X-Cluster": "logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if !strings.Contains(body, `{"index":{"_index":"apisix-logs"}}`+"\n") {
			t.Fatalf("bulk body = %q, want index action", body)
		}
		if !strings.Contains(body, `"path":"/orders"`) {
			t.Fatalf("bulk body = %q, want log document", body)
		}
		if !strings.HasSuffix(body, "\n") {
			t.Fatalf("bulk body = %q, want trailing newline", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestSendBatchDetectsBulkItemFailures(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			`{"errors":true,"items":[` +
				`{"index":{"_index":"apisix-logs","status":201}},` +
				`{"index":{"_index":"apisix-logs","status":429,"error":{"type":"too_many_requests"}}},` +
				`{"index":{"_index":"apisix-logs","status":201}}]}`,
		))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		BatchMaxSize:  3,
		Timeout:       10,
	})

	firstFail, err := p.SendBatch(
		context.Background(),
		[]map[string]any{{"path": "/a"}, {"path": "/b"}, {"path": "/c"}},
		3,
	)
	if err == nil {
		t.Fatal("SendBatch() error = nil, want detected bulk item failure")
	}
	if firstFail != 2 {
		t.Fatalf("firstFail = %d, want 2 (second bulk item)", firstFail)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestSendBatchMalformedBulkResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		BatchMaxSize:  1,
		Timeout:       10,
	})

	firstFail, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}}, 1)
	if err == nil {
		t.Fatal("SendBatch() error = nil, want malformed bulk response error")
	}
	if firstFail != 1 {
		t.Fatalf("firstFail = %d, want 1 for an undecodable bulk result", firstFail)
	}
}

func TestSendBatchWritesMultipleBulkEntries(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		BatchMaxSize:  2,
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}, {"path": "/b"}}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-received:
		lines := strings.Split(strings.TrimSpace(body), "\n")
		if len(lines) != 4 {
			t.Fatalf("bulk lines = %d, want 4, body = %q", len(lines), body)
		}
		if !strings.Contains(body, `"path":"/a"`) || !strings.Contains(body, `"path":"/b"`) {
			t.Fatalf("bulk body = %q, want both documents", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch batch bulk request")
	}
}

func TestSendSelectsRandomEndpointAddr(t *testing.T) {
	firstRequests := make(chan struct{}, 1)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequests <- struct{}{}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(first.Close)

	secondRequests := make(chan struct{}, 1)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
		}
		secondRequests <- struct{}{}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(second.Close)

	oldRandomEndpointIndex := randomEndpointIndex
	randomEndpointIndex = func(n int) int {
		if n != 2 {
			t.Fatalf("random endpoint count = %d, want 2", n)
		}
		return 1
	}
	t.Cleanup(func() {
		randomEndpointIndex = oldRandomEndpointIndex
	})

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{first.URL, second.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case <-secondRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for selected Elasticsearch endpoint")
	}

	select {
	case <-firstRequests:
		t.Fatal("first Elasticsearch endpoint received request, want selected second endpoint only")
	default:
	}
}

func TestSendDiscoversOlderElasticsearchVersion(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"6.8.23"}}`))
		case "/_bulk":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read bulk body: %v", err)
			}
			received <- string(body)
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errors":false}`))
		default:
			t.Fatalf("path = %q, want / or /_bulk", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if !strings.Contains(body, `"_type":"_doc"`) {
			t.Fatalf("bulk body = %q, want _type _doc for Elasticsearch 6", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestBulkBodyIgnoresUnsupportedConfiguredType(t *testing.T) {
	configuredType := "collector"
	for _, test := range []struct {
		version  string
		wantType string
	}{
		{version: "9"},
		{version: "8"},
		{version: "7"},
		{version: "6", wantType: "_doc"},
	} {
		t.Run(test.version, func(t *testing.T) {
			p := &Plugin{
				config: Config{
					Field: FieldConfig{Index: "services", Type: &configuredType},
				},
				esVersion: test.version,
			}

			body, err := p.bulkBodyEntry(map[string]any{"test": "test"})
			if err != nil {
				t.Fatalf("bulkBodyEntry() error = %v", err)
			}
			action := strings.SplitN(string(body), "\n", 2)[0]
			if strings.Contains(action, configuredType) {
				t.Fatalf("bulk action = %s, want unsupported configured type omitted", action)
			}
			if test.wantType == "" {
				if strings.Contains(action, `"_type"`) {
					t.Fatalf("bulk action = %s, want no _type for Elasticsearch %s", action, test.version)
				}
				return
			}
			if !strings.Contains(action, `"_type":"`+test.wantType+`"`) {
				t.Fatalf("bulk action = %s, want _type %q", action, test.wantType)
			}
		})
	}
}

func TestElasticsearchLogFieldsPreservesNginxHostAndRemoteAddress(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://unused.example/orders", nil)
	req.Host = "logs.example"
	req.RemoteAddr = "127.0.0.1:54321"

	fields := elasticsearchLogFields(req, map[string]string{
		"custom_host":      "$host",
		"custom_client_ip": "$remote_addr",
	})
	if fields["custom_host"] != "logs.example" {
		t.Fatalf("custom_host = %#v, want logs.example", fields["custom_host"])
	}
	if fields["custom_client_ip"] != "127.0.0.1" {
		t.Fatalf("custom_client_ip = %#v, want 127.0.0.1", fields["custom_client_ip"])
	}
}

func TestHandlerResolvesIndexTimeAndApisixVariables(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-$route_id-{%Y}"},
		LogFormat:     map[string]string{"path": "$uri"},
		Timeout:       10,
		BatchMaxSize:  1,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{"$route_id": "route-1"})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		wantIndex := `"apisix-route-1-` + time.Now().Format("2006") + `"`
		if !strings.Contains(body, `"_index":`+wantIndex) {
			t.Fatalf("bulk body = %q, want resolved index containing %s", body, wantIndex)
		}
		if strings.Contains(body, elasticsearchIndexField) {
			t.Fatalf("bulk body = %q, want internal index field omitted from document", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:    []string{server.URL},
		Field:            FieldConfig{Index: "apisix-logs"},
		Timeout:          10,
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
	case body := <-received:
		document := extractBulkDocument(t, body)
		request, ok := document["request"].(map[string]any)
		if !ok {
			t.Fatalf("document request = %#v, want object", document["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("document request body = %#v, want original request body", request["body"])
		}

		response, ok := document["response"].(map[string]any)
		if !ok {
			t.Fatalf("document response = %#v, want object", document["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("document response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Field:               FieldConfig{Index: "apisix-logs"},
		Timeout:             10,
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
	case body := <-received:
		document := extractBulkDocument(t, body)
		request, ok := document["request"].(map[string]any)
		if !ok {
			t.Fatalf("document request = %#v, want object", document["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("document request body = %#v, want captured request body", request["body"])
		}

		response, ok := document["response"].(map[string]any)
		if !ok {
			t.Fatalf("document response = %#v, want object", document["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("document response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Field:               FieldConfig{Index: "apisix-logs"},
		Timeout:             10,
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
	case body := <-received:
		document := extractBulkDocument(t, body)
		if _, ok := document["request"]; ok {
			t.Fatalf("document request = %#v, want no request body", document["request"])
		}
		if _, ok := document["response"]; ok {
			t.Fatalf("document response = %#v, want no response body", document["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":          "http://127.0.0.1:9200",
		"field":                  map[string]any{"index": "apisix"},
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsEndpointAddrHeadersAndBodyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":       "http://127.0.0.1:9200",
		"field":               map[string]any{"index": "apisix"},
		"headers":             map[string]any{"X-Cluster": "logs"},
		"batch_max_size":      2,
		"max_pending_entries": 100,
		"include_req_body":    true,
		"include_resp_body":   true,
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected config fields: %v", err)
	}
}

func encryptElasticsearchTestValue(t *testing.T, key string, value string) string {
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

func extractBulkDocument(t *testing.T, body string) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("bulk body = %q, want action and document lines", body)
	}

	var document map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &document); err != nil {
		t.Fatalf("unmarshal bulk document: %v", err)
	}
	return document
}

func TestHandlerResolvesBraceFormApisixVariableInIndex(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "services-${arg_id}-{%Y.%m.%d}"},
		LogFormat:     map[string]string{"path": "$uri"},
		Timeout:       10,
		BatchMaxSize:  1,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/hello?id=myservice", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		wantIndex := `"services-myservice-` + time.Now().Format("2006.01.02") + `"`
		if !strings.Contains(body, `"_index":`+wantIndex) {
			t.Fatalf("bulk body = %q, want resolved brace-form index containing %s", body, wantIndex)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestResolveIndexVariableReferencesMatchesAPISIXTemplateContract(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/orders?id=", nil)
	req.Host = "logs.example"

	got := resolveIndexVariableReferences(
		`plain-$host-${ arg_id }-${arg_missing ?? fallback}-${arg_id ?? fallback}-${consumer_name ?? anonymous}-$foo.bar-\$host-${}`,
		req,
	)
	const want = `plain-logs.example--fallback--anonymous--\$host-${}`
	if got != want {
		t.Fatalf("resolved index = %q, want %q", got, want)
	}
}

func TestVersionDetectionRunsOncePerStableConfig(t *testing.T) {
	var versionGets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if versionGets.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/a"})
	p.Send(map[string]any{"path": "/b"})

	if got := versionGets.Load(); got != 1 {
		t.Fatalf("version detection requests = %d, want 1 per stable config", got)
	}
}
