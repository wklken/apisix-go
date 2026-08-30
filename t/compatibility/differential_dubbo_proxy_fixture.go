package pluginintegration

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/apache/dubbo-go-hessian2"
)

const differentialDubboProxyProtocolVersion = "2.0.2"

const (
	differentialFixtureWireDubboProxyHessian2 = "dubbo-proxy-hessian2"
	differentialDubboProxyWireMethod          = "DUBBO"
	differentialDubboProxyParamsTypeHeader    = "X-Dubbo-Params-Type-Desc"
	differentialDubboProxyHTTPHostHeader      = "X-Dubbo-HTTP-Host"
	differentialDubboProxyHTTPBodyHeader      = "X-Dubbo-HTTP-Body"
	differentialDubboProxyMaxPayloadBytes     = 1 << 20
)

type differentialDubboProxyInvocation struct {
	RequestID       uint64              `json:"request_id"`
	ProtocolVersion string              `json:"protocol_version"`
	ServiceName     string              `json:"service_name"`
	ServiceVersion  string              `json:"service_version"`
	Method          string              `json:"method"`
	ParamsTypeDesc  string              `json:"params_type_desc"`
	HTTPContext     map[string][]string `json:"http_context"`
	HTTPBody        string              `json:"http_body"`
	Attachments     map[string][]string `json:"attachments"`
}

func differentialDubboProxyFixtureUsesHostOracle(fixture DifferentialFixture) bool {
	return fixture.WireProtocol == differentialFixtureWireDubboProxyHessian2
}

func startDifferentialDubboProxyFixture(
	spec DifferentialFixture,
) (*differentialFixtureServer, error) {
	if spec.WireProtocol != differentialFixtureWireDubboProxyHessian2 {
		return nil, fmt.Errorf("dubbo-proxy fixture wire protocol = %q", spec.WireProtocol)
	}
	if spec.ExpectedCalls != 1 {
		return nil, fmt.Errorf("dubbo-proxy fixture expected_calls = %d, want 1", spec.ExpectedCalls)
	}
	if spec.Response.Status != http.StatusOK ||
		spec.Response.Headers["Got-extra-arg-k"] != differentialDubboProxyHeaderValue ||
		spec.Response.Body != differentialDubboProxyResponseBody {
		return nil, errors.New("dubbo-proxy fixture requires the pinned APISIX 3.17 TEST 3 response")
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic dubbo-proxy fixture: %w", err)
	}
	fixture, err := newDifferentialRawFixture(spec, listener)
	if err != nil {
		return nil, err
	}
	fixture.serveWG.Add(1)
	go fixture.serveDubboProxy()
	return fixture, nil
}

func (fixture *differentialFixtureServer) serveDubboProxy() {
	defer fixture.serveWG.Done()
	for {
		connection, err := fixture.listener.Accept()
		if err != nil {
			return
		}
		fixture.connectionWG.Go(func() {
			defer func() { _ = connection.Close() }()
			_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
			fixture.serveDubboProxyConnection(bufio.NewReader(connection), connection)
		})
	}
}

func (fixture *differentialFixtureServer) serveDubboProxyConnection(
	reader *bufio.Reader,
	connection net.Conn,
) {
	prefix, err := reader.Peek(2)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			fixture.reportError(fmt.Errorf("sniff dubbo-proxy fixture connection: %w", err))
		}
		return
	}
	if prefix[0] != 0xda || prefix[1] != 0xbb {
		fixture.captureHTTPRequest(reader, connection)
		return
	}
	frame, err := readDifferentialDubboProxyFrame(reader)
	if err != nil {
		fixture.reportError(fmt.Errorf("read dubbo-proxy fixture frame: %w", err))
		return
	}
	invocation, err := decodeDifferentialDubboProxyInvocation(frame)
	if err != nil {
		fixture.reportError(err)
		return
	}
	if err := validateDifferentialDubboProxyInvocation(invocation); err != nil {
		fixture.reportError(err)
		return
	}
	encoded, err := json.Marshal(invocation)
	if err != nil {
		fixture.reportError(fmt.Errorf("marshal dubbo-proxy invocation: %w", err))
		return
	}
	fixture.capture(differentialCapturedRequest{
		Method: differentialDubboProxyWireMethod,
		Path:   invocation.ServiceName + "/" + invocation.Method,
		Host:   invocation.ServiceVersion,
		Headers: http.Header{
			differentialDubboProxyParamsTypeHeader: []string{invocation.ParamsTypeDesc},
			differentialDubboProxyHTTPHostHeader:   append([]string(nil), invocation.HTTPContext["host"]...),
			differentialDubboProxyHTTPBodyHeader:   []string{invocation.HTTPBody},
			"Extra-Arg-K":                          append([]string(nil), invocation.HTTPContext["extra-arg-k"]...),
		},
		Body: string(encoded),
	})
	response, err := buildDifferentialDubboProxyResponseFrame(invocation.RequestID)
	if err != nil {
		fixture.reportError(err)
		return
	}
	if _, err := connection.Write(response); err != nil {
		fixture.reportError(fmt.Errorf("write dubbo-proxy fixture response: %w", err))
	}
}

