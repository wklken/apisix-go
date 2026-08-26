package file_logger

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

var _ interface{ QuiesceGenerationTasks() } = (*Plugin)(nil)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Tasks: newFileLoggerTestTaskOwner(t)})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, metadata map[string]any) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		Tasks:    newFileLoggerTestTaskOwner(t),
		Metadata: mustMetadataView(t, metadata),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func newFileLoggerTestTaskOwner(t *testing.T) *runtime.TaskOwner {
	t.Helper()
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newFileLoggerTaskOwnerForTest(t, tasks, "plugin/test/file-logger")
	t.Cleanup(func() { stopFileLoggerTaskRegistryForTest(t, tasks) })
	return owner
}

func mustMetadataView(t *testing.T, metadata map[string]any) runtime.MetadataView {
	t.Helper()
	document, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	view, err := runtime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
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

func TestSnapshotDefaultLogFieldsBoundsHostnameResolution(t *testing.T) {
	original := fileLoggerLookupIP
	fileLoggerLookupIP = func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { fileLoggerLookupIP = original })
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{APISIXVars: map[string]any{
		"$balancer_ip":   "upstream.example",
		"$balancer_port": "1982",
	}}}
	started := time.Now()
	fields := snapshotDefaultLogFields(snapshot)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("hostname resolution blocked field worker for %s", elapsed)
	}
	if fields["upstream"] != "upstream.example:1982" {
		t.Fatalf("upstream = %#v, want unresolved fallback", fields["upstream"])
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

func TestSharedWriterWatcherSurvivesOldGenerationRelease(t *testing.T) {
	registry := newFileWriterRegistryForTest(t)
	reopened := make(chan error, 1)
	registry.reopenAll = func() error {
		err := registry.flushAndReopenAll()
		reopened <- err
		return err
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "access.log")
	rotated := filepath.Join(directory, "access.log.old")
	old := acquireWriterLeaseForTest(t, registry, path)
	next := acquireWriterLeaseForTest(t, registry, filepath.Join(directory, ".", "access.log"))
	if old.writer != next.writer {
		t.Fatal("canonical path leases did not share a writer")
	}
	if _, err := old.writer.Write([]byte("before\n")); err != nil {
		t.Fatalf("Write(before) error = %v", err)
	}
	if err := old.writer.Sync(); err != nil {
		t.Fatalf("Sync(before) error = %v", err)
	}
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rename current log: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("recreate current log: %v", err)
	}

	old.release()
	registry.signalForTest(syscall.SIGUSR1)
	select {
	case err := <-reopened:
		if err != nil {
			t.Fatalf("signal reopen error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shared writer watcher did not process SIGUSR1")
	}
	if _, err := next.writer.Write([]byte("after\n")); err != nil {
		t.Fatalf("Write(after) error = %v", err)
	}
	if err := next.writer.Sync(); err != nil {
		t.Fatalf("Sync(after) error = %v", err)
	}
	if content := readLogFile(t, path); content != "after\n" {
		t.Fatalf("current log content = %q, want post-reopen entry", content)
	}
	active := registry.activeTaskOwnersForTest()
	if !slices.Contains(active, "core/file-writer-registry/signal-watch") {
		t.Fatalf("active task owners = %v, want shared signal watcher", active)
	}
	next.release()
}

func TestSharedWriterWatcherStopsAndRestartsByRegistryEpoch(t *testing.T) {
	registry := newFileWriterRegistryForTest(t)
	first := acquireWriterLeaseForTest(t, registry, filepath.Join(t.TempDir(), "first.log"))
	firstEpoch := registry.watcherEpochForTest()
	if firstEpoch == nil {
		t.Fatal("first writer lease did not start a watcher epoch")
	}
	if active := firstEpoch.Active(); !slices.Equal(active, []string{"core/file-writer-registry/signal-watch"}) {
		t.Fatalf("first epoch active task owners = %v", active)
	}
	first.release()
	if active := firstEpoch.Active(); len(active) != 0 {
		t.Fatalf("first epoch active task owners after release = %v, want none", active)
	}
	if registry.watcherEpochForTest() != nil {
		t.Fatal("first watcher epoch remains published after last release")
	}

	second := acquireWriterLeaseForTest(t, registry, filepath.Join(t.TempDir(), "second.log"))
	secondEpoch := registry.watcherEpochForTest()
	if secondEpoch == nil || secondEpoch == firstEpoch {
		t.Fatalf("second watcher epoch = %p, want new registry distinct from %p", secondEpoch, firstEpoch)
	}
	if active := secondEpoch.Active(); !slices.Equal(active, []string{"core/file-writer-registry/signal-watch"}) {
		t.Fatalf("second epoch active task owners = %v", active)
	}
	second.release()
}

func TestFileLoggerSharedWriterAcquireWaitsForStoppingRegistryEpoch(t *testing.T) {
	registry := newFileWriterRegistryForTest(t)
	watcherEntered := make(chan struct{})
	releaseWatcher := make(chan struct{})
	registry.reopenAll = func() error {
		close(watcherEntered)
		<-releaseWatcher
		return nil
	}
	stopCalled := make(chan struct{})
	var stopOnce sync.Once
	registry.stopSignal = func(chan<- os.Signal) {
		stopOnce.Do(func() { close(stopCalled) })
	}
	old := acquireWriterLeaseForTest(t, registry, filepath.Join(t.TempDir(), "old.log"))
	oldEpoch := registry.watcherEpochForTest()
	registry.signalForTest(syscall.SIGUSR1)
	select {
	case <-watcherEntered:
	case <-time.After(time.Second):
		t.Fatal("old watcher did not enter the blocked reopen callback")
	}

	released := make(chan struct{})
	go func() {
		old.release()
		close(released)
	}()
	select {
	case <-stopCalled:
	case <-time.After(time.Second):
		t.Fatal("last lease did not stop old signal notification")
	}
	type acquireResult struct {
		lease *fileWriterLease
		err   error
	}
	nextPath := filepath.Join(t.TempDir(), "next.log")
	acquired := make(chan acquireResult, 1)
	go func() {
		lease, err := registry.acquire(nextPath)
		acquired <- acquireResult{lease: lease, err: err}
	}()
	select {
	case result := <-acquired:
		if result.lease != nil {
			result.lease.release()
		}
		t.Fatalf("acquire completed before old watcher joined: error = %v", result.err)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseWatcher)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("old lease release did not finish after watcher callback returned")
	}
	var next *fileWriterLease
	select {
	case result := <-acquired:
		if result.err != nil {
			t.Fatalf("acquire after old watcher join error = %v", result.err)
		}
		next = result.lease
	case <-time.After(time.Second):
		t.Fatal("acquire did not resume after old watcher joined")
	}
	if nextEpoch := registry.watcherEpochForTest(); nextEpoch == nil || nextEpoch == oldEpoch {
		t.Fatalf("next watcher epoch = %p, want distinct from old epoch %p", nextEpoch, oldEpoch)
	}
	next.release()
}

