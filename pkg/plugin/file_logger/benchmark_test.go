package file_logger

import (
	"context"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
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
	record := fileLogRecord{
		kind: fileLogSnapshotRecord,
		snapshot: base.LogSnapshot{
			Request: apisixlog.RequestLogSnapshot{
				APISIXVars: map[string]any{"$benchmark_fields": fields},
			},
		},
	}
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

	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: "GET",
			URI:    "/orders/123?include=summary",
			Header: map[string][]string{"Host": {"gateway.test"}, "User-Agent": {"benchmark"}},
		},
		Outcome: apisixctx.ResponseOutcome{Status: 200, Bytes: 512},
	}

	b.SetBytes(256)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.RunLogPhase(snapshot); err != nil {
			b.Fatalf("RunLogPhase() error = %v", err)
		}
		if (i+1)%fileLoggerBatchMaxEntries == 0 {
			ack, err := p.processor.pushBarrier()
			if err != nil {
				b.Fatalf("pushBarrier() error = %v", err)
			}
			if err := <-ack; err != nil {
				b.Fatalf("barrier error = %v", err)
			}
		}
	}
	b.StopTimer()
	ack, err := p.processor.pushBarrier()
	if err != nil {
		b.Fatalf("pushBarrier() error = %v", err)
	}
	if err := <-ack; err != nil {
		b.Fatalf("barrier error = %v", err)
	}
}
