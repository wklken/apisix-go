package pluginintegration

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDifferentialMQTTFixtureCapturesExactCONNECTAndReplies(t *testing.T) {
	spec := differentialCasesForPlugin("mqtt-proxy")[0].Fixture
	fixture, err := startDifferentialMQTTFixture(spec)
	if err != nil {
		t.Fatalf("startDifferentialMQTTFixture() error = %v", err)
	}
	t.Cleanup(fixture.close)
	if err := probeDifferentialMQTTFixture(fixture); err != nil {
		t.Fatalf("probeDifferentialMQTTFixture() error = %v", err)
	}

	packet := []byte(differentialCasesForPlugin("mqtt-proxy")[0].Steps[1].Request.Body)
	connection, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())),
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := connection.Write(packet); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	response, readErr := io.ReadAll(connection)
	closeErr := connection.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close MQTT fixture response = %v / %v", readErr, closeErr)
	}
	if string(response) != "hello world" {
		t.Fatalf("fixture response = %q, want hello world", response)
	}

	calls, err := fixture.collectWithTimeout(1, time.Second)
	if err != nil {
		t.Fatalf("collectWithTimeout() error = %v", err)
	}
	if len(calls) != 1 || calls[0].Method != "MQTT" || calls[0].Path != "CONNECT" ||
		!bytes.Equal([]byte(calls[0].Body), packet) {
		t.Fatalf("captured MQTT call = %#v", calls)
	}
}

func TestParseDifferentialMQTTCONNECTRejectsNonCONNECTAndTrailingPayload(t *testing.T) {
	packet := []byte(differentialCasesForPlugin("mqtt-proxy")[0].Steps[1].Request.Body)
	if _, err := parseDifferentialMQTTCONNECT(
		[]byte("mmm"),
	); err == nil ||
		!strings.Contains(err.Error(), "packet type") {
		t.Fatalf("non-CONNECT error = %v", err)
	}
	if _, err := parseDifferentialMQTTCONNECT(
		append(packet, 0),
	); err == nil ||
		!strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing-byte error = %v", err)
	}
}
