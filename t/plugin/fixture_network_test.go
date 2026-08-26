package pluginintegration

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

type namedFixture interface {
	address() string
	host() string
	port() string
	url() string
	close()
	assert(*testing.T, FixtureSpec)
}

type networkFixture struct {
	kind      string
	listener  net.Listener
	packet    net.PacketConn
	server    *httptest.Server
	caPath    string
	expect    []NetworkAssertion
	respond   []NetworkResponse
	received  chan []byte
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
	sequence  sync.Mutex
	next      int
	wg        sync.WaitGroup
}

type httpsConnectFixture struct {
	listener    net.Listener
	certificate tls.Certificate
	caPath      string
	respond     []HTTPResponse
	requests    chan capturedRequest
	errors      chan error
	done        chan struct{}
	closeOnce   sync.Once
	wg          sync.WaitGroup
}

const defaultZeroPacketObservation = 250 * time.Millisecond

func isExactZeroUDPFixture(spec FixtureSpec) bool {
	return spec.Kind == "udp" && spec.Count != nil &&
		spec.Count.AtLeast == 0 && spec.Count.AtMost == 0
}

func isExactHTTPFixture(spec FixtureSpec) bool {
	switch spec.Kind {
	case "http", "https", "h2c", "https-connect":
	default:
		return false
	}
	return spec.Count == nil && (len(spec.Expect) > 0 || spec.ExpectRequests != nil)
}

func startHTTPSConnectFixture(spec FixtureSpec) (namedFixture, error) {
	authority := *spec.Expect[0].Host.Equals
	hostname, _, err := net.SplitHostPort(authority)
	if err != nil {
		return nil, fmt.Errorf("parse HTTPS CONNECT authority %q: %w", authority, err)
	}
	certPEM, keyPEM, err := generateFrontendCertificate(hostname)
	if err != nil {
		return nil, fmt.Errorf("generate HTTPS CONNECT fixture certificate: %w", err)
	}
	certificate, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("load HTTPS CONNECT fixture certificate: %w", err)
	}
	caFile, err := os.CreateTemp("", "apisix-go-https-connect-ca-*.pem")
	if err != nil {
		return nil, fmt.Errorf("create HTTPS CONNECT fixture CA file: %w", err)
	}
	caPath := caFile.Name()
	if _, err = caFile.WriteString(certPEM); err != nil {
		_ = caFile.Close()
		_ = os.Remove(caPath)
		return nil, fmt.Errorf("write HTTPS CONNECT fixture CA file: %w", err)
	}
	if err = caFile.Close(); err != nil {
		_ = os.Remove(caPath)
		return nil, fmt.Errorf("close HTTPS CONNECT fixture CA file: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.Remove(caPath)
		return nil, fmt.Errorf("listen HTTPS CONNECT fixture: %w", err)
	}
	fixture := &httpsConnectFixture{
		listener:    listener,
		certificate: certificate,
		caPath:      caPath,
		respond:     spec.Respond,
		requests:    make(chan capturedRequest, len(spec.Expect)+1),
		errors:      make(chan error, len(spec.Expect)+1),
		done:        make(chan struct{}),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func (f *httpsConnectFixture) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.reportError(fmt.Errorf("accept HTTPS CONNECT fixture connection: %w", err))
			return
		}
		f.wg.Go(func() {
			f.handleConnection(connection)
		})
	}
}

func (f *httpsConnectFixture) handleConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))

	connectRequest, err := http.ReadRequest(bufio.NewReader(connection))
	if err != nil {
		f.reportError(fmt.Errorf("read HTTPS CONNECT request: %w", err))
		return
	}
	connectCaptured, err := captureFixtureRequest(connectRequest)
	if err != nil {
		f.reportError(fmt.Errorf("capture HTTPS CONNECT request: %w", err))
		return
	}
	f.requests <- connectCaptured
	if err := writeRawFixtureResponse(connection, f.respond[0], false); err != nil {
		f.reportError(fmt.Errorf("write HTTPS CONNECT response: %w", err))
		return
	}

	tunnel := tls.Server(connection, &tls.Config{Certificates: []tls.Certificate{f.certificate}})
	if err := tunnel.Handshake(); err != nil {
		f.reportError(fmt.Errorf("handshake HTTPS CONNECT tunnel: %w", err))
		return
	}
	providerRequest, err := http.ReadRequest(bufio.NewReader(tunnel))
	if err != nil {
		f.reportError(fmt.Errorf("read tunneled provider request: %w", err))
		return
	}
	providerCaptured, err := captureFixtureRequest(providerRequest)
	if err != nil {
		f.reportError(fmt.Errorf("capture tunneled provider request: %w", err))
		return
	}
	f.requests <- providerCaptured
	if err := writeRawFixtureResponse(tunnel, f.respond[1], true); err != nil {
		f.reportError(fmt.Errorf("write tunneled provider response: %w", err))
	}
}

func captureFixtureRequest(request *http.Request) (capturedRequest, error) {
	defer func() { _ = request.Body.Close() }()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return capturedRequest{}, err
	}
	return capturedRequest{
		method:   request.Method,
		path:     request.URL.RequestURI(),
		host:     request.Host,
		protocol: request.Proto,
		headers:  request.Header.Clone(),
		body:     string(body),
	}, nil
}

func writeRawFixtureResponse(writer io.Writer, configured HTTPResponse, closeConnection bool) error {
	status := configured.Status
	if status == 0 {
		status = http.StatusOK
	}
	body := configured.Body
	header := make(http.Header, len(configured.Headers)+1)
	for name, value := range configured.Headers {
		header.Set(name, value)
	}
	response := &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         closeConnection,
	}
	return response.Write(writer)
}

func (f *httpsConnectFixture) reportError(err error) {
	select {
	case f.errors <- err:
	case <-f.done:
	default:
	}
}

func (f *httpsConnectFixture) address() string { return f.listener.Addr().String() }

func (f *httpsConnectFixture) host() string {
	host, _, err := net.SplitHostPort(f.address())
	if err != nil {
		return ""
	}
	return host
}

