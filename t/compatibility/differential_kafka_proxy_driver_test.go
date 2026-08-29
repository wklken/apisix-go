package pluginintegration

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
)

type differentialKafkaProxyFakeConsumer struct {
	requests []kafka_proxy.PubSubRequest
}

func (c *differentialKafkaProxyFakeConsumer) ListOffset(
	_ context.Context, topic string, partition int32, timestamp int64,
) (int64, error) {
	c.requests = append(c.requests, kafka_proxy.PubSubRequest{
		Command: kafka_proxy.CmdKafkaListOffset, Topic: topic,
		Partition: partition, Position: timestamp,
	})
	return differentialKafkaPubSubHighWatermark, nil
}

func (c *differentialKafkaProxyFakeConsumer) Fetch(
	_ context.Context, topic string, partition int32, offset int64,
) ([]kafka_proxy.KafkaMessage, error) {
	c.requests = append(c.requests, kafka_proxy.PubSubRequest{
		Command: kafka_proxy.CmdKafkaFetch, Topic: topic,
		Partition: partition, Position: offset,
	})
	return []kafka_proxy.KafkaMessage{{
		Offset: differentialKafkaPubSubRecordOffset, Timestamp: differentialKafkaPubSubRecordTimestamp,
		Key: []byte(differentialKafkaPubSubRecordKey), Value: []byte(differentialKafkaPubSubRecordValue),
	}}, nil
}

