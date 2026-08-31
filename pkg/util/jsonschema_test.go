package util

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestCompileSchemaWithoutExternalReferencesAllowsLocalReference(t *testing.T) {
	compiled, err := CompileSchemaWithoutExternalReferences(`{
		"$defs":{"value":{"type":"string"}},
		"$ref":"#/$defs/value"
	}`)
	if err != nil {
		t.Fatalf("CompileSchemaWithoutExternalReferences() error = %v", err)
	}
	if err := compiled.Validate("ok"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestCompileSchemaWithoutExternalReferencesDoesNotLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external.json")
	if err := os.WriteFile(path, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := (&url.URL{Scheme: "file", Path: path}).String()
	if _, err := CompileSchemaWithoutExternalReferences(`{"$ref":"` + ref + `"}`); err == nil {
		t.Fatal("CompileSchemaWithoutExternalReferences() error = nil, want external reference rejected")
	}
}
