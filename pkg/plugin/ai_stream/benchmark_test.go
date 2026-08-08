package ai_stream

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

// BenchmarkStreamUsageAccumulation measures per-chunk usage text accumulation
// (Usage.AppendText) at 100 chunks of 100B (10KiB total payload).
func BenchmarkStreamUsageAccumulation(b *testing.B) {
	chunk := strings.Repeat("a", 100)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var usage Usage
		for range 100 {
			usage.AppendText(chunk)
		}
		if len(usage.Text) == 0 {
			b.Fatal("accumulation produced no text")
		}
	}
}

// streamSizes are the streaming payload sizes measured by BenchmarkAIStreaming;
// transform cost must scale linearly across them.
var streamSizes = []int{1 << 10, 64 << 10, 1 << 20}

// BenchmarkAIStreaming measures per-stream transform cost at 1 KiB, 64 KiB,
// and 1 MiB for the SSE forwarder, the Anthropic conversion forwarder, and the
// AWS EventStream forwarder.
func BenchmarkAIStreaming(b *testing.B) {
	b.Run("forward-sse", func(b *testing.B) {
		for _, size := range streamSizes {
			b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
				body := buildOpenAIStream(size)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for i := 0; i < b.N; i++ {
					rr := httptest.NewRecorder()
					if _, err := ForwardSSE(rr, strings.NewReader(body), ai_protocols.OpenAIChat, 0); err != nil {
						b.Fatalf("ForwardSSE() error = %v", err)
					}
					if rr.Body.Len() != len(body) {
						b.Fatalf("forwarded %d bytes, want %d", rr.Body.Len(), len(body))
					}
				}
			})
		}
	})

	b.Run("forward-anthropic", func(b *testing.B) {
		for _, size := range streamSizes {
			b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
				body := buildOpenAIStream(size)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for i := 0; i < b.N; i++ {
					rr := httptest.NewRecorder()
					if _, err := ForwardOpenAIAsAnthropicSSE(rr, strings.NewReader(body), 0, nil); err != nil {
						b.Fatalf("ForwardOpenAIAsAnthropicSSE() error = %v", err)
					}
					if rr.Body.Len() == 0 {
						b.Fatal("converted stream is empty")
					}
				}
			})
		}
	})

	b.Run("forward-aws", func(b *testing.B) {
		for _, size := range streamSizes {
			b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
				frames := buildAWSStream(size)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for i := 0; i < b.N; i++ {
					rr := httptest.NewRecorder()
					if _, err := ForwardAWSEventStream(rr, strings.NewReader(frames), 0); err != nil {
						b.Fatalf("ForwardAWSEventStream() error = %v", err)
					}
					if rr.Body.Len() == 0 {
						b.Fatal("forwarded event stream is empty")
					}
				}
			})
		}
	})
}

// buildOpenAIStream builds an OpenAI chat SSE body of at least size bytes with
// evenly spaced text deltas.
func buildOpenAIStream(size int) string {
	var body strings.Builder
	chunk := strings.Repeat("x", 64)
	for body.Len() < size {
		body.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"")
		body.WriteString(chunk)
		body.WriteString("\"}}]}\n\n")
	}
	return body.String()
}

// buildAWSStream builds an AWS EventStream body of at least size bytes from
// contentBlockDelta frames.
func buildAWSStream(size int) string {
	var body bytes.Buffer
	payload := `{"delta":{"text":"` + strings.Repeat("x", 64) + `"}}`
	for body.Len() < size {
		body.Write(encodeAWSFrame(":event-type", "contentBlockDelta"))
		body.Write(encodeAWSFrame(":content-type", "application/json"))
		body.Write(encodeAWSFrame("payload", payload))
	}
	return body.String()
}

// encodeAWSFrame encodes one AWS EventStream message whose single header is
// name=value and whose payload is value.
func encodeAWSFrame(name string, value string) []byte {
	headerName := []byte(name)
	headerValue := []byte(value)
	var headers bytes.Buffer
	headers.WriteByte(byte(len(headerName)))
	headers.WriteString(name)
	headers.WriteByte(7) // string header value type
	_ = binary.Write(&headers, binary.BigEndian, uint16(len(headerValue)))
	headers.WriteString(value)
	totalLength := 16 + headers.Len() + len(headerValue)
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(headers.Len()))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headers.Bytes()...)
	frame = append(frame, headerValue...)
	messageCRC := crc32.ChecksumIEEE(frame)
	return append(frame, byte(messageCRC>>24), byte(messageCRC>>16), byte(messageCRC>>8), byte(messageCRC))
}
