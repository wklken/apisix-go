package pluginintegration

import (
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestDifferentialHTTPDubboFixtureServesAndCapturesExactFastJSONExchange(t *testing.T) {
	spec := DifferentialFixture{
		Name:          "http-dubbo",
		WireProtocol:  differentialFixtureWireHTTPDubboFastJSON,
		ExpectedCalls: 1,
		Response: DifferentialFixtureResponse{
			Status: http.StatusOK,
			Body:   differentialHTTPDubboPOJOJSON,
		},
	}
	fixture, err := startDifferentialHTTPDubboFixture(spec)
	if err != nil {
		t.Fatalf("startDifferentialHTTPDubboFixture() error = %v", err)
	}
	t.Cleanup(fixture.close)

	requestFrame, err := base64.StdEncoding.DecodeString(differentialHTTPDubboRequestFrameBase64)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout(
		"tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())), time.Second,
	)
	if err != nil {
		t.Fatalf("dial Dubbo fixture: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.Write(requestFrame); err != nil {
		t.Fatalf("write Dubbo request frame: %v", err)
	}
	responseFrame, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("read Dubbo response frame: %v", err)
	}
	wantResponse, err := base64.StdEncoding.DecodeString(differentialHTTPDubboResponseFrameBase64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(responseFrame, wantResponse) {
		t.Fatalf("response frame = %x, want %x", responseFrame, wantResponse)
	}

	captured, err := fixture.collectWithTimeout(1, time.Second)
	if err != nil {
		t.Fatalf("collectWithTimeout() error = %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured calls = %d, want 1", len(captured))
	}
	call := captured[0]
	if call.Method != differentialHTTPDubboMethod ||
		call.Path != differentialHTTPDubboServiceName+"/"+differentialHTTPDubboMethodName ||
		call.Host != differentialHTTPDubboServiceVersion {
		t.Fatalf("captured method/path/host = %q/%q/%q", call.Method, call.Path, call.Host)
	}
	if call.Headers.Get(differentialHTTPDubboParamsTypeHeader) != differentialHTTPDubboParamsTypeDesc {
		t.Fatalf("captured params type = %q", call.Headers.Get(differentialHTTPDubboParamsTypeHeader))
	}
	if !bytes.Equal([]byte(call.Body), requestFrame) {
		t.Fatalf("captured request frame = %x, want %x", []byte(call.Body), requestFrame)
	}
}

func TestValidateDifferentialHTTPDubboRequestFrameRejectsChangedService(t *testing.T) {
	frame, err := base64.StdEncoding.DecodeString(differentialHTTPDubboRequestFrameBase64)
	if err != nil {
		t.Fatal(err)
	}
	mutated := bytes.Replace(
		frame,
		[]byte(differentialHTTPDubboServiceName),
		[]byte("org.apache.dubbo.backend.DubboSerializationTestServicX"),
		1,
	)
	if err := validateDifferentialHTTPDubboRequestFrame(mutated); err == nil {
		t.Fatal("validateDifferentialHTTPDubboRequestFrame() error = nil, want service mutation rejection")
	}
}

func TestDifferentialHTTPDubboFixtureDeclaresOracleHostRouting(t *testing.T) {
	fixture := DifferentialFixture{WireProtocol: differentialFixtureWireHTTPDubboFastJSON}
	if !differentialHTTPDubboFixtureUsesHostOracle(fixture) {
		t.Fatal("http-dubbo fixture was not marked for Oracle host routing")
	}
	if differentialHTTPDubboFixtureUsesHostOracle(DifferentialFixture{}) {
		t.Fatal("ordinary fixture was marked for Oracle host routing")
	}
}
