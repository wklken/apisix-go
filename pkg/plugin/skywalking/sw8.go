package skywalking

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

func parseSW8(header string) (sw8Context, bool) {
	if header == "" {
		return sw8Context{}, false
	}
	parts := strings.Split(header, "-")
	if len(parts) != 8 {
		return sw8Context{}, false
	}

	traceID, err := decodeBase64URL(parts[1])
	if err != nil {
		return sw8Context{}, false
	}
	segmentID, err := decodeBase64URL(parts[2])
	if err != nil {
		return sw8Context{}, false
	}
	spanID := 0
	if _, err := fmt.Sscanf(parts[3], "%d", &spanID); err != nil {
		return sw8Context{}, false
	}
	parentService, _ := decodeBase64URL(parts[4])
	parentInstance, _ := decodeBase64URL(parts[5])
	parentEndpoint, _ := decodeBase64URL(parts[6])
	addressUsedAtClient, _ := decodeBase64URL(parts[7])

	return sw8Context{
		TraceID:              traceID,
		ParentTraceSegmentID: segmentID,
		ParentSpanID:         spanID,
		ParentService:        parentService,
		ParentInstance:       parentInstance,
		ParentEndpoint:       parentEndpoint,
		AddressUsedAtClient:  addressUsedAtClient,
	}, true
}

func (ctx sw8Context) header(service, instance, endpoint string) string {
	return strings.Join([]string{
		"1",
		encodeBase64URL(ctx.TraceID),
		encodeBase64URL(ctx.TraceSegmentID),
		fmt.Sprint(ctx.SpanID),
		encodeBase64URL(service),
		encodeBase64URL(instance),
		encodeBase64URL(endpoint),
		encodeBase64URL("apisix-go"),
	}, "-")
}

func decodeBase64URL(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return string(decoded), nil
	}
	decoded, err = base64.URLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func encodeBase64URL(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func randomID(reader io.Reader, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func randomUnit(reader io.Reader) (float64, error) {
	var raw [8]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return 0, fmt.Errorf("read random bytes: %w", err)
	}
	return float64(binary.BigEndian.Uint64(raw[:])>>11) / (1 << 53), nil
}

func intFromAttr(attr map[string]any, key string) int {
	value, ok := attr[key]
	if !ok {
		return 0
	}
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case uint64:
		return int(v)
	default:
		return 0
	}
}
