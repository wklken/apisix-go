package file_logger

import (
	"context"
	"testing"
)

func BenchmarkFileLoggerWrite(b *testing.B) {
	p := &Plugin{config: Config{Path: b.TempDir() + "/access.log"}}
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
