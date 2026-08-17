package file_logger

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

var (
	metadataStoreOnce   sync.Once
	metadataStoreEvents chan *store.Event
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
	t.Cleanup(p.Stop)
	return p
}

func TestHandlerWritesLogWhenMatchPasses(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path: path,
		LogFormat: map[string]any{
			"path":   "$uri",
			"status": "$status",
		},
		Match: []any{
			[]any{"uri", "==", "/orders"},
			[]any{"status", "==", 201},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", 201)
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	content := readLogFile(t, path)
	if !strings.Contains(content, `"path":"/orders"`) {
		t.Fatalf("log content = %q, want matched request path", content)
	}
	if !strings.Contains(content, `"status":201`) {
		t.Fatalf("log content = %q, want matched response status", content)
	}
}

func TestSnapshotDefaultLogFieldsPreservesResolvedUpstream(t *testing.T) {
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			APISIXVars: map[string]any{
				"$balancer_ip":   "127.0.0.1",
				"$balancer_port": "1982",
			},
		},
	}
	fields := snapshotDefaultLogFields(snapshot)
	if fields["upstream"] != "127.0.0.1:1982" {
		t.Fatalf("upstream = %#v, want resolved address", fields["upstream"])
	}
}

func TestDefaultFileLoggerFieldsRedactSensitiveHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil)
	r.Host = "gateway.test:9443"
	r.Header = testFileLoggerHeaders()
	request := captureRequest(r)

	live := defaultLogFields(
		r,
		request,
		testFileLoggerHeaders(),
		httpsnoop.Metrics{Code: http.StatusOK, Written: 1},
		time.Unix(100, 0),
	)
	assertSafeFileLoggerHeaders(t, live)

	detached := snapshotDefaultLogFields(base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodGet,
			URI:    "/orders",
			Host:   "gateway.test:9443",
			Header: testFileLoggerHeaders(),
		},
		Response: apisixlog.ResponseLogSnapshot{Header: testFileLoggerHeaders()},
		Started:  time.Unix(100, 0),
		Finished: time.Unix(101, 0),
	})
	assertSafeFileLoggerHeaders(t, detached)
}

func TestCustomFileLoggerFormatRetainsSensitiveHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/orders", nil)
	r.Header.Set("Authorization", "Bearer explicit")
	r = apisixctx.WithRequestVars(r)
	apisixctx.RegisterRequestVar(r, "$http_authorization", r.Header.Get("Authorization"))
	p := &Plugin{logFormat: map[string]any{"authorization": "$http_authorization"}}

	fields := p.buildLogFields(
		r,
		captureRequest(r),
		nil,
		httpsnoop.Metrics{Code: http.StatusOK},
		time.Unix(100, 0),
	)
	if got := fields["authorization"]; got != "Bearer explicit" {
		t.Fatalf("custom authorization = %#v, want explicit header value", got)
	}
}

func TestAppendFileWriteSyncerFileModes(t *testing.T) {
	t.Run("new file is private", func(t *testing.T) {
		path := t.TempDir() + "/new.log"
		writer := &appendFileWriteSyncer{path: path}
		if _, err := writer.Write([]byte("entry")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat new file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("new file mode = %o, want 600", got)
		}
	})

	t.Run("existing file keeps mode", func(t *testing.T) {
		path := t.TempDir() + "/existing.log"
		if err := os.WriteFile(path, nil, 0o640); err != nil {
			t.Fatalf("create existing file: %v", err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatalf("chmod existing file: %v", err)
		}
		writer := &appendFileWriteSyncer{path: path}
		if _, err := writer.Write([]byte("entry")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat existing file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Fatalf("existing file mode = %o, want 640", got)
		}
	})
}

func TestBufferedWriterDefersWriteUntilSync(t *testing.T) {
	path := t.TempDir() + "/buffered.log"
	lease, err := sharedFileWriters.acquire(path)
	if err != nil {
		t.Fatalf("acquire() error = %v", err)
	}
	t.Cleanup(lease.release)

	if _, err := lease.writer.Write([]byte("buffered entry")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if content := readLogFile(t, path); content != "" {
		t.Fatalf("log content before Sync() = %q, want empty", content)
	}

	if err := lease.writer.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if content := readLogFile(t, path); content != "buffered entry" {
		t.Fatalf("log content after Sync() = %q, want buffered entry", content)
	}
}

func TestBufferedWriterRecoversAfterTransientSyncFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "access.log")
	lease, err := sharedFileWriters.acquire(path)
	if err != nil {
		t.Fatalf("acquire() error = %v", err)
	}
	t.Cleanup(lease.release)

	if _, err := lease.writer.Write([]byte("lost entry")); err != nil {
		t.Fatalf("Write(lost entry) error = %v", err)
	}
	// Use the zap buffer directly to model its background flush loop: that
	// failure is not returned through the wrapper and leaves bufio's error
	// sticky until the next Write observes it.
	if err := lease.writer.buffer.Sync(); err == nil {
		t.Fatal("background Sync() error = nil, want missing parent error")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if _, err := lease.writer.Write([]byte("recovered entry")); err != nil {
		t.Fatalf("Write(recovered entry) error = %v", err)
	}
	if err := lease.writer.Sync(); err != nil {
		t.Fatalf("Sync(recovered entry) error = %v", err)
	}
	if content := readLogFile(t, path); content != "recovered entry" {
		t.Fatalf("recovered log content = %q, want recovered entry", content)
	}
}

var testSensitiveFileLoggerHeaders = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"set-cookie",
	"x-api-key",
	"x-functions-key",
	"x-amz-security-token",
	"x-goog-api-key",
}