func (f *httpsConnectFixture) port() string {
	_, port, err := net.SplitHostPort(f.address())
	if err != nil {
		return ""
	}
	return port
}

func (f *httpsConnectFixture) url() string    { return "http://" + f.address() }
func (f *httpsConnectFixture) caFile() string { return f.caPath }

func (f *httpsConnectFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.listener.Close()
		f.wg.Wait()
		_ = os.Remove(f.caPath)
	})
}

func (f *httpsConnectFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
	for i, expected := range spec.Expect {
		select {
		case received := <-f.requests:
			assertUpstreamRequest(t, expected, received)
		case <-time.After(2 * time.Second):
			t.Errorf("fixture %s did not receive expected request %d", spec.Name, i+1)
		}
	}
	select {
	case err := <-f.errors:
		t.Errorf("fixture %s: %v", spec.Name, err)
	default:
	}
	select {
	case extra := <-f.requests:
		t.Errorf(
			"fixture %s received unexpected extra request %s %s",
			spec.Name,
			extra.method,
			extra.path,
		)
	default:
	}
}

func startNetworkFixture(spec FixtureSpec) (namedFixture, error) {
	fixture := &networkFixture{
		kind:     spec.Kind,
		expect:   spec.NetworkExpect,
		respond:  spec.NetworkRespond,
		received: make(chan []byte, len(spec.NetworkExpect)+1),
		errors:   make(chan error, len(spec.NetworkExpect)+1),
		done:     make(chan struct{}),
	}
	switch spec.Kind {
	case "tcp":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen TCP fixture: %w", err)
		}
		fixture.listener = listener
		fixture.wg.Add(1)
		go fixture.serveTCP()
	case "tls-tcp":
		certPEM, keyPEM, err := generateFrontendCertificate("localhost")
		if err != nil {
			return nil, fmt.Errorf("generate TLS TCP fixture certificate: %w", err)
		}
		certificate, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
		if err != nil {
			return nil, fmt.Errorf("load TLS TCP fixture certificate: %w", err)
		}
		caFile, err := os.CreateTemp("", "apisix-go-tls-tcp-ca-*.pem")
		if err != nil {
			return nil, fmt.Errorf("create TLS TCP fixture CA file: %w", err)
		}
		fixture.caPath = caFile.Name()
		if _, err = caFile.WriteString(certPEM); err != nil {
			_ = caFile.Close()
			_ = os.Remove(fixture.caPath)
			return nil, fmt.Errorf("write TLS TCP fixture CA file: %w", err)
		}
		if err = caFile.Close(); err != nil {
			_ = os.Remove(fixture.caPath)
			return nil, fmt.Errorf("close TLS TCP fixture CA file: %w", err)
		}
		listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}})
		if err != nil {
			_ = os.Remove(fixture.caPath)
			return nil, fmt.Errorf("listen TLS TCP fixture: %w", err)
		}
		fixture.listener = listener
		fixture.wg.Add(1)
		go fixture.serveTCP()
	case "udp":
		packet, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen UDP fixture: %w", err)
		}
		fixture.packet = packet
		fixture.wg.Add(1)
		go fixture.serveUDP()
	case "grpc":
		fixture.server = httptest.NewUnstartedServer(http.HandlerFunc(fixture.serveGRPCRequest))
		fixture.server.EnableHTTP2 = true
		fixture.server.StartTLS()
	case "redis", "redis-cluster", "redis-sentinel":
		return startRedisFixture(spec)
	case "kafka":
		return startKafkaFixture(spec)
	case "rocketmq":
		return startRocketMQFixture(spec)
	case "dubbo":
		return startDubboFixture(spec)
	case "ldap":
		return startLDAPFixture(spec)
	default:
		return nil, fmt.Errorf("network fixture kind %q is not implemented", spec.Kind)
	}
	return fixture, nil
}

func (f *networkFixture) serveTCP() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.reportError(fmt.Errorf("accept TCP fixture connection: %w", err))
			return
		}
		f.wg.Go(func() {
			f.handleTCPConnection(connection)
		})
	}
}

func (f *networkFixture) handleTCPConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	for {
		index := f.nextResponse()
		if index >= len(f.expect) {
			return
		}
		_ = connection.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := readUntilIdle(connection)
		if err != nil {
			f.reportError(fmt.Errorf("read TCP fixture payload %d: %w", index+1, err))
			return
		}
		f.received <- payload
		response, err := networkResponseBytes(f.respond[index])
		if err != nil {
			f.reportError(fmt.Errorf("decode TCP fixture response %d: %w", index+1, err))
			return
		}
		if f.respond[index].Delay > 0 {
			time.Sleep(f.respond[index].Delay)
		}
		if len(response) > 0 {
			if _, err := connection.Write(response); err != nil {
				f.reportError(fmt.Errorf("write TCP fixture response %d: %w", index+1, err))
				return
			}
		}
		if f.respond[index].Close || index == len(f.expect)-1 {
			return
		}
	}
}

func (f *networkFixture) serveUDP() {
	defer f.wg.Done()
	buffer := make([]byte, 64*1024)
	for {
		count, address, err := f.packet.ReadFrom(buffer)
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.reportError(fmt.Errorf("read UDP fixture packet: %w", err))
			return
		}
		index := f.nextResponse()
		if index >= len(f.expect) {
			f.reportError(fmt.Errorf("UDP fixture received more than %d expected payloads", len(f.expect)))
			continue
		}
		payload := append([]byte(nil), buffer[:count]...)
		f.received <- payload
		response, err := networkResponseBytes(f.respond[index])
		if err != nil {
			f.reportError(fmt.Errorf("decode UDP fixture response %d: %w", index+1, err))
			continue
		}
		if f.respond[index].Delay > 0 {
			time.Sleep(f.respond[index].Delay)
		}
		if _, err := f.packet.WriteTo(response, address); err != nil {
			f.reportError(fmt.Errorf("write UDP fixture response %d: %w", index+1, err))
		}
	}
}

