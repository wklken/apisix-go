package pluginintegration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
)

type differentialKafkaProxyOffsetTranscript struct {
	Sequence int64 `json:"sequence"`
	Offset   int64 `json:"offset"`
}

type differentialKafkaProxyMessageTranscript struct {
	Offset    int64  `json:"offset"`
	Timestamp int64  `json:"timestamp"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}

type differentialKafkaProxyFetchTranscript struct {
	Sequence int64                                     `json:"sequence"`
	Messages []differentialKafkaProxyMessageTranscript `json:"messages"`
}

type differentialKafkaProxyTranscript struct {
	ListOffset differentialKafkaProxyOffsetTranscript `json:"list_offset"`
	Fetch      differentialKafkaProxyFetchTranscript  `json:"fetch"`
}

func differentialKafkaProxyProtocolDriver() differentialProtocolDriver {
	return differentialProtocolDriver{
		observeCandidate: observeDifferentialKafkaProxyCandidate,
		observeOracle:    observeDifferentialKafkaProxyOracle,
	}
}

// attachDifferentialKafkaProxyFixtureEvidence is called by the shared
// protocol-driver hook after the WebSocket exchange. It makes the four native
// broker operations part of the compared observation instead of treating a
// successful WebSocket response as sufficient evidence.
func attachDifferentialKafkaProxyFixtureEvidence(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	observation *DifferentialObservation,
	upstreamAddress string,
	expectedCalls int,
) error {
	if fixture == nil || observation == nil {
		return errors.New("kafka Proxy fixture evidence requires fixture and observation")
	}
	if err := validateDifferentialKafkaProxyDriverSpec(spec); err != nil {
		return err
	}
	received, err := fixture.collectWithTimeout(
		expectedCalls,
		differentialCandidateFixtureCollectTimeout(spec.Fixture),
	)
	if err != nil {
		return err
	}
	applyDifferentialSequenceFixtureObservation(
		observation, spec.Fixture, received, upstreamAddress,
	)
	return nil
}

func observeDifferentialKafkaProxyCandidate(
	spec DifferentialCase,
	dataPort int,
) (DifferentialObservation, error) {
	if err := validateDifferentialKafkaProxyDriverSpec(spec); err != nil {
		return DifferentialObservation{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 5 * time.Second}
	requestHeaders := http.Header{"Host": []string{spec.Request.Host}}
	conn, response, err := dialer.DialContext(
		ctx,
		"ws://127.0.0.1:"+strconv.Itoa(dataPort)+spec.Request.Path,
		requestHeaders,
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("open Kafka PubSub WebSocket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		return DifferentialObservation{}, fmt.Errorf("kafka PubSub WebSocket did not return status 101")
	}
	if err := validateDifferentialKafkaProxyHandshake(response.Header); err != nil {
		return DifferentialObservation{}, err
	}

	listOffset, fetch, err := exchangeDifferentialKafkaProxyCommands(conn)
	if err != nil {
		return DifferentialObservation{}, err
	}
	return newDifferentialKafkaProxyObservation(spec, listOffset, fetch)
}

func observeDifferentialKafkaProxyOracle(
	spec DifferentialCase,
	child *differentialChild,
) (DifferentialObservation, error) {
	if err := validateDifferentialKafkaProxyDriverSpec(spec); err != nil {
		return DifferentialObservation{}, err
	}
	if child == nil || !child.container || child.runtime == "" || child.name == "" {
		return DifferentialObservation{}, errors.New("kafka Proxy Oracle driver requires a running Oracle container")
	}
	requests, err := differentialKafkaProxyRequests()
	if err != nil {
		return DifferentialObservation{}, err
	}
	output, err := runDifferentialPodmanCommand(
		child.runtime,
		differentialPodmanTimeout,
		nil,
		nil,
		"exec",
		child.name,
		"perl",
		"-MIO::Socket::INET",
		"-e",
		differentialKafkaProxyOraclePerlScript,
		spec.Request.Path,
		spec.Request.Host,
		hex.EncodeToString(requests[0]),
		hex.EncodeToString(requests[1]),
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf(
			"execute Kafka Proxy Oracle WebSocket driver: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return parseDifferentialKafkaProxyOraclePacket(spec, output)
}

func validateDifferentialKafkaProxyDriverSpec(spec DifferentialCase) error {
	if !reflect.DeepEqual(spec, mustDifferentialCase(spec.Name)) {
		return fmt.Errorf(
			"protocol policy %q requires the exact pinned kafka-proxy case",
			spec.ComparisonPolicy,
		)
	}
	return nil
}

func exchangeDifferentialKafkaProxyCommands(
	conn *websocket.Conn,
) (kafka_proxy.PubSubResponse, kafka_proxy.PubSubResponse, error) {
	requests, err := differentialKafkaProxyRequests()
	if err != nil {
		return kafka_proxy.PubSubResponse{}, kafka_proxy.PubSubResponse{}, err
	}
	responses := make([]kafka_proxy.PubSubResponse, 0, len(requests))
	for index, request := range requests {
		if err := conn.WriteMessage(websocket.BinaryMessage, request); err != nil {
			return kafka_proxy.PubSubResponse{}, kafka_proxy.PubSubResponse{}, fmt.Errorf(
				"write Kafka PubSub command %d: %w", index+1, err,
			)
		}
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return kafka_proxy.PubSubResponse{}, kafka_proxy.PubSubResponse{}, fmt.Errorf(
				"read Kafka PubSub response %d: %w", index+1, err,
			)
		}
		if messageType != websocket.BinaryMessage {
			return kafka_proxy.PubSubResponse{}, kafka_proxy.PubSubResponse{}, fmt.Errorf(
				"kafka PubSub response %d message type = %d, want binary", index+1, messageType,
			)
		}
		response, err := kafka_proxy.ParsePubSubResponse(payload)
		if err != nil {
			return kafka_proxy.PubSubResponse{}, kafka_proxy.PubSubResponse{}, fmt.Errorf(
				"parse Kafka PubSub response %d: %w", index+1, err,
			)
		}
		responses = append(responses, response)
	}
	return responses[0], responses[1], nil
}

func differentialKafkaProxyRequests() ([2][]byte, error) {
	var requests [2][]byte
	commands := [2]kafka_proxy.PubSubRequest{
		{
			Sequence: 3, Command: kafka_proxy.CmdKafkaListOffset,
			Topic: differentialKafkaPubSubTopic, Partition: differentialKafkaPubSubPartition, Position: -1,
		},
		{
			Sequence: 6, Command: kafka_proxy.CmdKafkaFetch,
			Topic: differentialKafkaPubSubTopic, Partition: differentialKafkaPubSubPartition,
			Position: differentialKafkaPubSubRecordOffset,
		},
	}
	for index, command := range commands {
		encoded, err := kafka_proxy.MarshalPubSubRequest(command)
		if err != nil {
			return requests, fmt.Errorf("marshal Kafka PubSub command %d: %w", index+1, err)
		}
		requests[index] = encoded
	}
	return requests, nil
}

func newDifferentialKafkaProxyObservation(
	spec DifferentialCase,
	listOffset kafka_proxy.PubSubResponse,
	fetch kafka_proxy.PubSubResponse,
) (DifferentialObservation, error) {
	if listOffset.Kind != kafka_proxy.RespKafkaListOffset {
		return DifferentialObservation{}, fmt.Errorf(
			"kafka PubSub list-offset response kind = %d, want %d",
			listOffset.Kind, kafka_proxy.RespKafkaListOffset,
		)
	}
	if fetch.Kind != kafka_proxy.RespKafkaFetch {
		return DifferentialObservation{}, fmt.Errorf(
			"kafka PubSub fetch response kind = %d, want %d",
			fetch.Kind, kafka_proxy.RespKafkaFetch,
		)
	}
	messages := make([]differentialKafkaProxyMessageTranscript, 0, len(fetch.Messages))
	for _, message := range fetch.Messages {
		messages = append(messages, differentialKafkaProxyMessageTranscript{
			Offset: message.Offset, Timestamp: message.Timestamp,
			Key: string(message.Key), Value: string(message.Value),
		})
	}
	encoded, err := json.Marshal(differentialKafkaProxyTranscript{
		ListOffset: differentialKafkaProxyOffsetTranscript{
			Sequence: listOffset.Sequence,
			Offset:   listOffset.Offset,
		},
		Fetch: differentialKafkaProxyFetchTranscript{
			Sequence: fetch.Sequence,
			Messages: messages,
		},
	})
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("marshal Kafka PubSub transcript: %w", err)
	}
	return DifferentialObservation{
		Status: http.StatusSwitchingProtocols,
		Headers: map[string][]string{
			"Connection": {"upgrade"},
			"Upgrade":    {"websocket"},
		},
		Body:             string(encoded),
		Host:             spec.Request.Host,
		SNI:              spec.Request.SNI,
		SecurityDecision: spec.SecurityDecision,
	}, nil
}

func decodeDifferentialKafkaProxyTranscript(body string) (differentialKafkaProxyTranscript, error) {
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	var transcript differentialKafkaProxyTranscript
	if err := decoder.Decode(&transcript); err != nil {
		return differentialKafkaProxyTranscript{}, fmt.Errorf("decode Kafka PubSub transcript: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return differentialKafkaProxyTranscript{}, errors.New("decode Kafka PubSub transcript: trailing data")
	}
	return transcript, nil
}

func validateDifferentialKafkaProxyHandshake(headers http.Header) error {
	if !strings.EqualFold(strings.TrimSpace(headers.Get("Upgrade")), "websocket") {
		return fmt.Errorf("kafka PubSub Upgrade header = %q, want websocket", headers.Get("Upgrade"))
	}
	for token := range strings.SplitSeq(headers.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return nil
		}
	}
	return fmt.Errorf("kafka PubSub Connection header = %q, want Upgrade token", headers.Get("Connection"))
}

func parseDifferentialKafkaProxyOraclePacket(
	spec DifferentialCase,
	packet []byte,
) (DifferentialObservation, error) {
	frames, err := decodeDifferentialKafkaProxyOracleFrames(packet, 3)
	if err != nil {
		return DifferentialObservation{}, err
	}
	response, err := http.ReadResponse(
		bufio.NewReader(bytes.NewReader(frames[0])),
		&http.Request{Method: http.MethodGet},
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse Kafka Proxy Oracle handshake: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusSwitchingProtocols {
		return DifferentialObservation{}, fmt.Errorf(
			"kafka Proxy Oracle handshake status = %d, want 101", response.StatusCode,
		)
	}
	if err := validateDifferentialKafkaProxyHandshake(response.Header); err != nil {
		return DifferentialObservation{}, err
	}
	listOffset, err := kafka_proxy.ParsePubSubResponse(frames[1])
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse Kafka Proxy Oracle list-offset response: %w", err)
	}
	fetch, err := kafka_proxy.ParsePubSubResponse(frames[2])
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse Kafka Proxy Oracle fetch response: %w", err)
	}
	return newDifferentialKafkaProxyObservation(spec, listOffset, fetch)
}

func decodeDifferentialKafkaProxyOracleFrames(packet []byte, count int) ([][]byte, error) {
	frames := make([][]byte, 0, count)
	for index := range count {
		if len(packet) < 4 {
			return nil, fmt.Errorf("kafka Proxy Oracle packet missing frame %d length", index+1)
		}
		length := int(binary.BigEndian.Uint32(packet[:4]))
		packet = packet[4:]
		if length < 0 || len(packet) < length {
			return nil, fmt.Errorf("kafka Proxy Oracle packet truncated at frame %d", index+1)
		}
		frames = append(frames, append([]byte(nil), packet[:length]...))
		packet = packet[length:]
	}
	if len(packet) != 0 {
		return nil, fmt.Errorf("kafka Proxy Oracle packet has %d trailing bytes", len(packet))
	}
	return frames, nil
}

const differentialKafkaProxyOraclePerlScript = `
use strict;
use warnings;
binmode STDOUT;

my ($path, $host, $list_hex, $fetch_hex) = @ARGV;
my $socket = IO::Socket::INET->new(
    PeerAddr => '127.0.0.1', PeerPort => 9080, Proto => 'tcp', Timeout => 5,
) or die "connect: $!";
$socket->autoflush(1);
my $key = 'dGhlIHNhbXBsZSBub25jZQ==';
write_all($socket, "GET $path HTTP/1.1\r\nHost: $host\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: $key\r\nSec-WebSocket-Version: 13\r\n\r\n");
my $head = read_head($socket);
$head =~ m{^HTTP/1\.[01] 101 } or die "status is not 101";
my $accept = 's3pPLMBiTxaQ9kYGzzhZRbK+xOo=';
$head =~ /^Sec-WebSocket-Accept:\s*\Q$accept\E\s*$/mi or die "invalid Sec-WebSocket-Accept";
emit_frame($head);
for my $request_hex ($list_hex, $fetch_hex) {
    send_frame($socket, 2, pack('H*', $request_hex));
    my ($opcode, $payload) = read_frame($socket);
    $opcode == 2 or die "response opcode $opcode is not binary";
    emit_frame($payload);
}

sub write_all {
    my ($fh, $data) = @_;
    while (length($data)) {
        my $written = syswrite($fh, $data);
        defined($written) && $written > 0 or die "write: $!";
        substr($data, 0, $written, '');
    }
}

sub read_exact {
    my ($fh, $length) = @_;
    my $data = '';
    while (length($data) < $length) {
        my $read = sysread($fh, my $chunk, $length - length($data));
        defined($read) or die "read: $!";
        $read > 0 or die "unexpected EOF";
        $data .= $chunk;
    }
    return $data;
}

sub read_head {
    my ($fh) = @_;
    my $data = '';
    while (index($data, "\r\n\r\n") < 0) {
        $data .= read_exact($fh, 1);
        length($data) <= 16384 or die "handshake too large";
    }
    return $data;
}

sub send_frame {
    my ($fh, $opcode, $payload) = @_;
    my $length = length($payload);
    my $head = pack('C', 0x80 | $opcode);
    if ($length < 126) {
        $head .= pack('C', 0x80 | $length);
    } elsif ($length <= 65535) {
        $head .= pack('Cn', 0x80 | 126, $length);
    } else {
        $head .= pack('CNN', 0x80 | 127, 0, $length);
    }
    my $mask = pack('C4', 0x12, 0x34, 0x56, 0x78);
    my $masked = '';
    for (my $index = 0; $index < $length; $index++) {
        $masked .= chr(ord(substr($payload, $index, 1)) ^ ord(substr($mask, $index % 4, 1)));
    }
    write_all($fh, $head . $mask . $masked);
}

sub read_frame {
    my ($fh) = @_;
    my ($first, $second) = unpack('CC', read_exact($fh, 2));
    ($first & 0x80) or die "fragmented response";
    ($second & 0x80) == 0 or die "masked server response";
    my $length = $second & 0x7f;
    $length = unpack('n', read_exact($fh, 2)) if $length == 126;
    if ($length == 127) {
        my ($high, $low) = unpack('NN', read_exact($fh, 8));
        $high == 0 or die "response too large";
        $length = $low;
    }
    return ($first & 0x0f, read_exact($fh, $length));
}

sub emit_frame {
    my ($payload) = @_;
    print STDOUT pack('N', length($payload)), $payload;
}
`
