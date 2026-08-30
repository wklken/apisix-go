package pluginintegration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type differentialMCPBridgeSSEEvent struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type differentialMCPBridgeTranscript struct {
	Endpoint differentialMCPBridgeSSEEvent `json:"endpoint"`
	Ping     differentialMCPBridgeSSEEvent `json:"ping"`
	Message  differentialMCPBridgeSSEEvent `json:"message"`
}

type differentialProtocolDriver struct {
	observeCandidate func(DifferentialCase, int) (DifferentialObservation, error)
	observeOracle    func(DifferentialCase, *differentialChild) (DifferentialObservation, error)
}

var differentialProtocolDriverRegistry = map[string]differentialProtocolDriver{
	differentialMCPBridgeSSESessionPolicy: {
		observeCandidate: observeDifferentialMCPBridgeCandidate,
		observeOracle:    observeDifferentialMCPBridgeOracle,
	},
	differentialKafkaProxyPubSubPolicy: differentialKafkaProxyProtocolDriver(),
}

// observeDifferentialProtocolCandidate is the single candidate-side dispatch
// hook needed by the shared runner before it executes an ordinary HTTP request.
func observeDifferentialProtocolCandidate(
	spec DifferentialCase,
	dataPort int,
) (DifferentialObservation, bool, error) {
	driver, ok := differentialProtocolDriverRegistry[spec.ComparisonPolicy]
	if !ok {
		return DifferentialObservation{}, false, nil
	}
	if driver.observeCandidate == nil {
		return DifferentialObservation{}, true, fmt.Errorf(
			"differential protocol policy %q has no candidate driver",
			spec.ComparisonPolicy,
		)
	}
	observation, err := driver.observeCandidate(spec, dataPort)
	return observation, true, err
}

// observeDifferentialProtocolOracle is the matching Oracle-side dispatch hook.
func observeDifferentialProtocolOracle(
	spec DifferentialCase,
	child *differentialChild,
) (DifferentialObservation, bool, error) {
	driver, ok := differentialProtocolDriverRegistry[spec.ComparisonPolicy]
	if !ok {
		return DifferentialObservation{}, false, nil
	}
	if driver.observeOracle == nil {
		return DifferentialObservation{}, true, fmt.Errorf(
			"differential protocol policy %q has no Oracle driver",
			spec.ComparisonPolicy,
		)
	}
	observation, err := driver.observeOracle(spec, child)
	return observation, true, err
}

func observeDifferentialMCPBridgeCandidate(
	spec DifferentialCase,
	dataPort int,
) (DifferentialObservation, error) {
	if err := validateDifferentialMCPBridgeDriverSpec(spec); err != nil {
		return DifferentialObservation{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	baseURL := "http://127.0.0.1:" + strconv.Itoa(dataPort)
	sseRequest, err := http.NewRequestWithContext(
		ctx, spec.Request.Method, baseURL+spec.Request.Path, nil,
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("build MCP SSE request: %w", err)
	}
	sseRequest.Host = spec.Request.Host
	for name, value := range spec.Request.Headers {
		sseRequest.Header.Set(name, value)
	}
	sseResponse, err := client.Do(sseRequest)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("open MCP SSE stream: %w", err)
	}
	defer func() { _ = sseResponse.Body.Close() }()
	if sseResponse.StatusCode != http.StatusOK {
		return DifferentialObservation{}, fmt.Errorf(
			"MCP SSE status = %d, want 200", sseResponse.StatusCode,
		)
	}

	reader := bufio.NewReader(sseResponse.Body)
	endpoint, err := readDifferentialMCPBridgeSSEEvent(reader)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("read MCP endpoint event: %w", err)
	}
	ping, err := readDifferentialMCPBridgeSSEEvent(reader)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("read MCP ping event: %w", err)
	}
	endpointReference, err := url.Parse(endpoint.Data)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse MCP message endpoint: %w", err)
	}
	if endpointReference.IsAbs() || endpointReference.Host != "" || !strings.HasPrefix(endpointReference.Path, "/") {
		return DifferentialObservation{}, fmt.Errorf("MCP message endpoint %q is not origin-relative", endpoint.Data)
	}
	postRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+endpointReference.RequestURI(),
		strings.NewReader(differentialMCPBridgePostedPayload),
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("build MCP message request: %w", err)
	}
	postRequest.Host = spec.Request.Host
	postRequest.Header.Set("Content-Type", "application/json")
	postResponse, err := client.Do(postRequest)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("post MCP message: %w", err)
	}
	postBody, readErr := io.ReadAll(postResponse.Body)
	closeErr := postResponse.Body.Close()
	if readErr != nil {
		return DifferentialObservation{}, fmt.Errorf("read MCP message response: %w", readErr)
	}
	if closeErr != nil {
		return DifferentialObservation{}, fmt.Errorf("close MCP message response: %w", closeErr)
	}
	message, err := readDifferentialMCPBridgeSSEEvent(reader)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("read MCP response event: %w", err)
	}
	return newDifferentialMCPBridgeObservation(
		spec,
		sseResponse.StatusCode,
		differentialHTTPHeaders(sseResponse.Header),
		endpoint,
		ping,
		message,
		postResponse.StatusCode,
		differentialHTTPHeaders(postResponse.Header),
		string(postBody),
	)
}