func (f *networkFixture) serveGRPCRequest(writer http.ResponseWriter, request *http.Request) {
	index := f.nextResponse()
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		f.reportError(fmt.Errorf("read gRPC fixture payload %d: %w", index+1, err))
		return
	}
	f.received <- payload
	if index >= len(f.respond) {
		f.reportError(fmt.Errorf("gRPC fixture received more than %d expected payloads", len(f.expect)))
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	response, err := networkResponseBytes(f.respond[index])
	if err != nil {
		f.reportError(fmt.Errorf("decode gRPC fixture response %d: %w", index+1, err))
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	if f.respond[index].Delay > 0 {
		time.Sleep(f.respond[index].Delay)
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(response)
}

func (f *networkFixture) nextResponse() int {
	f.sequence.Lock()
	defer f.sequence.Unlock()
	index := f.next
	f.next++
	return index
}

func (f *networkFixture) reportError(err error) {
	select {
	case f.errors <- err:
	case <-f.done:
	default:
	}
}

func readUntilIdle(connection net.Conn) ([]byte, error) {
	var payload []byte
	buffer := make([]byte, 16*1024)
	for {
		count, err := connection.Read(buffer)
		if count > 0 {
			payload = append(payload, buffer[:count]...)
		}
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() && len(payload) > 0 {
				return payload, nil
			}
			if err == io.EOF && len(payload) > 0 {
				return payload, nil
			}
			return nil, err
		}
	}
}

func networkResponseBytes(response NetworkResponse) ([]byte, error) {
	if response.PayloadBase64 != "" {
		return base64.StdEncoding.DecodeString(response.PayloadBase64)
	}
	return []byte(response.Payload), nil
}

func (f *networkFixture) address() string {
	if f.server != nil {
		return strings.TrimPrefix(strings.TrimPrefix(f.server.URL, "http://"), "https://")
	}
	if f.listener != nil {
		return f.listener.Addr().String()
	}
	return f.packet.LocalAddr().String()
}

func (f *networkFixture) host() string {
	host, _, err := net.SplitHostPort(f.address())
	if err != nil {
		return ""
	}
	return host
}

func (f *networkFixture) port() string {
	_, port, err := net.SplitHostPort(f.address())
	if err != nil {
		return ""
	}
	return port
}

func (f *networkFixture) url() string {
	if f.server != nil {
		return f.server.URL
	}
	return f.kind + "://" + f.address()
}

func (f *networkFixture) caFile() string { return f.caPath }

func (f *networkFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		if f.listener != nil {
			_ = f.listener.Close()
		}
		if f.packet != nil {
			_ = f.packet.Close()
		}
		if f.server != nil {
			f.server.Close()
		}
		f.wg.Wait()
		if f.caPath != "" {
			_ = os.Remove(f.caPath)
		}
	})
}

func (f *networkFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
	if isExactZeroUDPFixture(spec) {
		if err := f.zeroPacketAssertionError(spec); err != nil {
			t.Error(err)
		}
		return
	}
	for i, expected := range spec.NetworkExpect {
		select {
		case received := <-f.received:
			if err := matchNetworkAssertion(expected, received); err != nil {
				t.Errorf("fixture %s payload %d: %v", spec.Name, i+1, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("fixture %s did not receive expected payload %d", spec.Name, i+1)
		}
	}
	select {
	case err := <-f.errors:
		t.Errorf("fixture %s: %v", spec.Name, err)
	default:
	}
	select {
	case extra := <-f.received:
		t.Errorf("fixture %s received unexpected extra payload %q", spec.Name, extra)
	default:
	}
}

func (f *networkFixture) zeroPacketAssertionError(spec FixtureSpec) error {
	observation := spec.Count.Timeout
	if observation == 0 {
		observation = defaultZeroPacketObservation
	}
	timer := time.NewTimer(observation)
	defer timer.Stop()

	for {
		select {
		case err := <-f.errors:
			return fmt.Errorf(
				"fixture %s expected zero UDP packets during %s: %w",
				spec.Name,
				observation,
				err,
			)
		case extra := <-f.received:
			return fmt.Errorf(
				"fixture %s expected zero UDP packets during %s, received %q",
				spec.Name,
				observation,
				extra,
			)
		case <-timer.C:
			select {
			case err := <-f.errors:
				return fmt.Errorf(
					"fixture %s expected zero UDP packets during %s: %w",
					spec.Name,
					observation,
					err,
				)
			case extra := <-f.received:
				return fmt.Errorf(
					"fixture %s expected zero UDP packets during %s, received %q",
					spec.Name,
					observation,
					extra,
				)
			default:
				return nil
			}
		}
	}
}

func matchNetworkAssertion(assertion NetworkAssertion, payload []byte) error {
	var err error
	if len(assertion.RFC5424JSONFields) > 0 {
		var message []byte
		message, err = extractRFC5424Message(payload)
		if err == nil {
			err = matchNetworkJSONFields(assertion.RFC5424JSONFields, message)
		}
	} else if len(assertion.JSONFields) > 0 {
		err = matchNetworkJSONFields(assertion.JSONFields, payload)
	} else if assertion.PayloadBase64 != nil {
		err = assertion.PayloadBase64.match(base64.StdEncoding.EncodeToString(payload), true)
	} else {
		err = assertion.Payload.match(string(payload), true)
	}
	if err != nil {
		return err
	}
	for _, pattern := range assertion.ForbiddenMatches {
		matched, matchErr := regexp.MatchString(pattern, string(payload))
		if matchErr != nil {
			return fmt.Errorf("compile forbidden network pattern %q: %w", pattern, matchErr)
		}
		if matched {
			return fmt.Errorf("network payload matches forbidden pattern %q", pattern)
		}
	}
	return nil
}

func extractRFC5424Message(payload []byte) ([]byte, error) {
	const timestampLayout = "2006-01-02T15:04:05.000Z"
	pattern := regexp.MustCompile(
		`(?s)^<46>1 ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z) ` +
			`([^ ]+) apisix ([1-9][0-9]*) - - (.*)\n$`,
	)
	matches := pattern.FindSubmatch(payload)
	if matches == nil {
		return nil, errors.New("payload does not match the APISIX RFC5424 envelope")
	}
	if _, err := time.Parse(timestampLayout, string(matches[1])); err != nil {
		return nil, fmt.Errorf("RFC5424 envelope timestamp: %w", err)
	}
	return matches[4], nil
}

func matchNetworkJSONFields(fields []NetworkJSONFieldAssertion, payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode JSON payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON payload")
		}
		return fmt.Errorf("trailing JSON payload: %w", err)
	}
	for _, field := range fields {
		value, err := resolveJSONPointer(document, field.Path)
		if err != nil {
			return err
		}
		encoded, err := networkJSONValue(value)
		if err != nil {
			return fmt.Errorf("JSON field %s: %w", field.Path, err)
		}
		if field.RFC3339 {
			if _, err := time.Parse(time.RFC3339, encoded); err != nil {
				return fmt.Errorf("JSON field %s is not RFC3339: %w", field.Path, err)
			}
			continue
		}
		if err := field.Value.match(encoded, true); err != nil {
			return fmt.Errorf("JSON field %s: %w", field.Path, err)
		}
	}
	return nil
}

