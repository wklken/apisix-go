package route

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

type fakeKafkaPubSubConsumer struct {
	listOffset int64
	messages   []kafka_proxy.KafkaMessage
	listErr    error
	fetchErr   error
}

func (f fakeKafkaPubSubConsumer) ListOffset(context.Context, string, int32, int64) (int64, error) {
	if f.listErr != nil {
		return 0, f.listErr
	}
	return f.listOffset, nil
}

func (f fakeKafkaPubSubConsumer) Fetch(context.Context, string, int32, int64) ([]kafka_proxy.KafkaMessage, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.messages, nil
}

func TestBuildKafkaPubSubHandlerFetchesKafkaMessages(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{messages: []kafka_proxy.KafkaMessage{{
			Offset: 11, Timestamp: 22, Key: []byte("key"), Value: []byte("value"),
		}}}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()
	writeRouteWebSocketHandshake(t, conn)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 7, Command: kafka_proxy.CmdKafkaFetch, Topic: "topic", Partition: 2, Position: 10,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	if err := writeRouteMaskedWebSocketFrame(conn, request); err != nil {
		t.Fatalf("write PubSub request: %v", err)
	}
	opcode, payload, err := readRouteWebSocketFrame(conn)
	if err != nil {
		t.Fatalf("read PubSub response: %v", err)
	}
	if opcode != 2 {
		t.Fatalf("response opcode = %d, want binary", opcode)
	}
	response, err := kafka_proxy.ParsePubSubResponse(payload)
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if response.Sequence != 7 || response.Kind != kafka_proxy.RespKafkaFetch || len(response.Messages) != 1 {
		t.Fatalf("response = %#v, want sequence 7 fetch with one message", response)
	}
	if got := response.Messages[0]; got.Offset != 11 || got.Timestamp != 22 ||
		!bytes.Equal(got.Key, []byte("key")) || !bytes.Equal(got.Value, []byte("value")) {
		t.Fatalf("Kafka message = %#v, want offset/timestamp/key/value", got)
	}
}

func TestBuildKafkaPubSubHandlerListsOffset(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{listOffset: 42}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()
	writeRouteWebSocketHandshake(t, conn)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 8, Command: kafka_proxy.CmdKafkaListOffset, Topic: "topic", Partition: 1, Position: -2,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	if err := writeRouteMaskedWebSocketFrame(conn, request); err != nil {
		t.Fatalf("write PubSub request: %v", err)
	}
	opcode, payload, err := readRouteWebSocketFrame(conn)
	if err != nil {
		t.Fatalf("read PubSub response: %v", err)
	}
	if opcode != 2 {
		t.Fatalf("response opcode = %d, want binary", opcode)
	}
	response, err := kafka_proxy.ParsePubSubResponse(payload)
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if response.Sequence != 8 || response.Kind != kafka_proxy.RespKafkaListOffset || response.Offset != 42 {
		t.Fatalf("response = %#v, want sequence 8 list-offset 42", response)
	}
}

func TestBuildKafkaPubSubHandlerPassesUpstreamTLS(t *testing.T) {
	received := make(chan *tls.Config, 1)
	factory := func(_ context.Context, _ []string, options kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		received <- options.TLSConfig
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		TLS:   &resource.UpstreamTLS{Verify: true},
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9093, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()
	writeRouteWebSocketHandshake(t, conn)
	var receivedTLS *tls.Config
	select {
	case receivedTLS = <-received:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for consumer TLS config")
	}
	if receivedTLS == nil {
		t.Fatal("consumer TLS config = nil, want upstream TLS config")
	}
	if receivedTLS.InsecureSkipVerify {
		t.Fatal("consumer TLS config has InsecureSkipVerify=true, want verify=true")
	}
}

func TestBuildReverseHandlerRejectsKafkaTLSClientCertID(t *testing.T) {
	_, err := (&Builder{}).buildReverseHandler(resource.Route{Upstream: resource.Upstream{
		Scheme: "kafka",
		TLS:    &resource.UpstreamTLS{ClientCertID: "ssl-resource"},
		Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 9093, Weight: 1}},
	}}, resource.Service{})
	if err == nil {
		t.Fatal("buildReverseHandler() error = nil, want missing SSL resource rejection")
	}
}

