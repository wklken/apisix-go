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

	"github.com/spf13/cobra"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
)

func TestParseSetOverridesPreservesValuesAndRejectsUnsafeSyntax(t *testing.T) {
	got, err := parseSetOverrides([]string{
		"apisix.id=a,b",
		"proxy.max_in_flight=",
		"nginx_config.http.client_body_timeout=1s",
	})
	if err != nil {
		t.Fatalf("parseSetOverrides() error = %v", err)
	}
	want := map[string]any{
		"apisix.id":                             "a,b",
		"proxy.max_in_flight":                   "",
		"nginx_config.http.client_body_timeout": "1s",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSetOverrides() = %#v, want %#v", got, want)
	}

	tests := []struct {
		name      string
		args      []string
		path      string
		allowPath bool
	}{
		{name: "missing equals", args: []string{"must-not-appear"}},
		{name: "empty path", args: []string{"=must-not-appear"}},
		{name: "repeated path", args: []string{"apisix.id=first", "apisix.id=must-not-appear"}, path: "apisix.id"},
		{
			name: "unknown path",
			args: []string{"not.a.real.static.path=must-not-appear"},
			path: "not.a.real.static.path",
		},
		{
			name:      "removed path",
			args:      []string{"deployment.profile=must-not-appear"},
			path:      "deployment.profile",
			allowPath: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseSetOverrides(test.args)
			if err == nil {
				t.Fatal("parseSetOverrides() error = nil")
			}
			if strings.Contains(err.Error(), "must-not-appear") {
				t.Fatalf("parseSetOverrides() leaked value: %v", err)
			}
			if strings.Contains(err.Error(), test.args[len(test.args)-1]) {
				t.Fatalf("parseSetOverrides() leaked raw argument: %v", err)
			}
			if test.path != "" && !test.allowPath && strings.Contains(err.Error(), test.path) {
				t.Fatalf("parseSetOverrides() leaked raw path: %v", err)
			}
		})
	}
}

func TestRootSetFlagUsesStringArrayValue(t *testing.T) {
	root := newRootCommand()
	flag := root.PersistentFlags().Lookup("set")
	if flag == nil {
		t.Fatal("set flag is not registered")
	}
	if got := flag.Value.Type(); got != "stringArray" {
		t.Fatalf("set flag value type = %q, want stringArray", got)
	}
}

func TestMalformedSetCommandDoesNotEchoSensitiveInput(t *testing.T) {
	for _, setValues := range [][]string{
		{"must-not-appear"},
		{"=must-not-appear"},
		{"apisix.id=first", "apisix.id=must-not-appear"},
		{"not.a.real.path=must-not-appear"},
		{"deployment.profile=must-not-appear"},
	} {
		root := newRootCommand()
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		args := []string{"config", "test"}
		for _, setValue := range setValues {
			args = append(args, "--set", setValue)
		}
		root.SetArgs(args)
		err := root.Execute()
		if err == nil {
			t.Fatalf("malformed --set command succeeded for %q", setValues)
		}
		if stdout.Len() != 0 {
			t.Fatalf("malformed --set wrote stdout for %q: %q", setValues, stdout.String())
		}
		for _, output := range []string{err.Error(), stdout.String(), stderr.String()} {
			if strings.Contains(output, "must-not-appear") {
				t.Fatalf("malformed --set output leaked value %q: %q", setValues, output)
			}
			for _, setValue := range setValues {
				if strings.Contains(output, setValue) {
					t.Fatalf("malformed --set output leaked raw argument %q: %q", setValue, output)
				}
			}
		}
	}
}

