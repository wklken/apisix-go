package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/version"
)

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

func TestStartupConfigSummaryExcludesSecrets(t *testing.T) {
	cfg := &config.Config{}
	cfg.Deployment.Admin.AdminKey = []config.AdminKey{{Key: "admin-secret"}}
	cfg.Deployment.Etcd.Password = "etcd-secret"
	cfg.Apisix.DataEncryption.Keyring = []string{"0123456789abcdef"}

	encoded, err := json.Marshal(startupConfigSummary(cfg))
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	for _, secret := range []string{"admin-secret", "etcd-secret", "0123456789abcdef"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("startup summary contains %q: %s", secret, encoded)
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

func versionFieldsForTest(v, commit, buildTime, goVersion string) func() {
	origVersion, origCommit, origBuildTime, origGoVersion := version.Version, version.Commit, version.BuildTime, version.GoVersion
	version.Version, version.Commit, version.BuildTime, version.GoVersion = v, commit, buildTime, goVersion
	return func() {
		version.Version, version.Commit, version.BuildTime, version.GoVersion = origVersion, origCommit, origBuildTime, origGoVersion
	}
}
