package pluginintegration

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	differentialFixtureWireHTTPDubboFastJSON = "dubbo-fastjson"
	differentialHTTPDubboMethod              = "DUBBO"
	differentialHTTPDubboParamsTypeHeader    = "X-Dubbo-Params-Type-Desc"
	differentialHTTPDubboServiceName         = "org.apache.dubbo.backend.DubboSerializationTestService"
	differentialHTTPDubboServiceVersion      = "1.0.0"
	differentialHTTPDubboMethodName          = "testPoJo"
	differentialHTTPDubboParamsTypeDesc      = "Lorg/apache/dubbo/backend/PoJo;"
	differentialHTTPDubboPOJOJSON            = `{"aBoolean":true,"aByte":1,"aDouble":1.1,"aFloat":1.2,"aInt":2,"aLong":3,"aShort":4,"aString":"aa","acharacter":"a","stringMap":{"key":"value"},"strings":["aa","bb"]}`
	differentialHTTPDubboRequestFrameBase64  = "2rvGAAAAAAAAAAABAAABICIyLjAuMiIKIm9yZy5hcGFjaGUuZHViYm8uYmFja2VuZC5EdWJib1NlcmlhbGl6YXRpb25UZXN0U2VydmljZSIKIjEuMC4wIgoidGVzdFBvSm8iCiJMb3JnL2FwYWNoZS9kdWJiby9iYWNrZW5kL1BvSm87Igp7ImFCb29sZWFuIjp0cnVlLCJhQnl0ZSI6MSwiYURvdWJsZSI6MS4xLCJhRmxvYXQiOjEuMiwiYUludCI6MiwiYUxvbmciOjMsImFTaG9ydCI6NCwiYVN0cmluZyI6ImFhIiwiYWNoYXJhY3RlciI6ImEiLCJzdHJpbmdNYXAiOnsia2V5IjoidmFsdWUifSwic3RyaW5ncyI6WyJhYSIsImJiIl19Cnt9Cg=="
	differentialHTTPDubboResponseFrameBase64 = "2rsGFAAAAAAAAAABAAAAqTEKeyJhQm9vbGVhbiI6dHJ1ZSwiYUJ5dGUiOjEsImFEb3VibGUiOjEuMSwiYUZsb2F0IjoxLjIsImFJbnQiOjIsImFMb25nIjozLCJhU2hvcnQiOjQsImFTdHJpbmciOiJhYSIsImFjaGFyYWN0ZXIiOiJhIiwic3RyaW5nTWFwIjp7ImtleSI6InZhbHVlIn0sInN0cmluZ3MiOlsiYWEiLCJiYiJdfQo="
	differentialHTTPDubboMaxPayloadBytes     = 1 << 20
)

func differentialHTTPDubboFixtureUsesHostOracle(fixture DifferentialFixture) bool {
	return fixture.WireProtocol == differentialFixtureWireHTTPDubboFastJSON
}