func TestFileWriterSignalTaskPanicUsesCoreFatalPolicy(t *testing.T) {
	if os.Getenv("APISIX_GO_TEST_FILE_WRITER_CORE_PANIC") == "1" {
		runFileWriterSignalCorePanicFixture(t, "file-writer-core-fatal")
		fmt.Fprintln(os.Stderr, "file-writer-returned-after-core-panic")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileWriterSignalTaskPanicUsesCoreFatalPolicy$")
	cmd.Env = append(os.Environ(), "APISIX_GO_TEST_FILE_WRITER_CORE_PANIC=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !bytes.Contains(output, []byte("file-writer-core-fatal")) ||
		bytes.Contains(output, []byte("file-writer-returned-after-core-panic")) {
		t.Fatalf("core panic subprocess context = %v, error = %v, output = %s", ctx.Err(), err, output)
	}
}

func newFileWriterRegistryForTest(t *testing.T) *fileWriterRegistry {
	t.Helper()
	return &fileWriterRegistry{
		writers:      make(map[string]*registeredFileWriter),
		notifySignal: func(chan<- os.Signal, ...os.Signal) {},
		stopSignal:   func(chan<- os.Signal) {},
	}
}

func acquireWriterLeaseForTest(t *testing.T, registry *fileWriterRegistry, path string) *fileWriterLease {
	t.Helper()
	lease, err := registry.acquire(path)
	if err != nil {
		t.Fatalf("acquire(%q) error = %v", path, err)
	}
	return lease
}

func (r *fileWriterRegistry) signalForTest(value os.Signal) {
	r.mu.Lock()
	signals := r.signals
	r.mu.Unlock()
	if signals == nil {
		panic("file writer signal watcher is not running")
	}
	signals <- value
}

func (r *fileWriterRegistry) activeTaskOwnersForTest() []string {
	epoch := r.watcherEpochForTest()
	if epoch == nil {
		return nil
	}
	return epoch.Active()
}

func (r *fileWriterRegistry) watcherEpochForTest() *runtime.TaskRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.watcherEpoch
}

func runFileWriterSignalCorePanicFixture(t *testing.T, marker string) {
	t.Helper()
	registry := newFileWriterRegistryForTest(t)
	registry.reopenAll = func() error {
		panic(marker)
	}
	acquireWriterLeaseForTest(t, registry, filepath.Join(t.TempDir(), "panic.log"))
	active := registry.activeTaskOwnersForTest()
	if !slices.Equal(active, []string{"core/file-writer-registry/signal-watch"}) {
		t.Fatalf("active task owners = %v, want core signal watcher", active)
	}
	registry.signalForTest(syscall.SIGUSR1)
	select {}
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

func TestFileLoggerRegistryCancellationBeforePluginStopRunsLateCleanupOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late-cleanup.log")
	key, err := canonicalWriterPath(path)
	if err != nil {
		t.Fatalf("canonicalWriterPath() error = %v", err)
	}
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newFileLoggerTaskOwnerForTest(t, tasks, "plugin/test/file-logger/late-cleanup")
	p := &Plugin{config: Config{Path: path}}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	writer := p.writer

	stopFileLoggerTaskRegistryForTest(t, tasks)
	if !sharedFileWriters.has(key) {
		t.Fatal("registry cancellation released the writer before Plugin.Stop registered cleanup")
	}
	p.Stop()
	p.Stop()
	if sharedFileWriters.has(key) {
		t.Fatal("late Plugin.Stop did not release the writer lease")
	}
	if _, err := writer.Write([]byte("late")); !errors.Is(err, errFileLoggerWriterStopped) {
		t.Fatalf("Write() after repeated Plugin.Stop error = %v, want writer stopped", err)
	}
}

func TestFileLoggerQuiesceReleasesWriterBeforeRegistryStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiesce.log")
	key, err := canonicalWriterPath(path)
	if err != nil {
		t.Fatalf("canonicalWriterPath() error = %v", err)
	}
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newFileLoggerTaskOwnerForTest(t, tasks, "plugin/test/file-logger/quiesce")
	p := &Plugin{config: Config{Path: path}}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if !sharedFileWriters.has(key) {
		t.Fatal("PostInit did not register the writer lease")
	}

	p.QuiesceGenerationTasks()
	if sharedFileWriters.has(key) {
		t.Fatal("QuiesceGenerationTasks did not release the writer before registry Stop")
	}
	stopFileLoggerTaskRegistryForTest(t, tasks)
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
	p := newTestPluginWithMetadata(t, Config{}, map[string]any{
		"path":       path,
		"log_format": map[string]any{"path": "$uri"},
	})

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

func TestPreparedGenerationsRetainMetadataPathAndFormat(t *testing.T) {
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "generation-n.log")
	secondPath := filepath.Join(directory, "generation-n-plus-one.log")
	first := newTestPluginWithMetadata(t, Config{}, map[string]any{
		"path": firstPath,
		"log_format": map[string]any{
			"generation": "n",
		},
	})
	second := newTestPluginWithMetadata(t, Config{}, map[string]any{
		"path": secondPath,
		"log_format": map[string]any{
			"generation": "n-plus-one",
		},
	})

	if first.config.Path != firstPath || first.logFormat["generation"] != "n" {
		t.Fatalf("generation N metadata = %q/%#v", first.config.Path, first.logFormat)
	}
	if second.config.Path != secondPath || second.logFormat["generation"] != "n-plus-one" {
		t.Fatalf("generation N+1 metadata = %q/%#v", second.config.Path, second.logFormat)
	}
	first.Stop()
	second.Stop()
}

