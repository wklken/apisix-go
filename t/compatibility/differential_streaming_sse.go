package pluginintegration

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	differentialFixtureWireSSEHTTP   = "sse-http-stream"
	differentialSSEOraclePacketMagic = "APISIX-GO-SSE/1"
)

type differentialSSEStreamContract struct {
	Frames           []string
	RequiredFrames   int
	InterFrameDelay  time.Duration
	OpenProbeWindow  time.Duration
	CloseAfterFrames bool
}

type differentialSSEStreamObservation struct {
	Status                            int      `json:"status"`
	ContentType                       string   `json:"content_type"`
	Frames                            []string `json:"frames"`
	ConnectionOpenAfterRequiredFrames bool     `json:"connection_open_after_required_frames"`
}

func init() {
	differentialProtocolDriverRegistry[differentialProxyBufferingSSEPolicy] = differentialProtocolDriver{
		observeCandidate: observeDifferentialProxyBufferingSSECandidate,
		observeOracle:    observeDifferentialProxyBufferingSSEOracle,
	}
}

func observeDifferentialProxyBufferingSSECandidate(
	spec DifferentialCase,
	dataPort int,
) (DifferentialObservation, error) {
	streamCase, err := differentialProxyBufferingStreamCaseForSpec(spec)
	if err != nil {
		return DifferentialObservation{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := observeDifferentialSSE(
		ctx,
		net.JoinHostPort("127.0.0.1", strconv.Itoa(dataPort)),
		spec.Request,
		streamCase.Contract,
	)
	if err != nil {
		return DifferentialObservation{}, err
	}
	return differentialSSEObservationEnvelope(spec, stream)
}

func observeDifferentialProxyBufferingSSEOracle(
	spec DifferentialCase,
	child *differentialChild,
) (DifferentialObservation, error) {
	if _, err := differentialProxyBufferingStreamCaseForSpec(spec); err != nil {
		return DifferentialObservation{}, err
	}
	if child == nil || !child.container || child.runtime == "" || child.name == "" {
		return DifferentialObservation{}, errors.New(
			"proxy-buffering SSE Oracle driver requires a running Oracle container",
		)
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
		"-MIO::Select",
		"-e",
		differentialProxyBufferingSSEOraclePerlScript,
	)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf(
			"execute proxy-buffering Oracle SSE driver: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	stream, err := parseDifferentialSSEOraclePacket(output)
	if err != nil {
		return DifferentialObservation{}, err
	}
	return differentialSSEObservationEnvelope(spec, stream)
}

func differentialProxyBufferingStreamCaseForSpec(
	spec DifferentialCase,
) (differentialProxyBufferingStreamCase, error) {
	pinned := mustDifferentialCase(spec.Name)
	if !reflect.DeepEqual(spec, pinned) {
		return differentialProxyBufferingStreamCase{}, fmt.Errorf(
			"protocol policy %q requires the exact pinned proxy-buffering SSE case",
			spec.ComparisonPolicy,
		)
	}
	return newDifferentialProxyBufferingStreamCase(spec), nil
}

func differentialSSEObservationEnvelope(
	spec DifferentialCase,
	stream differentialSSEStreamObservation,
) (DifferentialObservation, error) {
	body, err := encodeDifferentialSSEDriverOutput(stream)
	if err != nil {
		return DifferentialObservation{}, err
	}
	return DifferentialObservation{
		Status:           stream.Status,
		Headers:          map[string][]string{"Content-Type": {stream.ContentType}},
		Body:             string(body),
		Host:             spec.Request.Host,
		SecurityDecision: spec.SecurityDecision,
	}, nil
}

// observeDifferentialSSE is the common candidate/oracle wire driver. It must be
// invoked from the network namespace that can reach address. The oracle runner
// can therefore execute the same driver logic in its container and return the
// JSON observation through encodeDifferentialSSEDriverOutput.
func observeDifferentialSSE(
	ctx context.Context,
	address string,
	request DifferentialRequest,
	contract differentialSSEStreamContract,
) (differentialSSEStreamObservation, error) {
	if err := validateDifferentialSSEContract(contract); err != nil {
		return differentialSSEStreamObservation{}, err
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf("dial SSE endpoint: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return differentialSSEStreamObservation{}, fmt.Errorf("set SSE deadline: %w", err)
		}
	}

	httpRequest, err := differentialSSEHTTPRequest(address, request)
	if err != nil {
		return differentialSSEStreamObservation{}, err
	}
	if err := httpRequest.Write(conn); err != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf("write SSE request: %w", err)
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), httpRequest)
	if err != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf("read SSE response: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf("parse SSE Content-Type: %w", err)
	}

	observation := differentialSSEStreamObservation{
		Status:      response.StatusCode,
		ContentType: mediaType,
		Frames:      make([]string, 0, contract.RequiredFrames),
	}
	reader := bufio.NewReader(response.Body)
	for len(observation.Frames) < contract.RequiredFrames {
		frame, err := readDifferentialSSEFrame(reader)
		if err != nil {
			return differentialSSEStreamObservation{}, fmt.Errorf(
				"read SSE frame %d of %d: %w",
				len(observation.Frames)+1,
				contract.RequiredFrames,
				err,
			)
		}
		observation.Frames = append(observation.Frames, frame)
	}

	if err := conn.SetReadDeadline(time.Now().Add(contract.OpenProbeWindow)); err != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf("set SSE open probe deadline: %w", err)
	}
	var probe [1]byte
	n, probeErr := reader.Read(probe[:])
	switch {
	case n > 0:
		observation.ConnectionOpenAfterRequiredFrames = true
	case probeErr == nil:
		return differentialSSEStreamObservation{}, errors.New("SSE open probe made no progress")
	case errors.Is(probeErr, io.EOF):
		observation.ConnectionOpenAfterRequiredFrames = false
	default:
		var netErr net.Error
		if !errors.As(probeErr, &netErr) || !netErr.Timeout() {
			return differentialSSEStreamObservation{}, fmt.Errorf("probe SSE connection: %w", probeErr)
		}
		observation.ConnectionOpenAfterRequiredFrames = true
	}
	return observation, nil
}

