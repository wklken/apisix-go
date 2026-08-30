package log_rotate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/file_logger"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	p, _ := newTestPluginWithTasks(t, cfg)
	return p
}

func newTestPluginWithTasks(t *testing.T, cfg Config) (*Plugin, *runtime.TaskRegistry) {
	t.Helper()

	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/log-rotate/attempt-1", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		Config: &config.EffectiveConfig{},
		Tasks:  owner,
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})

	return p, tasks
}

func newRotationPlugin(
	t *testing.T,
	tasks *runtime.TaskRegistry,
	owner *runtime.TaskOwner,
	cfg Config,
) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		Config: &config.EffectiveConfig{},
		Tasks:  owner,
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	return p
}

func newRotationTasks(
	t *testing.T,
	prefix string,
	onFailure func(runtime.TaskFailure),
) (*runtime.TaskRegistry, *runtime.TaskOwner) {
	t.Helper()
	tasks := runtime.NewTaskRegistry(context.Background(), onFailure)
	owner, err := runtime.NewTaskOwner(tasks, prefix, runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	return tasks, owner
}

func stopTestRegistry(t *testing.T, tasks *runtime.TaskRegistry) {
	t.Helper()
	residuals, err := tasks.Stop(context.Background())
	if err != nil || len(residuals) != 0 {
		t.Errorf("TaskRegistry.Stop() = (%v, %v)", residuals, err)
	}
}

func awaitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for completion")
	}
}

func assertNotClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("operation completed before the admitted rotation was released")
	default:
	}
}

func awaitTaskFailure(t *testing.T, failures <-chan runtime.TaskFailure) runtime.TaskFailure {
	t.Helper()
	select {
	case failure := <-failures:
		return failure
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task failure")
		return runtime.TaskFailure{}
	}
}

func awaitOwnerExit(t *testing.T, tasks *runtime.TaskRegistry, owner string) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		active := tasks.Active()
		if !slices.Contains(active, owner) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("owner %q did not exit; active = %v", owner, active)
		case <-ticker.C:
		}
	}
}

func newBlockingRotationPlugin(
	t *testing.T,
	prefix string,
) (*Plugin, *runtime.TaskRegistry, <-chan struct{}, <-chan struct{}, func(), func()) {
	t.Helper()
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	failures := make(chan runtime.TaskFailure, 4)
	tasks := runtime.NewTaskRegistry(generationCtx, func(failure runtime.TaskFailure) {
		failures <- failure
	})
	owner, err := runtime.NewTaskOwner(tasks, prefix, runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	p := newRotationPlugin(t, tasks, owner, Config{})
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var startOnce, cancelOnce, releaseOnce sync.Once
	releaseRotation := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRotation)
	p.rotate = func(time.Time) error {
		startOnce.Do(func() { close(started) })
		select {
		case <-generationCtx.Done():
			cancelOnce.Do(func() { close(canceled) })
			<-release
		case <-release:
		}
		return nil
	}
	return p, tasks, started, canceled, releaseRotation, cancelGeneration
}

func TestRotationWorkerUsesGenerationTaskOwner(t *testing.T) {
	p, tasks, started, _, release, _ := newBlockingRotationPlugin(t, "plugin/test/log-rotate/attempt-1")
	p.requestRotation()
	awaitClosed(t, started)
	active := tasks.Active()
	if len(active) != 1 || active[0] != "plugin/test/log-rotate/attempt-1/rotation" {
		t.Fatalf("active task owners = %v, want generation-qualified rotation owner", active)
	}
	release()
	stopTestRegistry(t, tasks)
	p.Stop()
}

func TestRotationTaskCancellationWaitsForInFlightRotate(t *testing.T) {
	p, tasks, started, canceled, release, cancelGeneration := newBlockingRotationPlugin(
		t,
		"plugin/test/log-rotate/attempt-1",
	)
	p.requestRotation()
	awaitClosed(t, started)
	stopped := make(chan struct{})
	go func() {
		// The generation context represents the compiler-owned cancellation;
		// registry.Stop then joins the admitted worker before Plugin.Stop.
		cancelGeneration()
		stopTestRegistry(t, tasks)
		p.Stop()
		close(stopped)
	}()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation did not observe generation cancellation")
	}
	assertNotClosed(t, stopped)
	release()
	awaitClosed(t, stopped)
}

func TestRotationPanicReportsPluginOwner(t *testing.T) {
	failures := make(chan runtime.TaskFailure, 4)
	tasks, owner := newRotationTasks(t, "plugin/test/log-rotate/attempt-1", func(failure runtime.TaskFailure) {
		failures <- failure
	})
	p := newRotationPlugin(t, tasks, owner, Config{})
	wantPanic := &struct{ marker string }{marker: "rotation-panic"}
	p.rotate = func(time.Time) error { panic(wantPanic) }
	p.requestRotation()
	failure := awaitTaskFailure(t, failures)
	if failure.Owner != "plugin/test/log-rotate/attempt-1/rotation" || failure.PanicValue != wantPanic {
		t.Fatalf("failure = %#v", failure)
	}
	awaitOwnerExit(t, tasks, "plugin/test/log-rotate/attempt-1/rotation")
	stopTestRegistry(t, tasks)
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
	tasks, owner := newRotationTasks(t, "plugin/test/log-rotate/attempt-1", nil)
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}, Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	rotateStarted := make(chan struct{})
	releaseRotation := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRotation) }) }
	p.now = func() time.Time { return time.Now() }
	p.rotate = func(time.Time) error {
		close(rotateStarted)
		<-releaseRotation
		return nil
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	t.Cleanup(release)

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
	release()
}

