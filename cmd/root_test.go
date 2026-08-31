package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/version"
)

type fakeServerLifecycle struct {
	start        func(context.Context) error
	shutdown     func(context.Context) error
	startDone    chan struct{}
	shutdownDone chan struct{}
}

func (f *fakeServerLifecycle) Start(ctx context.Context) error {
	if f.startDone != nil {
		close(f.startDone)
	}
	return f.start(ctx)
}

func (f *fakeServerLifecycle) Shutdown(ctx context.Context) error {
	if f.shutdownDone != nil {
		close(f.shutdownDone)
	}
	return f.shutdown(ctx)
}

func TestStartHasNoDebugBannerPrint(t *testing.T) {
	source, err := os.ReadFile("root.go")
	if err != nil {
		t.Fatalf("read root.go: %v", err)
	}
	for line := range strings.SplitSeq(string(source), "\n") {
		if strings.Contains(line, `fmt.Println("It's apisix")`) {
			t.Fatalf("stray debug banner print remains in root.go: %q", line)
		}
	}
}

func TestConfigureLoggerUsesLoadedErrorLogLevel(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	cfg := &config.Config{NginxConfig: config.NginxConfig{ErrorLogLevel: "debug"}}
	if err := configureLogger(cfg); err != nil {
		t.Fatalf("configureLogger() error = %v", err)
	}
	if !logger.DebugEnabled() {
		t.Fatal("debug logging is disabled after debug configuration")
	}
}

func TestConfigureLoggerAcceptsNilAndRejectsInvalidLevel(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	if err := configureLogger(nil); err != nil {
		t.Fatalf("configureLogger(nil) error = %v", err)
	}
	cfg := &config.Config{NginxConfig: config.NginxConfig{ErrorLogLevel: "bogus"}}
	if err := configureLogger(cfg); err == nil {
		t.Fatal("configureLogger(invalid level) error = nil")
	}
}

func TestRootCommandConfigFlagMetadata(t *testing.T) {
	root := newRootCommand()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("config flag is not registered")
	}
	if flag.Shorthand != "c" {
		t.Fatalf("config shorthand = %q, want c", flag.Shorthand)
	}
	if flag.DefValue != "conf/config-default.yaml" {
		t.Fatalf("config default = %q, want conf/config-default.yaml", flag.DefValue)
	}
}

func TestRootCommandDoesNotExposeObsoleteViperFlag(t *testing.T) {
	root := newRootCommand()
	if flag := root.PersistentFlags().Lookup("viper"); flag != nil {
		t.Fatalf("obsolete viper flag remains registered: %#v", flag)
	}
}

func TestRootCommandDoesNotExposeSetFlag(t *testing.T) {
	root := newRootCommand()
	if flag := root.PersistentFlags().Lookup("set"); flag != nil {
		t.Fatalf("non-APISIX set flag remains registered: %#v", flag)
	}
}

func TestEnvironmentMapPreservesValuesAndSkipsMalformedEntries(t *testing.T) {
	got := environmentMap([]string{"PLAIN=value", "WITH_EQUALS=a=b=c", "EMPTY=", "MALFORMED"})
	want := map[string]string{"PLAIN": "value", "WITH_EQUALS": "a=b=c", "EMPTY": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environmentMap() = %#v, want %#v", got, want)
	}
}

func TestStartReturnsStartupErrorWithoutPanic(t *testing.T) {
	root := newRootCommand()
	root.SetArgs([]string{"-c", filepath.Join(t.TempDir(), "missing.yaml")})
	err := root.Execute()
	if err == nil {
		t.Fatal("Start() error = nil with an unreadable config file")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "config") {
		t.Fatalf("Start() error = %v, want configuration context in the error", err)
	}
}

func TestRootCommandRejectsUnknownArgs(t *testing.T) {
	root := newRootCommand()
	root.SetErr(io.Discard)
	root.SetArgs([]string{"definitely-not-a-real-command"})
	if err := root.Execute(); err == nil {
		t.Fatal("root command accepted an unknown positional argument")
	}
}