func resolveJSONPointer(document any, pointer string) (any, error) {
	current := document
	parts, err := parseJSONPointer(pointer)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		switch value := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = value[part]
			if !ok {
				return nil, fmt.Errorf("JSON field %s is missing", pointer)
			}
		case []any:
			index, err := canonicalJSONPointerIndex(part)
			if err != nil || index >= len(value) {
				return nil, fmt.Errorf("JSON field %s has invalid array index %q", pointer, part)
			}
			current = value[index]
		default:
			return nil, fmt.Errorf("JSON field %s traverses a non-container value", pointer)
		}
	}
	return current, nil
}

func canonicalJSONPointerIndex(value string) (int, error) {
	if value == "0" {
		return 0, nil
	}
	if value == "" || value[0] < '1' || value[0] > '9' {
		return 0, errors.New("array index is not canonical")
	}
	for i := 1; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, errors.New("array index is not canonical")
		}
	}
	return strconv.Atoi(value)
}

func networkJSONValue(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case json.Number:
		return typed.String(), nil
	case bool:
		return strconv.FormatBool(typed), nil
	case nil:
		return "null", nil
	default:
		encoded, err := json.Marshal(value)
		return string(encoded), err
	}
}

func assertAfterShutdown(t *testing.T, assertions []FileAssertion, replacements map[string]string) {
	assertFiles(t, assertions, replacements, "after_shutdown assertion")
}

func assertFiles(
	t *testing.T,
	assertions []FileAssertion,
	replacements map[string]string,
	kind string,
) {
	t.Helper()
	for i, assertion := range assertions {
		path := *assertion.Path.Equals
		for placeholder, value := range replacements {
			path = strings.ReplaceAll(path, placeholder, value)
		}
		workDir := replacements["{{WORK_DIR}}"]
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			t.Errorf("%s %d path: %v", kind, i+1, err)
			continue
		}
		relativePath, err := filepath.Rel(workDir, absolutePath)
		if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
			t.Errorf("%s %d path escapes work directory: %s", kind, i+1, path)
			continue
		}
		body, err := os.ReadFile(absolutePath)
		if assertion.Absent {
			if err == nil {
				t.Errorf("%s %d path exists, want absent: %s", kind, i+1, absolutePath)
			} else if !os.IsNotExist(err) {
				t.Errorf("%s %d stat %s: %v", kind, i+1, absolutePath, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s %d read %s: %v", kind, i+1, absolutePath, err)
			continue
		}
		if assertion.JSONLines != nil {
			if err := matchFileJSONLines(body, *assertion.JSONLines, replacements); err != nil {
				t.Errorf("%s %d json_lines: %v", kind, i+1, err)
			}
			continue
		}
		if assertion.BboltJSON != nil {
			if err := matchFileBboltJSON(absolutePath, *assertion.BboltJSON, replacements); err != nil {
				t.Errorf("%s %d bbolt_json: %v", kind, i+1, err)
			}
			continue
		}
		if err := assertion.Body.match(string(body), true); err != nil {
			t.Errorf("%s %d body: %v", kind, i+1, err)
		}
	}
}

func matchFileBboltJSON(
	path string,
	assertion FileBboltJSONAssertion,
	replacements map[string]string,
) error {
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return fmt.Errorf("open bbolt: %w", err)
	}
	defer func() { _ = db.Close() }()

	var raw []byte
	err = db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(assertion.Bucket))
		if bucket == nil {
			return fmt.Errorf("bucket %q is missing", assertion.Bucket)
		}
		value := bucket.Get([]byte(assertion.Key))
		if value == nil {
			return fmt.Errorf("key %q is missing from bucket %q", assertion.Key, assertion.Bucket)
		}
		raw = bytes.Clone(value)
		return nil
	})
	if err != nil {
		return err
	}
	for _, pattern := range assertion.ForbiddenMatches {
		if regexp.MustCompile(pattern).Match(raw) {
			return fmt.Errorf("stored JSON matches forbidden pattern %q", pattern)
		}
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode stored JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode stored JSON: trailing JSON value")
		}
		return fmt.Errorf("decode stored JSON trailing data: %w", err)
	}
	for pointer, expected := range assertion.Fields {
		actual, err := resolveJSONPointer(document, pointer)
		if err != nil {
			return err
		}
		for placeholder, replacement := range replacements {
			expected = strings.ReplaceAll(expected, placeholder, replacement)
		}
		actualString, err := networkJSONValue(actual)
		if err != nil {
			return fmt.Errorf("field %s: %w", pointer, err)
		}
		if actualString != expected {
			return fmt.Errorf("field %s = %q, want %q", pointer, actualString, expected)
		}
	}
	return nil
}

