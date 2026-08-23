package log_rotate

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/file_logger"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitRequiresEffectiveConfig(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || err.Error() != "effective config is required" {
		t.Fatalf("PostInit() error = %v, want stable missing-config error", err)
	}
}

func TestLogRotateDoesNotBlockRequest(t *testing.T) {
	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	rotateStarted := make(chan struct{})
	releaseRotation := make(chan struct{})
	p.now = func() time.Time { return time.Now() }
	p.rotate = func(time.Time) error {
		close(rotateStarted)
		<-releaseRotation
		return nil
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	response := httptest.NewRecorder()

	requestDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(requestDone)
	}()

	select {
	case <-rotateStarted:
	case <-time.After(time.Second):
		t.Fatal("background rotation did not start")
	}
	select {
	case <-requestDone:
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("request blocked on log rotation")
	}
	close(releaseRotation)
}

func TestLogRotateUsesInheritedRequestAccess(t *testing.T) {
	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	triggered := make(chan struct{}, 1)
	p.rotate = func(time.Time) error {
		triggered <- struct{}{}
		return nil
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://example.test/", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue || result.Request != request {
		t.Fatalf("request phase result = %+v, want unchanged continuing request", result)
	}
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("request-access trigger did not reach the generation worker")
	}
}

func TestRotateByMaxSizeRenamesLogsAndRecreatesCurrentFiles(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "access.log")
	errorLog := filepath.Join(dir, "error.log")
	if err := os.WriteFile(access, []byte("access"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(errorLog, []byte("error"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newTestPlugin(t, Config{
		AccessLog:       access,
		ErrorLog:        errorLog,
		EnableAccessLog: new(true),
		MaxSize:         1,
		MaxKept:         10,
	})

	now := time.Date(2026, 7, 6, 13, 14, 15, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	rotatedAccess := filepath.Join(dir, "2026-07-06_13-14-15__access.log")
	rotatedError := filepath.Join(dir, "2026-07-06_13-14-15__error.log")
	if got, err := os.ReadFile(rotatedAccess); err != nil || string(got) != "access" {
		t.Fatalf("rotated access = %q/%v, want original content", got, err)
	}
	if got, err := os.ReadFile(rotatedError); err != nil || string(got) != "error" {
		t.Fatalf("rotated error = %q/%v, want original content", got, err)
	}
	if info, err := os.Stat(access); err != nil || info.Size() != 0 {
		t.Fatalf("current access stat = %+v/%v, want recreated empty file", info, err)
	}
	if info, err := os.Stat(errorLog); err != nil || info.Size() != 0 {
		t.Fatalf("current error stat = %+v/%v, want recreated empty file", info, err)
	}
}

func TestRotatePrunesOldHistoryBeyondMaxKept(t *testing.T) {
	dir := t.TempDir()
	errorLog := filepath.Join(dir, "error.log")
	if err := os.WriteFile(errorLog, []byte("error"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "2026-07-06_01-00-00__error.log")
	older := filepath.Join(dir, "2026-07-06_00-00-00__error.log")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(older, []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newTestPlugin(t, Config{
		ErrorLog: errorLog,
		MaxSize:  1,
		MaxKept:  1,
	})

	now := time.Date(2026, 7, 6, 2, 0, 0, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	newest := filepath.Join(dir, "2026-07-06_02-00-00__error.log")
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newest rotated file missing: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old file stat err = %v, want removed", err)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("older file stat err = %v, want removed", err)
	}
}

func TestRotateCompressesRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	errorLog := filepath.Join(dir, "error.log")
	if err := os.WriteFile(errorLog, []byte("error"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newTestPlugin(t, Config{
		ErrorLog:          errorLog,
		MaxSize:           1,
		MaxKept:           10,
		EnableCompression: true,
	})

	now := time.Date(2026, 7, 6, 3, 0, 0, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	plain := filepath.Join(dir, "2026-07-06_03-00-00__error.log")
	compressed := plain + ".tar.gz"
	if _, err := os.Stat(compressed); err != nil {
		t.Fatalf("compressed file missing: %v", err)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Fatalf("plain rotated file stat err = %v, want removed after compression", err)
	}
}

func TestRotateReopensFileLoggerAfterCurrentPathIsRecreated(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "access.log")

	filePlugin := &file_logger.Plugin{}
	if err := filePlugin.Init(); err != nil {
		t.Fatalf("file logger Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"path":       access,
		"log_format": map[string]any{"path": "$uri"},
	}, filePlugin.Config()); err != nil {
		t.Fatalf("parse file logger config: %v", err)
	}
	if err := filePlugin.PostInit(); err != nil {
		t.Fatalf("file logger PostInit() error = %v", err)
	}
	t.Cleanup(filePlugin.Stop)

	handler := filePlugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/before", nil))

	rotatePlugin := newTestPlugin(t, Config{
		AccessLog:       access,
		ErrorLog:        filepath.Join(dir, "missing-error.log"),
		EnableAccessLog: new(true),
		MaxSize:         1,
		MaxKept:         10,
	})
	now := time.Date(2026, 7, 6, 13, 14, 15, 0, time.UTC)
	if err := rotatePlugin.Rotate(now); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/after", nil))
	current, err := os.ReadFile(access)
	if err != nil {
		t.Fatalf("read current access log: %v", err)
	}
	if !strings.Contains(string(current), `"/after"`) {
		t.Fatalf("current access log = %q, want post-rotation request", current)
	}
	if strings.Contains(string(current), `"/before"`) {
		t.Fatalf("current access log = %q, want pre-rotation request only in rotated history", current)
	}
}

func TestExplicitZeroMaxKeptPrunesAllHistory(t *testing.T) {
	dir := t.TempDir()
	errorLog := filepath.Join(dir, "error.log")
	if err := os.WriteFile(errorLog, []byte("error"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"error_log": errorLog,
		"max_size":  1,
		"max_kept":  0,
	}, p.Config()); err != nil {
		t.Fatalf("parse log-rotate config: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	now := time.Date(2026, 7, 6, 13, 14, 15, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	rotated := filepath.Join(dir, "2026-07-06_13-14-15__error.log")
	if _, err := os.Stat(rotated); !os.IsNotExist(err) {
		t.Fatalf("rotated history stat error = %v, want max_kept 0 to remove it", err)
	}
	if info, err := os.Stat(errorLog); err != nil || info.Size() != 0 {
		t.Fatalf("current error log stat = %+v/%v, want recreated empty file", info, err)
	}
}

func TestNextRotateTimeAlignsToIntervalBoundary(t *testing.T) {
	now := time.Date(2026, 7, 6, 13, 14, 15, 0, time.UTC)
	got := nextRotateTime(now, time.Hour)
	want := time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("nextRotateTime() = %s, want %s", got, want)
	}
}

func TestDefaultsMatchOfficialPluginAttr(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.Interval != 3600 {
		t.Fatalf("interval = %d, want 3600", p.config.Interval)
	}
	if p.config.MaxKept != 168 {
		t.Fatalf("max_kept = %d, want 168", p.config.MaxKept)
	}
	if p.config.MaxSize != -1 {
		t.Fatalf("max_size = %d, want -1", p.config.MaxSize)
	}
	if p.config.Timeout != 10000 {
		t.Fatalf("timeout = %d, want 10000", p.config.Timeout)
	}
	if p.config.EnableCompression {
		t.Fatal("enable_compression = true, want false")
	}
}