func startDifferentialHTTPDubboFixture(
	spec DifferentialFixture,
) (*differentialFixtureServer, error) {
	if spec.WireProtocol != differentialFixtureWireHTTPDubboFastJSON {
		return nil, fmt.Errorf("http-dubbo fixture wire protocol = %q", spec.WireProtocol)
	}
	if spec.ExpectedCalls != 1 {
		return nil, fmt.Errorf("http-dubbo fixture expected_calls = %d, want 1", spec.ExpectedCalls)
	}
	if spec.Response.Status != http.StatusOK || spec.Response.Body != differentialHTTPDubboPOJOJSON {
		return nil, errors.New("http-dubbo fixture requires the pinned TEST 1 POJO response")
	}
	probeToken, err := newDifferentialRunNonce()
	if err != nil {
		return nil, fmt.Errorf("create http-dubbo fixture probe token: %w", err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic http-dubbo fixture: %w", err)
	}
	fixture := &differentialFixtureServer{
		listener: listener, requests: make(chan differentialCapturedRequest, 16),
		errors: make(chan error, 16), response: spec.Response, fixture: spec.Name,
		probeToken: probeToken, wire: spec.WireProtocol,
	}
	fixture.serveWG.Add(1)
	go fixture.serveHTTPDubbo()
	return fixture, nil
}

func (fixture *differentialFixtureServer) serveHTTPDubbo() {
	defer fixture.serveWG.Done()
	for {
		connection, err := fixture.listener.Accept()
		if err != nil {
			return
		}
		fixture.connectionWG.Go(func() {
			defer func() { _ = connection.Close() }()
			_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
			fixture.serveHTTPDubboConnection(bufio.NewReader(connection), connection)
		})
	}
}

func (fixture *differentialFixtureServer) serveHTTPDubboConnection(
	reader *bufio.Reader,
	connection net.Conn,
) {
	prefix, err := reader.Peek(2)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			fixture.reportError(fmt.Errorf("sniff http-dubbo fixture connection: %w", err))
		}
		return
	}
	if prefix[0] != 0xda || prefix[1] != 0xbb {
		fixture.captureHTTPRequest(reader, connection)
		return
	}
	frame, err := readDifferentialHTTPDubboFrame(reader)
	if err != nil {
		fixture.reportError(fmt.Errorf("read http-dubbo fixture frame: %w", err))
		return
	}
	if err := validateDifferentialHTTPDubboRequestFrame(frame); err != nil {
		fixture.reportError(err)
		return
	}
	fixture.capture(differentialCapturedRequest{
		Method: differentialHTTPDubboMethod,
		Path:   differentialHTTPDubboServiceName + "/" + differentialHTTPDubboMethodName,
		Host:   differentialHTTPDubboServiceVersion,
		Headers: http.Header{
			differentialHTTPDubboParamsTypeHeader: []string{differentialHTTPDubboParamsTypeDesc},
		},
		Body: string(frame),
	})
	response := buildDifferentialHTTPDubboResponseFrame(
		binary.BigEndian.Uint64(frame[4:12]), fixture.response.Body,
	)
	if _, err := connection.Write(response); err != nil {
		fixture.reportError(fmt.Errorf("write http-dubbo fixture response: %w", err))
	}
}

func readDifferentialHTTPDubboFrame(reader io.Reader) ([]byte, error) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	payloadLength := int(binary.BigEndian.Uint32(header[12:16]))
	if payloadLength > differentialHTTPDubboMaxPayloadBytes {
		return nil, fmt.Errorf("dubbo payload length = %d, max %d", payloadLength, differentialHTTPDubboMaxPayloadBytes)
	}
	frame := make([]byte, 16+payloadLength)
	copy(frame, header)
	if _, err := io.ReadFull(reader, frame[16:]); err != nil {
		return nil, err
	}
	return frame, nil
}

func validateDifferentialHTTPDubboRequestFrame(frame []byte) error {
	if len(frame) < 16 {
		return fmt.Errorf("http-dubbo frame length = %d, want at least 16", len(frame))
	}
	if frame[0] != 0xda || frame[1] != 0xbb {
		return fmt.Errorf("http-dubbo magic = %x%02x, want dabb", frame[0], frame[1])
	}
	if frame[2] != 0xc6 || frame[3] != 0 {
		return fmt.Errorf("http-dubbo flags/status = %02x/%d, want c6/0", frame[2], frame[3])
	}
	if requestID := binary.BigEndian.Uint64(frame[4:12]); requestID != 1 {
		return fmt.Errorf("http-dubbo request ID = %d, want 1", requestID)
	}
	if payloadLength := int(binary.BigEndian.Uint32(frame[12:16])); payloadLength != len(frame)-16 {
		return fmt.Errorf("http-dubbo payload length = %d, actual %d", payloadLength, len(frame)-16)
	}
	want, err := base64.StdEncoding.DecodeString(differentialHTTPDubboRequestFrameBase64)
	if err != nil {
		return fmt.Errorf("decode pinned http-dubbo request frame: %w", err)
	}
	if !bytes.Equal(frame, want) {
		return errors.New("http-dubbo request frame differs from APISIX 3.17 TEST 1")
	}
	return nil
}

func buildDifferentialHTTPDubboResponseFrame(requestID uint64, body string) []byte {
	payload := []byte("1\n" + body + "\n")
	frame := make([]byte, 16+len(payload))
	frame[0], frame[1], frame[2], frame[3] = 0xda, 0xbb, 0x06, 20
	binary.BigEndian.PutUint64(frame[4:12], requestID)
	binary.BigEndian.PutUint32(frame[12:16], uint32(len(payload)))
	copy(frame[16:], payload)
	return frame
}
