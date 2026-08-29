package oas_validator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/testutil"
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
			refreshFetched := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if fetches.Add(1) == 2 {
					refreshFetched <- struct{}{}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testSpec()))
			}))
			b.Cleanup(server.Close)

			p := &Plugin{config: Config{
				SpecURL:                 server.URL,
				SpecURLAllowedAddresses: []string{"127.0.0.1"},
			}}
			b.Cleanup(func() {
				if scenario.due && fetches.Load() < 2 {
					b.Fatalf("due refresh fetches = %d, want at least 2", fetches.Load())
				}
			})
			failures := make(chan runtime.TaskFailure, 4)
			tasks := runtime.NewTaskRegistry(context.Background(), func(failure runtime.TaskFailure) {
				failures <- failure
			})
			owner, err := runtime.NewTaskOwner(tasks, "plugin/test/oas/benchmark", runtime.TaskPlugin)
			if err != nil {
				b.Fatal(err)
			}
			p.SetDependencies(base.Dependencies{Tasks: owner})
			b.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				residuals, stopErr := tasks.Stop(ctx)
				if stopErr != nil || len(residuals) != 0 {
					b.Errorf("TaskRegistry.Stop() = (%v, %v)", residuals, stopErr)
				}
				p.Stop()
				select {
				case failure := <-failures:
					b.Errorf("unexpected task failure = %#v", failure)
				default:
				}
			})
			if err := p.Init(); err != nil {
				b.Fatalf("Init() error = %v", err)
			}
			capabilityValue, scope, closeAttempt := testutil.ScopedSecretHarness(
				b,
				name,
				nil,
				generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
			)
			b.Cleanup(closeAttempt)
			if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
				b.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			if err := p.PostInit(); err != nil {
				b.Fatalf("PostInit() error = %v", err)
			}
			var clock atomic.Int64
			clock.Store(time.Unix(100, 0).UnixNano())
			p.now = func() time.Time { return time.Unix(0, clock.Load()) }
			p.metadata.SpecURLTTL = 10

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
			b.StopTimer()
			if scenario.due {
				select {
				case <-refreshFetched:
				case <-time.After(2 * time.Second):
					b.Fatalf("due refresh fetches = %d, want at least 2", fetches.Load())
				}
			}
		})
	}
}
