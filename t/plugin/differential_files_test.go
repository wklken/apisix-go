package pluginintegration

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPrepareDifferentialFileSidesUsesIndependentWritableDirectories(t *testing.T) {
	workDir := t.TempDir()
	sides, err := prepareDifferentialFileSides(workDir, DifferentialFileCapture{
		Name: "access.log", MaxBytes: 4096, WaitTimeoutMillis: 500, ExpectedLines: 1,
	})
	if err != nil {
		t.Fatalf("prepareDifferentialFileSides() error = %v", err)
	}
	if sides.CandidatePath == sides.OracleHostPath {
		t.Fatalf("candidate and oracle file paths share storage: %q", sides.CandidatePath)
	}
	if !filepath.IsAbs(sides.CandidatePath) || !filepath.IsAbs(sides.OracleHostPath) {
		t.Fatalf("side-local host paths must be absolute: %#v", sides)
	}
	if sides.OracleConfigPath != "/tmp/apisix-go-differential-files/access.log" {
		t.Fatalf("oracle config path = %q", sides.OracleConfigPath)
	}
	for _, dir := range []string{filepath.Dir(sides.CandidatePath), sides.OracleHostDir} {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatalf("stat side directory %s: %v", dir, statErr)
		}
		if !info.IsDir() || info.Mode().Perm()&0o200 == 0 {
			t.Fatalf("side directory %s mode = %v, want writable directory", dir, info.Mode())
		}
	}
}

func TestProjectDifferentialSideFileReplacesOnlyDeclaredPlaceholder(t *testing.T) {
	config := map[string]any{
		"routes": []any{map[string]any{
			"plugins": map[string]any{"file-logger": map[string]any{
				"path": differentialSideFilePlaceholder,
			}},
		}},
	}
	projected, err := projectDifferentialSideFile(config, "/private/run/candidate/access.log")
	if err != nil {
		t.Fatalf("projectDifferentialSideFile() error = %v", err)
	}
	path := projected["routes"].([]any)[0].(map[string]any)["plugins"].(map[string]any)["file-logger"].(map[string]any)["path"]
	if path != "/private/run/candidate/access.log" {
		t.Fatalf("projected path = %#v", path)
	}
	original := config["routes"].([]any)[0].(map[string]any)["plugins"].(map[string]any)["file-logger"].(map[string]any)["path"]
	if original != differentialSideFilePlaceholder {
		t.Fatalf("source config was mutated: %#v", original)
	}
}

func TestCollectDifferentialFileObservationWaitsForCompleteBoundedJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(path, []byte("{\"host\":\"127.0.0.1\"}\n"), 0o600)
	}()

	observation, err := collectDifferentialFileObservation(path, DifferentialFileCapture{
		Name: "access.log", MaxBytes: 64, WaitTimeoutMillis: 500, ExpectedLines: 1,
	})
	if err != nil {
		t.Fatalf("collectDifferentialFileObservation() error = %v", err)
	}
	if !observation.Exists || observation.Truncated || observation.Size != int64(len(observation.Content)) {
		t.Fatalf("observation = %#v", observation)
	}
	if observation.Content != "{\"host\":\"127.0.0.1\"}\n" {
		t.Fatalf("content = %q", observation.Content)
	}
}

func TestCollectDifferentialFileObservationCapsCapturedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	observation, err := collectDifferentialFileObservation(path, DifferentialFileCapture{
		Name: "access.log", MaxBytes: 8, WaitTimeoutMillis: 100, ExpectedLines: 0,
	})
	if err != nil {
		t.Fatalf("collectDifferentialFileObservation() error = %v", err)
	}
	if !observation.Truncated || len(observation.Content) != 8 || observation.Size != 33 {
		t.Fatalf("bounded observation = %#v", observation)
	}
}

func TestDifferentialOracleRunArgsMountsOnlyDeclaredWritableSideDirectory(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "")
	identity := OracleIdentity{
		ImageRepository: "example.test/apisix",
		ImageLinuxAMD64: "sha256:" + strings.Repeat("a", 64),
	}
	args, err := differentialOracleRunArgsWithFileMount(
		identity,
		"oracle",
		"/host/config.yaml",
		"/host/apisix.yaml",
		nil,
		"/host/oracle-files",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "/host/oracle-files:/tmp/apisix-go-differential-files:rw"
	if !slices.Contains(args, want) {
		t.Fatalf("oracle run args = %#v, want writable mount %q", args, want)
	}
}