func TestFreshRootCommandsDoNotShareFlagsOutputsOrVersionCommands(t *testing.T) {
	first := newRootCommand()
	second := newRootCommand()
	if first == second {
		t.Fatal("newRootCommand() returned the same command")
	}
	if first.PersistentFlags().Lookup("config") == second.PersistentFlags().Lookup("config") {
		t.Fatal("fresh root commands share the config flag")
	}

	firstOut, secondOut := new(bytes.Buffer), new(bytes.Buffer)
	first.SetOut(firstOut)
	second.SetOut(secondOut)
	first.SetArgs([]string{"--config", "first.yaml", "--set", "apisix.id=first", "version"})
	second.SetArgs([]string{"version"})
	if err := first.Execute(); err != nil {
		t.Fatalf("first version command failed: %v", err)
	}
	if err := second.Execute(); err != nil {
		t.Fatalf("second version command failed: %v", err)
	}
	if firstOut.Len() == 0 || secondOut.Len() == 0 {
		t.Fatalf("version output was not isolated: first=%q second=%q", firstOut, secondOut)
	}
	if !strings.Contains(firstOut.String(), "Version:") || !strings.Contains(secondOut.String(), "Version:") {
		t.Fatalf("unexpected version output: first=%q second=%q", firstOut, secondOut)
	}
	if got := first.PersistentFlags().Lookup("config").Value.String(); got != "first.yaml" {
		t.Fatalf("first config flag = %q, want first.yaml", got)
	}
	if got := second.PersistentFlags().Lookup("config").Value.String(); got != config.DefaultConfigFile {
		t.Fatalf("second config flag = %q, want default %q", got, config.DefaultConfigFile)
	}
	if got := second.PersistentFlags().Lookup("set").Value.String(); got != "[]" {
		t.Fatalf("second set flag = %q, want empty", got)
	}
}

func TestFreshRootCommandsKeepErrorOutputIsolated(t *testing.T) {
	first := newRootCommand()
	second := newRootCommand()
	var firstOut, firstErr, secondOut, secondErr bytes.Buffer
	first.SetOut(&firstOut)
	first.SetErr(&firstErr)
	second.SetOut(&secondOut)
	second.SetErr(&secondErr)
	first.SetArgs([]string{"version", "unexpected"})
	if err := first.Execute(); err == nil {
		t.Fatal("first root accepted positional arguments")
	}
	second.SetArgs([]string{"version"})
	if err := second.Execute(); err != nil {
		t.Fatalf("second root version failed: %v", err)
	}
	if firstOut.Len() != 0 || firstErr.Len() != 0 {
		t.Fatalf(
			"first root printed an error before the process boundary: stdout=%q stderr=%q",
			firstOut.String(),
			firstErr.String(),
		)
	}
	if !strings.Contains(secondOut.String(), "Version:") || secondErr.Len() != 0 {
		t.Fatalf("second root output was contaminated: stdout=%q stderr=%q", secondOut.String(), secondErr.String())
	}
}

