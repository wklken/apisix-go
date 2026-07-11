package ai_stream

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"

	"github.com/wklken/apisix-go/pkg/json"
)

const maxAWSEventStreamFrameSize = 16 * 1024 * 1024

func ForwardAWSEventStream(w http.ResponseWriter, body io.Reader, maxBytes int64) (Usage, error) {
	usage := Usage{Raw: make(map[string]any), PromptTokens: -1, CompletionTokens: -1}
	var totalBytes int64
	for {
		prelude := make([]byte, 12)
		_, err := io.ReadFull(body, prelude)
		if err != nil {
			if err == io.EOF {
				return usage, nil
			}
			if err == io.ErrUnexpectedEOF {
				return usage, fmt.Errorf("truncated AWS EventStream prelude")
			}
			return usage, err
		}
		totalLength := binary.BigEndian.Uint32(prelude[:4])
		headersLength := binary.BigEndian.Uint32(prelude[4:8])
		if totalLength < 16 || totalLength > maxAWSEventStreamFrameSize {
			return usage, fmt.Errorf("invalid AWS EventStream frame length %d", totalLength)
		}
		if headersLength > totalLength-16 {
			return usage, fmt.Errorf("AWS EventStream headers exceed frame body")
		}
		if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
			return usage, fmt.Errorf("AWS EventStream prelude CRC mismatch")
		}
		totalBytes += int64(totalLength)
		if maxBytes > 0 && totalBytes > maxBytes {
			return usage, fmt.Errorf("max_response_bytes exceeded")
		}
		remainder := make([]byte, int(totalLength)-12)
		if _, err := io.ReadFull(body, remainder); err != nil {
			return usage, fmt.Errorf("read AWS EventStream frame: %w", err)
		}
		frame := append(prelude, remainder...)
		messageCRC := binary.BigEndian.Uint32(frame[len(frame)-4:])
		if crc32.ChecksumIEEE(frame[:len(frame)-4]) != messageCRC {
			return usage, fmt.Errorf("AWS EventStream message CRC mismatch")
		}
		headers, err := parseAWSEventStreamHeaders(frame[12 : 12+headersLength])
		if err != nil {
			return usage, err
		}
		payload := frame[12+headersLength : len(frame)-4]
		terminal := mergeBedrockEventStreamUsage(&usage, headers, payload)
		if _, err := w.Write(frame); err != nil {
			return usage, err
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if terminal {
			return usage, nil
		}
	}
}

func parseAWSEventStreamHeaders(data []byte) (map[string]any, error) {
	headers := make(map[string]any)
	for position := 0; position < len(data); {
		nameLength := int(data[position])
		position++
		if position+nameLength+1 > len(data) {
			return nil, fmt.Errorf("truncated AWS EventStream header")
		}
		name := string(data[position : position+nameLength])
		position += nameLength
		valueType := data[position]
		position++
		value, consumed, err := decodeAWSEventStreamHeaderValue(valueType, data[position:])
		if err != nil {
			return nil, fmt.Errorf("decode AWS EventStream header %q: %w", name, err)
		}
		position += consumed
		headers[name] = value
	}
	return headers, nil
}

func decodeAWSEventStreamHeaderValue(valueType byte, data []byte) (any, int, error) {
	switch valueType {
	case 0:
		return true, 0, nil
	case 1:
		return false, 0, nil
	case 2:
		if len(data) < 1 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		return data[0], 1, nil
	case 3:
		if len(data) < 2 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		return binary.BigEndian.Uint16(data[:2]), 2, nil
	case 4:
		if len(data) < 4 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		return binary.BigEndian.Uint32(data[:4]), 4, nil
	case 5, 8:
		if len(data) < 8 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		return append([]byte(nil), data[:8]...), 8, nil
	case 6, 7:
		if len(data) < 2 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		length := int(binary.BigEndian.Uint16(data[:2]))
		if len(data) < 2+length {
			return nil, 0, io.ErrUnexpectedEOF
		}
		value := append([]byte(nil), data[2:2+length]...)
		if valueType == 7 {
			return string(value), 2 + length, nil
		}
		return value, 2 + length, nil
	case 9:
		if len(data) < 16 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		return append([]byte(nil), data[:16]...), 16, nil
	default:
		return nil, 0, fmt.Errorf("unknown value type %d", valueType)
	}
}

func mergeBedrockEventStreamUsage(usage *Usage, headers map[string]any, payload []byte) bool {
	messageType, _ := headers[":message-type"].(string)
	if messageType == "exception" || messageType == "error" {
		return true
	}
	eventType, _ := headers[":event-type"].(string)
	if eventType == "messageStop" {
		return false
	}
	if eventType != "metadata" {
		return false
	}
	var metadata struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(payload, &metadata) != nil || metadata.Usage == nil {
		return true
	}
	for key, value := range metadata.Usage {
		usage.Raw[key] = value
	}
	usage.PromptTokens = numericUsage(metadata.Usage["inputTokens"])
	usage.CompletionTokens = numericUsage(metadata.Usage["outputTokens"])
	return true
}