func matchFileJSONLines(
	body []byte,
	assertion FileJSONLinesAssertion,
	replacements map[string]string,
) error {
	rawLines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(rawLines) != assertion.Count {
		return fmt.Errorf("record count = %d, want %d", len(rawLines), assertion.Count)
	}

	matchedCounts := make([]int, len(assertion.Records))
	for i, rawLine := range rawLines {
		var value any
		decoder := json.NewDecoder(strings.NewReader(rawLine))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("record %d decode: %w", i+1, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return fmt.Errorf("record %d decode: trailing JSON value", i+1)
			}
			return fmt.Errorf("record %d decode trailing value: %w", i+1, err)
		}

		matchedRecord := -1
		for j, record := range assertion.Records {
			matches := true
			for path, expected := range record.Fields {
				actual, err := resolveJSONPointer(value, path)
				if err != nil {
					matches = false
					break
				}
				for placeholder, replacement := range replacements {
					expected = strings.ReplaceAll(expected, placeholder, replacement)
				}
				actualString, err := networkJSONValue(actual)
				if err != nil || actualString != expected {
					matches = false
					break
				}
			}
			if !matches {
				continue
			}
			if matchedRecord >= 0 {
				return fmt.Errorf("record %d matches multiple expected record groups", i+1)
			}
			matchedRecord = j
		}
		if matchedRecord < 0 {
			return fmt.Errorf("record %d does not match any expected record group: %s", i+1, rawLine)
		}
		matchedCounts[matchedRecord]++
	}
	for i, record := range assertion.Records {
		if matchedCounts[i] != record.Count {
			return fmt.Errorf("record group %d count = %d, want %d", i+1, matchedCounts[i], record.Count)
		}
	}
	return nil
}

func TestHTTPSConnectFixtureCapturesOuterAndTunneledRequest(t *testing.T) {
	connectAuthority := "api.openai.com:443"
	providerPath := "/v1/chat/completions"
	authorization := "some-key"
	requestBody := `{"messages":[{"role":"user","content":"hello"}],"model":"gpt-4"}`
	spec := FixtureSpec{
		Name: "provider-proxy",
		Kind: "https-connect",
		Expect: []HTTPAssertion{
			{
				Method: http.MethodConnect,
				Host:   &Matcher{Equals: &connectAuthority},
			},
			{
				Method: http.MethodPost,
				Path:   &Matcher{Equals: &providerPath},
				Headers: map[string]Matcher{
					"Authorization": {Equals: &authorization},
				},
				Body: &Matcher{JSONEquals: &requestBody},
			},
		},
		Respond: []HTTPResponse{
			{Status: http.StatusOK},
			{
				Status:  http.StatusUnauthorized,
				Headers: map[string]string{"Content-Type": "text/plain"},
				Body:    "Unauthorized",
			},
		},
	}
	fixture, err := startNamedFixture(spec)
	if err != nil {
		t.Fatalf("start HTTPS CONNECT fixture: %v", err)
	}
	defer fixture.close()

	trusted, ok := fixture.(interface{ caFile() string })
	if !ok {
		t.Fatal("HTTPS CONNECT fixture does not expose its CA file")
	}
	caPEM, err := os.ReadFile(trusted.caFile())
	if err != nil {
		t.Fatalf("read HTTPS CONNECT fixture CA file: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("load HTTPS CONNECT fixture CA")
	}
	proxyURL, err := url.Parse(fixture.url())
	if err != nil {
		t.Fatalf("parse HTTPS CONNECT fixture URL: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: roots},
	}}
	request, err := http.NewRequest(
		http.MethodPost,
		"https://api.openai.com/v1/chat/completions",
		strings.NewReader(requestBody),
	)
	if err != nil {
		t.Fatalf("create tunneled provider request: %v", err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send tunneled provider request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read tunneled provider response: %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("provider response status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if string(body) != "Unauthorized" {
		t.Fatalf("provider response body = %q, want %q", body, "Unauthorized")
	}

	fixture.assert(t, spec)
}

func TestHarnessRunsTCPFixture(t *testing.T) {
	payloadPattern := `(?s)^GET /tcp HTTP/1\.[01]\r\n.*\r\n\r\n$`
	caseSpec := Case{
		Name:   "tcp-fixture",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  "tcp-fixture",
				"uri": "/tcp",
				"upstream": map[string]any{
					"type":  "roundrobin",
					"nodes": map[string]any{"{{FIXTURE.sink.ADDR}}": 1},
				},
			}},
		},
		Fixtures: []FixtureSpec{{
			Name: "sink",
			Kind: "tcp",
			NetworkExpect: []NetworkAssertion{{
				Payload: &Matcher{Matches: &payloadPattern},
			}},
			NetworkRespond: []NetworkResponse{{
				Payload: "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
			}},
		}},
		Steps: []CaseStep{{
			Name:   "request",
			Input:  HTTPInput{Path: "/tcp"},
			Output: HTTPOutput{Status: http.StatusOK, Body: &Matcher{Equals: new("ok")}},
		}},
	}

	runReadyCase(t, caseSpec)
}

func TestHarnessRunsUDPFixture(t *testing.T) {
	payload := []byte("udp-payload")
	response := []byte("udp-response")
	spec := FixtureSpec{
		Name: "sink",
		Kind: "udp",
		NetworkExpect: []NetworkAssertion{{
			PayloadBase64: &Matcher{Equals: new(base64.StdEncoding.EncodeToString(payload))},
		}},
		NetworkRespond: []NetworkResponse{{
			PayloadBase64: base64.StdEncoding.EncodeToString(response),
		}},
	}
	fixture, err := startNetworkFixture(spec)
	if err != nil {
		t.Fatalf("start UDP fixture: %v", err)
	}
	defer fixture.close()
	connection, err := net.Dial("udp", fixture.address())
	if err != nil {
		t.Fatalf("dial UDP fixture: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write UDP payload: %v", err)
	}
	got := make([]byte, len(response))
	if _, err := io.ReadFull(connection, got); err != nil {
		t.Fatalf("read UDP response: %v", err)
	}
	if string(got) != string(response) {
		t.Fatalf("UDP response = %q, want %q", got, response)
	}
	fixture.assert(t, spec)
}

