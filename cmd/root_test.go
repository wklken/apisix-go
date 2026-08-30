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
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/version"
	bolt "go.etcd.io/bbolt"
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
			signals := make(chan os.Signal, 1)
			result := make(chan error, 1)
			go func() { result <- runServerWithSignals(lifecycle, signals) }()

			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("runServerWithSignals() did not start the server")
			}
			signals <- signal

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
		})
	}
}

func TestRunServerReturnsUnsupportedReloadErrorAfterSIGHUP(t *testing.T) {
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
	signals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	go func() { result <- runServerWithSignals(lifecycle, signals) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not start the server")
	}
	signals <- syscall.SIGHUP

	select {
	case err := <-result:
		if !errors.Is(err, errSIGHUPReloadUnsupported) {
			t.Fatalf("runServerWithSignals() error = %v, want %v", err, errSIGHUPReloadUnsupported)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not return after SIGHUP")
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("Shutdown() was not called for SIGHUP")
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
	signals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	go func() { result <- runServerWithSignals(lifecycle, signals) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not start the server")
	}
	signals <- syscall.SIGHUP

	select {
	case err := <-result:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("runServerWithSignals() error = %v, want shutdown error %v", err, shutdownErr)
		}
		if errors.Is(err, errSIGHUPReloadUnsupported) {
			t.Fatalf("runServerWithSignals() error = %v, want shutdown failure instead of reload sentinel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerWithSignals() did not return after shutdown failure")
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

type startupTestJournal struct {
	generation.Journal
	recover func(context.Context) (generation.RecoveryState, error)
	close   func() error
}

func (j *startupTestJournal) Recover(ctx context.Context) (generation.RecoveryState, error) {
	return j.recover(ctx)
}

func (j *startupTestJournal) Close() error { return j.close() }

func TestStartupLoadsOneManifestForEffectiveConfigAndCompiler(t *testing.T) {
	recorder := &startupCallRecorder{}
	manifest, effective, catalog, encryption := startupInputs(t)
	journal := &startupTestJournal{
		recover: func(context.Context) (generation.RecoveryState, error) {
			recorder.record("recover")
			return generation.RecoveryState{}, nil
		},
		close: func() error { recorder.record("journal-close"); return nil },
	}
	factories := startupFactories{
		loadManifest: func() (*capability.Manifest, error) {
			recorder.record("manifest")
			return manifest, nil
		},
		loadEffective: func(_ string, _ []string, got *capability.Manifest) (*config.EffectiveConfig, error) {
			recorder.record("effective")
			if got != manifest {
				t.Fatalf("effective manifest = %p, want startup manifest %p", got, manifest)
			}
			return effective, nil
		},
		newCatalog: func(got *capability.Manifest) (*capability.SecretDeclarationCatalog, error) {
			recorder.record("catalog")
			if got != manifest {
				t.Fatalf("catalog manifest = %p, want startup manifest %p", got, manifest)
			}
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
		mkdirAll: func(string, os.FileMode) error { recorder.record("mkdir"); return nil },
		openJournal: func(string) (generation.Journal, error) {
			recorder.record("open-journal")
			return journal, nil
		},
		newServer: func(
			gotEffective *config.EffectiveConfig,
			gotManifest *capability.Manifest,
			_ data_encryption.Service,
			resolver *secret.GenerationSecretResolver,
			gotJournal generation.Journal,
			_ generation.RecoveryState,
		) (serverLifecycle, error) {
			recorder.record("new-server")
			if gotEffective != effective || gotManifest != manifest || gotJournal != journal {
				t.Fatalf("NewServer dependencies do not preserve startup identities")
			}
			_ = resolver.Close(context.Background())
			_ = gotJournal.Close()
			return &fakeServerLifecycle{
				start:    func(context.Context) error { return nil },
				shutdown: func(context.Context) error { return nil },
			}, nil
		},
		runServer: func(serverLifecycle) error { recorder.record("run-server"); return nil },
	}

	if err := startWithOptionsWithFactories(rootOptions{}, factories); err != nil {
		t.Fatalf("startWithOptionsWithFactories() error = %v", err)
	}
	want := []string{
		"manifest", "effective", "catalog", "encryption", "logger", "resolver",
		"mkdir", "open-journal", "recover", "new-server", "journal-close", "run-server",
	}
	if got := recorder.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("startup calls = %v, want %v", got, want)
	}
}

func TestStartupOpensAndRecoversJournalBeforeServerConstruction(t *testing.T) {
	recorder := &startupCallRecorder{}
	factories := startupSuccessFactories(t, recorder)
	if err := startWithOptionsWithFactories(rootOptions{}, factories); err != nil {
		t.Fatalf("startWithOptionsWithFactories() error = %v", err)
	}
	calls := recorder.snapshot()
	openIndex := slices.Index(calls, "open-journal")
	recoverIndex := slices.Index(calls, "recover")
	serverIndex := slices.Index(calls, "new-server")
	runIndex := slices.Index(calls, "run-server")
	if openIndex >= recoverIndex || recoverIndex >= serverIndex || serverIndex >= runIndex {
		t.Fatalf("startup ownership order = %v", calls)
	}
}

func TestStartupJournalRecoveryFailureClosesJournalOnly(t *testing.T) {
	recorder := &startupCallRecorder{}
	recoveryErr := errors.New("recovery failed")
	factories := startupSuccessFactories(t, recorder)
	journal := &startupTestJournal{
		recover: func(context.Context) (generation.RecoveryState, error) {
			recorder.record("recover")
			return generation.RecoveryState{}, recoveryErr
		},
		close: func() error { recorder.record("journal-close"); return nil },
	}
	factories.openJournal = func(string) (generation.Journal, error) {
		recorder.record("open-journal")
		return journal, nil
	}
	factories.closeResolver = func(resolver *secret.GenerationSecretResolver, ctx context.Context) error {
		recorder.record("resolver-close")
		return resolver.Close(ctx)
	}

	err := startWithOptionsWithFactories(rootOptions{}, factories)
	if !errors.Is(err, recoveryErr) {
		t.Fatalf("startWithOptionsWithFactories() error = %v, want %v", err, recoveryErr)
	}
	calls := recorder.snapshot()
	if slices.Contains(calls, "new-server") || slices.Contains(calls, "run-server") {
		t.Fatalf("recovery failure transferred ownership or served: %v", calls)
	}
	wantTail := []string{"recover", "journal-close", "resolver-close"}
	if len(calls) < len(wantTail) || !slices.Equal(calls[len(calls)-len(wantTail):], wantTail) {
		t.Fatalf("recovery cleanup tail = %v, want %v", calls, wantTail)
	}
}

func TestStartupRejectsUnrecognizedJournalBeforeServerConstruction(t *testing.T) {
	recorder := &startupCallRecorder{}
	factories := startupSuccessFactories(t, recorder)
	manifest, effective, catalog, encryption := startupInputs(t)
	factories.loadManifest = func() (*capability.Manifest, error) { return manifest, nil }
	factories.loadEffective = func(string, []string, *capability.Manifest) (*config.EffectiveConfig, error) {
		return effective, nil
	}
	factories.newCatalog = func(*capability.Manifest) (*capability.SecretDeclarationCatalog, error) {
		return catalog, nil
	}
	factories.newEncryption = func(
		*config.EffectiveConfig,
		*capability.SecretDeclarationCatalog,
	) data_encryption.Service {
		return encryption
	}
	path := config.JournalPath(effective)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket, createErr := tx.CreateBucket([]byte("routes"))
		if createErr != nil {
			return createErr
		}
		return bucket.Put([]byte("unexpected"), []byte(`{"id":"unexpected","uri":"/unexpected"}`))
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	factories.openJournal = func(gotPath string) (generation.Journal, error) {
		recorder.record("open-journal")
		if gotPath != path {
			t.Fatalf("OpenJournal(%q), want %q", gotPath, path)
		}
		return store.OpenJournal(gotPath)
	}

	err = startWithOptionsWithFactories(rootOptions{}, factories)
	if !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("startWithOptionsWithFactories() error = %v, want ErrIntegrity", err)
	}
	if calls := recorder.snapshot(); slices.Contains(calls, "new-server") || slices.Contains(calls, "run-server") {
		t.Fatalf("invalid journal reached server construction: %v", calls)
	}
	db, err = bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("routes")) == nil {
			t.Fatal("rejected routes bucket was modified")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func startupInputs(
	t *testing.T,
) (*capability.Manifest, *config.EffectiveConfig, *capability.SecretDeclarationCatalog, data_encryption.Service) {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	effective := &config.EffectiveConfig{Paths: config.RuntimePaths{DataDir: t.TempDir()}}
	return manifest, effective, catalog, data_encryption.NewService(false, nil, catalog)
}

func startupSuccessFactories(t *testing.T, recorder *startupCallRecorder) startupFactories {
	t.Helper()
	manifest, effective, catalog, encryption := startupInputs(t)
	journal := &startupTestJournal{
		recover: func(context.Context) (generation.RecoveryState, error) {
			recorder.record("recover")
			return generation.RecoveryState{}, nil
		},
		close: func() error { recorder.record("journal-close"); return nil },
	}
	return startupFactories{
		loadManifest: func() (*capability.Manifest, error) {
			recorder.record("manifest")
			return manifest, nil
		},
		loadEffective: func(string, []string, *capability.Manifest) (*config.EffectiveConfig, error) {
			recorder.record("effective")
			return effective, nil
		},
		newCatalog: func(*capability.Manifest) (*capability.SecretDeclarationCatalog, error) {
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
		closeResolver: func(resolver *secret.GenerationSecretResolver, ctx context.Context) error {
			return resolver.Close(ctx)
		},
		mkdirAll: func(string, os.FileMode) error { recorder.record("mkdir"); return nil },
		openJournal: func(string) (generation.Journal, error) {
			recorder.record("open-journal")
			return journal, nil
		},
		newServer: func(
			_ *config.EffectiveConfig,
			_ *capability.Manifest,
			_ data_encryption.Service,
			resolver *secret.GenerationSecretResolver,
			journal generation.Journal,
			_ generation.RecoveryState,
		) (serverLifecycle, error) {
			recorder.record("new-server")
			_ = resolver.Close(context.Background())
			_ = journal.Close()
			return &fakeServerLifecycle{
				start:    func(context.Context) error { return nil },
				shutdown: func(context.Context) error { return nil },
			}, nil
		},
		runServer: func(serverLifecycle) error { recorder.record("run-server"); return nil },
	}
}
