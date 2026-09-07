package pluginintegration

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadAssertionFilesConcatenatesRotatedLogs(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"2026-01-01__access.log": "first\n", "access.log": "second\n", "error.log": "ignore\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body, err := readAssertionFiles(filepath.Join(dir, "*access.log"), true)
	if err != nil || string(body) != "first\nsecond\n" {
		t.Fatalf("body=%q err=%v", body, err)
	}
	if _, err := readAssertionFiles(filepath.Join(dir, "missing*.log"), true); err == nil {
		t.Fatal("missing glob accepted")
	}
	if _, err := readAssertionFiles(filepath.Join(dir, "["), true); err == nil {
		t.Fatal("malformed glob accepted")
	}
}

func TestValidateFileAssertionsRejectsAbsentConcatGlob(t *testing.T) {
	err := validateFileAssertions(
		[]FileAssertion{{Path: &Matcher{Equals: new("{{WORK_DIR}}/*.log")}, ConcatGlob: true, Absent: true}},
		"test",
	)
	if err == nil {
		t.Fatal("absent concatenation accepted")
	}
}
