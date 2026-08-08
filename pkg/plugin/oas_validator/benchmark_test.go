package oas_validator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkValidatorRefresh measures validation latency with a fresh spec and
// with a due spec_url whose remote refresh must not block requests. The due
// state must stay within 10% of the fresh state.
func BenchmarkValidatorRefresh(b *testing.B) {
	for _, scenario := range []struct {
		name string
		due  bool
	}{
		{name: "fresh", due: false},
		{name: "due", due: true},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			var fetches atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fetches.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testSpec()))
			}))
			b.Cleanup(server.Close)

			p := &Plugin{config: Config{SpecURL: server.URL}}
			if err := p.Init(); err != nil {
				b.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err != nil {
				b.Fatalf("PostInit() error = %v", err)
			}
			var clock atomic.Int64
			clock.Store(time.Unix(100, 0).UnixNano())
			p.now = func() time.Time { return time.Unix(0, clock.Load()) }
			p.metadata.SpecURLTTL = 10
			if stopper, ok := any(p).(interface{ Stop() }); ok {
				b.Cleanup(stopper.Stop)
			}

			if _, err := p.validator(); err != nil {
				b.Fatalf("prime validator: %v", err)
			}
			req := httptest.NewRequest(
				http.MethodPost,
				"/pets/123?verbose=true",
				strings.NewReader(`{"name":"doggie"}`),
			)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Trace", "trace-id")

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if scenario.due {
					clock.Store(clock.Load() + int64(11*time.Second))
				}
				current, err := p.validator()
				if err != nil {
					b.Fatalf("validator() error = %v", err)
				}
				if err := validateRequest(context.Background(), req, current, p.config); err != nil {
					b.Fatalf("validateRequest() error = %v", err)
				}
			}
		})
	}
}