func TestUDPZeroPacketAssertionRejectsDelayedPacket(t *testing.T) {
	spec := FixtureSpec{
		Name:  "sink",
		Kind:  "udp",
		Count: &FixtureCountAssertion{AtLeast: 0, AtMost: 0, Timeout: 100 * time.Millisecond},
	}
	fixture, err := startNetworkFixture(spec)
	if err != nil {
		t.Fatalf("start UDP fixture: %v", err)
	}
	defer fixture.close()

	go func() {
		time.Sleep(20 * time.Millisecond)
		connection, dialErr := net.Dial("udp", fixture.address())
		if dialErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = connection.Write([]byte("late-datagram"))
	}()

	assertionErr := fixture.(*networkFixture).zeroPacketAssertionError(spec)
	if assertionErr == nil || !strings.Contains(assertionErr.Error(), "received more than 0 expected payloads") {
		t.Fatalf("assertion error = %v, want delayed UDP packet rejection", assertionErr)
	}
}

func TestUDPZeroPacketAssertionObservesConfiguredWindow(t *testing.T) {
	const observationWindow = 40 * time.Millisecond
	spec := FixtureSpec{
		Name:  "sink",
		Kind:  "udp",
		Count: &FixtureCountAssertion{AtLeast: 0, AtMost: 0, Timeout: observationWindow},
	}
	fixture, err := startNetworkFixture(spec)
	if err != nil {
		t.Fatalf("start UDP fixture: %v", err)
	}
	defer fixture.close()

	started := time.Now()
	assertionErr := fixture.(*networkFixture).zeroPacketAssertionError(spec)
	elapsed := time.Since(started)
	if assertionErr != nil {
		t.Fatalf("assertion error = %v, want exact zero packets", assertionErr)
	}
	if elapsed < observationWindow {
		t.Fatalf("zero-packet assertion returned after %s, want at least %s", elapsed, observationWindow)
	}
}

func TestUDPZeroPacketBurstFailsWithoutBlockingShutdown(t *testing.T) {
	spec := FixtureSpec{
		Name:  "sink",
		Kind:  "udp",
		Count: &FixtureCountAssertion{AtLeast: 0, AtMost: 0, Timeout: 100 * time.Millisecond},
	}
	named, err := startNetworkFixture(spec)
	if err != nil {
		t.Fatalf("start UDP fixture: %v", err)
	}
	fixture := named.(*networkFixture)

	connection, err := net.Dial("udp", fixture.address())
	if err != nil {
		fixture.close()
		t.Fatalf("dial UDP fixture: %v", err)
	}
	for range 8 {
		if _, err = connection.Write([]byte("unexpected")); err != nil {
			_ = connection.Close()
			fixture.close()
			t.Fatalf("write UDP burst: %v", err)
		}
	}
	_ = connection.Close()

	assertionErr := fixture.zeroPacketAssertionError(spec)
	if assertionErr == nil || !strings.Contains(assertionErr.Error(), "received more than 0 expected payloads") {
		fixture.close()
		t.Fatalf("assertion error = %v, want burst rejection", assertionErr)
	}
	time.Sleep(20 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		fixture.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("UDP fixture shutdown blocked after an unexpected packet burst")
	}
}

func TestMatchNetworkAssertionJSONFields(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("matchNetworkAssertion() panicked for JSON fields: %v", recovered)
		}
	}()
	assertion := NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{
		{Path: "/request/body", Value: Matcher{Equals: new(`{"sample_payload":"hello"}`)}},
		{Path: "/response/body", Value: Matcher{Equals: new("hello world\n")}},
		{Path: "/response/status", Value: Matcher{Equals: new("200")}},
	}}
	payload := []byte(`{
        "request":{"body":"{\"sample_payload\":\"hello\"}"},
        "response":{"body":"hello world\n","status":200}
    }`)

	if err := matchNetworkAssertion(assertion, payload); err != nil {
		t.Fatalf("matchNetworkAssertion() error = %v", err)
	}
}

func TestMatchNetworkAssertionRFC5424JSONFields(t *testing.T) {
	assertion := NetworkAssertion{RFC5424JSONFields: []NetworkJSONFieldAssertion{{
		Path:  "/request/uri",
		Value: Matcher{Equals: new("/hello")},
	}}}
	payload := []byte("<46>1 2026-07-28T06:07:08.987Z example.com apisix 4242 - - " +
		"{\"request\":{\"uri\":\"/hello\"}}\n")
	if err := matchNetworkAssertion(assertion, payload); err != nil {
		t.Fatalf("matchNetworkAssertion() error = %v", err)
	}
}

func TestMatchNetworkAssertionRFC5424JSONFieldsRejectsInvalidEnvelope(t *testing.T) {
	assertion := NetworkAssertion{RFC5424JSONFields: []NetworkJSONFieldAssertion{{
		Path:  "/request/uri",
		Value: Matcher{Equals: new("/hello")},
	}}}
	payload := []byte("<30>1 2026-07-28T06:07:08Z example.com apisix 4242 - - " +
		"{\"request\":{\"uri\":\"/hello\"}}\n")
	err := matchNetworkAssertion(assertion, payload)
	if err == nil || !strings.Contains(err.Error(), "RFC5424 envelope") {
		t.Fatalf("matchNetworkAssertion() error = %v, want envelope rejection", err)
	}
}

func TestMatchNetworkAssertionRejectsForbiddenHeaderPatterns(t *testing.T) {
	positive := `(?s)^GET /auth HTTP/1\.1\r\n.*\r\n\r\n$`
	for _, header := range []string{"Content-Length", "Transfer-Encoding", "Content-Encoding"} {
		t.Run(header, func(t *testing.T) {
			assertion := NetworkAssertion{
				Payload:          &Matcher{Matches: &positive},
				ForbiddenMatches: []string{`(?im)^` + regexp.QuoteMeta(header) + `:`},
			}
			payload := []byte("GET /auth HTTP/1.1\r\nHost: example.com\r\n" + header + ": forbidden\r\n\r\n")
			err := matchNetworkAssertion(assertion, payload)
			if err == nil || !strings.Contains(err.Error(), "matches forbidden pattern") {
				t.Fatalf("matchNetworkAssertion() error = %v, want forbidden %s rejection", err, header)
			}
		})
	}
}

