package ai_stream

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"net/http/httptest"
	"testing"
)

func TestForwardAWSEventStreamPreservesFramesAndExtractsUsage(t *testing.T) {
	content := awsEventStreamFrame(map[string]string{
		":message-type": "event", ":event-type": "contentBlockDelta",
	}, `{"delta":{"text":"hello"}}`)
	metadata := awsEventStreamFrame(map[string]string{
		":message-type": "event", ":event-type": "metadata",
	}, `{"usage":{"inputTokens":4,"outputTokens":2,"totalTokens":6}}`)
	body := append(content, metadata...)
	rr := httptest.NewRecorder()

	usage, err := ForwardAWSEventStream(rr, bytes.NewReader(body), 0)
	if err != nil {
		t.Fatalf("ForwardAWSEventStream() error = %v", err)
	}
	if !bytes.Equal(rr.Body.Bytes(), body) {
		t.Fatal("forwarded AWS EventStream bytes changed")
	}
	if !rr.Flushed {
		t.Fatal("AWS EventStream response was not flushed")
	}
	if usage.PromptTokens != 4 || usage.CompletionTokens != 2 || usage.Raw["totalTokens"] != float64(6) ||
		usage.Text != "hello" {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestForwardAWSEventStreamRejectsBadCRC(t *testing.T) {
	frame := awsEventStreamFrame(map[string]string{":event-type": "metadata"}, `{}`)
	frame[len(frame)-1] ^= 0xff
	if _, err := ForwardAWSEventStream(httptest.NewRecorder(), bytes.NewReader(frame), 0); err == nil {
		t.Fatal("ForwardAWSEventStream() error = nil, want CRC error")
	}
}

func awsEventStreamFrame(headers map[string]string, payload string) []byte {
	headerBytes := make([]byte, 0)
	for name, value := range headers {
		headerBytes = append(headerBytes, byte(len(name)))
		headerBytes = append(headerBytes, name...)
		headerBytes = append(headerBytes, 7)
		length := make([]byte, 2)
		binary.BigEndian.PutUint16(length, uint16(len(value)))
		headerBytes = append(headerBytes, length...)
		headerBytes = append(headerBytes, value...)
	}
	totalLength := 16 + len(headerBytes) + len(payload)
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headerBytes)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headerBytes...)
	frame = append(frame, payload...)
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(frame))
	return append(frame, crc...)
}