func TestDifferentialKafkaProxyCandidateDriverExchangesListOffsetAndFetch(t *testing.T) {
	consumer := &differentialKafkaProxyFakeConsumer{}
	handlerErrors := make(chan error, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kafka" || r.Host != "gateway.example.test" {
			handlerErrors <- fmt.Errorf("request path/host = %s/%s", r.URL.Path, r.Host)
		}
		if err := kafka_proxy.ServePubSubWebSocket(
			w, r, []string{"kafka://fixture.invalid:9092"}, kafka_proxy.TransportOptions{},
			func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
				return consumer, nil
			},
		); err != nil {
			handlerErrors <- err
		}
	}))
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("split server address: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	observation, err := observeDifferentialKafkaProxyCandidate(differentialCasesForPlugin("kafka-proxy")[0], port)
	if err != nil {
		t.Fatalf("observe candidate: %v", err)
	}
	select {
	case err := <-handlerErrors:
		t.Fatal(err)
	default:
	}
	wantRequests := []kafka_proxy.PubSubRequest{
		{Command: kafka_proxy.CmdKafkaListOffset, Topic: differentialKafkaPubSubTopic, Partition: 0, Position: -1},
		{Command: kafka_proxy.CmdKafkaFetch, Topic: differentialKafkaPubSubTopic, Partition: 0, Position: 14},
	}
	if !reflect.DeepEqual(consumer.requests, wantRequests) {
		t.Fatalf("consumer requests = %#v, want %#v", consumer.requests, wantRequests)
	}
	transcript, err := decodeDifferentialKafkaProxyTranscript(observation.Body)
	if err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	if transcript.ListOffset.Sequence != 3 || transcript.ListOffset.Offset != 15 ||
		transcript.Fetch.Sequence != 6 || len(transcript.Fetch.Messages) != 1 ||
		transcript.Fetch.Messages[0].Value != "testmsg15" {
		t.Fatalf("transcript = %#v", transcript)
	}
	if observation.Status != http.StatusSwitchingProtocols || len(observation.Steps) != 0 {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestDifferentialKafkaProxyWebSocketFlowCapturesRealBrokerOperations(t *testing.T) {
	spec := differentialCasesForPlugin("kafka-proxy")[0]
	fixture, err := newDifferentialFixture(spec.Fixture)
	if err != nil {
		t.Fatalf("start Kafka fixture: %v", err)
	}
	defer fixture.close()
	fixture.reset()

	broker := net.JoinHostPort("127.0.0.1", fmt.Sprint(fixture.port()))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = kafka_proxy.ServePubSubWebSocket(
			w, r, []string{"kafka://" + broker}, kafka_proxy.TransportOptions{}, nil,
		)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("split server address: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	observation, err := observeDifferentialSide(fixture, spec, port, broker)
	if err != nil {
		t.Fatalf("observe registered candidate flow: %v", err)
	}
	wantCalls := differentialKafkaProxyCandidateCallsForTest(spec)
	if !reflect.DeepEqual(observation.UpstreamCalls, wantCalls) {
		t.Fatalf("native broker calls = %#v, want %#v", observation.UpstreamCalls, wantCalls)
	}
	transcript, err := decodeDifferentialKafkaProxyTranscript(observation.Body)
	if err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	if transcript.ListOffset.Offset != differentialKafkaPubSubHighWatermark ||
		len(transcript.Fetch.Messages) != 1 ||
		transcript.Fetch.Messages[0].Value != differentialKafkaPubSubRecordValue {
		t.Fatalf("real broker transcript = %#v", transcript)
	}
}

func TestDifferentialKafkaProxyOraclePacketParserPreservesPubSubRecord(t *testing.T) {
	listOffset, err := kafka_proxy.MarshalPubSubResponse(kafka_proxy.PubSubResponse{
		Sequence: 3, Kind: kafka_proxy.RespKafkaListOffset, Offset: differentialKafkaPubSubHighWatermark,
	})
	if err != nil {
		t.Fatalf("marshal list-offset response: %v", err)
	}
	fetch, err := kafka_proxy.MarshalPubSubResponse(kafka_proxy.PubSubResponse{
		Sequence: 6, Kind: kafka_proxy.RespKafkaFetch,
		Messages: []kafka_proxy.KafkaMessage{{
			Offset: differentialKafkaPubSubRecordOffset, Timestamp: differentialKafkaPubSubRecordTimestamp,
			Key: []byte(differentialKafkaPubSubRecordKey), Value: []byte(differentialKafkaPubSubRecordValue),
		}},
	})
	if err != nil {
		t.Fatalf("marshal fetch response: %v", err)
	}
	packet := encodeDifferentialKafkaProxyOracleTestPacket(
		[]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"),
		listOffset,
		fetch,
	)
	observation, err := parseDifferentialKafkaProxyOraclePacket(differentialCasesForPlugin("kafka-proxy")[0], packet)
	if err != nil {
		t.Fatalf("parse Oracle packet: %v", err)
	}
	transcript, err := decodeDifferentialKafkaProxyTranscript(observation.Body)
	if err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	if transcript.ListOffset.Offset != 15 || transcript.Fetch.Messages[0].Offset != 14 {
		t.Fatalf("transcript = %#v", transcript)
	}
	if _, err := parseDifferentialKafkaProxyOraclePacket(
		differentialCasesForPlugin("kafka-proxy")[0], packet[:len(packet)-1],
	); err == nil || !strings.Contains(err.Error(), "frame") {
		t.Fatalf("truncated packet error = %v, want frame error", err)
	}
}

func TestDifferentialKafkaProxyProtocolDriverProvidesBothSides(t *testing.T) {
	driver, ok := differentialProtocolDriverRegistry[differentialKafkaProxyPubSubPolicy]
	if !ok || driver.observeCandidate == nil || driver.observeOracle == nil {
		t.Fatalf("Kafka Proxy protocol driver = %#v, registered = %t", driver, ok)
	}
}

func TestDifferentialKafkaProxyRegisteredOracleFlowAttachesBrokerEvidence(t *testing.T) {
	spec := differentialCasesForPlugin("kafka-proxy")[0]
	baseObservation, err := newDifferentialKafkaProxyObservation(
		spec,
		kafka_proxy.PubSubResponse{
			Sequence: 3, Kind: kafka_proxy.RespKafkaListOffset,
			Offset: differentialKafkaPubSubHighWatermark,
		},
		kafka_proxy.PubSubResponse{
			Sequence: 6, Kind: kafka_proxy.RespKafkaFetch,
			Messages: []kafka_proxy.KafkaMessage{{
				Offset:    differentialKafkaPubSubRecordOffset,
				Timestamp: differentialKafkaPubSubRecordTimestamp,
				Key:       []byte(differentialKafkaPubSubRecordKey),
				Value:     []byte(differentialKafkaPubSubRecordValue),
			}},
		},
	)
	if err != nil {
		t.Fatalf("build Oracle observation: %v", err)
	}

	original := differentialProtocolDriverRegistry[differentialKafkaProxyPubSubPolicy]
	differentialProtocolDriverRegistry[differentialKafkaProxyPubSubPolicy] = differentialProtocolDriver{
		observeOracle: func(DifferentialCase, *differentialChild) (DifferentialObservation, error) {
			return baseObservation, nil
		},
	}
	t.Cleanup(func() {
		differentialProtocolDriverRegistry[differentialKafkaProxyPubSubPolicy] = original
	})
	fixture := &differentialFixtureServer{
		fixture:  spec.Fixture.Name,
		requests: make(chan differentialCapturedRequest, spec.Fixture.ExpectedCalls),
		errors:   make(chan error, 1),
	}
	wantCalls := differentialKafkaProxyOracleCallsForTest(spec)
	for _, call := range wantCalls {
		fixture.requests <- differentialCapturedRequest{
			Method: call.Method, Path: call.Path, Host: call.Host, Body: call.Body,
		}
	}

	observation, err := observeDifferentialOracleSide(fixture, spec, nil, "oracle-broker:9092")
	if err != nil {
		t.Fatalf("observe registered Oracle flow: %v", err)
	}
	if !reflect.DeepEqual(observation.UpstreamCalls, wantCalls) ||
		observation.UpstreamAddress != "oracle-broker:9092" {
		t.Fatalf("Oracle broker evidence = %#v / %q", observation.UpstreamCalls, observation.UpstreamAddress)
	}
}

func TestDifferentialKafkaProxyOraclePerlScriptCompiles(t *testing.T) {
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("Perl is not installed on this host")
	}
	command := exec.Command(
		perl,
		"-MIO::Socket::INET",
		"-c",
		"-e",
		differentialKafkaProxyOraclePerlScript,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile Oracle Perl script: %v: %s", err, output)
	}
}

func encodeDifferentialKafkaProxyOracleTestPacket(frames ...[]byte) []byte {
	var packet []byte
	for _, frame := range frames {
		length := make([]byte, 4)
		binary.BigEndian.PutUint32(length, uint32(len(frame)))
		packet = append(packet, length...)
		packet = append(packet, frame...)
	}
	return packet
}
