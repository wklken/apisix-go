package ai_stream

import (
	"bytes"
	"encoding/binary"
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
	if _, limited := body.(*durationReader); limited {
		return forwardTimedAWSEventStream(w, body, maxBytes, usage)
	}
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

// Duration is measured at upstream read boundaries. Buffer incomplete frames
// only for usage extraction; forwarding must never wait for the next frame part.
func forwardTimedAWSEventStream(w http.ResponseWriter, body io.Reader, maxBytes int64, usage Usage) (Usage, error) {
	chunk := make([]byte, 32*1024)
	var pending []byte
	var total int64
	decoder := eventstream.NewDecoder()
	for {
		n, readErr := body.Read(chunk)
		terminal := false
		if n > 0 {
			total += int64(n)
			if maxBytes > 0 && total > maxBytes {
				return usage, fmt.Errorf("max_response_bytes exceeded")
			}
			pending = append(pending, chunk[:n]...)
			for len(pending) >= 4 {
				size := int(binary.BigEndian.Uint32(pending[:4]))
				if size < 16 || size > maxAWSEventStreamFrameSize {
					return usage, fmt.Errorf("invalid AWS EventStream frame length")
				}
				if len(pending) < size {
					break
				}
				message, err := decoder.Decode(bytes.NewReader(pending[:size]), nil)
				if err != nil {
					return usage, fmt.Errorf("decode AWS EventStream: %w", err)
				}
				terminal = mergeBedrockEventStreamUsage(&usage, message) || terminal
				pending = pending[size:]
			}
			if _, err := w.Write(chunk[:n]); err != nil {
				return usage, fmt.Errorf("%w: %v", ErrClientDisconnected, err)
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			// The wrapper records the crossing read before forwarding. Check it even
			// for terminal metadata, without issuing another underlying Read.
			if body.(*durationReader).exceeded {
				return usage, ErrMaxStreamDuration
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return usage, nil
			}
			return usage, readErr
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