func TestBuildKafkaPubSubHandlerResolvesTLSClientCertID(t *testing.T) {
	certPEM, keyPEM := testKafkaClientCertificate(t)
	received := make(chan *tls.Config, 1)
	resolver := func(id string) (resource.SSL, error) {
		if id != "ssl-resource" {
			t.Fatalf("SSL resolver id = %q, want ssl-resource", id)
		}
		return resource.SSL{Cert: certPEM, Key: keyPEM}, nil
	}
	factory := func(_ context.Context, _ []string, options kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		received <- options.TLSConfig
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler, err := buildKafkaPubSubProxyHandlerStrictWithSSLResolver(resource.Upstream{
		TLS:   &resource.UpstreamTLS{ClientCertID: "ssl-resource", Verify: true},
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9093, Weight: 1}},
	}, factory, resolver)
	if err != nil {
		t.Fatalf("buildKafkaPubSubProxyHandlerStrictWithSSLResolver() error = %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()
	writeRouteWebSocketHandshake(t, conn)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 9, Command: kafka_proxy.CmdPing,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	if err := writeRouteMaskedWebSocketFrame(conn, request); err != nil {
		t.Fatalf("write PubSub request: %v", err)
	}
	if _, _, err := readRouteWebSocketFrame(conn); err != nil {
		t.Fatalf("read PubSub response: %v", err)
	}
	select {
	case tlsConfig := <-received:
		if tlsConfig == nil || tlsConfig.InsecureSkipVerify || len(tlsConfig.Certificates) != 1 {
			t.Fatalf("resolved TLS config = %#v, want verified client certificate", tlsConfig)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for consumer TLS config")
	}
}

func TestNormalizeKafkaSSLID(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{name: "string", value: "ssl-1", want: "ssl-1"},
		{name: "number", value: float64(17), want: "17"},
		{name: "fraction", value: 1.5, wantErr: true},
		{name: "empty", value: " ", wantErr: true},
		{name: "unsupported", value: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeKafkaSSLID(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeKafkaSSLID(%#v) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("normalizeKafkaSSLID(%#v) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func testKafkaClientCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatalf("rand.Int() error = %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kafka-test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(certPEM), string(keyPEM)
}

func TestBuildReverseHandlerRejectsInvalidKafkaTLSClientCertificate(t *testing.T) {
	_, err := (&Builder{}).buildReverseHandler(resource.Route{Upstream: resource.Upstream{
		Scheme: "kafka",
		TLS: &resource.UpstreamTLS{
			ClientCert: "not-a-certificate",
			ClientKey:  "not-a-key",
		},
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9093, Weight: 1}},
	}}, resource.Service{})
	if err == nil {
		t.Fatal("buildReverseHandler() error = nil, want invalid client certificate rejection")
	}
}

func TestBuildKafkaPubSubHandlerClosesMalformedRequest(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()
	writeRouteWebSocketHandshake(t, conn)
	if err := writeRouteMaskedWebSocketFrame(conn, []byte{0x8a, 0x02, 0x05, 0x08, 0x01}); err != nil {
		t.Fatalf("write malformed PubSub request: %v", err)
	}
	opcode, payload, err := readRouteWebSocketFrame(conn)
	if err != nil {
		t.Fatalf("read malformed-request close: %v", err)
	}
	if opcode != 8 || len(payload) < 2 || binary.BigEndian.Uint16(payload[:2]) != 1002 {
		t.Fatalf("malformed-request close = opcode %d payload %x, want opcode 8/code 1002", opcode, payload)
	}
}

func TestBuildKafkaPubSubHandlerMapsKafkaAuthError(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{fetchErr: kafka.SASLAuthenticationFailed}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()
	writeRouteWebSocketHandshake(t, conn)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 9, Command: kafka_proxy.CmdKafkaFetch, Topic: "topic", Partition: 0, Position: 0,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	if err := writeRouteMaskedWebSocketFrame(conn, request); err != nil {
		t.Fatalf("write PubSub request: %v", err)
	}
	_, payload, err := readRouteWebSocketFrame(conn)
	if err != nil {
		t.Fatalf("read PubSub error response: %v", err)
	}
	response, err := kafka_proxy.ParsePubSubResponse(payload)
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if response.Sequence != 9 || response.Kind != kafka_proxy.RespError || response.Code != 502 ||
		response.Message != "Kafka authentication failed" {
		t.Fatalf("response = %#v, want sanitized 502 authentication error", response)
	}
}

func TestBuildKafkaPubSubHandlerMapsTimeout(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{listErr: context.DeadlineExceeded}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()
	writeRouteWebSocketHandshake(t, conn)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 10, Command: kafka_proxy.CmdKafkaListOffset, Topic: "topic", Partition: 0, Position: -2,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	if err := writeRouteMaskedWebSocketFrame(conn, request); err != nil {
		t.Fatalf("write PubSub request: %v", err)
	}
	_, payload, err := readRouteWebSocketFrame(conn)
	if err != nil {
		t.Fatalf("read PubSub error response: %v", err)
	}
	response, err := kafka_proxy.ParsePubSubResponse(payload)
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if response.Sequence != 10 || response.Kind != kafka_proxy.RespError || response.Code != 504 {
		t.Fatalf("response = %#v, want sanitized 504 timeout error", response)
	}
}

func writeRouteWebSocketHandshake(t *testing.T, conn net.Conn) {
	t.Helper()
	_, err := fmt.Fprint(conn,
		"GET /kafka HTTP/1.1\r\nHost: gateway.test\r\nConnection: Upgrade\r\n"+
			"Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"+
			"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if err != nil {
		t.Fatalf("write WebSocket handshake: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read WebSocket handshake: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("WebSocket status = %d, want 101", response.StatusCode)
	}
}

func TestBuildKafkaRawCompatibilityHandlerProxiesWebSocketFrames(t *testing.T) {
	broker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Kafka broker: %v", err)
	}
	defer broker.Close()

	request := routeKafkaFrame([]byte("request"))
	response := routeKafkaFrame([]byte("response"))
	brokerResult := make(chan error, 1)
	go func() {
		conn, acceptErr := broker.Accept()
		if acceptErr != nil {
			brokerResult <- acceptErr
			return
		}
		defer conn.Close()
		got, readErr := readRouteKafkaFrame(conn)
		if readErr != nil {
			brokerResult <- readErr
			return
		}
		if !bytes.Equal(got, request) {
			brokerResult <- fmt.Errorf("Kafka request frame = %x, want %x", got, request)
			return
		}
		_, writeErr := conn.Write(response)
		brokerResult <- writeErr
	}()

	port := broker.Addr().(*net.TCPAddr).Port
	lb, err := pxy.NewUpstreamLoadBalance(map[string]int{fmt.Sprintf("kafka://127.0.0.1:%d", port): 1}, nil)
	if err != nil {
		t.Fatalf("NewUpstreamLoadBalance() error = %v", err)
	}
	handler := buildKafkaRawProxyHandler(lb, resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: port, Weight: 1}},
	})

	server := httptest.NewServer(handler)
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()

	const websocketKey = "dGhlIHNhbXBsZSBub25jZQ=="
	_, _ = fmt.Fprintf(
		conn,
		"GET /kafka HTTP/1.1\r\nHost: gateway.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n",
		websocketKey,
	)
	responseHeaders, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read WebSocket handshake: %v", err)
	}
	if responseHeaders.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("WebSocket status = %d, want 101", responseHeaders.StatusCode)
	}
	if got := responseHeaders.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("Sec-WebSocket-Accept = %q, want RFC example value", got)
	}

	if err := writeRouteMaskedWebSocketFrame(conn, request); err != nil {
		t.Fatalf("write WebSocket frame: %v", err)
	}
	opcode, got, err := readRouteWebSocketFrame(conn)
	if err != nil {
		t.Fatalf("read WebSocket response frame: %v", err)
	}
	if opcode != 2 || !bytes.Equal(got, response) {
		t.Fatalf("WebSocket response opcode/payload = %d/%x, want 2/%x", opcode, got, response)
	}
	if err := <-brokerResult; err != nil {
		t.Fatalf("Kafka broker: %v", err)
	}
}

