package base

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"runtime"
	"testing"

	brotli "github.com/andybalholm/brotli"
	"github.com/wklken/apisix-go/pkg/logger"
)

// Benchmark corpus for request-body capture and shared response-body capture
// used by logger plugins. Payloads are deterministic; the response capture
// fixtures are measured at their cold (per-request) and cached (adjacent
// logger reuse) phases.

var benchmarkBodyPayloads = map[string][]byte{
	"1KiB":  bytes.Repeat([]byte("x"), 1<<10),
	"64KiB": bytes.Repeat([]byte("x"), 64<<10),
	"1MiB":  bytes.Repeat([]byte("x"), 1<<20),
}

var benchmarkBodySizes = []struct {
	name string
	size int
}{
	{name: "1KiB", size: 1 << 10},
	{name: "64KiB", size: 64 << 10},
	{name: "1MiB", size: 1 << 20},
}

// benchmarkReadCloser is a resettable request body so the read is restarted
// from the beginning without constructing a new request each round.
type benchmarkReadCloser struct {
	reader bytes.Reader
}

func (r *benchmarkReadCloser) Reset(body []byte) {
	r.reader.Reset(body)
}

func (r *benchmarkReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (*benchmarkReadCloser) Close() error {
	return nil
}

// benchmarkWriter only accumulates bytes so the response write is measured
// without the overhead of an httptest.ResponseRecorder.
type benchmarkWriter struct {
	bytes int
}

func (w *benchmarkWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	return len(p), nil
}

// Header is never touched by the benchmarked write path.
func (*benchmarkWriter) Header() http.Header {
	return nil
}

func (*benchmarkWriter) WriteHeader(int) {}

func BenchmarkReadAndRestoreRequestBody(b *testing.B) {
	for _, spec := range benchmarkBodySizes {
		b.Run("size="+spec.name, func(b *testing.B) {
			benchmarkReadAndRestoreRequestBody(b, benchmarkBodyPayloads[spec.name])
		})
	}
}

func benchmarkReadAndRestoreRequestBody(b *testing.B, body []byte) {
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	reader := &benchmarkReadCloser{}
	var request http.Request
	var sink string
	for b.Loop() {
		reader.Reset(body)
		request = http.Request{Method: http.MethodPost, Body: reader}
		bodyText, err := ReadAndRestoreRequestBody(&request, 0)
		if err != nil {
			b.Fatal(err)
		}
		sink = bodyText
	}
	runtime.KeepAlive(sink)
}

func BenchmarkReadSharedRequestBody(b *testing.B) {
	for _, loggers := range []int{1, 3} {
		for _, spec := range benchmarkBodySizes {
			b.Run(fmt.Sprintf("loggers=%d/size=%s", loggers, spec.name), func(b *testing.B) {
				benchmarkReadSharedRequestBody(b, benchmarkBodyPayloads[spec.name], loggers)
			})
		}
	}
}

func benchmarkReadSharedRequestBody(b *testing.B, body []byte, loggers int) {
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	reader := &benchmarkReadCloser{}
	reader.Reset(body)
	request := http.Request{Method: http.MethodPost, Body: reader}
	if _, err := ReadSharedRequestBody(&request, 0); err != nil {
		b.Fatal(err)
	}
	var sink string
	for b.Loop() {
		for range loggers {
			bodyText, err := ReadSharedRequestBody(&request, 0)
			if err != nil {
				b.Fatal(err)
			}
			sink = bodyText
		}
	}
	runtime.KeepAlive(sink)
}

func BenchmarkSharedResponseRecorderWrite(b *testing.B) {
	for _, spec := range benchmarkBodySizes {
		b.Run("size="+spec.name, func(b *testing.B) {
			benchmarkSharedResponseRecorderWrite(b, benchmarkBodyPayloads[spec.name])
		})
	}
}

func benchmarkSharedResponseRecorderWrite(b *testing.B, payload []byte) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	writer := &benchmarkWriter{}
	var sink int
	for b.Loop() {
		recorder := NewSharedResponseRecorder(writer)
		n, err := recorder.Write(payload)
		if err != nil {
			b.Fatal(err)
		}
		sink += n
	}
	runtime.KeepAlive(sink)
}

func BenchmarkSharedResponseRecorderBody(b *testing.B) {
	for _, phase := range []string{"cold", "cached"} {
		for _, loggers := range []int{1, 3} {
			for _, spec := range benchmarkBodySizes {
				b.Run(fmt.Sprintf("phase=%s/loggers=%d/size=%s", phase, loggers, spec.name), func(b *testing.B) {
					benchmarkSharedResponseRecorderBody(b, benchmarkBodyPayloads[spec.name], phase, loggers)
				})
			}
		}
	}
}

func benchmarkSharedResponseRecorderBody(b *testing.B, payload []byte, phase string, loggers int) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	recorder := NewSharedResponseRecorder(&benchmarkWriter{})
	if _, err := recorder.Write(payload); err != nil {
		b.Fatal(err)
	}
	capture := recorder.capture
	if phase == "cached" {
		if got := recorder.Body(); got == "" {
			b.Fatal("unexpected empty captured body")
		}
	}
	var sink string
	for b.Loop() {
		if phase == "cold" {
			capture.bodyText = ""
			capture.bodyTextLen = 0
		}
		for range loggers {
			bodyText := recorder.Body()
			sink = bodyText
		}
	}
	runtime.KeepAlive(sink)
}

func BenchmarkSharedResponseRecorderDecoded(b *testing.B) {
	for _, encoding := range []string{"identity", "gzip", "br"} {
		for _, phase := range []string{"cold", "cached"} {
			for _, loggers := range []int{1, 3} {
				for _, spec := range benchmarkBodySizes {
					b.Run(
						fmt.Sprintf("encoding=%s/phase=%s/loggers=%d/size=%s", encoding, phase, loggers, spec.name),
						func(b *testing.B) {
							benchmarkSharedResponseRecorderDecoded(
								b,
								benchmarkBodyPayloads[spec.name],
								encoding,
								phase,
								loggers,
							)
						},
					)
				}
			}
		}
	}
}

func benchmarkSharedResponseRecorderDecoded(b *testing.B, payload []byte, encoding, phase string, loggers int) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	recorder := NewSharedResponseRecorder(&benchmarkWriter{})
	capture := recorder.capture
	switch encoding {
	case "identity":
		capture.buf.Write(payload)
	case "gzip":
		capture.buf.Write(compressBenchmarkGzip(b, payload))
	case "br":
		capture.buf.Write(compressBenchmarkBrotli(b, payload))
	default:
		b.Fatalf("unknown encoding %q", encoding)
	}
	if phase == "cached" {
		if got := recorder.BodyDecoded(0, encoding); got == "" {
			b.Fatal("unexpected empty decoded body")
		}
	}
	var sink string
	for b.Loop() {
		if phase == "cold" {
			capture.decodedBody = ""
			capture.decodedEncoding = ""
			capture.decodedReady = false
		}
		for range loggers {
			bodyText := recorder.BodyDecoded(0, encoding)
			sink = bodyText
		}
	}
	runtime.KeepAlive(sink)
}

func compressBenchmarkGzip(b *testing.B, payload []byte) []byte {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(payload); err != nil {
		b.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

func compressBenchmarkBrotli(b *testing.B, payload []byte) []byte {
	var buf bytes.Buffer
	writer := brotli.NewWriter(&buf)
	if _, err := writer.Write(payload); err != nil {
		b.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

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