func TestPersistentConfigFlagsWorkBeforeAndAfterSubcommand(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	path := writeCommandConfig(t, "apisix:\n  id: command-test\n")
	first := newRootCommand()
	first.SetArgs([]string{"-c", path, "--set", "apisix.id=first", "config", "test"})
	if err := first.Execute(); err != nil {
		t.Fatalf("flags before subcommand failed: %v", err)
	}

	second := newRootCommand()
	second.SetArgs([]string{"config", "test", "-c", path, "--set", "apisix.id=second"})
	if err := second.Execute(); err != nil {
		t.Fatalf("flags after subcommand failed: %v", err)
	}
	for name, root := range map[string]*cobra.Command{"first": first, "second": second} {
		flag := root.PersistentFlags().Lookup("set")
		if flag == nil || !strings.Contains(flag.Value.String(), name) {
			t.Fatalf("%s root set flag = %v", name, flag)
		}
	}
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func TestConfigCommandTestValidatesWithoutStartingServer(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	dataDir := filepath.Join(t.TempDir(), "must-stay-absent")
	path := writeCommandConfig(t, validCommandConfigWithDataDir(dataDir))
	cmd := newRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"config", "test", "-c", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "configuration is valid\n" {
		t.Fatalf("stdout = %q", got)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config test created runtime path: %v", err)
	}
	assertNoInspectionArtifacts(t, workingDirectory, dataDir)
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func TestConfigCommandDumpRequiresEffectiveAndRedacted(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	dataDir := filepath.Join(t.TempDir(), "dump-data")
	path := writeCommandConfig(t, validCommandConfigWithSecrets(dataDir))
	cmd := newRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"config", "dump", "--effective", "--redacted", "-c", path,
		"--set", "proxy.max_in_flight=77",
		"--set", "apisix_go.runtime_paths.log_dir=relative-logs",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	output := stdout.String() + stderr.String()
	for _, secret := range []string{
		"etcd-password", "url-password", "admin-secret", "encryption-key",
		"plugin-attr-secret",
	} {
		if strings.Contains(output, secret) {
			t.Fatalf("dump leaked %q: %s", secret, output)
		}
	}
	for _, want := range []string{
		`"max_in_flight": 77`, `"kind": "cli"`, `"profiles"`, `"paths"`, `"ignored_fields"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("dump missing %q: %s", want, output)
		}
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config dump created runtime path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "apisix-go-store.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config dump created journal: %v", err)
	}
	assertNoInspectionArtifacts(t, workingDirectory, dataDir)
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func TestConfigCommandDumpRequiresBothSafetyFlags(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	path := writeCommandConfig(t, "")
	for _, test := range []struct {
		effective bool
		redacted  bool
	}{
		{effective: false, redacted: false},
		{effective: true, redacted: false},
		{effective: false, redacted: true},
		{effective: true, redacted: true},
	} {
		name := fmt.Sprintf("effective=%t/redacted=%t", test.effective, test.redacted)
		t.Run(name, func(t *testing.T) {
			root := newRootCommand()
			args := []string{"config", "dump", "-c", path}
			if test.effective {
				args = append(args, "--effective")
			}
			if test.redacted {
				args = append(args, "--redacted")
			}
			root.SetArgs(args)
			err := root.Execute()
			if test.effective && test.redacted {
				if err != nil {
					t.Fatalf("config dump failed with both safety flags: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("config dump succeeded without both safety flags")
			}
		})
	}
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func TestConfigCommandsRejectPositionalArguments(t *testing.T) {
	for _, args := range [][]string{
		{"config", "unexpected"},
		{"config", "test", "unexpected"},
		{"config", "dump", "unexpected"},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			root := newRootCommand()
			root.SetArgs(args)
			if err := root.Execute(); err == nil {
				t.Fatalf("command accepted positional arguments: %v", args)
			}
		})
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

func TestConfigCommandInspectionDoesNotConfigureLogger(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	if err := logger.ConfigureLevel("debug"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	path := writeCommandConfig(t, "nginx_config:\n  error_log_level: info\n")
	for _, args := range [][]string{
		{"config", "test", "-c", path},
		{"config", "dump", "--effective", "--redacted", "-c", path},
	} {
		root := newRootCommand()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("inspection command %v failed: %v", args, err)
		}
		if !logger.DebugEnabled() {
			t.Fatalf("inspection command %v changed logger level", args)
		}
	}
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func TestDefaultConfigPathRetainsDefaultFileProvenance(t *testing.T) {
	workingDirectory := isolatedCommandRoot(t)
	before := snapshotWorkingDirectory(t, workingDirectory)
	effective, err := loadEffectiveForCommand(config.DefaultConfigFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := effective.Provenance["proxy.max_in_flight"]
	if !ok {
		t.Fatal("default proxy.max_in_flight provenance is missing")
	}
	if source.Kind != config.SourceDefaultFile {
		t.Fatalf("default config provenance kind = %q, want %q", source.Kind, config.SourceDefaultFile)
	}
	assertSnapshotUnchanged(t, before, snapshotWorkingDirectory(t, workingDirectory))
}

func writeCommandConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write command config: %v", err)
	}
	return path
}

func isolatedCommandRoot(t *testing.T) string {
	t.Helper()
	repositoryRoot := findRepositoryRoot(t)
	defaultConfig, err := os.ReadFile(filepath.Join(repositoryRoot, config.DefaultConfigFile))
	if err != nil {
		t.Fatalf("read default config: %v", err)
	}
	workingDirectory := t.TempDir()
	if err := os.Mkdir(filepath.Join(workingDirectory, "conf"), 0o700); err != nil {
		t.Fatalf("create isolated conf directory: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(workingDirectory, config.DefaultConfigFile),
		defaultConfig,
		0o600,
	); err != nil {
		t.Fatalf("write isolated default config: %v", err)
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
	entries, err := os.ReadDir(workingDirectory)
	if err != nil {
		t.Fatalf("read isolated working directory: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "conf" {
			t.Fatalf("inspection created working-directory artifact %q", entry.Name())
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
		t.Fatalf("snapshot working directory: %v", err)
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

func validCommandConfigWithSecrets(dataDir string) string {
	return fmt.Sprintf(`apisix:
  data_encryption:
    keyring:
      - encryption-key
deployment:
  admin:
    admin_key:
      - name: admin
        key: admin-secret
        role: admin
  etcd:
    host:
      - "http://url-user:url-password@127.0.0.1:2379"
    user: etcd-user
    password: etcd-password
plugin_attr:
  example-plugin:
    token: plugin-attr-secret
apisix_go:
  runtime_paths:
    data_dir: %q
`, dataDir)
}