func differentialSSEHTTPRequest(address string, request DifferentialRequest) (*http.Request, error) {
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	path := request.Path
	if path == "" {
		path = "/"
	}
	target := &url.URL{Scheme: "http", Host: address, Path: path}
	httpRequest, err := http.NewRequest(method, target.String(), strings.NewReader(request.Body))
	if err != nil {
		return nil, fmt.Errorf("build SSE request: %w", err)
	}
	if request.Host != "" {
		httpRequest.Host = request.Host
	}
	for name, value := range request.Headers {
		httpRequest.Header.Set(name, value)
	}
	return httpRequest, nil
}

func readDifferentialSSEFrame(reader *bufio.Reader) (string, error) {
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", io.ErrUnexpectedEOF
			}
			return "", err
		}
		if line == "\n" || line == "\r\n" {
			if frame.Len() == 0 {
				continue
			}
			frame.WriteString(line)
			return frame.String(), nil
		}
		frame.WriteString(line)
	}
}

func validateDifferentialSSEContract(contract differentialSSEStreamContract) error {
	if contract.RequiredFrames <= 0 {
		return fmt.Errorf("required SSE frames = %d, want greater than zero", contract.RequiredFrames)
	}
	if len(contract.Frames) < contract.RequiredFrames && !contract.CloseAfterFrames {
		return fmt.Errorf(
			"SSE fixture frames = %d, want at least %d",
			len(contract.Frames),
			contract.RequiredFrames,
		)
	}
	if contract.InterFrameDelay < 0 {
		return fmt.Errorf("SSE inter-frame delay = %s, want non-negative", contract.InterFrameDelay)
	}
	if contract.OpenProbeWindow <= 0 {
		return fmt.Errorf("SSE open probe window = %s, want greater than zero", contract.OpenProbeWindow)
	}
	return nil
}

func encodeDifferentialSSEDriverOutput(observation differentialSSEStreamObservation) ([]byte, error) {
	return json.Marshal(observation)
}

func parseDifferentialSSEDriverOutput(raw []byte) (differentialSSEStreamObservation, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var observation differentialSSEStreamObservation
	if err := decoder.Decode(&observation); err != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf("decode SSE driver output: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return differentialSSEStreamObservation{}, errors.New("SSE driver output contains multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return differentialSSEStreamObservation{}, fmt.Errorf("decode trailing SSE driver output: %w", err)
	}
	return observation, nil
}

func parseDifferentialSSEOraclePacket(raw []byte) (differentialSSEStreamObservation, error) {
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) < 5 || lines[0] != differentialSSEOraclePacketMagic {
		return differentialSSEStreamObservation{}, errors.New("invalid proxy-buffering Oracle SSE packet")
	}
	status, err := strconv.Atoi(lines[1])
	if err != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf("parse Oracle SSE status: %w", err)
	}
	contentType, err := hexDecodeDifferentialSSEPacketField(lines[2], "Content-Type")
	if err != nil {
		return differentialSSEStreamObservation{}, err
	}
	open := false
	switch lines[3] {
	case "0":
	case "1":
		open = true
	default:
		return differentialSSEStreamObservation{}, fmt.Errorf("oracle SSE open flag = %q", lines[3])
	}
	count, err := strconv.Atoi(lines[4])
	if err != nil || count < 0 || len(lines) != 5+count {
		return differentialSSEStreamObservation{}, fmt.Errorf(
			"oracle SSE frame count = %q with %d packet lines",
			lines[4],
			len(lines),
		)
	}
	frames := make([]string, count)
	for index := range frames {
		frames[index], err = hexDecodeDifferentialSSEPacketField(lines[5+index], "frame")
		if err != nil {
			return differentialSSEStreamObservation{}, err
		}
	}
	return differentialSSEStreamObservation{
		Status:                            status,
		ContentType:                       contentType,
		Frames:                            frames,
		ConnectionOpenAfterRequiredFrames: open,
	}, nil
}

