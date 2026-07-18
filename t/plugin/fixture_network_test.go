package pluginintegration

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
		listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}})
		if err != nil {
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
			f.errors <- fmt.Errorf("accept TCP fixture connection: %w", err)
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
			f.errors <- fmt.Errorf("read TCP fixture payload %d: %w", index+1, err)
			return
		}
		f.received <- payload
		response, err := networkResponseBytes(f.respond[index])
		if err != nil {
			f.errors <- fmt.Errorf("decode TCP fixture response %d: %w", index+1, err)
			return
		}
		if f.respond[index].Delay > 0 {
			time.Sleep(f.respond[index].Delay)
		}
		if len(response) > 0 {
			if _, err := connection.Write(response); err != nil {
				f.errors <- fmt.Errorf("write TCP fixture response %d: %w", index+1, err)
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
			f.errors <- fmt.Errorf("read UDP fixture packet: %w", err)
			return
		}
		index := f.nextResponse()
		if index >= len(f.expect) {
			f.errors <- fmt.Errorf("UDP fixture received more than %d expected payloads", len(f.expect))
			continue
		}
		payload := append([]byte(nil), buffer[:count]...)
		f.received <- payload
		response, err := networkResponseBytes(f.respond[index])
		if err != nil {
			f.errors <- fmt.Errorf("decode UDP fixture response %d: %w", index+1, err)
			continue
		}
		if f.respond[index].Delay > 0 {
			time.Sleep(f.respond[index].Delay)
		}
		if _, err := f.packet.WriteTo(response, address); err != nil {
			f.errors <- fmt.Errorf("write UDP fixture response %d: %w", index+1, err)
		}
	}
}

func (f *networkFixture) serveGRPCRequest(writer http.ResponseWriter, request *http.Request) {
	index := f.nextResponse()
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		f.errors <- fmt.Errorf("read gRPC fixture payload %d: %w", index+1, err)
		return
	}
	f.received <- payload
	if index >= len(f.respond) {
		f.errors <- fmt.Errorf("gRPC fixture received more than %d expected payloads", len(f.expect))
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	response, err := networkResponseBytes(f.respond[index])
	if err != nil {
		f.errors <- fmt.Errorf("decode gRPC fixture response %d: %w", index+1, err)
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
	})
}

func (f *networkFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
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

func matchNetworkAssertion(assertion NetworkAssertion, payload []byte) error {
	if len(assertion.JSONFields) > 0 {
		return matchNetworkJSONFields(assertion.JSONFields, payload)
	}
	if assertion.PayloadBase64 != nil {
		return assertion.PayloadBase64.match(base64.StdEncoding.EncodeToString(payload), true)
	}
	return assertion.Payload.match(string(payload), true)
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
		if err := field.Value.match(encoded, true); err != nil {
			return fmt.Errorf("JSON field %s: %w", field.Path, err)
		}
	}
	return nil
}

func resolveJSONPointer(document any, pointer string) (any, error) {
	current := document
	for raw := range strings.SplitSeq(strings.TrimPrefix(pointer, "/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch value := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = value[part]
			if !ok {
				return nil, fmt.Errorf("JSON field %s is missing", pointer)
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(value) {
				return nil, fmt.Errorf("JSON field %s has invalid array index %q", pointer, part)
			}
			current = value[index]
		default:
			return nil, fmt.Errorf("JSON field %s traverses a non-container value", pointer)
		}
	}
	return current, nil
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
	t.Helper()
	for i, assertion := range assertions {
		path := *assertion.Path.Equals
		for placeholder, value := range replacements {
			path = strings.ReplaceAll(path, placeholder, value)
		}
		workDir := replacements["{{WORK_DIR}}"]
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			t.Errorf("after_shutdown assertion %d path: %v", i+1, err)
			continue
		}
		relativePath, err := filepath.Rel(workDir, absolutePath)
		if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
			t.Errorf("after_shutdown assertion %d path escapes work directory: %s", i+1, path)
			continue
		}
		body, err := os.ReadFile(absolutePath)
		if err != nil {
			t.Errorf("after_shutdown assertion %d read %s: %v", i+1, absolutePath, err)
			continue
		}
		if err := assertion.Body.match(string(body), true); err != nil {
			t.Errorf("after_shutdown assertion %d body: %v", i+1, err)
		}
	}
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

	runCase(t, caseSpec)
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

func TestMatchNetworkAssertionJSONFieldsRejectsTrailingData(t *testing.T) {
	assertion := NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{
		{Path: "/status", Value: Matcher{Equals: new("200")}},
	}}

	err := matchNetworkAssertion(assertion, []byte(`{"status":200} trailing`))
	if err == nil || !strings.Contains(err.Error(), "trailing JSON payload") {
		t.Fatalf("matchNetworkAssertion() error = %v, want trailing JSON payload rejection", err)
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
