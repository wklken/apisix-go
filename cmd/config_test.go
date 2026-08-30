package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
)

func TestFreshRootCommandsDoNotShareFlagsOrOutputs(t *testing.T) {
	first := newRootCommand()
	second := newRootCommand()
	if first == second || first.PersistentFlags().Lookup("config") == second.PersistentFlags().Lookup("config") {
		t.Fatal("fresh root commands share command or flag state")
	}
	firstOut, secondOut := new(bytes.Buffer), new(bytes.Buffer)
	first.SetOut(firstOut)
	second.SetOut(secondOut)
	first.SetArgs([]string{"--config", "first.yaml", "version"})
	second.SetArgs([]string{"version"})
	if err := first.Execute(); err != nil {
		t.Fatal(err)
	}
	if err := second.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstOut.String(), "Version:") || !strings.Contains(secondOut.String(), "Version:") {
		t.Fatalf("unexpected version outputs: %q / %q", firstOut, secondOut)
	}
	if got := first.PersistentFlags().Lookup("config").Value.String(); got != "first.yaml" {
		t.Fatalf("first config flag = %q", got)
	}
	if got := second.PersistentFlags().Lookup("config").Value.String(); got != config.DefaultConfigFile {
		t.Fatalf("second config flag = %q", got)
	}
}

func TestPersistentConfigFlagWorksBeforeAndAfterSubcommand(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	path := writeCommandConfig(t, "apisix:\n  id: command-test\n")
	for _, args := range [][]string{
		{"-c", path, "config", "test"},
		{"config", "test", "-c", path},
	} {
		root := newRootCommand()
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("config flag placement %v failed: %v", args, err)
		}
	}
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func TestConfigCommandTestValidatesWithoutStartingServer(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	dataDir := filepath.Join(t.TempDir(), "must-stay-absent")
	path := writeCommandConfig(t, validCommandConfigWithDataDir(dataDir))
	root := newRootCommand()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"config", "test", "-c", path})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "configuration is valid\n" {
		t.Fatalf("stdout = %q", got)
	}
	assertNoInspectionArtifacts(t, workingDirectory, dataDir)
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func TestConfigCommandsRejectPositionalArguments(t *testing.T) {
	for _, args := range [][]string{{"config", "unexpected"}, {"config", "test", "unexpected"}} {
		root := newRootCommand()
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Fatalf("command accepted positional arguments: %v", args)
		}
	}
}

func TestConfigCommandTestDoesNotBindConfiguredListener(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	path := writeCommandConfig(t, fmt.Sprintf("apisix:\n  node_listen:\n    - port: %d\n", port))
	root := newRootCommand()
	root.SetArgs([]string{"config", "test", "-c", path})
	if err := root.Execute(); err != nil {
		t.Fatalf("config test failed for occupied listener: %v", err)
	}
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func TestConfigCommandTestDoesNotConfigureLogger(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	if err := logger.ConfigureLevel("debug"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	path := writeCommandConfig(t, "nginx_config:\n  error_log_level: info\n")
	root := newRootCommand()
	root.SetArgs([]string{"config", "test", "-c", path})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !logger.DebugEnabled() {
		t.Fatal("config test changed logger level")
	}
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func writeCommandConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func isolatedCommandRoot(t *testing.T) string {
	t.Helper()
	repositoryRoot := findRepositoryRoot(t)
	defaultConfig, err := os.ReadFile(filepath.Join(repositoryRoot, config.DefaultConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory := t.TempDir()
	if err := os.Mkdir(filepath.Join(workingDirectory, "conf"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workingDirectory, config.DefaultConfigFile),
		defaultConfig,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workingDirectory)
	return workingDirectory
}

func findRepositoryRoot(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for current := workingDirectory; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, config.DefaultConfigFile)); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatalf("could not locate repository root from %q", workingDirectory)
		}
	}
}

func assertNoInspectionArtifacts(t *testing.T, workingDirectory, dataDir string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(workingDirectory, "apisix-go-store.db"),
		filepath.Join(workingDirectory, "conf", "apisix.uid"),
		dataDir,
	} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("inspection created artifact %q: %v", path, err)
		}
	}
}

func snapshotWorkingDirectory(t *testing.T, workingDirectory string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(workingDirectory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(workingDirectory, path)
		if err != nil {
			return err
		}
		snapshot[relative] = fmt.Sprintf("%s:%d", info.Mode(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertSnapshotUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("inspection changed working directory: before=%v after=%v", before, after)
	}
}

func validCommandConfigWithDataDir(dataDir string) string {
	return fmt.Sprintf("apisix_go:\n  runtime_paths:\n    data_dir: %q\n", dataDir)
}
