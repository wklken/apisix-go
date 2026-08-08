package exit_transformer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BenchmarkStaticConfigPath measures the per-request exit-transformer path
// with valid static Lua functions. The function prototypes must be compiled
// once at PostInit; requests must never recompile the Lua source.
func BenchmarkStaticConfigPath(b *testing.B) {
	p := &Plugin{config: Config{Functions: []string{`
		return function(code, body, header)
			if type(body) == "table" then
				body.message = "transformed"
			end
			return code, body, header
		end
	`}}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	if len(p.compiled) != 1 {
		b.Fatal("Lua functions were not compiled at PostInit")
	}

	compilationsBefore := compileFunctionCount.Load()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"original"}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		p.Handler(upstream).ServeHTTP(rr, req)
		if !strings.Contains(rr.Body.String(), "transformed") {
			b.Fatalf("body = %q, want transformed output", rr.Body.String())
		}
	}
	b.StopTimer()
	if got := compileFunctionCount.Load(); got != compilationsBefore {
		b.Fatalf("Lua functions were recompiled %d times during the benchmark", got-compilationsBefore)
	}
}
