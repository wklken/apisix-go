package version

import (
	"runtime"
	"testing"
)

func TestBuildMetadataDefaultsAreNonEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version is empty")
	}
	if Commit == "" {
		t.Fatal("Commit is empty")
	}
	if BuildTime == "" {
		t.Fatal("BuildTime is empty")
	}
	if GoVersion == "" {
		t.Fatal("GoVersion is empty")
	}
}

func TestGoVersionDefaultMatchesRuntimeVersion(t *testing.T) {
	if GoVersion != runtime.Version() {
		t.Fatalf("GoVersion = %q, want %q", GoVersion, runtime.Version())
	}
}