func observeDifferentialMCPBridgeOracle(
	spec DifferentialCase,
	child *differentialChild,
) (DifferentialObservation, error) {
	if err := validateDifferentialMCPBridgeDriverSpec(spec); err != nil {
		return DifferentialObservation{}, err
	}
	if child == nil || !child.container || child.runtime == "" || child.name == "" {
		return DifferentialObservation{}, errors.New("MCP Oracle driver requires a running Oracle container")
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
		differentialMCPBridgeOraclePerlScript,
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf(
			"execute MCP Oracle SSE driver: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return parseDifferentialMCPBridgeOraclePacket(spec, output)
}

func validateDifferentialMCPBridgeDriverSpec(spec DifferentialCase) error {
	if !reflect.DeepEqual(spec, mustDifferentialCase(spec.Name)) {
		return fmt.Errorf(
			"protocol policy %q requires the exact pinned mcp-bridge case",
			spec.ComparisonPolicy,
		)
	}
	return nil
}

func newDifferentialMCPBridgeObservation(
	spec DifferentialCase,
	sseStatus int,
	sseHeaders map[string][]string,
	endpoint differentialMCPBridgeSSEEvent,
	ping differentialMCPBridgeSSEEvent,
	message differentialMCPBridgeSSEEvent,
	postStatus int,
	postHeaders map[string][]string,
	postBody string,
) (DifferentialObservation, error) {
	encoded, err := json.Marshal(differentialMCPBridgeTranscript{
		Endpoint: endpoint,
		Ping:     ping,
		Message:  message,
	})
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("marshal MCP transcript: %w", err)
	}
	return DifferentialObservation{
		Status:           sseStatus,
		Headers:          sseHeaders,
		Body:             string(encoded),
		Host:             spec.Request.Host,
		SNI:              spec.Request.SNI,
		SecurityDecision: spec.SecurityDecision,
		Steps: []DifferentialStepObservation{{
			Status:           postStatus,
			Headers:          postHeaders,
			Body:             postBody,
			Host:             spec.Request.Host,
			SNI:              spec.Request.SNI,
			SecurityDecision: spec.SecurityDecision,
		}},
	}, nil
}

func readDifferentialMCPBridgeSSEEvent(
	reader *bufio.Reader,
) (differentialMCPBridgeSSEEvent, error) {
	var event differentialMCPBridgeSSEEvent
	var data []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return differentialMCPBridgeSSEEvent{}, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if event.Event == "" || len(data) == 0 {
				return differentialMCPBridgeSSEEvent{}, errors.New("SSE event is missing event or data")
			}
			event.Data = strings.Join(data, "\n")
			return event, nil
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}

func decodeDifferentialMCPBridgeTranscript(raw string) (differentialMCPBridgeTranscript, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var transcript differentialMCPBridgeTranscript
	if err := decoder.Decode(&transcript); err != nil {
		return differentialMCPBridgeTranscript{}, fmt.Errorf("decode MCP transcript: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return differentialMCPBridgeTranscript{}, errors.New("MCP transcript contains trailing JSON")
		}
		return differentialMCPBridgeTranscript{}, fmt.Errorf("decode trailing MCP transcript: %w", err)
	}
	return transcript, nil
}

func parseDifferentialMCPBridgeOraclePacket(
	spec DifferentialCase,
	packet []byte,
) (DifferentialObservation, error) {
	frames, err := decodeDifferentialMCPBridgeOracleFrames(packet, 5)
	if err != nil {
		return DifferentialObservation{}, err
	}
	sseResponse, err := parseDifferentialMCPBridgeRawResponse(
		http.MethodGet, spec.Request.Path, spec.Request.Host, frames[0],
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse MCP Oracle SSE response: %w", err)
	}
	defer func() { _ = sseResponse.Body.Close() }()
	endpoint, err := readDifferentialMCPBridgeSSEEvent(bufio.NewReader(bytes.NewReader(frames[1])))
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse MCP Oracle endpoint event: %w", err)
	}
	ping, err := readDifferentialMCPBridgeSSEEvent(bufio.NewReader(bytes.NewReader(frames[2])))
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse MCP Oracle ping event: %w", err)
	}
	postResponse, err := parseDifferentialMCPBridgeRawResponse(
		http.MethodPost, endpoint.Data, spec.Request.Host, frames[3],
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse MCP Oracle message response: %w", err)
	}
	postBody, readErr := io.ReadAll(postResponse.Body)
	closeErr := postResponse.Body.Close()
	if readErr != nil {
		return DifferentialObservation{}, fmt.Errorf("read MCP Oracle message response: %w", readErr)
	}
	if closeErr != nil {
		return DifferentialObservation{}, fmt.Errorf("close MCP Oracle message response: %w", closeErr)
	}
	message, err := readDifferentialMCPBridgeSSEEvent(bufio.NewReader(bytes.NewReader(frames[4])))
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("parse MCP Oracle response event: %w", err)
	}
	return newDifferentialMCPBridgeObservation(
		spec,
		sseResponse.StatusCode,
		differentialHTTPHeaders(sseResponse.Header),
		endpoint,
		ping,
		message,
		postResponse.StatusCode,
		differentialHTTPHeaders(postResponse.Header),
		string(postBody),
	)
}

