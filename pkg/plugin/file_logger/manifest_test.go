package file_logger

import (
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

type fileLoggerManifestScenario struct {
	Name          string                       `yaml:"name"`
	Steps         []fileLoggerManifestStep     `yaml:"steps"`
	AfterShutdown []map[string]any             `yaml:"after_shutdown"`
	Variants      []fileLoggerManifestScenario `yaml:"variants"`
}

type fileLoggerManifestStep struct {
	Name           string           `yaml:"name"`
	Wait           string           `yaml:"wait"`
	FileAssertions []map[string]any `yaml:"file_assertions"`
}

func TestManifestReadsBufferedLogContentAfterShutdown(t *testing.T) {
	manifest := readFileLoggerManifest(t)

	for _, name := range []string{
		"metadata-format-and-cached-file-write",
		"missing-parent-open-error",
		"route-log-format-without-metadata",
		"route-log-format-wins-over-metadata",
		"nested-format-and-depth-limit",
		"metadata-path-is-used",
		"response-body-capture",
		"response-body-expression",
		"metadata-resp-body-variable",
		"request-body-capture",
		"request-body-expression",
		"gzip-response-logs-uncompressed-body",
	} {
		scenario := findFileLoggerScenario(t, manifest, name)
		assertBufferedAssertionsRunAfterShutdown(t, scenario)
	}
}

func TestManifestFlushesBeforeDestructiveFileLifecycleActions(t *testing.T) {
	manifest := readFileLoggerManifest(t)
	for _, expectation := range []struct {
		caseName string
		stepName string
	}{
		{"cached-file-stays-unlinked", "first-request-creates-and-caches-file"},
		{"cached-file-stays-unlinked", "unlinked-cached-file-is-not-recreated"},
		{"sigusr1-reopens-cached-file", "pre-signal-request-creates-and-opens-file"},
		{"sigusr1-reopens-cached-file", "sigusr1-reopens-the-file"},
		{"sigusr1-reopens-cached-file", "reopened-file-is-cached-again"},
		{"request-match-controls-file-write", "matching-request-is-written"},
		{"request-match-controls-file-write", "nonmatching-request-does-not-recreate-unlinked-file"},
	} {
		scenario := findFileLoggerScenario(t, manifest, expectation.caseName)
		step := findFileLoggerStep(t, scenario, expectation.stepName)
		if step.Wait != "1100ms" {
			t.Errorf(
				"%s/%s wait = %q, want 1100ms before the following destructive action",
				expectation.caseName,
				expectation.stepName,
				step.Wait,
			)
		}
	}
}

func assertBufferedAssertionsRunAfterShutdown(t *testing.T, scenario fileLoggerManifestScenario) {
	t.Helper()
	if len(scenario.AfterShutdown) == 0 {
		t.Errorf("scenario %q has no after_shutdown file evidence", scenario.Name)
	}
	for _, step := range scenario.Steps {
		if len(step.FileAssertions) != 0 {
			t.Errorf("scenario %q step %q reads buffered output before shutdown", scenario.Name, step.Name)
		}
	}
}

func readFileLoggerManifest(t *testing.T) []fileLoggerManifestScenario {
	t.Helper()
	path := filepath.Join("..", "..", "..", "t", "plugin", "file-logger.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file-logger manifest: %v", err)
	}
	var manifest struct {
		Cases []fileLoggerManifestScenario `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode file-logger manifest: %v", err)
	}
	return manifest.Cases
}

func findFileLoggerScenario(
	t *testing.T,
	manifest []fileLoggerManifestScenario,
	name string,
) fileLoggerManifestScenario {
	t.Helper()
	for _, scenario := range manifest {
		if scenario.Name == name {
			return scenario
		}
	}
	t.Fatalf("file-logger manifest lacks scenario %q", name)
	return fileLoggerManifestScenario{}
}

func findFileLoggerStep(
	t *testing.T,
	scenario fileLoggerManifestScenario,
	name string,
) fileLoggerManifestStep {
	t.Helper()
	for _, step := range scenario.Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("scenario %q lacks step %q", scenario.Name, name)
	return fileLoggerManifestStep{}
}
