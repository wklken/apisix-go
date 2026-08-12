package base

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/andybalholm/brotli"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
)

func TestBaseLoggerRunLogPhaseUsesBoundedBatchQueue(t *testing.T) {
	started := make(chan struct{})
	processor := logger_batch.NewWithContext(logger_batch.Config{
		Name:              "test",
		BatchMaxSize:      100,
		MaxPendingEntries: 1,
		BufferDuration:    0,
		InactiveTimeout:   0,
		ShutdownTimeout:   1,
	}, func(context.Context, []map[string]any, int) (int, error) {
		<-started
		return 0, nil
	})
	defer func() {
		close(started)
		processor.Stop()
	}()

	plugin := &BaseLoggerPlugin{BatchProcessor: processor, LogFormat: map[string]string{"method": "$request_method"}}
	snapshot := LogSnapshot{Request: apisixlog.RequestLogSnapshot{Method: http.MethodPost}}
	if err := plugin.RunLogPhase(snapshot); err != nil {
		t.Fatalf("first RunLogPhase() error = %v", err)
	}
	if err := plugin.RunLogPhase(snapshot); !errors.Is(err, ErrLogQueueFull) {
		t.Fatalf("second RunLogPhase() error = %v, want ErrLogQueueFull", err)
	}
}

func TestBaseLoggerLogCapturePolicyIncludesExtraFormatBodies(t *testing.T) {
	plugin := &BaseLoggerPlugin{
		SnapshotLogFormatExtra: map[string]any{
			"request":  map[string]any{"body": "$request_body"},
			"response": map[string]any{"body": "$response_body"},
		},
	}

	policy := plugin.LogCapturePolicy()
	if policy.RequestBodyBytes != MAX_REQ_BODY {
		t.Fatalf("request body limit = %d, want %d", policy.RequestBodyBytes, MAX_REQ_BODY)
	}
	if policy.ResponseBodyBytes != MAX_RESP_BODY {
		t.Fatalf("response body limit = %d, want %d", policy.ResponseBodyBytes, MAX_RESP_BODY)
	}
}

func TestLogCapturePolicyForFormatsIncludesNestedAndStringBodies(t *testing.T) {
	policy := LogCapturePolicyForFormats(
		17,
		23,
		map[string]any{"nested": map[string]any{"body": "$request_body"}},
		map[string]string{"response": "$response_body"},
	)
	if policy.RequestBodyBytes != 17 || policy.ResponseBodyBytes != 23 {
		t.Fatalf("policy = %#v, want request=17 response=23", policy)
	}
}

func TestSnapshotExpressionMatchesStatus(t *testing.T) {
	snapshot := LogSnapshot{Outcome: apisixctx.ResponseOutcome{Status: http.StatusCreated}}
	if !SnapshotExpressionMatches(snapshot, [][]any{{"$status", "==", 201}}) {
		t.Fatal("status expression did not match")
	}
}

func TestSnapshotExpressionMatchesPreservesLegacyNestedOperators(t *testing.T) {
	snapshot := LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Header: http.Header{"X-Environment": []string{"production"}},
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusCreated},
	}
	expressions := [][]any{
		{"$http_x_environment", "==", "production"},
		{"AND"},
		{"$status", "==", http.StatusCreated},
	}
	if !SnapshotExpressionMatches(snapshot, expressions) {
		t.Fatal("snapshot expression rejected the legacy nested operator shape")
	}
}

func TestSnapshotResponseBodyStopsDecodingAtBindingLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("expanded-sensitive-body"), 1<<15)
	tests := []struct {
		name     string
		encoding string
		compress func(*testing.T, []byte) []byte
	}{
		{
			name:     "gzip",
			encoding: "gzip",
			compress: func(t *testing.T, body []byte) []byte {
				t.Helper()
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write(body); err != nil {
					t.Fatalf("gzip write: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("gzip close: %v", err)
				}
				return compressed.Bytes()[:compressed.Len()-4]
			},
		},
		{
			name:     "brotli",
			encoding: "br",
			compress: func(t *testing.T, body []byte) []byte {
				t.Helper()
				var compressed bytes.Buffer
				writer := brotli.NewWriter(&compressed)
				if _, err := writer.Write(body[:128]); err != nil {
					t.Fatalf("brotli write: %v", err)
				}
				if err := writer.Flush(); err != nil {
					t.Fatalf("brotli flush: %v", err)
				}
				if _, err := writer.Write(body[128:]); err != nil {
					t.Fatalf("brotli write remainder: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("brotli close: %v", err)
				}
				return compressed.Bytes()[:compressed.Len()-1]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := LogSnapshot{}
			snapshot.Response.Header = http.Header{"Content-Encoding": []string{test.encoding}}
			snapshot.Response.Body = test.compress(t, payload)
			if got, want := SnapshotResponseBody(snapshot, 32), string(payload[:32]); got != want {
				t.Fatalf("SnapshotResponseBody() = %q, want decoded prefix %q", got, want)
			}
		})
	}
}