func TestVersionCommandPrintsFullVersionInfo(t *testing.T) {
	restore := versionFieldsForTest("1.2.3", "abc1234", "2026-08-09_12:00:00", "go1.26.5")
	t.Cleanup(restore)

	var buf bytes.Buffer
	root := newRootCommand()
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	want := "Version: 1.2.3\nCommit: abc1234\nBuild Time: 2026-08-09_12:00:00\nGo Version: go1.26.5"
	if got := strings.TrimSpace(buf.String()); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRunServerReturnsNilForTerminationSignals(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT} {
		t.Run(signal.String(), func(t *testing.T) {
			started := make(chan struct{})
			shutdown := make(chan struct{})
			lifecycle := &fakeServerLifecycle{
				startDone:    started,
				shutdownDone: shutdown,
				start: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
				shutdown: func(context.Context) error { return nil },
			}
			reloadSignals := make(chan os.Signal, 1)
			terminationSignals := make(chan os.Signal, 1)
			result := make(chan error, 1)
			reloadCalls := 0
			go func() {
				result <- runServerWithSignals(lifecycle, reloadSignals, terminationSignals, func() error {
					reloadCalls++
					return nil
				})
			}()

			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("runServerWithSignals() did not start the server")
			}
			terminationSignals <- signal

			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("runServerWithSignals() error = %v, want nil", err)
				}
			case <-time.After(time.Second):
				t.Fatal("runServerWithSignals() did not return after termination signal")
			}
			select {
			case <-shutdown:
			case <-time.After(time.Second):
				t.Fatal("Shutdown() was not called")
			}
			if reloadCalls != 0 {
				t.Fatalf("reload calls = %d, want 0", reloadCalls)
			}
		})
	}
}

