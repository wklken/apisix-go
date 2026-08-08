package ai_stream

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

const maxAWSEventStreamFrameSize = 16 * 1024 * 1024

func ForwardAWSEventStream(w http.ResponseWriter, body io.Reader, maxBytes int64) (Usage, error) {
	usage := Usage{Raw: make(map[string]any), PromptTokens: -1, CompletionTokens: -1}
	decoder := eventstream.NewDecoder()
	var totalBytes int64
	for {
		limited := &io.LimitedReader{R: body, N: maxAWSEventStreamFrameSize + 1}
		var raw bytes.Buffer
		message, err := decoder.Decode(io.TeeReader(limited, &raw), nil)
		if errors.Is(err, io.EOF) {
			return usage, nil
		}
		if err != nil {
			return usage, fmt.Errorf("decode AWS EventStream: %w", err)
		}
		if limited.N <= 0 {
			return usage, fmt.Errorf("invalid AWS EventStream frame length")
		}
		frame := raw.Bytes()
		totalBytes += int64(len(frame))
		if maxBytes > 0 && totalBytes > maxBytes {
			return usage, fmt.Errorf("max_response_bytes exceeded")
		}
		terminal := mergeBedrockEventStreamUsage(&usage, message)
		if _, err := w.Write(frame); err != nil {
			return usage, fmt.Errorf("%w: %v", ErrClientDisconnected, err)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if terminal {
			return usage, nil
		}
	}
}

func headerString(headers eventstream.Headers, name string) string {
	value := headers.Get(name)
	if value == nil {
		return ""
	}
	got, _ := value.Get().(string)
	return got
}

func mergeBedrockEventStreamUsage(usage *Usage, message eventstream.Message) bool {
	messageType := headerString(message.Headers, ":message-type")
	if messageType == "exception" || messageType == "error" {
		return true
	}
	eventType := headerString(message.Headers, ":event-type")
	if eventType == "contentBlockDelta" {
		var content struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(message.Payload, &content) == nil {
			usage.AppendText(content.Delta.Text)
		}
		return false
	}
	if eventType == "messageStop" {
		return false
	}
	if eventType != "metadata" {
		return false
	}
	var metadata struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(message.Payload, &metadata) != nil || metadata.Usage == nil {
		return true
	}
	maps.Copy(usage.Raw, metadata.Usage)
	usage.PromptTokens = ai_protocols.NumericUsage(metadata.Usage["inputTokens"], false)
	usage.CompletionTokens = ai_protocols.NumericUsage(metadata.Usage["outputTokens"], false)
	return true
}