func readDifferentialDubboProxyFrame(reader io.Reader) ([]byte, error) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	payloadLength := int(binary.BigEndian.Uint32(header[12:16]))
	if payloadLength <= 0 || payloadLength > differentialDubboProxyMaxPayloadBytes {
		return nil, fmt.Errorf(
			"dubbo payload length = %d, want 1..%d",
			payloadLength,
			differentialDubboProxyMaxPayloadBytes,
		)
	}
	frame := make([]byte, 16+payloadLength)
	copy(frame, header)
	if _, err := io.ReadFull(reader, frame[16:]); err != nil {
		return nil, err
	}
	return frame, nil
}

func decodeDifferentialDubboProxyInvocation(
	frame []byte,
) (differentialDubboProxyInvocation, error) {
	if len(frame) < 16 {
		return differentialDubboProxyInvocation{}, fmt.Errorf(
			"dubbo-proxy frame length = %d, want at least 16",
			len(frame),
		)
	}
	if frame[0] != 0xda || frame[1] != 0xbb || frame[2] != 0xc2 || frame[3] != 0 {
		return differentialDubboProxyInvocation{}, fmt.Errorf(
			"dubbo-proxy magic/flags/status = %x%02x/%02x/%d, want dabb/c2/0",
			frame[0], frame[1], frame[2], frame[3],
		)
	}
	if payloadLength := int(binary.BigEndian.Uint32(frame[12:16])); payloadLength != len(frame)-16 {
		return differentialDubboProxyInvocation{}, fmt.Errorf(
			"dubbo-proxy payload length = %d, actual %d",
			payloadLength,
			len(frame)-16,
		)
	}
	invocation := differentialDubboProxyInvocation{
		RequestID: binary.BigEndian.Uint64(frame[4:12]),
	}
	decoder := hessian.NewDecoder(frame[16:])
	metadata := []*string{
		&invocation.ProtocolVersion,
		&invocation.ServiceName,
		&invocation.ServiceVersion,
		&invocation.Method,
		&invocation.ParamsTypeDesc,
	}
	for index, target := range metadata {
		value, err := decoder.Decode()
		if err != nil {
			return differentialDubboProxyInvocation{}, fmt.Errorf(
				"decode dubbo-proxy metadata %d: %w",
				index,
				err,
			)
		}
		text, ok := value.(string)
		if !ok {
			return differentialDubboProxyInvocation{}, fmt.Errorf(
				"dubbo-proxy metadata %d is %T, want string",
				index,
				value,
			)
		}
		*target = text
	}
	contextValue, err := decoder.Decode()
	if err != nil {
		return differentialDubboProxyInvocation{}, fmt.Errorf("decode dubbo-proxy HTTP context: %w", err)
	}
	contextMap, err := differentialDubboProxyStringMap(contextValue)
	if err != nil {
		return differentialDubboProxyInvocation{}, fmt.Errorf("decode dubbo-proxy HTTP context: %w", err)
	}
	invocation.HTTPContext = make(map[string][]string, len(contextMap)-1)
	for name, value := range contextMap {
		name = strings.ToLower(name)
		if name == "body" {
			invocation.HTTPBody, err = differentialDubboProxyBodyString(value)
			if err != nil {
				return differentialDubboProxyInvocation{}, err
			}
			continue
		}
		invocation.HTTPContext[name], err = differentialDubboProxyStringValues(value)
		if err != nil {
			return differentialDubboProxyInvocation{}, fmt.Errorf("HTTP context %q: %w", name, err)
		}
	}
	attachmentsValue, err := decoder.Decode()
	if err != nil {
		return differentialDubboProxyInvocation{}, fmt.Errorf("decode dubbo-proxy attachments: %w", err)
	}
	if attachmentsValue != nil {
		attachments, mapErr := differentialDubboProxyStringMap(attachmentsValue)
		if mapErr != nil {
			return differentialDubboProxyInvocation{}, fmt.Errorf("decode dubbo-proxy attachments: %w", mapErr)
		}
		invocation.Attachments = make(map[string][]string, len(attachments))
		for name, value := range attachments {
			invocation.Attachments[name], err = differentialDubboProxyStringValues(value)
			if err != nil {
				return differentialDubboProxyInvocation{}, fmt.Errorf("attachment %q: %w", name, err)
			}
		}
	}
	if decoder.Buffered() != 0 {
		return differentialDubboProxyInvocation{}, fmt.Errorf(
			"dubbo-proxy payload has %d trailing bytes",
			decoder.Buffered(),
		)
	}
	return invocation, nil
}

