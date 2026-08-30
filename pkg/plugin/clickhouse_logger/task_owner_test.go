package clickhouse_logger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var _ interface{ QuiesceGenerationTasks() } = (*Plugin)(nil)

func newLoggerTestTaskOwner(t *testing.T) *runtime.TaskOwner {
	t.Helper()
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/clickhouse_logger", runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if residuals, stopErr := tasks.Stop(ctx); stopErr != nil {
			t.Errorf("TaskRegistry.Stop() residuals=%v error=%v", residuals, stopErr)
		}
	})
	return owner
}

func TestPostInitTaskAdmissionFailureReleasesStagedClient(t *testing.T) {
	p := &Plugin{config: Config{
		EndpointAddr: "http://127.0.0.1:8123",
		User:         "default",
		Password:     "password",
		Database:     "default",
		LogTable:     "logs",
		LogFormat:    map[string]string{"route_id": "$route_id"},
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, name, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); !errors.Is(err, runtime.ErrTaskOwnerRequired) {
		t.Fatalf("PostInit() error = %v, want %v", err, runtime.ErrTaskOwnerRequired)
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"failed admission retained client=%v release=%v processor=%v",
			p.client != nil, p.clientRelease != nil, p.BatchProcessor != nil,
		)
	}
}