func TestLogRotateUsesInheritedRequestAccess(t *testing.T) {
	p := &Plugin{}
	tasks, owner := newRotationTasks(t, "plugin/test/log-rotate/attempt-1", nil)
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}, Tasks: owner})
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
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
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

	tasks, owner := newRotationTasks(t, "plugin/test/file-logger/rotate", nil)
	t.Cleanup(func() { stopTestRegistry(t, tasks) })
	filePlugin := &file_logger.Plugin{}
	filePlugin.SetDependencies(base.Dependencies{Tasks: owner})
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
	tasks, owner := newRotationTasks(t, "plugin/test/log-rotate/attempt-1", nil)
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}, Tasks: owner})
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
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})

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

func TestRotateCustomLogNamesCompressesAndPrunesHistory(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "acc1.log")
	errorLog := filepath.Join(dir, "err1.log")
	for path, body := range map[string]string{
		access: "custom access", errorLog: "custom error",
		filepath.Join(dir, "2020-01-01_00-00-00__acc1.log.tar.gz"): "old access",
		filepath.Join(dir, "2020-01-01_00-00-00__err1.log.tar.gz"): "old error",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := newTestPlugin(t, Config{
		AccessLog: access, ErrorLog: errorLog, EnableAccessLog: new(true),
		MaxSize: 1, MaxKept: 1, EnableCompression: true,
	})
	now := time.Date(2026, 7, 6, 13, 14, 15, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"acc1.log", "err1.log"} {
		newest := filepath.Join(dir, "2026-07-06_13-14-15__"+name+".tar.gz")
		if _, err := os.Stat(newest); err != nil {
			t.Fatalf("new compressed %s history: %v", name, err)
		}
		old := filepath.Join(dir, "2020-01-01_00-00-00__"+name+".tar.gz")
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("old compressed %s history stat = %v, want removed", name, err)
		}
	}
}

func TestRotateCustomNamesWithZeroMaxKeptRetainsCurrentFilesOnly(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "acc2.log")
	errorLog := filepath.Join(dir, "err2.log")
	for path, body := range map[string]string{access: "access", errorLog: "error"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &Plugin{}
	tasks, owner := newRotationTasks(t, "plugin/test/log-rotate/custom-zero", nil)
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}, Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(map[string]any{
		"access_log": access, "error_log": errorLog, "enable_access_log": true,
		"max_size": 1, "max_kept": 0,
	}, p.Config()); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	if err := p.Rotate(time.Date(2026, 7, 6, 13, 14, 15, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{access, errorLog} {
		if info, err := os.Stat(path); err != nil || info.Size() != 0 {
			t.Fatalf("current %s stat = %+v/%v, want recreated empty file", path, info, err)
		}
		rotated := filepath.Join(dir, "2026-07-06_13-14-15__"+filepath.Base(path))
		if _, err := os.Stat(rotated); !os.IsNotExist(err) {
			t.Fatalf("rotated %s stat = %v, want max_kept 0 removal", path, err)
		}
	}
}

func TestRotateSkipsAccessLogWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "acc3.log")
	errorLog := filepath.Join(dir, "error.log")
	if err := os.WriteFile(access, []byte("disabled access remains"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(errorLog, []byte("error rotates"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, Config{
		AccessLog: access, ErrorLog: errorLog, EnableAccessLog: new(false),
		MaxSize: 1, MaxKept: 2,
	})
	now := time.Date(2026, 7, 6, 13, 14, 15, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(access); err != nil || string(body) != "disabled access remains" {
		t.Fatalf("disabled access log = %q/%v, want unchanged", body, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-07-06_13-14-15__acc3.log")); !os.IsNotExist(err) {
		t.Fatalf("disabled access history stat = %v, want absent", err)
	}
	if body, err := os.ReadFile(
		filepath.Join(dir, "2026-07-06_13-14-15__error.log"),
	); err != nil ||
		string(body) != "error rotates" {
		t.Fatalf("rotated error log = %q/%v, want original error content", body, err)
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

func TestPluginAttrAcceptsEffectiveConfigNumbers(t *testing.T) {
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/log-rotate/config-numbers", runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	effective := &config.EffectiveConfig{}
	effective.Config.PluginAttr = map[string]map[string]any{name: {
		"interval": apisixjson.Number("7"),
		"max_kept": apisixjson.Number("5"),
		"max_size": apisixjson.Number("11"),
		"timeout":  apisixjson.Number("13"),
	}}
	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Config: effective, Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	if p.config.Interval != 7 || p.config.MaxKept != 5 ||
		p.config.MaxSize != 11 || p.config.Timeout != 13 {
		t.Fatalf("plugin_attr numbers = %+v, want interval/max_kept/max_size/timeout 7/5/11/13", p.config)
	}
}

func TestPostInitUsesAPISIXProcessLogSelection(t *testing.T) {
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/log-rotate/process-log-selection", runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	effective := &config.EffectiveConfig{
		Config: config.Config{NginxConfig: config.NginxConfig{
			ErrorLog: "logs/err3.log",
			HTTP: config.NginxHTTP{
				AccessLog: "logs/acc3.log", EnableAccessLog: false,
			},
		}},
	}
	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Config: effective, Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopTestRegistry(t, tasks)
		p.Stop()
	})
	if p.config.AccessLog != "logs/acc3.log" || p.config.ErrorLog != "logs/err3.log" ||
		p.config.EnableAccessLog == nil || *p.config.EnableAccessLog {
		t.Fatalf("process log selection = %+v, want custom paths with access disabled", p.config)
	}
}