func TestRunServerReloadsTwiceWithoutShuttingDown(t *testing.T) {
	started := make(chan struct{})
	shutdown := make(chan struct{})
	lifecycle := &fakeServerLifecycle{
		startDone:    started,
		shutdownDone: shutdown,
		start: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		shutdown: func(context.Context) error { return nil },
	}
	reloadSignals := make(chan os.Signal, 1)
	terminationSignals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	reloaded := make(chan struct{}, 2)
	go func() {
		result <- runServerWithSignals(lifecycle, reloadSignals, terminationSignals, func() error {
			reloaded <- struct{}{}
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not start the server")
	}
	for range 2 {
		reloadSignals <- syscall.SIGHUP
		select {
		case <-reloaded:
		case <-time.After(time.Second):
			t.Fatal("SIGHUP reload was not called")
		}
	}
	select {
	case <-shutdown:
		t.Fatal("Shutdown() was called for SIGHUP")
	default:
	}
	select {
	case err := <-result:
		t.Fatalf("runServerWithSignals() returned after SIGHUP: %v", err)
	default:
	}

	terminationSignals <- syscall.SIGTERM
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServerWithSignals() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not return after SIGTERM")
	}
}

func TestRunServerContinuesAfterSIGHUPReloadError(t *testing.T) {
	started := make(chan struct{})
	shutdown := make(chan struct{})
	lifecycle := &fakeServerLifecycle{
		startDone:    started,
		shutdownDone: shutdown,
		start: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		shutdown: func(context.Context) error { return nil },
	}
	reloadSignals := make(chan os.Signal, 1)
	terminationSignals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	reloaded := make(chan struct{}, 1)
	go func() {
		result <- runServerWithSignals(lifecycle, reloadSignals, terminationSignals, func() error {
			reloaded <- struct{}{}
			return errors.New("invalid reloaded config")
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not start the server")
	}
	reloadSignals <- syscall.SIGHUP
	select {
	case <-reloaded:
	case <-time.After(time.Second):
		t.Fatal("SIGHUP reload was not called")
	}
	select {
	case <-shutdown:
		t.Fatal("Shutdown() was called after failed SIGHUP reload")
	default:
	}
	select {
	case err := <-result:
		t.Fatalf("runServerWithSignals() returned after failed SIGHUP reload: %v", err)
	default:
	}

	terminationSignals <- syscall.SIGINT
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServerWithSignals() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not return after SIGINT")
	}
}

func TestRunServerReturnsShutdownError(t *testing.T) {
	started := make(chan struct{})
	shutdownErr := errors.New("shutdown failed")
	lifecycle := &fakeServerLifecycle{
		startDone: started,
		start: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		shutdown: func(context.Context) error { return shutdownErr },
	}
	reloadSignals := make(chan os.Signal, 1)
	terminationSignals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	go func() {
		result <- runServerWithSignals(lifecycle, reloadSignals, terminationSignals, func() error {
			return errors.New("unexpected reload for termination signal")
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not start the server")
	}
	terminationSignals <- syscall.SIGTERM

	select {
	case err := <-result:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("runServerWithSignals() error = %v, want shutdown error %v", err, shutdownErr)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not return after shutdown failure")
	}
}

func TestRunServerHandlesTerminationWhileReloadIsBlocked(t *testing.T) {
	started := make(chan struct{})
	shutdown := make(chan struct{})
	lifecycle := &fakeServerLifecycle{
		startDone:    started,
		shutdownDone: shutdown,
		start: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		shutdown: func(context.Context) error { return nil },
	}
	reloadSignals := make(chan os.Signal, 1)
	terminationSignals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	reloadStarted := make(chan struct{})
	releaseReload := make(chan struct{})
	go func() {
		result <- runServerWithSignals(lifecycle, reloadSignals, terminationSignals, func() error {
			close(reloadStarted)
			<-releaseReload
			return nil
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not start the server")
	}
	reloadSignals <- syscall.SIGHUP
	select {
	case <-reloadStarted:
	case <-time.After(time.Second):
		t.Fatal("SIGHUP reload did not start")
	}

	// Saturate the reload notification path while the active reload is blocked.
	// Termination must still be consumed from its independent signal channel.
	reloadSignals <- syscall.SIGHUP
	terminationSignals <- syscall.SIGTERM
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServerWithSignals() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not terminate while reload was blocked")
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("Shutdown() was not called while reload was blocked")
	}
	close(releaseReload)
}

func TestRunServerCoalescesAdditionalSIGHUPs(t *testing.T) {
	started := make(chan struct{})
	lifecycle := &fakeServerLifecycle{
		startDone: started,
		start: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		shutdown: func(context.Context) error { return nil },
	}
	reloadSignals := make(chan os.Signal, 1)
	terminationSignals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	reloadStarted := make(chan int, 3)
	releaseReload := make(chan struct{}, 2)
	reloadCalls := 0
	go func() {
		result <- runServerWithSignals(lifecycle, reloadSignals, terminationSignals, func() error {
			reloadCalls++
			reloadStarted <- reloadCalls
			<-releaseReload
			return nil
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not start the server")
	}
	reloadSignals <- syscall.SIGHUP
	select {
	case call := <-reloadStarted:
		if call != 1 {
			t.Fatalf("first reload call = %d, want 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first SIGHUP reload did not start")
	}

	reloadSignals <- syscall.SIGHUP
	deadline := time.After(time.Second)
sendThird:
	for {
		select {
		case reloadSignals <- syscall.SIGHUP:
			break sendThird
		case <-deadline:
			t.Fatal("second SIGHUP was not consumed while reload was active")
		}
	}
	releaseReload <- struct{}{}
	select {
	case call := <-reloadStarted:
		if call != 2 {
			t.Fatalf("pending reload call = %d, want 2", call)
		}
	case <-time.After(time.Second):
		t.Fatal("coalesced pending reload did not start")
	}
	releaseReload <- struct{}{}

	select {
	case call := <-reloadStarted:
		t.Fatalf("additional SIGHUP burst started reload call %d, want exactly 2", call)
	case <-time.After(100 * time.Millisecond):
	}
	terminationSignals <- syscall.SIGTERM
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServerWithSignals() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not return after SIGTERM")
	}
}

func TestLoggerReloaderAppliesOnlyErrorLogLevel(t *testing.T) {
	current := config.Config{
		Apisix:      config.Apisix{ID: "node-a"},
		NginxConfig: config.NginxConfig{ErrorLogLevel: "info"},
	}
	next := current
	next.NginxConfig.ErrorLogLevel = "debug"
	loadCalls := 0
	configureCalls := 0
	reload := newLoggerReloader(
		"config.yaml",
		current,
		func(path string) (*config.EffectiveConfig, error) {
			loadCalls++
			if path != "config.yaml" {
				t.Fatalf("reload path = %q, want config.yaml", path)
			}
			return &config.EffectiveConfig{Config: next}, nil
		},
		func(cfg *config.Config) error {
			configureCalls++
			if cfg.NginxConfig.ErrorLogLevel != "debug" {
				t.Fatalf("configured level = %q, want debug", cfg.NginxConfig.ErrorLogLevel)
			}
			return nil
		},
	)

	if err := reload(); err != nil {
		t.Fatalf("first reload error = %v", err)
	}
	if err := reload(); err != nil {
		t.Fatalf("no-op reload error = %v", err)
	}
	if loadCalls != 2 || configureCalls != 1 {
		t.Fatalf("reload calls = load %d, configure %d; want 2 and 1", loadCalls, configureCalls)
	}
}

func TestLoggerReloaderRejectsInvalidOrStaticChangesWithoutChangingLevel(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	if err := logger.ConfigureLevel("debug"); err != nil {
		t.Fatal(err)
	}
	current := config.Config{
		Apisix:      config.Apisix{ID: "node-a"},
		NginxConfig: config.NginxConfig{ErrorLogLevel: "debug"},
	}

	t.Run("load error", func(t *testing.T) {
		reload := newLoggerReloader(
			"config.yaml",
			current,
			func(string) (*config.EffectiveConfig, error) { return nil, errors.New("read failed") },
			configureLogger,
		)
		if err := reload(); err == nil || !strings.Contains(err.Error(), "reload effective config") {
			t.Fatalf("reload error = %v, want load context", err)
		}
	})

	t.Run("static change", func(t *testing.T) {
		next := current
		next.Apisix.ID = "node-b"
		next.NginxConfig.ErrorLogLevel = "info"
		configureCalls := 0
		reload := newLoggerReloader(
			"config.yaml",
			current,
			func(string) (*config.EffectiveConfig, error) {
				return &config.EffectiveConfig{Config: next}, nil
			},
			func(*config.Config) error { configureCalls++; return nil },
		)
		if err := reload(); err == nil || !strings.Contains(err.Error(), "static configuration") {
			t.Fatalf("reload error = %v, want static-change rejection", err)
		}
		if configureCalls != 0 {
			t.Fatalf("configure calls = %d, want 0", configureCalls)
		}
	})

	t.Run("invalid level", func(t *testing.T) {
		next := current
		next.NginxConfig.ErrorLogLevel = "bogus"
		reload := newLoggerReloader(
			"config.yaml",
			current,
			func(string) (*config.EffectiveConfig, error) {
				return &config.EffectiveConfig{Config: next}, nil
			},
			configureLogger,
		)
		if err := reload(); err == nil || !strings.Contains(err.Error(), "configure logger") {
			t.Fatalf("reload error = %v, want logger context", err)
		}
	})

	if !logger.DebugEnabled() {
		t.Fatal("failed reload changed the current debug level")
	}
}

func versionFieldsForTest(v, commit, buildTime, goVersion string) func() {
	origVersion, origCommit, origBuildTime, origGoVersion := version.Version, version.Commit, version.BuildTime, version.GoVersion
	version.Version, version.Commit, version.BuildTime, version.GoVersion = v, commit, buildTime, goVersion
	return func() {
		version.Version, version.Commit, version.BuildTime, version.GoVersion = origVersion, origCommit, origBuildTime, origGoVersion
	}
}

type startupCallRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *startupCallRecorder) record(call string) {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
}

func (r *startupCallRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func TestStartupBuildsConfigCatalogAndServerInOrder(t *testing.T) {
	recorder := &startupCallRecorder{}
	effective, catalog, encryption := startupInputs(t)
	factories := startupFactories{
		loadEffective: func(string) (*config.EffectiveConfig, error) {
			recorder.record("effective")
			return effective, nil
		},
		newCatalog: func() (*capability.SecretDeclarationCatalog, error) {
			recorder.record("catalog")
			return catalog, nil
		},
		newEncryption: func(*config.EffectiveConfig, *capability.SecretDeclarationCatalog) data_encryption.Service {
			recorder.record("encryption")
			return encryption
		},
		configureLogger: func(*config.Config) error { recorder.record("logger"); return nil },
		newResolver: func(service data_encryption.Service) (*secret.GenerationSecretResolver, error) {
			recorder.record("resolver")
			return secret.NewGenerationSecretResolver(service)
		},
		newServer: func(
			gotEffective *config.EffectiveConfig,
			_ data_encryption.Service,
			resolver *secret.GenerationSecretResolver,
		) (serverLifecycle, error) {
			recorder.record("new-server")
			if gotEffective != effective {
				t.Fatalf("NewServer dependencies do not preserve startup identities")
			}
			_ = resolver.Close(context.Background())
			return &fakeServerLifecycle{
				start:    func(context.Context) error { return nil },
				shutdown: func(context.Context) error { return nil },
			}, nil
		},
		runServer: func(_ serverLifecycle, reload reloadFunc) error {
			recorder.record("run-server")
			if reload == nil {
				t.Fatal("runServer reload callback is nil")
			}
			return nil
		},
	}

	if err := startWithOptionsWithFactories(rootOptions{}, factories); err != nil {
		t.Fatalf("startWithOptionsWithFactories() error = %v", err)
	}
	want := []string{
		"effective", "catalog", "encryption", "logger", "resolver",
		"new-server", "run-server",
	}
	if got := recorder.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("startup calls = %v, want %v", got, want)
	}
}

func startupInputs(
	t *testing.T,
) (*config.EffectiveConfig, *capability.SecretDeclarationCatalog, data_encryption.Service) {
	t.Helper()
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	effective := &config.EffectiveConfig{}
	return effective, catalog, data_encryption.NewService(false, nil, catalog)
}
