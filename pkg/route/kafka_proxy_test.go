package route

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/segmentio/kafka-go"
	"github.com/wklken/apisix-go/pkg/json"
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

func dialKafkaWebSocket(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/kafka"
	conn, response, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Dial() response/error = %#v/%v", response, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeKafkaWebSocketMessage(t *testing.T, conn *websocket.Conn, payload []byte) {
	t.Helper()
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write WebSocket message: %v", err)
	}
}

func readKafkaWebSocketMessage(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read WebSocket message: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("WebSocket message type = %d, want binary", messageType)
	}
	return payload
}

func readKafkaWebSocketClose(t *testing.T, conn *websocket.Conn) *websocket.CloseError {
	t.Helper()
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read WebSocket close: error = %v, want close frame", err)
	}
	return closeErr
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
	conn := dialKafkaWebSocket(t, server.URL)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 7, Command: kafka_proxy.CmdKafkaFetch, Topic: "topic", Partition: 2, Position: 10,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	writeKafkaWebSocketMessage(t, conn, request)
	response, err := kafka_proxy.ParsePubSubResponse(readKafkaWebSocketMessage(t, conn))
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
	conn := dialKafkaWebSocket(t, server.URL)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 8, Command: kafka_proxy.CmdKafkaListOffset, Topic: "topic", Partition: 1, Position: -2,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	writeKafkaWebSocketMessage(t, conn, request)
	payload := readKafkaWebSocketMessage(t, conn)
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
	_ = dialKafkaWebSocket(t, server.URL)
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
	conn := dialKafkaWebSocket(t, server.URL)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 9, Command: kafka_proxy.CmdPing,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	writeKafkaWebSocketMessage(t, conn, request)
	_ = readKafkaWebSocketMessage(t, conn)
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
		{name: "json number", value: json.Number("3"), want: "3"},
		{name: "float32", value: float32(5), want: "5"},
		{name: "int", value: 6, want: "6"},
		{name: "int8", value: int8(7), want: "7"},
		{name: "int16", value: int16(8), want: "8"},
		{name: "int32", value: int32(9), want: "9"},
		{name: "int64", value: int64(10), want: "10"},
		{name: "uint", value: uint(11), want: "11"},
		{name: "uint8", value: uint8(12), want: "12"},
		{name: "uint16", value: uint16(13), want: "13"},
		{name: "uint32", value: uint32(14), want: "14"},
		{name: "uint64", value: uint64(15), want: "15"},
		{name: "nan float", value: math.NaN(), wantErr: true},
		{name: "inf float", value: math.Inf(1), wantErr: true},
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
	conn := dialKafkaWebSocket(t, server.URL)
	writeKafkaWebSocketMessage(t, conn, []byte{0x8a, 0x02, 0x05, 0x08, 0x01})
	if closeErr := readKafkaWebSocketClose(t, conn); closeErr.Code != 1002 {
		t.Fatalf("malformed-request close code = %d, want 1002", closeErr.Code)
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
	conn := dialKafkaWebSocket(t, server.URL)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 9, Command: kafka_proxy.CmdKafkaFetch, Topic: "topic", Partition: 0, Position: 0,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	writeKafkaWebSocketMessage(t, conn, request)
	response, err := kafka_proxy.ParsePubSubResponse(readKafkaWebSocketMessage(t, conn))
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
	conn := dialKafkaWebSocket(t, server.URL)
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 10, Command: kafka_proxy.CmdKafkaListOffset, Topic: "topic", Partition: 0, Position: -2,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	writeKafkaWebSocketMessage(t, conn, request)
	response, err := kafka_proxy.ParsePubSubResponse(readKafkaWebSocketMessage(t, conn))
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if response.Sequence != 10 || response.Kind != kafka_proxy.RespError || response.Code != 504 {
		t.Fatalf("response = %#v, want sanitized 504 timeout error", response)
	}
}