func testFileLoggerHeaders() http.Header {
	return http.Header{
		"aUtHoRiZaTiOn":        {"secret-authorization"},
		"pRoXy-AuThOrIzAtIoN":  {"secret-proxy-authorization"},
		"cOoKiE":               {"secret-cookie"},
		"sEt-CoOkIe":           {"secret-set-cookie"},
		"x-aPi-kEy":            {"secret-api-key"},
		"x-fUnCtIoNs-kEy":      {"secret-functions-key"},
		"x-aMz-SeCuRiTy-ToKeN": {"secret-amz-token"},
		"x-GoOg-aPi-KeY":       {"secret-goog-key"},
		"Host":                 {"gateway.test"},
		"X-Visible":            {"first", "second"},
	}
}

func assertSafeFileLoggerHeaders(t *testing.T, fields map[string]any) {
	t.Helper()
	for _, section := range []string{"request", "response"} {
		payload, ok := fields[section].(map[string]any)
		if !ok {
			t.Fatalf("%s payload = %#v, want object", section, fields[section])
		}
		headers, ok := payload["headers"].(map[string]any)
		if !ok {
			t.Fatalf("%s headers = %#v, want object", section, payload["headers"])
		}
		for _, name := range testSensitiveFileLoggerHeaders {
			if _, ok := headers[name]; ok {
				t.Fatalf("%s sensitive header %q = %#v, want omitted", section, name, headers[name])
			}
		}
		if got := headers["x-visible"]; got == nil {
			t.Fatalf("%s headers = %#v, want benign header", section, headers)
		}
	}
	request := fields["request"].(map[string]any)
	if got := request["headers"].(map[string]any)["host"]; got == nil {
		t.Fatalf("request headers = %#v, want host", request["headers"])
	}
}

func TestLogCapturePolicyIncludesCustomFormatBodies(t *testing.T) {
	p := &Plugin{
		config:         Config{MaxReqBodyBytes: 17, MaxRespBodyBytes: 23},
		logFormat:      map[string]any{"request": map[string]any{"body": "$request_body"}},
		logFormatExtra: map[string]string{"response": "$response_body"},
	}
	policy := p.LogCapturePolicy()
	if policy.RequestBodyBytes != 17 || policy.ResponseBodyBytes != 23 {
		t.Fatalf("policy = %#v, want request=17 response=23", policy)
	}
}

func TestFileSnapshotValueHonorsBodyExpressionAndDecodedLimit(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte("private-response-body"))
	_ = writer.Close()
	snapshot := base.LogSnapshot{Response: apisixlog.ResponseLogSnapshot{
		Header: http.Header{"Content-Encoding": {"gzip"}}, Body: compressed.Bytes(),
	}}
	if got := fileSnapshotValue(snapshot, "$resp_body", false, false, 8); got != "" {
		t.Fatalf("hidden response body = %#v, want empty", got)
	}
	if got := fileSnapshotValue(snapshot, "$resp_body", false, true, 8); got != "private-" {
		t.Fatalf("decoded response body = %#v, want bounded decoded prefix", got)
	}
}

