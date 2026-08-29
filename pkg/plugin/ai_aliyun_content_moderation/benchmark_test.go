package ai_aliyun_content_moderation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/testutil"
)

// streamChunkSize is the per-Write chunk size used by BenchmarkAIStreaming to
// model real network packets.
const streamChunkSize = 4 << 10

// BenchmarkAIStreaming measures the realtime SSE streaming transform at 1 KiB,
// 64 KiB, and 1 MiB payloads: content extraction, cross-chunk matching state,
// and risk moderation. Transform cost must scale linearly with the payload
// size; with a large stream cache the per-chunk content re-scan must not grow
// the total cost quadratically.
func BenchmarkAIStreaming(b *testing.B) {
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
	}))
	defer moderation.Close()

	for _, size := range []int{1 << 10, 64 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			p := &Plugin{config: Config{
				Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
				CheckResponse: true, StreamCheckMode: "realtime",
				StreamCheckCacheSize: 1 << 20, StreamCheckInterval: 1e9,
				ResponseCheckLengthLimit: size, RiskLevelBar: "high",
			}}
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
			stream := buildSSEStream(size)
			requestBody := map[string]any{"model": "gpt", "stream": true}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))

			b.ReportAllocs()
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				rr := httptest.NewRecorder()
				writer := newRealtimeResponseWriter(rr, req, p, ai_protocols.OpenAIChat, requestBody)
				written := 0
				for offset := 0; offset < len(stream); offset += streamChunkSize {
					end := min(offset+streamChunkSize, len(stream))
					n, err := writer.Write([]byte(stream[offset:end]))
					if err != nil {
						b.Fatalf("realtime writer Write() error = %v", err)
					}
					written += n
				}
				writer.Close()
				if written != len(stream) {
					b.Fatalf("moderated stream wrote %d bytes, want %d", written, len(stream))
				}
			}
		})
	}
}

// buildSSEStream builds an OpenAI chat SSE response body of at least size
// bytes with evenly spaced text deltas.
func buildSSEStream(size int) string {
	var body strings.Builder
	chunk := strings.Repeat("x", 64)
	for body.Len() < size {
		body.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"")
		body.WriteString(chunk)
		body.WriteString("\"}}]}\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	return body.String()
}
