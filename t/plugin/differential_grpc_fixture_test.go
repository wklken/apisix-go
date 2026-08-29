package pluginintegration

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestDifferentialGRPCH2CFixtureServesAndCapturesExactUnaryExchange(t *testing.T) {
	spec := DifferentialFixture{
		Name:          "grpc",
		WireProtocol:  differentialFixtureWireGRPCH2C,
		ExpectedCalls: 1,
		Response: DifferentialFixtureResponse{
			Status: http.StatusOK,
			Body:   "CgtIZWxsbyB3b3JsZA==",
		},
	}
	fixture, err := startDifferentialGRPCH2CFixture(spec)
	if err != nil {
		t.Fatalf("startDifferentialGRPCH2CFixture() error = %v", err)
	}
	t.Cleanup(fixture.close)

	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	requestFrame := []byte{0, 0, 0, 0, 7, 0x0a, 5, 'w', 'o', 'r', 'l', 'd'}
	request, err := http.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:"+strconv.Itoa(fixture.port())+"/helloworld.Greeter/SayHello",
		bytes.NewReader(requestFrame),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/grpc")
	response, err := (&http.Client{Transport: transport, Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("h2c unary request error = %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close h2c unary response = %v / %v", readErr, closeErr)
	}
	if response.Proto != "HTTP/2.0" || response.StatusCode != http.StatusOK {
		t.Fatalf("response protocol/status = %s/%d", response.Proto, response.StatusCode)
	}
	wantResponseFrame := []byte{
		0, 0, 0, 0, 13, 0x0a, 11,
		'H', 'e', 'l', 'l', 'o', ' ', 'w', 'o', 'r', 'l', 'd',
	}
	if !bytes.Equal(body, wantResponseFrame) {
		t.Fatalf("response frame = %x, want %x", body, wantResponseFrame)
	}
	if response.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("response grpc-status trailer = %q, want 0", response.Trailer.Get("Grpc-Status"))
	}

	captured, err := fixture.collectWithTimeout(1, time.Second)
	if err != nil {
		t.Fatalf("collectWithTimeout() error = %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured calls = %d, want 1", len(captured))
	}
	call := captured[0]
	if call.Method != http.MethodPost || call.Path != "/helloworld.Greeter/SayHello" {
		t.Fatalf("captured method/path = %q/%q", call.Method, call.Path)
	}
	if call.Headers.Get("Content-Type") != "application/grpc" {
		t.Fatalf("captured Content-Type = %q", call.Headers.Get("Content-Type"))
	}
	if !bytes.Equal([]byte(call.Body), requestFrame) {
		t.Fatalf("captured request frame = %x, want %x", []byte(call.Body), requestFrame)
	}
}

func TestValidateDifferentialUnaryGRPCFrameRejectsTrailingMessageBytes(t *testing.T) {
	frame := []byte{0, 0, 0, 0, 7, 0x0a, 5, 'w', 'o', 'r', 'l', 'd', 0}
	if err := validateDifferentialUnaryGRPCFrame(frame, "CgV3b3JsZA=="); err == nil {
		t.Fatal("validateDifferentialUnaryGRPCFrame() error = nil, want trailing-byte rejection")
	}
}

func TestDifferentialGRPCH2CFixtureUsesOracleHostRoute(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "192.168.127.254")
	fixture := DifferentialFixture{WireProtocol: differentialFixtureWireGRPCH2C}
	if !differentialFixtureUsesHostOracle(fixture) {
		t.Fatal("gRPC h2c fixture was not marked for Oracle host routing")
	}
	if got := differentialOracleFixtureEndpoint(fixture, 19051); got != "192.168.127.254:19051" {
		t.Fatalf("Oracle gRPC fixture endpoint = %q", got)
	}
}