func TestHandlerWritesToCurrentPathAfterExternalRotation(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/access.log"
	rotated := dir + "/2026-07-09_12-00-00__access.log"
	p := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]any{"path": "$uri"},
	})

	serveFileLoggerRequest(t, p, "/before")
	_ = p.logger.Sync()

	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rename log file: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("recreate current log file: %v", err)
	}

	serveFileLoggerRequest(t, p, "/after")
	_ = p.logger.Sync()

	currentContent := readLogFile(t, path)
	if currentContent != "" {
		t.Fatalf("current log content = %q, want cached writer to stay on rotated file", currentContent)
	}

	rotatedContent := readLogFile(t, rotated)
	if !strings.Contains(rotatedContent, `"path":"/before"`) {
		t.Fatalf("rotated log content = %q, want pre-rotation log line", rotatedContent)
	}
	if !strings.Contains(rotatedContent, `"path":"/after"`) {
		t.Fatalf("rotated log content = %q, want cached post-rotation write", rotatedContent)
	}

	if err := FlushAndReopen(path); err != nil {
		t.Fatalf("FlushAndReopen() error = %v", err)
	}
	serveFileLoggerRequest(t, p, "/reopened")
	currentContent = readLogFile(t, path)
	if !strings.Contains(currentContent, `"path":"/reopened"`) {
		t.Fatalf("current log content = %q, want post-reopen write", currentContent)
	}
}

func TestHandlerKeepsUnlinkedFileCachedUntilReopen(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]any{"path": "$uri"},
	})

	serveFileLoggerRequest(t, p, "/before")
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove current log: %v", err)
	}

	serveFileLoggerRequest(t, p, "/cached")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cached log path stat error = %v, want absent", err)
	}

	if err := FlushAndReopen(path); err != nil {
		t.Fatalf("FlushAndReopen() error = %v", err)
	}
	serveFileLoggerRequest(t, p, "/after")
	content := readLogFile(t, path)
	if !strings.Contains(content, `"path":"/after"`) {
		t.Fatalf("reopened log content = %q, want post-reopen entry", content)
	}
	if strings.Contains(content, "/before") || strings.Contains(content, "/cached") {
		t.Fatalf("reopened log content = %q, want only post-reopen entry", content)
	}
}

func TestHandlerResolvesNestedLogFormatAndTruncatesAfterDepthFive(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path: path,
		LogFormat: map[string]any{
			"request": map[string]any{
				"method": "$request_method",
				"headers": map[string]any{
					"user_agent": "$http_user_agent",
				},
			},
			"a": map[string]any{
				"b": map[string]any{
					"c": map[string]any{
						"d": map[string]any{
							"e": map[string]any{
								"f": "$host",
							},
						},
					},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/nested", nil)
	req.Header.Set("User-Agent", "pinned-agent")
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	request := logged["request"].(map[string]any)
	if request["method"] != http.MethodGet {
		t.Fatalf("request.method = %#v, want GET", request["method"])
	}
	headers := request["headers"].(map[string]any)
	if headers["user_agent"] != "pinned-agent" {
		t.Fatalf("request.headers.user_agent = %#v, want pinned-agent", headers["user_agent"])
	}
	e := logged["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)["d"].(map[string]any)["e"].(map[string]any)
	if _, exists := e["f"]; exists {
		t.Fatalf("depth-five object = %#v, want f truncated", e)
	}
}

func TestHandlerUsesRichDefaultAndRouteLogFormatExtra(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path: path,
		LogFormatExtra: map[string]string{
			"route_field":   "from route",
			"upstream_host": "$upstream_unresolved_host",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/default?foo=bar", nil)
	req = apisixctx.WithRequestVars(req)
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":      "route-1",
		"$balancer_ip":   "localhost",
		"$balancer_port": "1982",
	})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})).ServeHTTP(rr, req)

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	request := logged["request"].(map[string]any)
	if request["method"] != http.MethodGet {
		t.Fatalf("request.method = %#v, want GET", request["method"])
	}
	response := logged["response"].(map[string]any)
	if response["status"] != float64(http.StatusCreated) {
		t.Fatalf("response.status = %#v, want 201", response["status"])
	}
	if logged["route_id"] != "route-1" {
		t.Fatalf("route_id = %#v, want route-1", logged["route_id"])
	}
	if logged["route_field"] != "from route" {
		t.Fatalf("route_field = %#v, want route value", logged["route_field"])
	}
	if logged["upstream_host"] != "localhost" {
		t.Fatalf("upstream_host = %#v, want unresolved host", logged["upstream_host"])
	}
	if logged["upstream"] != "127.0.0.1:1982" {
		t.Fatalf("upstream = %#v, want resolved address", logged["upstream"])
	}
}

