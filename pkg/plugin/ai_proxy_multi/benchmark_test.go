package ai_proxy_multi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkHealthSelection measures the per-request selection cost with health
// checks fresh, exactly one instance due for a probe, and every instance due.
// The due states must stay within 10% of the fresh state because probing must
// never run on the request goroutine.
func BenchmarkHealthSelection(b *testing.B) {
	for _, scenario := range []struct {
		name string
		due  int
	}{
		{name: "fresh", due: 0},
		{name: "one-due", due: 1},
		{name: "all-due", due: 8},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			var probeCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				probeCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			b.Cleanup(server.Close)

			p := newBenchmarkPlugin(b, benchmarkHealthConfig(server.URL))
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			p.healthNow = func() time.Time { return time.Unix(0, clock.Load()) }

			// Configure due-ness before any refresher can start so the
			// benchmark never races with the async probe owner. The clock
			// advances only in due scenarios, so fresh checks never fall due.
			now := time.Unix(0, clock.Load())
			for index := range p.health {
				nextCheck := now.Add(2 * time.Second)
				if index < scenario.due {
					nextCheck = now.Add(-time.Second)
				}
				p.health[index].nextCheck = nextCheck
			}

			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if scenario.due > 0 {
					clock.Store(clock.Load() + int64(time.Second))
				}
				p.refreshHealth(ctx)
				if _, ok := p.pickInstance(nil, nil); !ok {
					b.Fatal("no instance selected")
				}
			}
		})
	}
}

// BenchmarkAISelection measures weighted selection and instance lookup at 2,
// 100, and 1,000 providers, plus a weighted set whose expanded slice would
// contain 1,000 repeated entries.
func BenchmarkAISelection(b *testing.B) {
	for _, size := range []int{2, 100, 1000} {
		b.Run("providers-"+strconv.Itoa(size), func(b *testing.B) {
			instances := make([]Instance, 0, size)
			for i := 0; i < size; i++ {
				instances = append(instances, Instance{
					Name:     "provider-" + strconv.Itoa(i),
					Provider: "openai-compatible",
					Weight:   1,
					Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
					Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
				})
			}
			p := newBenchmarkPlugin(b, Config{Instances: instances})
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := p.pickInstance(nil, nil); !ok {
					b.Fatal("no instance selected")
				}
			}
		})
	}

	b.Run("weighted-1000", func(b *testing.B) {
		instances := make([]Instance, 0, 10)
		for i := 0; i < 10; i++ {
			instances = append(instances, Instance{
				Name:     "weighted-" + strconv.Itoa(i),
				Provider: "openai-compatible",
				Weight:   100,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
				Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
			})
		}
		p := newBenchmarkPlugin(b, Config{Instances: instances})
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, ok := p.pickInstance(nil, nil); !ok {
				b.Fatal("no instance selected")
			}
		}
	})
}

func benchmarkHealthConfig(endpoint string) Config {
	instances := make([]Instance, 0, 8)
	for i := 0; i < 8; i++ {
		instances = append(instances, Instance{
			Name:     "probe-" + strconv.Itoa(i),
			Provider: "openai-compatible",
			Weight:   1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer probe"}},
			Override: Override{Endpoint: endpoint},
			Checks: &HealthChecks{Active: ActiveHealthCheck{
				Type:     "http",
				HTTPPath: "/health",
				Timeout:  1,
			}},
		})
	}
	return Config{Instances: instances}
}

func newBenchmarkPlugin(b testing.TB, cfg Config) *Plugin {
	b.Helper()
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	b.Cleanup(p.Stop)
	return p
}
