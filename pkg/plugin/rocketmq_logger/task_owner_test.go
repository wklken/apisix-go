package rocketmq_logger

import (
	"context"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var _ interface{ QuiesceGenerationTasks() } = (*Plugin)(nil)

func newLoggerTestTaskOwner(t *testing.T) *runtime.TaskOwner {
	t.Helper()
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/rocketmq_logger", runtime.TaskPlugin)
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

func newOwnedBatchProcessorForTest(
	t *testing.T,
	config logger_batch.Config,
	deliver logger_batch.ContextDeliveryFunc,
) *logger_batch.Processor {
	t.Helper()
	config.Tasks = newLoggerTestTaskOwner(t)
	processor, err := logger_batch.NewWithContext(config, deliver)
	if err != nil {
		t.Fatal(err)
	}
	return processor
}
