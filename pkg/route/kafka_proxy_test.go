package route

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/segmentio/kafka-go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
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

func buildKafkaPubSubProxyHandlerForTest(
	t *testing.T,
	upstream resource.Upstream,
	factory kafka_proxy.KafkaConsumerFactory,
) http.Handler {
	t.Helper()
	handler, err := buildKafkaPubSubProxyHandlerStrictWithSSLResolver(upstream, factory, nil)
	if err != nil {
		t.Fatalf("buildKafkaPubSubProxyHandlerStrictWithSSLResolver() error = %v", err)
	}
	return handler
}

func TestBuildKafkaPubSubHandlerFetchesKafkaMessages(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{messages: []kafka_proxy.KafkaMessage{{
			Offset: 11, Timestamp: 22, Key: []byte("key"), Value: []byte("value"),
		}}}, nil
	}
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
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

func TestBuildKafkaPubSubHandlerUsesBracketedIPv6BrokerURL(t *testing.T) {
	brokers := make(chan []string, 1)
	factory := func(_ context.Context, configured []string, _ kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		brokers <- append([]string(nil), configured...)
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
		Nodes: []resource.Node{{Host: "::1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	_ = dialKafkaWebSocket(t, server.URL)

	select {
	case got := <-brokers:
		if len(got) != 1 || got[0] != "kafka://[::1]:9092" {
			t.Fatalf("Kafka brokers = %#v, want [kafka://[::1]:9092]", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Kafka broker configuration")
	}
}

func TestBuildKafkaPubSubHandlerListsOffset(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{listOffset: 42}, nil
	}
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
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
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
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
	_, err := buildKafkaPubSubProxyHandlerStrictWithSSLResolver(
		resource.Upstream{
			Scheme: "kafka",
			TLS:    &resource.UpstreamTLS{ClientCertID: "ssl-resource"},
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 9093, Weight: 1}},
		},
		nil,
		plannedSSLResolver(nil),
	)
	if err == nil {
		t.Fatal("buildKafkaPubSubProxyHandlerStrictWithSSLResolver() error = nil, want missing SSL resource rejection")
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

func TestNormalizeSSLIDLegacyForms(t *testing.T) {
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
			got, err := normalizeSSLID(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeSSLID(%#v) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("normalizeSSLID(%#v) = %q, want %q", tt.value, got, tt.want)
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
	_, err := buildKafkaPubSubProxyHandlerStrictWithSSLResolver(
		resource.Upstream{
			Scheme: "kafka",
			TLS: &resource.UpstreamTLS{
				ClientCert: "not-a-certificate",
				ClientKey:  "not-a-key",
			},
			Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9093, Weight: 1}},
		},
		nil,
		plannedSSLResolver(nil),
	)
	if err == nil {
		t.Fatal(
			"buildKafkaPubSubProxyHandlerStrictWithSSLResolver() error = nil, " +
				"want invalid client certificate rejection",
		)
	}
}

func TestBuildKafkaPubSubHandlerReturnsWrongCommandAndKeepsSession(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn := dialKafkaWebSocket(t, server.URL)
	writeKafkaWebSocketMessage(t, conn, []byte{0x8a, 0x02, 0x05, 0x08, 0x01})
	response, err := kafka_proxy.ParsePubSubResponse(readKafkaWebSocketMessage(t, conn))
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if response.Sequence != 0 || response.Kind != kafka_proxy.RespError || response.Code != 0 ||
		response.Message != "wrong command" {
		t.Fatalf("malformed-request response = %#v, want sequence 0 wrong command", response)
	}

	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 11, Command: kafka_proxy.CmdPing, State: []byte("still-open"),
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	writeKafkaWebSocketMessage(t, conn, request)
	response, err = kafka_proxy.ParsePubSubResponse(readKafkaWebSocketMessage(t, conn))
	if err != nil {
		t.Fatalf("ParsePubSubResponse() after malformed request error = %v", err)
	}
	if response.Sequence != 11 || response.Kind != kafka_proxy.RespPong ||
		!bytes.Equal(response.State, []byte("still-open")) {
		t.Fatalf("post-malformed response = %#v, want pong sequence 11", response)
	}
}

func TestBuildKafkaPubSubHandlerMapsKafkaAuthError(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{fetchErr: kafka.SASLAuthenticationFailed}, nil
	}
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
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
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
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

func TestBuildReverseHandlerRejectsKafkaNonUpgrade(t *testing.T) {
	routeResource := resource.Route{ID: "kafka-non-upgrade", Upstream: resource.Upstream{
		Scheme: "kafka",
		Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}}
	upstream, err := PlanRouteUpstream(routeResource, resource.Service{}, nil, nil, &testEffectiveConfig().Config)
	if err != nil {
		t.Fatalf("PlanRouteUpstream() error = %v", err)
	}
	handler, err := BuildPreparedHandler(PreparedHandlerInput{
		Route: routeResource, Upstream: upstream, StaticConfig: testEffectiveConfig().Config,
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/kafka", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("non-upgrade status = %d, want 426", recorder.Code)
	}
}

func TestKafkaProxyMetadataBindingKeepsHandlerAcrossResponsePlanTerminal(t *testing.T) {
	routeResource := resource.Route{ID: "kafka-metadata-route", Uri: "/kafka"}
	provenance := plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID}
	plans, err := planPluginSources(
		materializedPluginSources(
			map[string]resource.PluginConfig{
				"kafka-proxy": map[string]any{
					"_meta": map[string]any{
						"filter": []any{[]any{"arg_enabled", "==", "yes"}},
						"error_response": map[string]any{
							"message": "metadata unavailable",
						},
					},
				},
			},
			provenance,
		),
		plugin.NewEnabledSet([]string{"kafka-proxy"}),
	)
	if err != nil {
		t.Fatalf("planPluginSources() error = %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("metadata plans = %d, want 1", len(plans))
	}
	var pluginStopped atomic.Bool
	binding, err := plugin.BindPluginChecked(
		"kafka-proxy",
		&preparedHandlerTestPlugin{
			name:     "kafka-proxy",
			priority: 2500,
			handler: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if pluginStopped.Load() {
						http.Error(writer, "Kafka credentials unavailable", http.StatusInternalServerError)
						return
					}
					next.ServeHTTP(writer, request)
				})
			},
		},
		plugin.ScopeRoute,
		provenance,
	)
	if err != nil {
		t.Fatalf("BindPluginChecked() error = %v", err)
	}
	binding, err = plans[0].Apply(binding)
	if err != nil {
		t.Fatalf("PluginPlan.Apply() error = %v", err)
	}
	bindings := []plugin.Binding{binding}
	wrapped, ok := binding.Plugin.(metadataPlugin)
	if !ok || wrapped.filter == nil || wrapped.errorResponse == nil {
		t.Fatalf("kafka metadata binding = %T/%#v, want filter and error_response wrapper", binding.Plugin, wrapped)
	}

	var (
		terminalCalls atomic.Int32
		retained      *http.Request
		wantPassword  string
		wantStatus    int
	)
	terminalHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		terminalCalls.Add(1)
		retained = r
		if got := kafka_proxy.SASLPassword(r); got != wantPassword {
			t.Errorf("terminal SASLPassword() = %q, want %q", got, wantPassword)
		}
		w.WriteHeader(wantStatus)
	})
	terminalCandidate := plugin.RouteTerminalCandidate{
		Identity: "kafka-proxy",
		Scope:    plugin.ScopeRoute,
		Protocol: plugin.ProtocolKafka,
		Provenance: plugin.ResourceProvenance{
			Kind: plugin.ResourceRoute,
			ID:   routeResource.ID,
		},
		Terminal: routeKafkaTerminal{handler: terminalHandler},
	}
	plan, err := plugin.BuildResponsePlan(plugin.ResponsePlanInput{
		StaticBindings: bindings,
		RouteTerminals: []plugin.RouteTerminalCandidate{terminalCandidate},
	})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	pipeline, err := newRequestPipelineWithLog(bindings, nil)
	if err != nil {
		t.Fatalf("newRequestPipelineWithLog() error = %v", err)
	}
	fallback := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("ordinary fallback ran despite Kafka route terminal ownership")
	})
	ordinary := ensureRouteLifecycle(plan.Install(pipeline, fallback))
	transparent, err := buildTransparentUpgradeHandler(pipeline, plan, fallback, true)
	if err != nil {
		t.Fatalf("buildTransparentUpgradeHandler() error = %v", err)
	}
	transparent = ensureRouteLifecycle(transparent)

	run := func(name string, handler http.Handler, enabled string, upgrade bool, password string, status int) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			terminalCalls.Store(0)
			retained = nil
			wantPassword = password
			wantStatus = status
			request := httptest.NewRequest(
				http.MethodGet,
				"http://gateway.test/kafka?enabled="+enabled,
				nil,
			)
			if upgrade {
				request.Header.Set("Connection", "upgrade")
				request.Header.Set("Upgrade", "websocket")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != status || terminalCalls.Load() != 1 {
				t.Fatalf(
					"response/terminal calls = %d/%d, want %d/1",
					response.Code,
					terminalCalls.Load(),
					status,
				)
			}
			if retained == nil {
				t.Fatal("route terminal did not retain request")
			}
			if got := kafka_proxy.SASLPassword(retained); got != "" {
				t.Fatalf("retained terminal request password = %q, want cleared", got)
			}
			if lifecycle := apisixctx.GetRequestLifecycle(retained); lifecycle == nil {
				t.Fatal("route terminal request lost production lifecycle")
			}
		})
	}

	run("ordinary non-upgrade once", ordinary, "yes", false, "", http.StatusUpgradeRequired)
	run("transparent upgrade once", transparent, "yes", true, "", http.StatusSwitchingProtocols)
	run("metadata filter bypass", transparent, "no", true, "", http.StatusSwitchingProtocols)

	pluginStopped.Store(true)
	terminalCalls.Store(0)
	response := httptest.NewRecorder()
	ordinary.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/kafka?enabled=yes", nil),
	)
	if response.Code != http.StatusInternalServerError ||
		strings.TrimSpace(response.Body.String()) != `{"message":"metadata unavailable"}` ||
		terminalCalls.Load() != 0 {
		t.Fatalf(
			"metadata error response/status/calls = %q/%d/%d",
			response.Body.String(),
			response.Code,
			terminalCalls.Load(),
		)
	}
}