func TestHandlerLogFormatReplacesDefaultAndIgnoresExtra(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:           path,
		LogFormat:      map[string]any{"msg": "precedence test"},
		LogFormatExtra: map[string]string{"ignored": "extra"},
	})

	req := httptest.NewRequest(http.MethodGet, "/precedence", nil)
	req = apisixctx.WithRequestVars(req)
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":   "route-1",
		"$service_id": "service-1",
	})
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	if logged["msg"] != "precedence test" {
		t.Fatalf("msg = %#v, want precedence test", logged["msg"])
	}
	if logged["route_id"] != "route-1" {
		t.Fatalf("route_id = %#v, want route-1", logged["route_id"])
	}
	if logged["service_id"] != "service-1" {
		t.Fatalf("service_id = %#v, want service-1", logged["service_id"])
	}
	for _, field := range []string{"request", "response", "ignored"} {
		if _, exists := logged[field]; exists {
			t.Fatalf("field %q exists in %#v, want log_format replacement", field, logged)
		}
	}
}

func TestAppendFileWriteSyncerDoesNotCreateMissingParent(t *testing.T) {
	writer := &appendFileWriteSyncer{path: t.TempDir() + "/missing/access.log"}
	if _, err := writer.Write([]byte("entry")); err == nil {
		t.Fatal("Write() error = nil, want missing parent error")
	}
}

func TestPluginsWithSamePathShareWriterLease(t *testing.T) {
	path := t.TempDir() + "/shared.log"
	first := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]any{"path": "$uri"},
	})
	second := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]any{"path": "$uri"},
	})
	if first.writer != second.writer {
		t.Fatal("same-path plugins use different writers")
	}

	serveFileLoggerRequest(t, first, "/first")
	first.Stop()
	serveFileLoggerRequest(t, second, "/second")
	content := readLogFile(t, path)
	if !strings.Contains(content, `"path":"/first"`) || !strings.Contains(content, `"path":"/second"`) {
		t.Fatalf("shared log content = %q, want both entries", content)
	}
}

func TestFlushAndReopenMovesWritesToCurrentPath(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/access.log"
	rotated := dir + "/access.log.old"
	p := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]any{"path": "$uri"},
	})

	serveFileLoggerRequest(t, p, "/before")
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rename current log: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("recreate current log: %v", err)
	}
	if err := FlushAndReopen(path); err != nil {
		t.Fatalf("FlushAndReopen() error = %v", err)
	}
	serveFileLoggerRequest(t, p, "/after")

	if content := readLogFile(t, rotated); !strings.Contains(content, `"path":"/before"`) ||
		strings.Contains(content, `"path":"/after"`) {
		t.Fatalf("rotated log content = %q, want only pre-reopen entry", content)
	}
	if content := readLogFile(t, path); !strings.Contains(content, `"path":"/after"`) {
		t.Fatalf("current log content = %q, want post-reopen entry", content)
	}
}

func TestFlushAndReopenFlushesBufferedBytesBeforeReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/access.log"
	rotated := dir + "/access.log.old"
	p := newTestPlugin(t, Config{Path: path})

	if _, err := p.sendBatch(context.Background(), []map[string]any{{"path": "/before"}}, 1); err != nil {
		t.Fatalf("sendBatch(before) error = %v", err)
	}
	if err := p.logger.Sync(); err != nil {
		t.Fatalf("Sync(before) error = %v", err)
	}
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rename current log: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("recreate current log: %v", err)
	}

	if _, err := p.sendBatch(context.Background(), []map[string]any{{"path": "/buffered"}}, 1); err != nil {
		t.Fatalf("sendBatch(buffered) error = %v", err)
	}
	if err := FlushAndReopen(path); err != nil {
		t.Fatalf("FlushAndReopen() error = %v", err)
	}

	rotatedContent := readLogFile(t, rotated)
	if !strings.Contains(rotatedContent, `"path":"/before"`) ||
		!strings.Contains(rotatedContent, `"path":"/buffered"`) {
		t.Fatalf("rotated log content = %q, want pre-reopen entries", rotatedContent)
	}
	if content := readLogFile(t, path); content != "" {
		t.Fatalf("current log content after reopen = %q, want empty", content)
	}

	if _, err := p.sendBatch(context.Background(), []map[string]any{{"path": "/after"}}, 1); err != nil {
		t.Fatalf("sendBatch(after) error = %v", err)
	}
	if err := p.logger.Sync(); err != nil {
		t.Fatalf("Sync(after) error = %v", err)
	}
	if content := readLogFile(t, path); !strings.Contains(content, `"path":"/after"`) {
		t.Fatalf("current log content = %q, want post-reopen entry", content)
	}
}