func decodeDifferentialMCPBridgeOracleFrames(packet []byte, count int) ([][]byte, error) {
	frames := make([][]byte, 0, count)
	for index := range count {
		if len(packet) < 4 {
			return nil, fmt.Errorf("MCP Oracle frame %d is missing its length", index+1)
		}
		length := int(binary.BigEndian.Uint32(packet[:4]))
		packet = packet[4:]
		if length < 0 || length > len(packet) {
			return nil, fmt.Errorf("MCP Oracle frame %d length %d exceeds remaining %d", index+1, length, len(packet))
		}
		frames = append(frames, append([]byte(nil), packet[:length]...))
		packet = packet[length:]
	}
	if len(packet) != 0 {
		return nil, fmt.Errorf("MCP Oracle packet has %d trailing bytes after frame %d", len(packet), count)
	}
	return frames, nil
}

func parseDifferentialMCPBridgeRawResponse(
	method string,
	path string,
	host string,
	raw []byte,
) (*http.Response, error) {
	request, err := http.NewRequest(method, "http://"+host+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

const differentialMCPBridgeOraclePerlScript = `
use strict;
use warnings;
binmode STDOUT;

sub connect_gateway {
    my $socket = IO::Socket::INET->new(PeerAddr => "127.0.0.1", PeerPort => 9080,
        Proto => "tcp", Timeout => 2) or die "connect: $!\n";
    return $socket;
}
sub write_all {
    my ($socket, $value) = @_;
    my $offset = 0;
    while ($offset < length($value)) {
        my $count = syswrite($socket, $value, length($value) - $offset, $offset);
        die "write: $!\n" unless defined $count;
        $offset += $count;
    }
}
sub read_more {
    my ($socket, $buffer) = @_;
    my $count = sysread($socket, my $chunk, 8192);
    die "read: $!\n" unless defined $count;
    die "unexpected EOF\n" if $count == 0;
    $$buffer .= $chunk;
}
sub read_head {
    my ($socket, $buffer) = @_;
    while (index($$buffer, "\r\n\r\n") < 0) { read_more($socket, $buffer); }
    my $end = index($$buffer, "\r\n\r\n") + 4;
    return substr($$buffer, 0, $end, "");
}
sub read_line {
    my ($socket, $buffer) = @_;
    while (index($$buffer, "\r\n") < 0) { read_more($socket, $buffer); }
    my $end = index($$buffer, "\r\n");
    my $line = substr($$buffer, 0, $end, "");
    substr($$buffer, 0, 2, "");
    return $line;
}
sub read_exact {
    my ($socket, $buffer, $length) = @_;
    while (length($$buffer) < $length) { read_more($socket, $buffer); }
    return substr($$buffer, 0, $length, "");
}
sub next_chunk {
    my ($socket, $buffer) = @_;
    my $line = read_line($socket, $buffer);
    $line =~ s/;.*$//;
    die "invalid chunk size\n" unless $line =~ /^[0-9A-Fa-f]+$/;
    my $size = hex($line);
    die "SSE ended before expected event\n" if $size == 0;
    my $data = read_exact($socket, $buffer, $size);
    die "invalid chunk terminator\n" unless read_exact($socket, $buffer, 2) eq "\r\n";
    return $data;
}
sub next_event {
    my ($socket, $wire, $events) = @_;
    while (index($$events, "\n\n") < 0) { $$events .= next_chunk($socket, $wire); }
    my $end = index($$events, "\n\n") + 2;
    return substr($$events, 0, $end, "");
}
sub read_to_eof {
    my ($socket) = @_;
    my $value = "";
    while (1) {
        my $count = sysread($socket, my $chunk, 8192);
        die "read: $!\n" unless defined $count;
        last if $count == 0;
        $value .= $chunk;
    }
    return $value;
}
sub emit_frame {
    my ($value) = @_;
    print pack("N", length($value)), $value;
}

my $host = "gateway.example.test";
my $payload = '{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}';
my $sse = connect_gateway();
write_all($sse, "GET /mcp/sse HTTP/1.1\r\nHost: $host\r\nAccept: text/event-stream\r\nConnection: keep-alive\r\n\r\n");
my $wire = "";
my $head = read_head($sse, \$wire);
die "SSE response is not chunked\n" unless $head =~ /transfer-encoding:\s*chunked/i;
my $events = "";
my $endpoint_event = next_event($sse, \$wire, \$events);
my $ping_event = next_event($sse, \$wire, \$events);
$endpoint_event =~ /^event:\s*endpoint\ndata:\s*([^\n]+)\n\n$/
    or die "invalid endpoint event\n";
my $endpoint = $1;
die "unsafe message endpoint\n"
    unless $endpoint =~ m{^/mcp/message\?sessionId=[0-9A-Fa-f-]+$};

my $post = connect_gateway();
my $post_request = "POST $endpoint HTTP/1.1\r\nHost: $host\r\nContent-Type: application/json\r\n" .
    "Content-Length: " . length($payload) . "\r\nConnection: close\r\n\r\n" . $payload;
write_all($post, $post_request);
my $post_response = read_to_eof($post);
my $message_event = next_event($sse, \$wire, \$events);

emit_frame($head);
emit_frame($endpoint_event);
emit_frame($ping_event);
emit_frame($post_response);
emit_frame($message_event);
`
