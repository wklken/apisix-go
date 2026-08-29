package pluginintegration

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// This test fails if the fixture stops serving the same real broker operations
// that kafka-proxy owns: ListOffsets must return the deterministic high-water
// offset, and Fetch at offset 14 must return the deterministic Kafka record rather than a synthetic
// HTTP response or a successful metadata handshake.
func TestDifferentialKafkaFixtureServesListOffsetsAndFetchRecord(t *testing.T) {
	spec := DifferentialFixture{
		Name: "kafka-pubsub-record", WireProtocol: differentialFixtureWireHTTPKafka,
		ExpectedCalls: 4, CaptureAllCalls: true, CollectTimeoutMillis: 3000,
	}
	fixture, err := newDifferentialFixture(spec)
	if err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	defer fixture.close()
	fixture.reset()

	dialer := &kafka.Dialer{Timeout: time.Second}
	conn, err := dialer.DialLeader(
		context.Background(),
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())),
		"test-consumer",
		0,
	)
	if err != nil {
		t.Fatalf("dial fixture leader: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set fixture deadline: %v", err)
	}

	lastOffset, err := conn.ReadLastOffset()
	if err != nil {
		t.Fatalf("list last offset: %v", err)
	}
	if lastOffset != 15 {
		t.Fatalf("last offset = %d, want 15", lastOffset)
	}
	if _, err := conn.Seek(14, kafka.SeekStart); err != nil {
		t.Fatalf("seek fixture record: %v", err)
	}
	batch := conn.ReadBatchWith(kafka.ReadBatchConfig{
		MinBytes: 1, MaxBytes: 1 << 20, MaxWait: time.Second,
	})
	defer func() { _ = batch.Close() }()
	message, err := batch.ReadMessage()
	if err != nil {
		calls, collectErr := fixture.collectWithTimeout(4, 3*time.Second)
		t.Fatalf("fetch fixture record: %v; calls=%#v; fixture error=%v", err, calls, collectErr)
	}
	if message.Offset != 14 || message.Time.UnixMilli() != 1_700_000_000_123 ||
		!bytes.Equal(message.Key, []byte("key14")) ||
		!bytes.Equal(message.Value, []byte("testmsg15")) {
		t.Fatalf("record = %#v, want exact offset/timestamp/key/value", message)
	}

	calls, err := fixture.collectWithTimeout(4, 3*time.Second)
	if err != nil {
		t.Fatalf("collect broker operations: %v", err)
	}
	if calls[0].Method != "KAFKA_LIST_OFFSETS" ||
		calls[0].Path != "test-consumer" || calls[0].Host != "0" ||
		calls[0].Body != "-1" {
		t.Fatalf("ListOffsets call = %#v", calls[0])
	}
	for index, wantTimestamp := range []string{"-2", "-1"} {
		call := calls[index+1]
		if call.Method != "KAFKA_LIST_OFFSETS" || call.Path != "test-consumer" ||
			call.Host != "0" || call.Body != wantTimestamp {
			t.Fatalf("Seek ListOffsets call %d = %#v", index, call)
		}
	}
	if calls[3].Method != "KAFKA_FETCH" ||
		calls[3].Path != "test-consumer" || calls[3].Host != "0" ||
		calls[3].Body != "14" {
		t.Fatalf("Fetch call = %#v", calls[3])
	}
}

func TestDifferentialHTTPKafkaFixtureCapturesOnlyOriginAndProduceRecord(t *testing.T) {
	spec := DifferentialFixture{
		Name: "origin-and-kafka-record", WireProtocol: differentialFixtureWireHTTPKafka,
		Response:      DifferentialFixtureResponse{Status: http.StatusOK, Body: "upstream-ok"},
		ExpectedCalls: 2, CaptureAllCalls: true, CollectTimeoutMillis: 3000,
	}
	fixture, err := newDifferentialFixture(spec)
	if err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	defer fixture.close()
	fixture.reset()

	request, err := http.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:"+strconv.Itoa(fixture.port())+"/hello?ab=cd",
		strings.NewReader("abcdef"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "differential.example.test"
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatalf("origin request: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	writer := &kafka.Writer{
		Addr:         kafka.TCP(net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port()))),
		RequiredAcks: 1, BatchSize: 1,
		ReadTimeout: time.Second, WriteTimeout: time.Second,
	}
	if err := writer.WriteMessages(context.Background(), kafka.Message{
		Topic: "test2", Key: []byte("key1"), Value: []byte("record-value"),
	}); err != nil {
		t.Fatalf("produce Kafka record: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close Kafka writer: %v", err)
	}

	calls, err := fixture.collectWithTimeout(2, 3*time.Second)
	if err != nil {
		t.Fatalf("collect fixture calls: %v", err)
	}
	if calls[0].Method != http.MethodGet || calls[0].Path != "/hello?ab=cd" ||
		calls[0].Host != "differential.example.test" || calls[0].Body != "abcdef" {
		t.Fatalf("origin call = %#v", calls[0])
	}
	if calls[1].Method != differentialKafkaMethod || calls[1].Path != "test2" ||
		calls[1].Host != "key1" || calls[1].Body != "record-value" {
		t.Fatalf("Kafka call = %#v", calls[1])
	}
}

func TestDifferentialKafkaAdvertisedHostUsesConnectionSide(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "")
	for _, test := range []struct {
		name   string
		remote net.Addr
		want   string
	}{
		{name: "candidate loopback", remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 3000}, want: "127.0.0.1"},
		{name: "oracle bridge", remote: &net.TCPAddr{IP: net.ParseIP("192.168.127.2"), Port: 3000}, want: differentialOracleHostGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := differentialKafkaAdvertisedHost(test.remote, false); got != test.want {
				t.Fatalf("advertised host = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDifferentialKafkaAdvertisesExplicitPodmanHostGateway(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "192.168.127.254")
	remote := &net.TCPAddr{IP: net.ParseIP("192.168.127.2"), Port: 3000}
	if got := differentialKafkaAdvertisedHost(remote, false); got != "192.168.127.254" {
		t.Fatalf("advertised host = %q, want explicit Podman host gateway", got)
	}
}

func TestDifferentialKafkaAdvertisesOracleGatewayBehindLoopbackForwarder(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "192.168.127.254")
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 3000}
	if got := differentialKafkaAdvertisedHost(remote, true); got != "192.168.127.254" {
		t.Fatalf("Oracle advertised host = %q, want explicit Podman host gateway", got)
	}
}

func TestDifferentialKafkaOracleEndpointUsesHostFixture(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "")
	fixture := DifferentialFixture{WireProtocol: differentialFixtureWireHTTPKafka}
	if got := differentialOracleFixtureEndpoint(fixture, 19092); got != "host.containers.internal:19092" {
		t.Fatalf("Oracle endpoint = %q", got)
	}
	if got := differentialOracleFixtureEndpoint(DifferentialFixture{}, 19092); got != "127.0.0.1:1980" {
		t.Fatalf("ordinary Oracle endpoint = %q", got)
	}
}

func TestDifferentialKafkaOracleEndpointUsesExplicitPodmanHostGateway(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "192.168.127.254")
	fixture := DifferentialFixture{WireProtocol: differentialFixtureWireHTTPKafka}
	if got := differentialOracleFixtureEndpoint(fixture, 19092); got != "192.168.127.254:19092" {
		t.Fatalf("Oracle endpoint = %q", got)
	}
}
