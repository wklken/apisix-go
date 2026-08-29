package pluginintegration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"golang.org/x/net/http2"
)

const (
	differentialFixtureWireGRPCH2C       = "grpc-h2c"
	differentialGRPCMaxUnaryFrameBytes   = 1 << 20
	differentialGRPCRequestMessageBase64 = "CgV3b3JsZA=="
)

func startDifferentialGRPCH2CFixture(
	spec DifferentialFixture,
) (*differentialFixtureServer, error) {
	if spec.WireProtocol != differentialFixtureWireGRPCH2C {
		return nil, fmt.Errorf("gRPC h2c fixture wire protocol = %q", spec.WireProtocol)
	}
	if spec.ExpectedCalls != 1 {
		return nil, fmt.Errorf("gRPC h2c fixture expected_calls = %d, want 1", spec.ExpectedCalls)
	}
	if spec.Response.Status != http.StatusOK {
		return nil, fmt.Errorf("gRPC h2c fixture response status = %d, want 200", spec.Response.Status)
	}
	responseMessage, err := base64.StdEncoding.DecodeString(spec.Response.Body)
	if err != nil {
		return nil, fmt.Errorf("decode gRPC h2c response message: %w", err)
	}
	probeToken, err := newDifferentialRunNonce()
	if err != nil {
		return nil, fmt.Errorf("create gRPC h2c fixture probe token: %w", err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic gRPC h2c fixture: %w", err)
	}
	fixture := &differentialFixtureServer{
		listener:   listener,
		requests:   make(chan differentialCapturedRequest, 16),
		errors:     make(chan error, 16),
		response:   spec.Response,
		fixture:    spec.Name,
		probeToken: probeToken,
		wire:       spec.WireProtocol,
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.RequestURI() == differentialFixtureProbePath &&
			request.Header.Get(differentialFixtureProbeHeader) == fixture.probeToken {
			writer.Header().Set(differentialFixtureProbeHeader, fixture.probeToken)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		if err := captureDifferentialGRPCH2CRequest(fixture, request); err != nil {
			fixture.reportError(err)
			http.Error(writer, "invalid gRPC fixture request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/grpc")
		writer.Header().Add("Trailer", "Grpc-Status")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(buildDifferentialUnaryGRPCFrame(responseMessage))
		writer.Header().Set("Grpc-Status", "0")
	})
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = protocols
	server.Start()
	fixture.server = server
	return fixture, nil
}

func captureDifferentialGRPCH2CRequest(
	fixture *differentialFixtureServer,
	request *http.Request,
) error {
	if request.ProtoMajor != 2 {
		return fmt.Errorf("gRPC fixture protocol = %s, want HTTP/2", request.Proto)
	}
	if request.Method != http.MethodPost {
		return fmt.Errorf("gRPC fixture method = %s, want POST", request.Method)
	}
	if request.URL.RequestURI() != "/helloworld.Greeter/SayHello" {
		return fmt.Errorf("gRPC fixture path = %q", request.URL.RequestURI())
	}
	if request.Header.Get("Content-Type") != "application/grpc" {
		return fmt.Errorf("gRPC fixture Content-Type = %q", request.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, differentialGRPCMaxUnaryFrameBytes+1))
	if err != nil {
		return fmt.Errorf("read gRPC fixture request: %w", err)
	}
	if err := request.Body.Close(); err != nil {
		return fmt.Errorf("close gRPC fixture request: %w", err)
	}
	if len(body) > differentialGRPCMaxUnaryFrameBytes {
		return fmt.Errorf("gRPC fixture request exceeds %d bytes", differentialGRPCMaxUnaryFrameBytes)
	}
	if err := validateDifferentialUnaryGRPCFrame(body, differentialGRPCRequestMessageBase64); err != nil {
		return fmt.Errorf("gRPC fixture request frame: %w", err)
	}
	fixture.capture(differentialCapturedRequest{
		Method:  request.Method,
		Path:    request.URL.RequestURI(),
		Host:    request.Host,
		Headers: request.Header.Clone(),
		Body:    string(body),
	})
	return nil
}

func buildDifferentialUnaryGRPCFrame(message []byte) []byte {
	frame := make([]byte, 5+len(message))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(message)))
	copy(frame[5:], message)
	return frame
}

func validateDifferentialUnaryGRPCFrame(frame []byte, messageBase64 string) error {
	if len(frame) < 5 {
		return fmt.Errorf("frame length = %d, want at least 5", len(frame))
	}
	if frame[0] != 0 {
		return fmt.Errorf("compressed flag = %d, want 0", frame[0])
	}
	declared := int(binary.BigEndian.Uint32(frame[1:5]))
	if declared != len(frame)-5 {
		return fmt.Errorf("declared message length = %d, payload bytes = %d", declared, len(frame)-5)
	}
	want, err := base64.StdEncoding.DecodeString(messageBase64)
	if err != nil {
		return fmt.Errorf("decode expected gRPC message: %w", err)
	}
	if !bytes.Equal(frame[5:], want) {
		return fmt.Errorf(
			"message base64 = %q, want %q",
			base64.StdEncoding.EncodeToString(frame[5:]),
			messageBase64,
		)
	}
	return nil
}

func probeDifferentialGRPCH2CFixture(fixture *differentialFixtureServer) error {
	if fixture == nil || fixture.port() <= 0 || fixture.probeToken == "" {
		return errors.New("gRPC h2c fixture probe requires a fixture port and token")
	}
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, network, address)
		},
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:"+strconv.Itoa(fixture.port())+differentialFixtureProbePath,
		nil,
	)
	if err != nil {
		return err
	}
	request.Header.Set(differentialFixtureProbeHeader, fixture.probeToken)
	response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("probe gRPC h2c fixture: %w", err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("read/close gRPC h2c probe response: %v / %v", readErr, closeErr)
	}
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusNoContent ||
		response.Header.Get(differentialFixtureProbeHeader) != fixture.probeToken {
		return errors.New("gRPC h2c fixture probe identity mismatch")
	}
	return nil
}
