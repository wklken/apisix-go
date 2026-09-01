package base

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"testing"

	"github.com/wklken/apisix-go/pkg/logger"
)

// benchmarkWriter only accumulates bytes so the response write is measured
// without the overhead of an httptest.ResponseRecorder.
type benchmarkWriter struct {
	bytes int
}

func (w *benchmarkWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	return len(p), nil
}

func BenchmarkResponseCaptureWrite(b *testing.B) {
	payload := []byte("response-body")
	underlying := &benchmarkWriter{}
	wrapped, _ := CaptureResponseOutcomeController(underlying)

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	var sink int
	for b.Loop() {
		n, err := wrapped.Write(payload)
		if err != nil {
			b.Fatal(err)
		}
		sink += n
	}
	b.StopTimer()
	runtime.KeepAlive(sink)
}

// Header is never touched by the benchmarked write path.
func (*benchmarkWriter) Header() http.Header {
	return nil
}

func (*benchmarkWriter) WriteHeader(int) {}

// BenchmarkLoggerObserver measures the logging fast path under three
// configurations: a disabled runtime level, an enabled level with no
// registered observers, and an enabled level with one observer. The
// formatted-message path (Infof) is exercised because it dominates the
// observer-side work in request handling.
func BenchmarkLoggerObserver(b *testing.B) {
	b.Run("level-disabled", func(b *testing.B) {
		if err := logger.ConfigureLevel("error"); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = logger.ConfigureLevel("info") })
		benchmarkLoggerObserverFormatting(b)
	})
	b.Run("enabled-zero-observers", func(b *testing.B) {
		benchmarkLoggerObserverFormatting(b)
	})
	b.Run("enabled-one-observer", func(b *testing.B) {
		stop := logger.ReplaceObserver("logger-observer-benchmark", func(logger.Entry) {})
		b.Cleanup(stop)
		benchmarkLoggerObserverFormatting(b)
	})
}

func benchmarkLoggerObserverFormatting(b *testing.B) {
	for b.Loop() {
		logger.Infof("request uri=%s status=%d", "/hello", 200)
	}
}

// BenchmarkExprMatched measures request-time expression evaluation for the
// shapes logger plugins use: no expression, an exact match, a single regexp
// condition, and multiple regexp alternatives. Regexp patterns are prepared
// once through the compiled store, mirroring plugin initialization.
func BenchmarkExprMatched(b *testing.B) {
	request := httptest.NewRequest(http.MethodGet, "/hello?source=bench", nil)
	request.Header.Set("X-Trace-Id", "abc-123")
	exact := []any{[]any{"$uri", "==", "/hello"}}
	oneRegexp := []any{[]any{"$uri", "~", "^/hello"}}
	multiRegexp := []any{
		[]any{"$uri", "~", "^/hello"},
		"AND",
		[]any{"$http_x_trace_id", "~", "^abc-[0-9]+$"},
	}
	for _, pattern := range []string{"^/hello", "^abc-[0-9]+$"} {
		preparedExprRegexps.Store(pattern, regexp.MustCompile(pattern))
	}
	cases := []struct {
		name        string
		expressions any
	}{
		{name: "no-expression", expressions: nil},
		{name: "exact-match", expressions: exact},
		{name: "one-regexp", expressions: oneRegexp},
		{name: "multi-regexp", expressions: multiRegexp},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				if !ExprMatched(request, tc.expressions, http.StatusOK) {
					b.Fatal("ExprMatched() = false, want true")
				}
			}
		})
	}
}
