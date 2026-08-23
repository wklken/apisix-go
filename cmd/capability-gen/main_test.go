package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestRenderRegistryIsDeterministic(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	first, err := renderRegistry(manifest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderRegistry(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("registry generation is nondeterministic")
	}
	normalized := bytes.Join(bytes.Fields(first), []byte(" "))
	if !bytes.Contains(normalized, []byte(`"request-id": func() Plugin { return &request_id.Plugin{} }`)) {
		t.Fatal("request-id constructor missing")
	}
}

func TestRenderRegistrySortsImportsAndFactories(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	generated, err := renderRegistry(manifest)
	if err != nil {
		t.Fatal(err)
	}

	aclImport := bytes.Index(generated, []byte(`"github.com/wklken/apisix-go/pkg/plugin/acl"`))
	aiImport := bytes.Index(generated, []byte(`"github.com/wklken/apisix-go/pkg/plugin/ai"`))
	if aclImport < 0 || aiImport < 0 || aclImport >= aiImport {
		t.Fatalf("imports are not sorted: acl=%d ai=%d", aclImport, aiImport)
	}
	aclFactory := bytes.Index(generated, []byte(`"acl":`))
	aiFactory := bytes.Index(generated, []byte(`"ai":`))
	if aclFactory < 0 || aiFactory < 0 || aclFactory >= aiFactory {
		t.Fatalf("factories are not sorted: acl=%d ai=%d", aclFactory, aiFactory)
	}
}

func TestCheckDetectsDrift(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	generated, err := renderRegistry(manifest)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []generatedOutput{{relativePath: registryOutputPath, content: generated}}
	root := t.TempDir()
	if err := writeOutputs(root, outputs); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, filepath.FromSlash(registryOutputPath))
	drifted := append([]byte(nil), generated...)
	drifted[0] ^= 1
	if err := os.WriteFile(path, drifted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkOutputs(root, outputs); err == nil {
		t.Fatal("checkOutputs() error = nil, want generated drift")
	} else if got, want := err.Error(), registryOutputPath+": generated output is stale"; got != want {
		t.Fatalf("checkOutputs() error = %q, want %q", got, want)
	}
}

func TestCheckRejectsOutputPathEscape(t *testing.T) {
	err := checkOutputs(t.TempDir(), []generatedOutput{{
		relativePath: "../registry_gen.go",
		content:      []byte("generated"),
	}})
	if err == nil || !strings.Contains(err.Error(), "escapes repository root") {
		t.Fatalf("checkOutputs() error = %v, want repository escape rejection", err)
	}
}

func TestOutputsRejectDirectorySymlink(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(string, []generatedOutput) error
	}{
		{name: "check", run: checkOutputs},
		{name: "write", run: writeOutputs},
	} {
		t.Run(operation.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			sentinelPath := filepath.Join(outside, "registry_gen.go")
			sentinel := []byte("external sentinel")
			if err := os.WriteFile(sentinelPath, sentinel, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "pkg", "plugin")); err != nil {
				t.Skipf("creating symlink is unavailable: %v", err)
			}

			err := operation.run(root, []generatedOutput{{
				relativePath: registryOutputPath,
				content:      []byte("generated"),
			}})
			if err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("operation error = %v, want symbolic-link rejection", err)
			}
			got, err := os.ReadFile(sentinelPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, sentinel) {
				t.Fatalf("external sentinel changed: got %q, want %q", got, sentinel)
			}
		})
	}
}

func TestOutputsRejectFinalFileSymlink(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(string, []generatedOutput) error
	}{
		{name: "check", run: checkOutputs},
		{name: "write", run: writeOutputs},
	} {
		t.Run(operation.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			sentinelPath := filepath.Join(outside, "registry_gen.go")
			sentinel := []byte("external sentinel")
			if err := os.WriteFile(sentinelPath, sentinel, 0o644); err != nil {
				t.Fatal(err)
			}
			outputDir := filepath.Join(root, "pkg", "plugin")
			if err := os.MkdirAll(outputDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sentinelPath, filepath.Join(outputDir, "registry_gen.go")); err != nil {
				t.Skipf("creating symlink is unavailable: %v", err)
			}

			err := operation.run(root, []generatedOutput{{
				relativePath: registryOutputPath,
				content:      []byte("generated"),
			}})
			if err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("operation error = %v, want symbolic-link rejection", err)
			}
			got, err := os.ReadFile(sentinelPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, sentinel) {
				t.Fatalf("external sentinel changed: got %q, want %q", got, sentinel)
			}
		})
	}
}
