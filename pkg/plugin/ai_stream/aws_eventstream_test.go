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

func TestForwardAWSEventStreamPreservesAllHeaderValueTypes(t *testing.T) {
	headerBytes := make([]byte, 0)
	typedHeaders := []struct {
		name string
		typ  byte
		data []byte
	}{
		{":message-type", 7, append([]byte{0x00, 0x05}, "event"...)},
		{":event-type", 7, append([]byte{0x00, 0x08}, "metadata"...)},
		{"bool-true", 0, nil},
		{"bool-false", 1, nil},
		{"byte", 2, []byte{0x7f}},
		{"int16", 3, []byte{0x12, 0x34}},
		{"int32", 4, []byte{0x12, 0x34, 0x56, 0x78}},
		{"int64", 5, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}},
		{"bytes", 6, []byte{0x00, 0x03, 0xaa, 0xbb, 0xcc}},
		{"timestamp", 8, []byte{0x00, 0x00, 0x01, 0x8d, 0x9e, 0x4f, 0x4b, 0x00}},
		{"uuid", 9, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
	}
	for _, header := range typedHeaders {
		headerBytes = append(headerBytes, byte(len(header.name)))
		headerBytes = append(headerBytes, header.name...)
		headerBytes = append(headerBytes, header.typ)
		headerBytes = append(headerBytes, header.data...)
	}
	payload := []byte(`{"usage":{"inputTokens":4,"outputTokens":2,"totalTokens":6}}`)
	totalLength := 16 + len(headerBytes) + len(payload)
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headerBytes)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headerBytes...)
	frame = append(frame, payload...)
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(frame))
	frame = append(frame, crc...)

	rr := httptest.NewRecorder()
	usage, err := ForwardAWSEventStream(rr, bytes.NewReader(frame), 0)
	if err != nil {
		t.Fatalf("ForwardAWSEventStream() error = %v", err)
	}
	if !bytes.Equal(rr.Body.Bytes(), frame) {
		t.Fatal("forwarded AWS EventStream bytes changed")
	}
	if usage.PromptTokens != 4 || usage.CompletionTokens != 2 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestForwardAWSEventStreamRejectsOversizedFrame(t *testing.T) {
	body := make([]byte, maxAWSEventStreamFrameSize+1)
	binary.BigEndian.PutUint32(body[:4], uint32(maxAWSEventStreamFrameSize+1))
	if _, err := ForwardAWSEventStream(httptest.NewRecorder(), bytes.NewReader(body), 0); err == nil {
		t.Fatal("ForwardAWSEventStream() error = nil, want oversized frame rejection")
	}
}

func TestForwardAWSEventStreamRejectsTruncatedFrame(t *testing.T) {
	frame := awsEventStreamFrame(map[string]string{":event-type": "metadata"}, `{}`)
	if _, err := ForwardAWSEventStream(httptest.NewRecorder(), bytes.NewReader(frame[:10]), 0); err == nil {
		t.Fatal("ForwardAWSEventStream() error = nil, want truncated frame rejection")
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