func TestMatchNetworkAssertionJSONFieldsRejectsTrailingData(t *testing.T) {
	assertion := NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{
		{Path: "/status", Value: Matcher{Equals: new("200")}},
	}}

	err := matchNetworkAssertion(assertion, []byte(`{"status":200} trailing`))
	if err == nil || !strings.Contains(err.Error(), "trailing JSON payload") {
		t.Fatalf("matchNetworkAssertion() error = %v, want trailing JSON payload rejection", err)
	}
}

func TestMatchNetworkAssertionJSONFieldsRFC3339(t *testing.T) {
	assertion := NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{{
		Path:    "/@timestamp",
		RFC3339: true,
	}}}
	if err := matchNetworkAssertion(assertion, []byte(`{"@timestamp":"2026-07-18T12:30:00+08:00"}`)); err != nil {
		t.Fatalf("matchNetworkAssertion() error = %v, want parsed RFC3339 timestamp", err)
	}
	if err := matchNetworkAssertion(assertion, []byte(`{"@timestamp":"2026-07-18 12:30:00"}`)); err == nil {
		t.Fatal("matchNetworkAssertion() error = nil, want invalid RFC3339 rejection")
	}
}

func TestMatchNetworkAssertionJSONFieldsSupportsRootPointer(t *testing.T) {
	want := `{"status":200}`
	assertion := NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{{
		Path:  "",
		Value: Matcher{Equals: &want},
	}}}
	if err := matchNetworkAssertion(assertion, []byte(want)); err != nil {
		t.Fatalf("matchNetworkAssertion() error = %v, want root pointer match", err)
	}
}

func TestMatchNetworkAssertionJSONFieldsSupportsEscapedKeys(t *testing.T) {
	want := "ok"
	assertion := NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{{
		Path:  "/a~1b/m~0n",
		Value: Matcher{Equals: &want},
	}}}
	if err := matchNetworkAssertion(assertion, []byte(`{"a/b":{"m~n":"ok"}}`)); err != nil {
		t.Fatalf("matchNetworkAssertion() error = %v, want escaped-key match", err)
	}
}

func TestMatchNetworkAssertionJSONFieldsRejectsNonCanonicalArrayIndex(t *testing.T) {
	want := "one"
	for _, pointer := range []string{"/+1/id", "/01/id", "/-1/id"} {
		t.Run(pointer, func(t *testing.T) {
			assertion := NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{{
				Path:  pointer,
				Value: Matcher{Equals: &want},
			}}}
			if err := matchNetworkAssertion(assertion, []byte(`[{"id":"zero"},{"id":"one"}]`)); err == nil {
				t.Fatalf("matchNetworkAssertion() error = nil, want non-canonical index %q rejection", pointer)
			}
		})
	}
}

func TestMatchNetworkAssertionJSONFieldsRejectionPaths(t *testing.T) {
	want := "expected"
	assertion := NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{{
		Path:  "/field",
		Value: Matcher{Equals: &want},
	}}}
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "malformed JSON", payload: `{"field":`, want: "decode JSON payload"},
		{name: "missing field", payload: `{}`, want: "is missing"},
		{name: "wrong exact value", payload: `{"field":"actual"}`, want: "want \"expected\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := matchNetworkAssertion(assertion, []byte(test.payload))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("matchNetworkAssertion() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHarnessRunsGRPCFixture(t *testing.T) {
	payload := []byte{0, 0, 0, 0, 0}
	spec := FixtureSpec{
		Name: "collector",
		Kind: "grpc",
		NetworkExpect: []NetworkAssertion{{
			PayloadBase64: &Matcher{Equals: new(base64.StdEncoding.EncodeToString(payload))},
		}},
		NetworkRespond: []NetworkResponse{{Payload: "accepted"}},
	}
	fixture, err := startNetworkFixture(spec)
	if err != nil {
		t.Fatalf("start gRPC fixture: %v", err)
	}
	defer fixture.close()
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // fixture certificate is ephemeral
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Post(fixture.url()+"/collector", "application/grpc", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST gRPC fixture: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read gRPC response: %v", err)
	}
	if string(body) != "accepted" {
		t.Fatalf("gRPC response = %q, want accepted", body)
	}
	fixture.assert(t, spec)
}

func TestFixtureServerCountRangeWaitsForEventualRequests(t *testing.T) {
	minCount, maxCount := 2, 3
	spec := FixtureSpec{
		Name:    "mirror",
		Kind:    "http",
		Expect:  []HTTPAssertion{{Method: http.MethodGet}},
		Respond: []HTTPResponse{{Status: http.StatusNoContent}},
		Count:   &FixtureCountAssertion{AtLeast: minCount, AtMost: maxCount, Timeout: time.Second},
	}
	fixture, err := startNamedFixture(spec)
	if err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	defer fixture.close()
	for range minCount {
		response, requestErr := http.Get(fixture.url())
		if requestErr != nil {
			t.Fatalf("GET fixture: %v", requestErr)
		}
		_ = response.Body.Close()
	}
	fixture.assert(t, spec)
}

func TestObserveFixtureRequestCountRejectsLateUpperBoundOverflow(t *testing.T) {
	requests := make(chan capturedRequest, 2)
	requests <- capturedRequest{method: http.MethodGet, path: "/first"}
	go func() {
		time.Sleep(150 * time.Millisecond)
		requests <- capturedRequest{method: http.MethodGet, path: "/late"}
	}()

	_, err := observeFixtureRequestCount(requests, FixtureCountAssertion{
		AtLeast: 1,
		AtMost:  1,
		Timeout: 300 * time.Millisecond,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds 1") {
		t.Fatalf("observeFixtureRequestCount() error = %v, want late upper-bound rejection", err)
	}
}

func TestBuildAndMatchUnaryGRPCFrame(t *testing.T) {
	message := []byte("\x0a\x06apisix")
	frame := buildUnaryGRPCFrame(message)
	if err := matchUnaryGRPCFrame(frame, base64.StdEncoding.EncodeToString(message)); err != nil {
		t.Fatalf("match unary gRPC frame: %v", err)
	}
	if err := matchUnaryGRPCFrame(append(frame, 0), base64.StdEncoding.EncodeToString(message)); err == nil {
		t.Fatal("match unary gRPC frame accepted trailing bytes")
	}
}

func TestHarnessAssertsFileAfterShutdown(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "output.log")
	if err := os.WriteFile(path, []byte("flushed"), 0o600); err != nil {
		t.Fatalf("write fixture output: %v", err)
	}
	body := "flushed"
	assertAfterShutdown(t, []FileAssertion{{
		Path: &Matcher{Equals: new("{{WORK_DIR}}/output.log")},
		Body: &Matcher{Equals: &body},
	}}, map[string]string{"{{WORK_DIR}}": workDir})
}

func TestHarnessAssertsTypedBboltJSONAfterShutdown(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "apisix-go-store.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("open bbolt fixture: %v", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucket([]byte("routes"))
		if err != nil {
			return err
		}
		return bucket.Put(
			[]byte("route-1"),
			[]byte(`{"plugins":{"ai-rate-limiting":{"redis_password":"ciphertext"}}}`),
		)
	})
	if err != nil {
		t.Fatalf("write bbolt fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close bbolt fixture: %v", err)
	}

	assertAfterShutdown(t, []FileAssertion{{
		Path: &Matcher{Equals: new("{{WORK_DIR}}/apisix-go-store.db")},
		BboltJSON: &FileBboltJSONAssertion{
			Bucket: "routes",
			Key:    "route-1",
			Fields: map[string]string{
				"/plugins/ai-rate-limiting/redis_password": "ciphertext",
			},
			ForbiddenMatches: []string{"plaintext"},
		},
	}}, map[string]string{"{{WORK_DIR}}": workDir})
}

