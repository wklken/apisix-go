package workflow

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// BenchmarkAISelection measures rule selection at 2, 100, and 1,000 workflow
// steps when no rule matches, exercising the full fall-through scan. Per-step
// matching must stay allocation-free so the scan scales linearly without GC
// pressure.
func BenchmarkAISelection(b *testing.B) {
	for _, size := range []int{2, 100, 1000} {
		b.Run(fmt.Sprintf("steps-%d", size), func(b *testing.B) {
			rules := make([]Rule, 0, size)
			for i := 0; i < size; i++ {
				rules = append(rules, Rule{Case: []any{
					[]any{"$http_x_workflow_bench", "==", "never-" + strconv.Itoa(i)},
				}})
			}
			p := &Plugin{config: Config{Rules: rules}}
			if err := p.Init(); err != nil {
				b.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err != nil {
				b.Fatalf("PostInit() error = %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rr := httptest.NewRecorder()
				p.Handler(next).ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					b.Fatalf("response code = %d, want 200", rr.Code)
				}
			}
		})
	}
}
