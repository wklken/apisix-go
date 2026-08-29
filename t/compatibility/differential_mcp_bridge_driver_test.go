package pluginintegration

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMCPBridgeCandidateDriverFollowsDynamicSessionEndpoint(t *testing.T) {
	const sessionID = "11111111-2222-4333-8444-555555555555"
	posted := make(chan struct{})
	handlerErrors := make(chan error, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/mcp/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, ok := w.(http.Flusher)
			if !ok {
				handlerErrors <- fmt.Errorf("response does not support flushing")
				return
			}
			_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /mcp/message?sessionId=%s\n\n", sessionID)
			_, _ = io.WriteString(
				w,
				"event: message\ndata: {\"jsonrpc\": \"2.0\",\"method\": \"ping\",\"id\":\"ping:1\"}\n\n",
			)
			flusher.Flush()
			select {
			case <-posted:
			case <-time.After(2 * time.Second):
				handlerErrors <- fmt.Errorf("dynamic session POST was not received")
				return
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", differentialMCPBridgePostedPayload)
			flusher.Flush()
		case r.Method == http.MethodPost && r.URL.Path == "/mcp/message":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				handlerErrors <- fmt.Errorf("read POST: %w", err)
				return
			}
			if r.URL.Query().Get("sessionId") != sessionID {
				handlerErrors <- fmt.Errorf("sessionId = %q", r.URL.Query().Get("sessionId"))
			}
			if string(body) != differentialMCPBridgePostedPayload {
				handlerErrors <- fmt.Errorf("POST body = %q", body)
			}
			if r.Host != "gateway.example.test" {
				handlerErrors <- fmt.Errorf("POST Host = %q", r.Host)
			}
			close(posted)
			w.WriteHeader(http.StatusAccepted)
		default:
			handlerErrors <- fmt.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			http.NotFound(w, r)
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

	spec := differentialCasesForPlugin("mcp-bridge")[0]
	observation, err := observeDifferentialMCPBridgeCandidate(spec, port)
	if err != nil {
		t.Fatalf("observe candidate: %v", err)
	}
	select {
	case err := <-handlerErrors:
		t.Fatal(err)
	default:
	}
	transcript, err := decodeDifferentialMCPBridgeTranscript(observation.Body)
	if err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	if transcript.Endpoint.Data != "/mcp/message?sessionId="+sessionID ||
		transcript.Ping.Event != "message" || transcript.Message.Data != differentialMCPBridgePostedPayload {
		t.Fatalf("transcript = %#v", transcript)
	}
	if observation.Status != http.StatusOK || len(observation.Steps) != 1 ||
		observation.Steps[0].Status != http.StatusAccepted {
		t.Fatalf("observation status/steps = %d/%#v", observation.Status, observation.Steps)
	}
}

func TestMCPBridgeOraclePacketParserPreservesSSEAndPOSTSemantics(t *testing.T) {
	frames := []string{
		"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\nTransfer-Encoding: chunked\r\n\r\n",
		"event: endpoint\ndata: /mcp/message?sessionId=aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee\n\n",
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":\"ping:1\"}\n\n",
		"HTTP/1.1 202 Accepted\r\nServer: APISIX\r\nConnection: close\r\nContent-Length: 0\r\n\r\n",
		"event: message\ndata: " + differentialMCPBridgePostedPayload + "\n\n",
	}
	packet := encodeMCPBridgeOracleTestPacket(frames...)
	observation, err := parseDifferentialMCPBridgeOraclePacket(differentialCasesForPlugin("mcp-bridge")[0], packet)
	if err != nil {
		t.Fatalf("parse oracle packet: %v", err)
	}
	transcript, err := decodeDifferentialMCPBridgeTranscript(observation.Body)
	if err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	if observation.Status != http.StatusOK || observation.Steps[0].Status != http.StatusAccepted ||
		transcript.Endpoint.Event != "endpoint" || transcript.Message.Data != differentialMCPBridgePostedPayload {
		t.Fatalf("observation/transcript = %#v / %#v", observation, transcript)
	}

	if _, err := parseDifferentialMCPBridgeOraclePacket(
		differentialCasesForPlugin("mcp-bridge")[0], packet[:len(packet)-1],
	); err == nil || !strings.Contains(err.Error(), "frame") {
		t.Fatalf("truncated packet error = %v, want frame error", err)
	}
}

func TestMCPBridgeProtocolDriverRegistryDispatchesOnlyPinnedPolicy(t *testing.T) {
	unknown := differentialCasesForPlugin("mcp-bridge")[0]
	unknown.ComparisonPolicy = ""
	if _, handled, err := observeDifferentialProtocolCandidate(unknown, 1); err != nil || handled {
		t.Fatalf("unknown policy dispatch = handled %t, err %v", handled, err)
	}

	driver, ok := differentialProtocolDriverRegistry[differentialMCPBridgeSSESessionPolicy]
	if !ok || driver.observeCandidate == nil || driver.observeOracle == nil {
		t.Fatalf("MCP bridge driver registration = %#v, %t", driver, ok)
	}
}

func TestDifferentialProtocolDriverHooksRunBeforeOrdinaryHTTPObservation(t *testing.T) {
	const policy = "test-mcp-protocol-hook"
	want := DifferentialObservation{Body: "protocol-driver"}
	differentialProtocolDriverRegistry[policy] = differentialProtocolDriver{
		observeCandidate: func(DifferentialCase, int) (DifferentialObservation, error) {
			return want, nil
		},
		observeOracle: func(DifferentialCase, *differentialChild) (DifferentialObservation, error) {
			return want, nil
		},
	}
	t.Cleanup(func() { delete(differentialProtocolDriverRegistry, policy) })
	spec := DifferentialCase{ComparisonPolicy: policy}

	candidate, err := observeDifferentialSideWithPorts(nil, spec, 0, 0, "")
	if err != nil || candidate.Body != want.Body {
		t.Fatalf("candidate protocol hook = %#v, err %v", candidate, err)
	}
	oracle, err := observeDifferentialOracleSide(nil, spec, nil, "")
	if err != nil || oracle.Body != want.Body {
		t.Fatalf("Oracle protocol hook = %#v, err %v", oracle, err)
	}
}

func encodeMCPBridgeOracleTestPacket(frames ...string) []byte {
	var packet []byte
	for _, frame := range frames {
		length := make([]byte, 4)
		binary.BigEndian.PutUint32(length, uint32(len(frame)))
		packet = append(packet, length...)
		packet = append(packet, frame...)
	}
	return packet
}
