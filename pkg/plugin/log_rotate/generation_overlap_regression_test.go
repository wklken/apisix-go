package log_rotate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestIdleGenerationOverlapPreservesCompressedArchive(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "error.log")
	if err := os.WriteFile(current, []byte("audit-entry"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	firstDone := make(chan struct{})
	var firstOnce, secondOnce sync.Once
	secondDone := make(chan struct{})

	start := func(prefix string, before func(), after func()) (*Plugin, func()) {
		tasks, owner := newRotationTasks(t, prefix, nil)
		p := &Plugin{config: Config{
			ErrorLog: current, EnableAccessLog: new(false), Interval: 1,
			MaxKept: 10, MaxSize: -1, EnableCompression: true,
		}}
		p.SetDependencies(
			base.Dependencies{
				WithHTTPPublication: func(action func() error) error { return action() },
				Config:              &config.EffectiveConfig{},
				Tasks:               owner,
			},
		)
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		p.now = func() time.Time { return now }
		p.rotateTime = now.Add(-time.Second)
		p.rotate = func(at time.Time) error {
			if before != nil {
				before()
			}
			err := p.Rotate(at)
			if after != nil {
				after()
			}
			return err
		}
		if err := p.PostInit(); err != nil {
			t.Fatal(err)
		}
		return p, func() {
			if residuals, err := tasks.Stop(context.Background()); err != nil || len(residuals) != 0 {
				t.Fatalf("stop %s: residuals=%v err=%v", prefix, residuals, err)
			}
		}
	}

	_, stopFirst := start(
		"plugin/test/log-rotate/old-generation",
		nil,
		func() { firstOnce.Do(func() { close(firstDone) }) },
	)
	_, stopSecond := start(
		"plugin/test/log-rotate/new-generation",
		func() { <-firstDone },
		func() { secondOnce.Do(func() { close(secondDone) }) },
	)
	defer stopSecond()
	defer stopFirst()

	select {
	case <-secondDone:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("both idle generation workers did not rotate")
	}
	stopSecond()
	stopFirst()

	archivePath := filepath.Join(dir, now.Format("2006-01-02_15-04-05")+"__error.log.tar.gz")
	archive, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "audit-entry" {
		t.Fatalf("compressed audit log = %q, want original entry preserved", body)
	}
}
