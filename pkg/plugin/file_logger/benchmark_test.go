package file_logger

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func BenchmarkFileLoggerPayloadEstimator(b *testing.B) {
	fields := map[string]any{
		"request": map[string]any{
			"uri":     "/orders/123?include=summary",
			"headers": map[string][]string{"host": {"gateway.test"}, "accept": {"application/json"}},
			"query":   []any{"summary", "details"},
		},
		"response": map[string]any{
			"status": 200,
			"body":   strings.Repeat("x", 512),
		},
	}
	record := fileLogRecord{kind: fileLogFieldsRecord, fields: fields}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fileLogRecordPayloadBytes(record)
	}
}

func BenchmarkFileLoggerWrite(b *testing.B) {
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/benchmark/file-logger", runtime.TaskPlugin)
	if err != nil {
		b.Fatalf("NewTaskOwner() error = %v", err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if residuals, stopErr := tasks.Stop(ctx); stopErr != nil {
			b.Errorf("TaskRegistry.Stop() residuals = %v, error = %v", residuals, stopErr)
		}
	})
	p := &Plugin{config: Config{Path: b.TempDir() + "/access.log"}}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	b.Cleanup(p.Stop)

	entry := map[string]any{
		"request": map[string]any{
			"method":  "GET",
			"uri":     "/orders/123?include=summary",
			"headers": map[string]any{"host": "gateway.test", "user-agent": "benchmark"},
		},
		"response": map[string]any{"status": 200, "size": 512},
		"route_id": "benchmark-route",
		"latency":  1.25,
	}

	b.SetBytes(256)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.sendBatch(context.Background(), []map[string]any{entry}, 1); err != nil {
			b.Fatalf("sendBatch() error = %v", err)
		}
	}
	b.StopTimer()
	if err := p.logger.Sync(); err != nil {
		b.Fatalf("Sync() error = %v", err)
	}
}