func TestMatchFileBboltJSONRejectsTrailingJSON(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "apisix-go-store.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("open bbolt fixture: %v", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucket([]byte("routes"))
		if err != nil {
			return err
		}
		return bucket.Put([]byte("route-1"), []byte(`{"id":"route-1"}{"trailing":true}`))
	})
	if err != nil {
		t.Fatalf("write bbolt fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close bbolt fixture: %v", err)
	}

	err = matchFileBboltJSON(path, FileBboltJSONAssertion{
		Bucket: "routes",
		Key:    "route-1",
		Fields: map[string]string{"/id": "route-1"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("matchFileBboltJSON() error = %v, want trailing JSON rejection", err)
	}
}

func TestHarnessAssertsAbsentFileMidCase(t *testing.T) {
	workDir := t.TempDir()
	assertFiles(t, []FileAssertion{{
		Path:   &Matcher{Equals: new("{{WORK_DIR}}/output.log")},
		Absent: true,
	}}, map[string]string{"{{WORK_DIR}}": workDir}, "step file assertion")
}

func TestMatchFileJSONLinesCorrelatesUnorderedRecords(t *testing.T) {
	assertion := FileJSONLinesAssertion{
		Count: 4,
		Records: []FileJSONRecordAssertion{
			{
				Count: 2,
				Fields: map[string]string{
					"/upstream":      "127.0.0.1:{{DOMAIN_PORT}}",
					"/upstream_host": "localhost",
				},
			},
			{
				Count: 2,
				Fields: map[string]string{
					"/upstream":      "127.0.0.1:1982",
					"/upstream_host": "127.0.0.1",
				},
			},
		},
	}
	body := []byte(
		"{\"upstream\":\"127.0.0.1:1982\",\"upstream_host\":\"127.0.0.1\"}\n" +
			"{\"upstream\":\"127.0.0.1:1980\",\"upstream_host\":\"localhost\"}\n" +
			"{\"upstream\":\"127.0.0.1:1980\",\"upstream_host\":\"localhost\"}\n" +
			"{\"upstream\":\"127.0.0.1:1982\",\"upstream_host\":\"127.0.0.1\"}\n",
	)
	if err := matchFileJSONLines(body, assertion, map[string]string{"{{DOMAIN_PORT}}": "1980"}); err != nil {
		t.Fatalf("matchFileJSONLines() error = %v", err)
	}
}

func TestMatchFileJSONLinesRejectsMalformedRecord(t *testing.T) {
	assertion := FileJSONLinesAssertion{
		Count: 1,
		Records: []FileJSONRecordAssertion{{
			Count:  1,
			Fields: map[string]string{"/route_id": "route-1"},
		}},
	}
	if err := matchFileJSONLines([]byte("{not-json}\n"), assertion, nil); err == nil {
		t.Fatal("matchFileJSONLines() error = nil, want malformed JSON rejection")
	}
}

func TestMatchFileJSONLinesRejectsUncorrelatedFields(t *testing.T) {
	assertion := FileJSONLinesAssertion{
		Count: 2,
		Records: []FileJSONRecordAssertion{
			{
				Count:  1,
				Fields: map[string]string{"/upstream": "127.0.0.1:1980", "/upstream_host": "localhost"},
			},
			{
				Count:  1,
				Fields: map[string]string{"/upstream": "127.0.0.1:1982", "/upstream_host": "127.0.0.1"},
			},
		},
	}
	body := []byte(
		"{\"upstream\":\"127.0.0.1:1980\",\"upstream_host\":\"127.0.0.1\"}\n" +
			"{\"upstream\":\"127.0.0.1:1982\",\"upstream_host\":\"localhost\"}\n",
	)
	if err := matchFileJSONLines(body, assertion, nil); err == nil {
		t.Fatal("matchFileJSONLines() error = nil, want correlation rejection")
	}
}