func hexDecodeDifferentialSSEPacketField(raw string, name string) (string, error) {
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decode Oracle SSE %s: %w", name, err)
	}
	return string(decoded), nil
}

func startDifferentialProxyBufferingSSEFixture(
	spec DifferentialFixture,
) (*differentialFixtureServer, error) {
	streamCase := newDifferentialProxyBufferingStreamCase(
		mustDifferentialCase("proxy-buffering-disabled-incremental-sse"),
	)
	if !reflect.DeepEqual(spec, streamCase.Spec.Fixture) {
		return nil, errors.New("SSE fixture requires the exact proxy-buffering stream fixture")
	}
	probeToken, err := newDifferentialRunNonce()
	if err != nil {
		return nil, fmt.Errorf("create SSE fixture probe token: %w", err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic SSE fixture: %w", err)
	}
	fixture := &differentialFixtureServer{
		listener:    listener,
		requests:    make(chan differentialCapturedRequest, 16),
		errors:      make(chan error, 16),
		response:    spec.Response,
		fixture:     spec.Name,
		probeToken:  probeToken,
		wire:        spec.WireProtocol,
		captureHTTP: true,
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.RequestURI() == differentialFixtureProbePath &&
			request.Header.Get(differentialFixtureProbeHeader) == fixture.probeToken {
			writer.Header().Set(differentialFixtureProbeHeader, fixture.probeToken)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		body, readErr := io.ReadAll(request.Body)
		closeErr := request.Body.Close()
		if readErr != nil || closeErr != nil {
			fixture.reportError(fmt.Errorf("read/close SSE fixture request body: %v / %v", readErr, closeErr))
			http.Error(writer, "invalid SSE fixture request", http.StatusBadRequest)
			return
		}
		fixture.capture(differentialCapturedRequest{
			Method:  request.Method,
			Path:    request.URL.RequestURI(),
			Host:    request.Host,
			Headers: request.Header.Clone(),
			Body:    string(body),
		})
		flusher, ok := writer.(http.Flusher)
		if !ok {
			fixture.reportError(errors.New("SSE fixture response writer cannot flush"))
			http.Error(writer, "SSE flushing unavailable", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.WriteHeader(http.StatusOK)
		for index, frame := range streamCase.Contract.Frames {
			if _, err := io.WriteString(writer, frame); err != nil {
				return
			}
			flusher.Flush()
			if index+1 < len(streamCase.Contract.Frames) && streamCase.Contract.InterFrameDelay > 0 {
				select {
				case <-request.Context().Done():
					return
				case <-time.After(streamCase.Contract.InterFrameDelay):
				}
			}
		}
		hold := 4 * streamCase.Contract.OpenProbeWindow
		select {
		case <-request.Context().Done():
		case <-time.After(hold):
		}
	})
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	fixture.server = server
	return fixture, nil
}

const differentialProxyBufferingSSEOraclePerlScript = `
my $socket = IO::Socket::INET->new(PeerAddr => "127.0.0.1", PeerPort => 9080, Proto => "tcp", Timeout => 5) or die "connect: $!\n";
binmode $socket; $socket->autoflush(1);
print {$socket} "GET /events HTTP/1.1\r\nHost: gateway.example.test\r\nAccept: text/event-stream\r\nConnection: close\r\n\r\n";
my $buffer = "";
sub read_more { my $count = sysread($socket, my $chunk, 8192); die "read: $!\n" unless defined $count; return 0 if $count == 0; $buffer .= $chunk; return 1; }
sub take_until { my ($delimiter) = @_; while ((my $index = index($buffer, $delimiter)) < 0) { die "unexpected EOF\n" unless read_more(); } my $index = index($buffer, $delimiter); my $value = substr($buffer, 0, $index); substr($buffer, 0, $index + length($delimiter), ""); return $value; }
sub take_bytes { my ($length) = @_; while (length($buffer) < $length) { die "unexpected EOF\n" unless read_more(); } my $value = substr($buffer, 0, $length); substr($buffer, 0, $length, ""); return $value; }
my $head = take_until("\r\n\r\n");
my ($status) = $head =~ m{\AHTTP/\S+\s+(\d+)}; die "missing status\n" unless defined $status;
my ($content_type) = $head =~ /\r\nContent-Type:\s*([^\r\n]+)/i; $content_type = "" unless defined $content_type;
my ($transfer_encoding) = $head =~ /\r\nTransfer-Encoding:\s*([^\r\n]+)/i; $transfer_encoding = "" unless defined $transfer_encoding;
my ($content_length) = $head =~ /\r\nContent-Length:\s*(\d+)/i;
my $decoded = ""; my @frames = (); my $wire_body_count = 0;
sub extract_frames { while (@frames < 3) { my $index = index($decoded, "\n\n"); last if $index < 0; push @frames, substr($decoded, 0, $index + 2); substr($decoded, 0, $index + 2, ""); } }
my $chunked = $transfer_encoding =~ /(?:^|,)\s*chunked\s*(?:,|$)/i;
while (@frames < 3) {
    if ($chunked) { my $line = take_until("\r\n"); $line =~ s/;.*\z//; die "invalid chunk size\n" unless $line =~ /\A[0-9A-Fa-f]+\z/; my $size = hex($line); die "SSE EOF before three frames\n" if $size == 0; my $chunk = take_bytes($size); die "invalid chunk delimiter\n" unless take_bytes(2) eq "\r\n"; $decoded .= $chunk; }
    elsif (defined $content_length) { die "SSE EOF before three frames\n" if $wire_body_count >= $content_length; my $need = $content_length - $wire_body_count; my $chunk = take_bytes($need); $wire_body_count += length($chunk); $decoded .= $chunk; }
    else { die "SSE EOF before three frames\n" unless length($buffer) || read_more(); $decoded .= $buffer; $buffer = ""; }
    extract_frames();
}
my $open = 1;
if ($chunked) {
    my $select = IO::Select->new($socket);
    if (index($buffer, "\r\n") < 0 && !$select->can_read(0.05)) { $open = 1; }
    else { my $line = take_until("\r\n"); $line =~ s/;.*\z//; die "invalid probe chunk size\n" unless $line =~ /\A[0-9A-Fa-f]+\z/; $open = hex($line) == 0 ? 0 : 1; }
} elsif (defined $content_length) { $open = $wire_body_count < $content_length ? 1 : 0; }
else { my $select = IO::Select->new($socket); if ($select->can_read(0.05)) { my $count = sysread($socket, my $byte, 1); die "probe read: $!\n" unless defined $count; $open = $count == 0 ? 0 : 1; } }
print "APISIX-GO-SSE/1\n", $status, "\n", unpack("H*", $content_type), "\n", $open, "\n", scalar(@frames), "\n";
for my $frame (@frames) { print unpack("H*", $frame), "\n"; }
`

type differentialSSEFixture struct {
	listener net.Listener
	contract differentialSSEStreamContract
	stop     chan struct{}
	close    sync.Once
	wg       sync.WaitGroup
}

func newDifferentialSSEFixture(contract differentialSSEStreamContract) (*differentialSSEFixture, error) {
	if err := validateDifferentialSSEContract(contract); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for SSE fixture: %w", err)
	}
	fixture := &differentialSSEFixture{
		listener: listener,
		contract: contract,
		stop:     make(chan struct{}),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func (f *differentialSSEFixture) Address() string {
	return f.listener.Addr().String()
}

func (f *differentialSSEFixture) Close() error {
	f.close.Do(func() {
		close(f.stop)
		_ = f.listener.Close()
	})
	f.wg.Wait()
	return nil
}

func (f *differentialSSEFixture) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go f.serveConnection(conn)
	}
}

func (f *differentialSSEFixture) serveConnection(conn net.Conn) {
	defer f.wg.Done()
	defer func() { _ = conn.Close() }()
	request, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	_ = request.Body.Close()

	writer := bufio.NewWriter(conn)
	if _, err := writer.WriteString(
		"HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/event-stream\r\n" +
			"Cache-Control: no-cache\r\n" +
			"Transfer-Encoding: chunked\r\n\r\n",
	); err != nil {
		return
	}
	if err := writer.Flush(); err != nil {
		return
	}
	for index, frame := range f.contract.Frames {
		if _, err := fmt.Fprintf(writer, "%x\r\n%s\r\n", len(frame), frame); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
		if index+1 < len(f.contract.Frames) && f.contract.InterFrameDelay > 0 {
			timer := time.NewTimer(f.contract.InterFrameDelay)
			select {
			case <-timer.C:
			case <-f.stop:
				timer.Stop()
				return
			}
		}
	}
	if f.contract.CloseAfterFrames {
		_, _ = writer.WriteString("0\r\n\r\n")
		_ = writer.Flush()
		return
	}
	<-f.stop
}