func TestBuildKafkaPubSubHandlerHandlesPingBeforeRequest(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{listOffset: 7}, nil
	}
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
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

func TestBuildKafkaPubSubHandlerIgnoresTextMessage(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
		Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9092, Weight: 1}},
	}, factory)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn := dialKafkaWebSocket(t, server.URL)

	if err := conn.WriteMessage(websocket.TextMessage, []byte("not binary")); err != nil {
		t.Fatalf("write text message: %v", err)
	}
	request, err := kafka_proxy.MarshalPubSubRequest(kafka_proxy.PubSubRequest{
		Sequence: 12, Command: kafka_proxy.CmdPing, State: []byte("after-text"),
	})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	writeKafkaWebSocketMessage(t, conn, request)
	response, err := kafka_proxy.ParsePubSubResponse(readKafkaWebSocketMessage(t, conn))
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if response.Sequence != 12 || response.Kind != kafka_proxy.RespPong ||
		!bytes.Equal(response.State, []byte("after-text")) {
		t.Fatalf("post-text response = %#v, want pong sequence 12", response)
	}
}

func TestBuildKafkaPubSubHandlerNormalCloseEndsCleanly(t *testing.T) {
	factory := func(context.Context, []string, kafka_proxy.ConsumerOptions) (kafka_proxy.KafkaConsumer, error) {
		return fakeKafkaPubSubConsumer{}, nil
	}
	handler := buildKafkaPubSubProxyHandlerForTest(t, resource.Upstream{
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