func TestFlushAndReopenAcceptsRegisteredMissingPath(t *testing.T) {
	path := t.TempDir() + "/missing.log"
	p := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]any{"path": "$uri"},
	})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("initial path stat error = %v, want absent", err)
	}
	if err := FlushAndReopen(path); err != nil {
		t.Fatalf("FlushAndReopen() missing path error = %v", err)
	}
	serveFileLoggerRequest(t, p, "/created-after-reopen")
	if content := readLogFile(t, path); !strings.Contains(content, "/created-after-reopen") {
		t.Fatalf("created log content = %q, want request after reopen", content)
	}
}

func TestFinalLeaseReleaseRemovesRegisteredWriter(t *testing.T) {
	path := t.TempDir() + "/released.log"
	p := newTestPlugin(t, Config{Path: path})
	key, err := canonicalWriterPath(path)
	if err != nil {
		t.Fatalf("canonicalWriterPath() error = %v", err)
	}
	if !sharedFileWriters.has(key) {
		t.Fatal("writer is not registered after PostInit")
	}

	p.Stop()
	if sharedFileWriters.has(key) {
		t.Fatal("writer remains registered after final lease release")
	}
	if sharedFileWriters.signalWatcherRunning() {
		t.Fatal("SIGUSR1 watcher remains after final lease release")
	}
}