func TestMetadataDecodeFailsBeforeFileWriterAcquisition(t *testing.T) {
	const pattern = "^/file-metadata-decode-failure-7b9a$"
	match := []any{[]any{"$uri", "~", pattern}}
	p := &Plugin{config: Config{
		Path:  filepath.Join(t.TempDir(), "decode-failure.log"),
		Match: match,
	}}
	p.SetDependencies(base.Dependencies{Metadata: mustMetadataView(t, map[string]any{
		"path": 42,
	})})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if p.lease != nil || p.writer != nil || p.logger != nil || p.processor != nil {
		t.Fatalf(
			"decode failure acquired file resources: lease=%v writer=%v logger=%v processor=%v",
			p.lease,
			p.writer,
			p.logger,
			p.processor,
		)
	}
	request := httptest.NewRequest(http.MethodGet, "/file-metadata-decode-failure-7b9a", nil)
	if base.ExprMatched(request, match, 0) {
		t.Fatal("metadata decode failure retained a prepared expression regexp")
	}
}

func TestPostInitRollsBackWriterLeaseWhenTaskAdmissionFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stopped-task-owner.log")
	key, err := canonicalWriterPath(path)
	if err != nil {
		t.Fatalf("canonicalWriterPath() error = %v", err)
	}
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newFileLoggerTaskOwnerForTest(t, tasks, "plugin/test/file-logger/stopped")
	stopFileLoggerTaskRegistryForTest(t, tasks)
	p := &Plugin{config: Config{Path: path}}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err = p.PostInit()
	if !errors.Is(err, runtime.ErrTaskRegistryStopped) {
		t.Fatalf("PostInit() error = %v, want ErrTaskRegistryStopped", err)
	}
	if p.lease != nil || p.writer != nil || p.logger != nil || p.processor != nil {
		t.Fatalf(
			"failed task admission published resources: lease=%v writer=%v logger=%v processor=%v",
			p.lease,
			p.writer,
			p.logger,
			p.processor,
		)
	}
	if sharedFileWriters.has(key) {
		t.Fatal("failed task admission retained the acquired writer lease")
	}
}

func TestPostInitRejectsMissingPath(t *testing.T) {
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