func validateDifferentialDubboProxyInvocation(invocation differentialDubboProxyInvocation) error {
	if invocation.RequestID == 0 ||
		invocation.ProtocolVersion != differentialDubboProxyProtocolVersion ||
		invocation.ServiceName != differentialDubboProxyServiceName ||
		invocation.ServiceVersion != differentialDubboProxyServiceVersion ||
		invocation.Method != differentialDubboProxyMethodName ||
		invocation.ParamsTypeDesc != differentialDubboProxyParamsTypeDesc {
		return fmt.Errorf("dubbo-proxy invocation metadata is not the pinned TEST 3 contract: %#v", invocation)
	}
	if values := invocation.HTTPContext["host"]; len(values) != 1 || values[0] != differentialDubboProxyHTTPHost {
		return fmt.Errorf("dubbo-proxy HTTP context host = %#v", values)
	}
	if values := invocation.HTTPContext["extra-arg-k"]; len(values) != 1 ||
		values[0] != differentialDubboProxyHeaderValue {
		return fmt.Errorf("dubbo-proxy HTTP context extra-arg-k = %#v", values)
	}
	if invocation.HTTPBody != differentialDubboProxyRequestBody {
		return fmt.Errorf("dubbo-proxy HTTP context body = %q", invocation.HTTPBody)
	}
	if len(invocation.Attachments) != 0 {
		return fmt.Errorf("dubbo-proxy attachments = %#v, want empty", invocation.Attachments)
	}
	return nil
}

func differentialDubboProxyStringMap(value any) (map[string]any, error) {
	result := make(map[string]any)
	switch typed := value.(type) {
	case map[any]any:
		for key, item := range typed {
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("map key %T is not a string", key)
			}
			result[name] = item
		}
	case map[string]any:
		maps.Copy(result, typed)
	default:
		return nil, fmt.Errorf("value is %T, want a map", value)
	}
	return result, nil
}

func differentialDubboProxyStringValues(value any) ([]string, error) {
	switch typed := value.(type) {
	case string:
		return []string{typed}, nil
	case []byte:
		return []string{string(typed)}, nil
	case []string:
		return append([]string(nil), typed...), nil
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			values, err := differentialDubboProxyStringValues(item)
			if err != nil || len(values) != 1 {
				return nil, fmt.Errorf("list item %T is not one string", item)
			}
			result = append(result, values[0])
		}
		return result, nil
	default:
		return nil, fmt.Errorf("value is %T, want string or string list", value)
	}
}

func differentialDubboProxyBodyString(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case []byte:
		return string(typed), nil
	default:
		return "", fmt.Errorf("dubbo-proxy HTTP context body is %T, want string or bytes", value)
	}
}

func buildDifferentialDubboProxyResponseFrame(requestID uint64) ([]byte, error) {
	encoder := hessian.NewEncoder()
	if err := encoder.Encode(int32(1)); err != nil {
		return nil, fmt.Errorf("encode dubbo-proxy response type: %w", err)
	}
	if err := encoder.Encode(map[string]any{
		"status":          "200",
		"body":            differentialDubboProxyResponseBody,
		"Got-extra-arg-k": differentialDubboProxyHeaderValue,
	}); err != nil {
		return nil, fmt.Errorf("encode dubbo-proxy response map: %w", err)
	}
	payload := encoder.Buffer()
	frame := make([]byte, 16+len(payload))
	frame[0], frame[1], frame[2], frame[3] = 0xda, 0xbb, 0x02, 20
	binary.BigEndian.PutUint64(frame[4:12], requestID)
	binary.BigEndian.PutUint32(frame[12:16], uint32(len(payload)))
	copy(frame[16:], payload)
	return frame, nil
}
