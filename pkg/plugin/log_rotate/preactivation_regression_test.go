package log_rotate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestPreparedWorkerCannotRotateBeforePublication(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "error.log")
	if err := os.WriteFile(current, []byte("must-survive-unpublished-candidate"), 0o644); err != nil {
		t.Fatal(err)
	}

	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/log-rotate/prepared", runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	p := &Plugin{config: Config{
		ErrorLog: current, EnableAccessLog: new(false), Interval: 1, MaxSize: -1,
		MaxKept: 0, maxKeptConfigured: true,
	}}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}, Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }
	p.rotateTime = now.Add(-time.Second)
	// PostInit is invoked by preparation before GenerationEngine publishes the candidate.
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = tasks.Stop(context.Background()) }()

	deadline := time.Now().Add(1300 * time.Millisecond)
	for time.Now().Before(deadline) {
		info, statErr := os.Stat(current)
		if statErr == nil && info.Size() == 0 {
			history, globErr := filepath.Glob(filepath.Join(dir, "*__error.log*"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			if len(history) == 0 {
				t.Fatal("unpublished prepared generation rotated and pruned the live log")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(current)
	if err != nil || string(data) != "must-survive-unpublished-candidate" {
		t.Fatalf("unpublished process log = %q, error = %v", data, err)
	}
}