func TestBuildKafkaRawCompatibilityHandlerProxiesWebSocketFrames(t *testing.T) {
	broker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Kafka broker: %v", err)
	}
	defer func() { _ = broker.Close() }()

	request := routeKafkaFrame([]byte("request"))
	response := routeKafkaFrame([]byte("response"))
	brokerResult := make(chan error, 1)
	go func() {
		conn, acceptErr := broker.Accept()
		if acceptErr != nil {
			brokerResult <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
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
	conn := dialKafkaWebSocket(t, server.URL)

	writeKafkaWebSocketMessage(t, conn, request)
	if got := readKafkaWebSocketMessage(t, conn); !bytes.Equal(got, response) {
		t.Fatalf("WebSocket response payload = %x, want %x", got, response)
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
	defer func() { _ = broker.Close() }()
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
	conn := dialKafkaWebSocket(t, server.URL)

	writeKafkaWebSocketMessage(t, conn, []byte{0, 0, 0, 5, 'x'})
	if closeErr := readKafkaWebSocketClose(t, conn); closeErr.Code != 1002 {
		t.Fatalf("malformed-frame close code = %d, want 1002", closeErr.Code)
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

func TestBuildKafkaRawCompatibilityHandlerAcceptsFragmentedBinaryMessage(t *testing.T) {
	broker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Kafka broker: %v", err)
	}
	defer func() { _ = broker.Close() }()

	request := routeKafkaFrame([]byte("fragmented"))
	response := routeKafkaFrame([]byte("response"))
	brokerResult := make(chan error, 1)
	go func() {
		conn, acceptErr := broker.Accept()
		if acceptErr != nil {
			brokerResult <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
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
	conn := dialKafkaWebSocket(t, server.URL)

	writer, err := conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		t.Fatalf("NextWriter() error = %v", err)
	}
	half := len(request) / 2
	if _, err := writer.Write(request[:half]); err != nil {
		t.Fatalf("write fragmented part 1: %v", err)
	}
	if _, err := writer.Write(request[half:]); err != nil {
		t.Fatalf("write fragmented part 2: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close fragmented message: %v", err)
	}
	if got := readKafkaWebSocketMessage(t, conn); !bytes.Equal(got, response) {
		t.Fatalf("WebSocket response payload = %x, want %x", got, response)
	}
	if err := <-brokerResult; err != nil {
		t.Fatalf("Kafka broker: %v", err)
	}
}

func TestBuildKafkaPubSubHandlerHandlesPingBeforeRequest(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{listOffset: 7}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn := dialKafkaWebSocket(t, server.URL)

	if err := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 3, Command: kafka_proxy.CmdKafkaListOffset, Topic: "topic", Partition: 0, Position: -2,
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	writeKafkaWebSocketMessage(t, conn, request)
	response, err := kafka_proxy.ParsePubSubResponse(readKafkaWebSocketMessage(t, conn))
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if response.Sequence != 3 || response.Kind != kafka_proxy.RespKafkaListOffset || response.Offset != 7 {
		t.Fatalf("response = %#v, want sequence 3 list-offset 7", response)
	}
}

func TestBuildKafkaPubSubHandlerRejectsTextMessage(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn := dialKafkaWebSocket(t, server.URL)

	if err := conn.WriteMessage(websocket.TextMessage, []byte("not binary")); err != nil {
		t.Fatalf("write text message: %v", err)
	}
	if closeErr := readKafkaWebSocketClose(t, conn); closeErr.Code != 1003 {
		t.Fatalf("text-message close code = %d, want 1003", closeErr.Code)
	}
}

func TestBuildKafkaRawCompatibilityHandlerRejectsOversizedMessage(t *testing.T) {
	broker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Kafka broker: %v", err)
	}
	defer func() { _ = broker.Close() }()
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
	conn := dialKafkaWebSocket(t, server.URL)

	// Declare a frame far beyond the read limit without sending its payload;
	// the server must reject it from the header alone.
	oversized := []byte{0x82, 0xff, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	if _, err := conn.UnderlyingConn().Write(oversized); err != nil {
		t.Fatalf("write oversized frame header: %v", err)
	}
	if closeErr := readKafkaWebSocketClose(t, conn); closeErr.Code != 1009 {
		t.Fatalf("oversized-message close code = %d, want 1009", closeErr.Code)
	}
	select {
	case <-brokerClosed:
	case <-time.After(time.Second):
		t.Fatal("Kafka broker connection did not close")
	}
}

func TestBuildKafkaPubSubHandlerNormalCloseEndsCleanly(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler := buildKafkaPubSubProxyHandler(resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn := dialKafkaWebSocket(t, server.URL)

	if err := conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatalf("write close: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("ReadMessage() error = nil, want closed connection")
	}
}
