package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
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
	flag := rootCmd.Flags().Lookup("config")
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

func TestStartReturnsStartupErrorWithoutPanic(t *testing.T) {
	previous := cfgFile
	t.Cleanup(func() { cfgFile = previous })
	cfgFile = filepath.Join(t.TempDir(), "missing.yaml")

	err := Start()
	if err == nil {
		t.Fatal("Start() error = nil with an unreadable config file")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "config") {
		t.Fatalf("Start() error = %v, want configuration context in the error", err)
	}
}

func TestRootCommandRejectsUnknownArgs(t *testing.T) {
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"definitely-not-a-real-command"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("root command accepted an unknown positional argument")
	}
}

func TestVersionCommandPrintsFullVersionInfo(t *testing.T) {
	restore := versionFieldsForTest("1.2.3", "abc1234", "2026-08-09_12:00:00", "go1.26.5")
	t.Cleanup(restore)

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetArgs([]string{"version"})
	if err := rootCmd.Execute(); err != nil {
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
