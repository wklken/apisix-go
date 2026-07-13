package log_rotate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestRotateByMaxSizeRenamesLogsAndRecreatesCurrentFiles(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "access.log")
	errorLog := filepath.Join(dir, "error.log")
	if err := os.WriteFile(access, []byte("access"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(errorLog, []byte("error"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newTestPlugin(t, Config{
		AccessLog:       access,
		ErrorLog:        errorLog,
		EnableAccessLog: new(true),
		MaxSize:         1,
		MaxKept:         10,
	})

	now := time.Date(2026, 7, 6, 13, 14, 15, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	rotatedAccess := filepath.Join(dir, "2026-07-06_13-14-15__access.log")
	rotatedError := filepath.Join(dir, "2026-07-06_13-14-15__error.log")
	if got, err := os.ReadFile(rotatedAccess); err != nil || string(got) != "access" {
		t.Fatalf("rotated access = %q/%v, want original content", got, err)
	}
	if got, err := os.ReadFile(rotatedError); err != nil || string(got) != "error" {
		t.Fatalf("rotated error = %q/%v, want original content", got, err)
	}
	if info, err := os.Stat(access); err != nil || info.Size() != 0 {
		t.Fatalf("current access stat = %+v/%v, want recreated empty file", info, err)
	}
	if info, err := os.Stat(errorLog); err != nil || info.Size() != 0 {
		t.Fatalf("current error stat = %+v/%v, want recreated empty file", info, err)
	}
}

func TestRotatePrunesOldHistoryBeyondMaxKept(t *testing.T) {
	dir := t.TempDir()
	errorLog := filepath.Join(dir, "error.log")
	if err := os.WriteFile(errorLog, []byte("error"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "2026-07-06_01-00-00__error.log")
	older := filepath.Join(dir, "2026-07-06_00-00-00__error.log")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(older, []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newTestPlugin(t, Config{
		ErrorLog: errorLog,
		MaxSize:  1,
		MaxKept:  1,
	})

	now := time.Date(2026, 7, 6, 2, 0, 0, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	newest := filepath.Join(dir, "2026-07-06_02-00-00__error.log")
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newest rotated file missing: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old file stat err = %v, want removed", err)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("older file stat err = %v, want removed", err)
	}
}

func TestRotateCompressesRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	errorLog := filepath.Join(dir, "error.log")
	if err := os.WriteFile(errorLog, []byte("error"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newTestPlugin(t, Config{
		ErrorLog:          errorLog,
		MaxSize:           1,
		MaxKept:           10,
		EnableCompression: true,
	})

	now := time.Date(2026, 7, 6, 3, 0, 0, 0, time.UTC)
	if err := p.Rotate(now); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	plain := filepath.Join(dir, "2026-07-06_03-00-00__error.log")
	compressed := plain + ".tar.gz"
	if _, err := os.Stat(compressed); err != nil {
		t.Fatalf("compressed file missing: %v", err)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Fatalf("plain rotated file stat err = %v, want removed after compression", err)
	}
}

func TestDefaultsMatchOfficialPluginAttr(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.Interval != 3600 {
		t.Fatalf("interval = %d, want 3600", p.config.Interval)
	}
	if p.config.MaxKept != 168 {
		t.Fatalf("max_kept = %d, want 168", p.config.MaxKept)
	}
	if p.config.MaxSize != -1 {
		t.Fatalf("max_size = %d, want -1", p.config.MaxSize)
	}
	if p.config.Timeout != 10000 {
		t.Fatalf("timeout = %d, want 10000", p.config.Timeout)
	}
	if p.config.EnableCompression {
		t.Fatal("enable_compression = true, want false")
	}
}
