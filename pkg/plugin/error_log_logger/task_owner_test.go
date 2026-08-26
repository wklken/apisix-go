package error_log_logger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/runtime"
)

var _ interface{ QuiesceGenerationTasks() } = (*Plugin)(nil)

func newLoggerTestTaskOwner(t *testing.T) *runtime.TaskOwner {
	t.Helper()
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/error_log_logger", runtime.TaskPlugin)
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

func TestPostInitTaskAdmissionFailureClosesStagedKafkaSender(t *testing.T) {
	p := &Plugin{config: Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
		},
		Level: "WARN",
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); !errors.Is(err, runtime.ErrTaskOwnerRequired) {
		t.Fatalf("PostInit() error = %v, want %v", err, runtime.ErrTaskOwnerRequired)
	}
	if p.kafkaSender != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"failed admission retained kafka sender=%v processor=%v",
			p.kafkaSender != nil, p.BatchProcessor != nil,
		)
	}
}