func TestFinalLeaseReleaseFlushesBufferedBytes(t *testing.T) {
	path := t.TempDir() + "/released.log"
	lease, err := sharedFileWriters.acquire(path)
	if err != nil {
		t.Fatalf("acquire() error = %v", err)
	}
	writer := lease.writer
	key, err := canonicalWriterPath(path)
	if err != nil {
		t.Fatalf("canonicalWriterPath() error = %v", err)
	}
	if _, err := writer.Write([]byte("released entry")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	lease.release()

	content := readLogFile(t, path)
	if content != "released entry" {
		t.Fatalf("released log content = %q, want released entry", content)
	}
	if sharedFileWriters.has(key) {
		t.Fatal("writer remains registered after final lease release")
	}
	_, err = writer.Write([]byte("late entry"))
	if !errors.Is(err, errFileLoggerWriterStopped) {
		t.Fatalf("Write() after release error = %v, want %v", err, errFileLoggerWriterStopped)
	}
}

func TestHandlerSkipsLogWhenMatchFails(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]any{"path": "$uri"},
		Match:     []any{[]any{"http_x_tenant", "==", "gold"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("X-Tenant", "silver")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", 200)
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	if content := readLogFile(t, path); content != "" {
		t.Fatalf("log content = %q, want no log line for non-matching request", content)
	}
}

func TestHandlerAcceptsOfficialNestedMatchGroup(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]any{"request": "$request"},
		Match: []any{
			[]any{
				[]any{"arg_name", "==", "jack"},
			},
		},
	})

	matched := httptest.NewRequest(http.MethodGet, "/orders?name=jack", nil)
	matched = apisixctx.WithRequestVars(matched)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), matched)

	content := readLogFile(t, path)
	if !strings.Contains(content, "name=jack") {
		t.Fatalf("log content = %q, want nested match request", content)
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:             path,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(`{"order":1}`))
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	if rr.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if body := rr.Body.String(); body != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream response body", body)
	}

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}

	request, ok := logged["request"].(map[string]any)
	if !ok {
		t.Fatalf("logged request = %#v, want object", logged["request"])
	}
	if request["body"] != `{"order":1}` {
		t.Fatalf("logged request body = %#v, want original request body", request["body"])
	}

	response, ok := logged["response"].(map[string]any)
	if !ok {
		t.Fatalf("logged response = %#v, want object", logged["response"])
	}
	if response["body"] != `{"ok":true}` {
		t.Fatalf("logged response body = %#v, want upstream response body", response["body"])
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:                path,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(`{"order":2}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}

	request, ok := logged["request"].(map[string]any)
	if !ok {
		t.Fatalf("logged request = %#v, want object", logged["request"])
	}
	if request["body"] != `{"order":2}` {
		t.Fatalf("logged request body = %#v, want captured request body", request["body"])
	}

	response, ok := logged["response"].(map[string]any)
	if !ok {
		t.Fatalf("logged response = %#v, want object", logged["response"])
	}
	if response["body"] != `{"created":true}` {
		t.Fatalf("logged response body = %#v, want captured response body", response["body"])
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:                path,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(`{"order":3}`))
	req = apisixctx.WithRequestVars(req)
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

		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	request, ok := logged["request"].(map[string]any)
	if !ok {
		t.Fatalf("logged request = %#v, want rich request object", logged["request"])
	}
	if _, ok := request["body"]; ok {
		t.Fatalf("logged request = %#v, want no request body", request)
	}
	response, ok := logged["response"].(map[string]any)
	if !ok {
		t.Fatalf("logged response = %#v, want rich response object", logged["response"])
	}
	if _, ok := response["body"]; ok {
		t.Fatalf("logged response = %#v, want no response body", response)
	}
}

func TestHandlerDoesNotExposeResponseBodyVariableWhenExpressionDoesNotMatch(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:                path,
		LogFormat:           map[string]any{"resp_body": "$resp_body"},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "500"}},
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", http.StatusOK)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`private-response`))
	})).ServeHTTP(rr, req)

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	if logged["resp_body"] != nil {
		t.Fatalf("resp_body = %#v, want null for non-matching expression", logged["resp_body"])
	}
	if strings.Contains(line, "private-response") {
		t.Fatalf("log line = %q, want response body kept private", line)
	}
}

func TestSchemaAcceptsMatchConditionsAndLogicalOperators(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"path": "/tmp/apisix-go-file-logger.log",
		"match": []any{
			[]any{"uri", "==", "/orders"},
			"AND",
			[]any{"status", ">=", 200},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected match config: %v", err)
	}
}

func TestSchemaAcceptsPathFromMetadata(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"log_format": map[string]any{"path": "$uri"},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected config without route path: %v", err)
	}
}

func TestPostInitUsesMetadataPath(t *testing.T) {
	path := t.TempDir() + "/metadata-access.log"
	putPluginMetadata(t, map[string]any{
		"path":       path,
		"log_format": map[string]any{"path": "$uri"},
	})
	t.Cleanup(func() {
		deletePluginMetadata(t)
	})

	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "/metadata", nil)
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", http.StatusOK)
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	content := readLogFile(t, path)
	if !strings.Contains(content, `"path":"/metadata"`) {
		t.Fatalf("log content = %q, want metadata path and log_format", content)
	}
}

func TestPostInitRejectsMissingPath(t *testing.T) {
	deletePluginMetadata(t)

	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want missing path error")
	}
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return string(content)
}

func serveFileLoggerRequest(t *testing.T, p *Plugin, path string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", http.StatusOK)
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)
}

func putPluginMetadata(t *testing.T, value map[string]any) {
	t.Helper()

	events := initMetadataStore(t)

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	events <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/plugin_metadata/file-logger"),
		Value: data,
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var metadata pluginMetadata
		if err := store.GetPluginMetadata(name, &metadata); err == nil && metadata.Path == value["path"] {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for file-logger plugin metadata")
}

func deletePluginMetadata(t *testing.T) {
	t.Helper()

	events := initMetadataStore(t)
	events <- &store.Event{
		Type: store.EventTypeDelete,
		Key:  []byte("/apisix/plugin_metadata/file-logger"),
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var metadata pluginMetadata
		if err := store.GetPluginMetadata(name, &metadata); err != nil || metadata.Path == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out deleting file-logger plugin metadata")
}

func initMetadataStore(t *testing.T) chan *store.Event {
	t.Helper()

	metadataStoreOnce.Do(func() {
		metadataStoreEvents = make(chan *store.Event, 16)
		st, err := store.GetStore(t.TempDir()+"/store.db", metadataStoreEvents)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		st.Start()
	})
	return metadataStoreEvents
}