func TestBuildReverseHandlerRejectsKafkaNonUpgrade(t *testing.T) {
	handler, err := (&Builder{}).buildReverseHandler(resource.Route{Upstream: resource.Upstream{
		Scheme: "kafka",
		Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/kafka", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("non-upgrade status = %d, want 426", recorder.Code)
	}
}

func TestBuildKafkaRawCompatibilityHandlerRejectsMalformedWebSocketFrame(t *testing.T) {
	broker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Kafka broker: %v", err)
	}
	defer broker.Close()
	brokerClosed := make(chan struct{})
	go func() {
		conn, acceptErr := broker.Accept()
		if acceptErr == nil {
			_, _ = io.Copy(io.Discard, conn)
			_ = conn.Close()
		}
		close(brokerClosed)
	}()

	port := broker.Addr().(*net.TCPAddr).Port
	lb, err := pxy.NewUpstreamLoadBalance(map[string]int{fmt.Sprintf("kafka://127.0.0.1:%d", port): 1}, nil)
	if err != nil {
		t.Fatalf("NewUpstreamLoadBalance() error = %v", err)
	}
	handler := buildKafkaRawProxyHandler(lb, resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: port, Weight: 1}},
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(
		conn,
		"GET /kafka HTTP/1.1\r\nHost: gateway.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
	)
	if response, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
		t.Fatalf("read WebSocket handshake: %v", err)
	} else if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("WebSocket status = %d, want 101", response.StatusCode)
	}

	if err := writeRouteMaskedWebSocketFrame(conn, []byte{0, 0, 0, 5, 'x'}); err != nil {
		t.Fatalf("write malformed WebSocket frame: %v", err)
	}
	opcode, payload, err := readRouteWebSocketFrame(conn)
	if err != nil {
		t.Fatalf("read malformed-frame close: %v", err)
	}
	if opcode != 8 || len(payload) < 2 || binary.BigEndian.Uint16(payload[:2]) != 1002 {
		t.Fatalf("malformed-frame close = opcode %d payload %x, want opcode 8/code 1002", opcode, payload)
	}
	select {
	case <-brokerClosed:
	case <-time.After(time.Second):
		t.Fatal("Kafka broker connection did not close")
	}
}

func routeKafkaFrame(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	return frame
}

func readRouteKafkaFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	frame := make([]byte, 4+int(binary.BigEndian.Uint32(header[:])))
	copy(frame, header[:])
	_, err := io.ReadFull(reader, frame[4:])
	return frame, err
}

func writeRouteMaskedWebSocketFrame(writer io.Writer, payload []byte) error {
	if len(payload) >= 126 {
		return fmt.Errorf("test payload too large")
	}
	frame := make([]byte, 6+len(payload))
	frame[0] = 0x82
	frame[1] = 0x80 | byte(len(payload))
	copy(frame[2:6], []byte{1, 2, 3, 4})
	for index, value := range payload {
		frame[6+index] = value ^ frame[2+index%4]
	}
	_, err := writer.Write(frame)
	return err
}

func readRouteWebSocketFrame(reader io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	if header[1]&0x80 != 0 || header[1]&0x7f >= 126 {
		return 0, nil, fmt.Errorf("unsupported test response frame")
	}
	payload := make([]byte, int(header[1]&0x7f))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0] & 0x0f, payload, nil
}
