package mqtt_proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestHandlerPassesThrough(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))

	if !called {
		t.Fatal("next handler was not called")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestPostInitFillsDefaultProtocolName(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	if p.config.ProtocolName != "MQTT" {
		t.Fatalf("ProtocolName = %q, want MQTT", p.config.ProtocolName)
	}
}

func TestSchemaValidatesOfficialConfig(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"protocol_name":  "MQTT",
		"protocol_level": 4,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("mqtt-proxy config should validate: %v", err)
	}
	if err := util.Validate(map[string]any{"protocol_name": "MQTT"}, p.GetSchema()); err == nil {
		t.Fatal("mqtt-proxy config without protocol_level should not validate")
	}
}

func TestAPISIX317MQTTConnectMatrix(t *testing.T) {
	tests := []struct {
		name       string
		packet     []byte
		level      int
		wantID     string
		wantDialID string
		wantErr    error
	}{
		{
			name:    "invalid packet header",
			packet:  []byte("mmm"),
			level:   4,
			wantErr: ErrMalformedConnect,
		},
		{
			name:       "MQTT 3.1.1 client ID",
			packet:     []byte("\x10\x0f\x00\x04MQTT\x04\x02\x00\x3c\x00\x03foo"),
			level:      4,
			wantID:     "foo",
			wantDialID: "foo",
		},
		{
			name:       "MQTT 3.1.1 empty client ID",
			packet:     []byte("\x10\x0c\x00\x04MQTT\x04\x02\x00\x3c\x00\x00"),
			level:      4,
			wantDialID: "192.0.2.10:1883",
		},
		{
			name:       "MQTT 5 empty properties and client ID",
			packet:     []byte("\x10\x0d\x00\x04MQTT\x05\x02\x00\x3c\x00\x00\x00"),
			level:      5,
			wantDialID: "192.0.2.10:1883",
		},
		{
			name:       "MQTT 5 session-expiry property and client ID",
			packet:     []byte("\x10\x1b\x00\x04MQTT\x05\x02\x00\x3c\x05\x11\x00\x00\x0e\x10\x00\x09clint-111"),
			level:      5,
			wantID:     "clint-111",
			wantDialID: "clint-111",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, _, err := decodeConnect(bytes.NewReader(tt.packet), "MQTT", tt.level)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("decodeConnect() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeConnect() error = %v", err)
			}
			if info.ClientID != tt.wantID {
				t.Fatalf("client ID = %q, want %q", info.ClientID, tt.wantID)
			}
			if got := ClientIDOrPeer(info, "192.0.2.10:1883"); got != tt.wantDialID {
				t.Fatalf("ClientIDOrPeer() = %q, want %q", got, tt.wantDialID)
			}
		})
	}
}

func TestDecodeConnectExtractsClientIDAndPreservesPacketLength(t *testing.T) {
	packet := mqttConnectPacket(4, 0x02, "client-1", nil, nil)
	packet = append(packet, []byte("next-packet")...)

	info, raw, err := decodeConnect(bytes.NewReader(packet), "MQTT", 4)
	if err != nil {
		t.Fatalf("decodeConnect() error = %v", err)
	}
	if info.ProtocolName != "MQTT" || info.ProtocolLevel != 4 {
		t.Fatalf("protocol = %q/%d, want MQTT/4", info.ProtocolName, info.ProtocolLevel)
	}
	if info.ClientID != "client-1" {
		t.Fatalf("client ID = %q, want client-1", info.ClientID)
	}
	if info.PacketLength != len(packet)-len("next-packet") {
		t.Fatalf("packet length = %d, want %d", info.PacketLength, len(packet)-len("next-packet"))
	}
	if !bytes.Equal(raw, packet[:len(packet)-len("next-packet")]) {
		t.Fatalf("raw packet = %x, want exact CONNECT bytes", raw)
	}
	if got := ClientIDOrPeer(info, "192.0.2.10:1234"); got != "client-1" {
		t.Fatalf("ClientIDOrPeer() = %q, want client-1", got)
	}
}

func TestDecodeConnectDoesNotConsumeFollowingPacket(t *testing.T) {
	connect := mqttConnectPacket(5, 0x02, "client-1", nil, nil)
	publish := []byte{0x30, 0x00}
	reader := bufio.NewReader(bytes.NewReader(append(append([]byte(nil), connect...), publish...)))
	info, raw, err := decodeConnect(reader, "MQTT", 5)
	if err != nil {
		t.Fatal(err)
	}
	if info.PacketLength != len(connect) || !bytes.Equal(raw, connect) {
		t.Fatalf("packet/raw = %d/%x", info.PacketLength, raw)
	}
	rest, _ := io.ReadAll(reader)
	if !bytes.Equal(rest, publish) {
		t.Fatalf("remaining = %x", rest)
	}
}

func TestDecodeConnectSupportsMQTT5PropertiesAndEmptyClientIDFallback(t *testing.T) {
	properties := []byte{
		0x11, 0, 0, 0, 30,
		0x21, 0, 10,
		0x26, 0, 3, 'k', 'e', 'y', 0, 5, 'v', 'a', 'l', 'u', 'e',
	}
	packet := mqttConnectPacket(5, 0x02, "", properties, nil)

	info, _, err := decodeConnect(bytes.NewReader(packet), "MQTT", 5)
	if err != nil {
		t.Fatalf("decodeConnect() error = %v", err)
	}
	if got := ClientIDOrPeer(info, "198.51.100.8:1883"); got != "198.51.100.8:1883" {
		t.Fatalf("ClientIDOrPeer() = %q, want peer fallback", got)
	}
}

func TestDecodeConnectSupportsMQTT31Packets(t *testing.T) {
	packet := mqttConnectPacketWithName("MQIsdp", 3, 0x02, "client-31", nil, nil)
	info, _, err := decodeConnect(bytes.NewReader(packet), "MQIsdp", 3)
	if err != nil {
		t.Fatalf("decodeConnect() error = %v", err)
	}
	if info.ProtocolName != "MQIsdp" || info.ProtocolLevel != 3 || info.ClientID != "client-31" {
		t.Fatalf("info = %#v, want MQIsdp/3/client-31", info)
	}
}

func TestDecodeConnectParsesWillUsernameAndPassword(t *testing.T) {
	payload := appendMQTTUTF8(nil, "will-topic")
	payload = append(payload, 0, 3, 'w', 'i', 'l')
	payload = append(payload, appendMQTTUTF8(nil, "user")...)
	payload = append(payload, 0, 4, 'p', 'a', 's', 's')
	packet := mqttConnectPacket(4, 0xc6, "client-4", nil, payload)
	info, _, err := decodeConnect(bytes.NewReader(packet), "MQTT", 4)
	if err != nil {
		t.Fatalf("decodeConnect() error = %v", err)
	}
	if info.ClientID != "client-4" {
		t.Fatalf("client ID = %q, want client-4", info.ClientID)
	}
}

func TestDecodeConnectRejectsMalformedPackets(t *testing.T) {
	valid := mqttConnectPacket(4, 0x02, "client", nil, nil)
	tests := []struct {
		name  string
		data  []byte
		level int
		want  error
	}{
		{name: "partial remaining length", data: []byte{0x10, 0x80}, want: ErrNeedMoreData},
		{name: "truncated packet body", data: []byte{0x10, 0x10, 'M', 'Q'}, want: ErrNeedMoreData},
		{name: "wrong packet type", data: append([]byte{0x20}, valid[1:]...), want: ErrMalformedConnect},
		{
			name: "invalid reserved flag",
			data: mqttConnectPacket(4, 0x03, "client", nil, nil),
			want: ErrMalformedConnect,
		},
		{
			name: "invalid password flags",
			data: mqttConnectPacket(4, 0x42, "client", nil, nil),
			want: ErrMalformedConnect,
		},
		{name: "wrong protocol level", data: valid, level: 5, want: ErrMalformedConnect},
		{
			name: "invalid protocol name",
			data: mqttConnectPacketWithName("AMQP", 4, 0x02, "client", nil, nil),
			want: ErrMalformedConnect,
		},
		{
			name: "invalid UTF-8 client ID",
			data: mqttConnectPacket(4, 0x02, "cli\xffent", nil, nil),
			want: ErrMalformedConnect,
		},
		{
			name:  "truncated properties",
			data:  mqttConnectPacket(5, 0x02, "client", []byte{0x11, 0, 0}, nil),
			level: 5,
			want:  ErrNeedMoreData,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := decodeConnect(bytes.NewReader(tt.data), "MQTT", tt.level)
			if err == nil || !errors.Is(err, tt.want) {
				t.Fatalf("decodeConnect() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDecodeConnectRejectsInvalidWillAndTrailingBytes(t *testing.T) {
	invalidWillQoS := mqttConnectPacket(4, 0x1a, "client", nil, nil)
	if _, _, err := decodeConnect(bytes.NewReader(invalidWillQoS), "MQTT", 4); err == nil {
		t.Fatal("decodeConnect() error = nil for invalid will QoS")
	}

	trailing := mqttConnectPacket(4, 0x02, "client", nil, []byte{0x01})
	if _, _, err := decodeConnect(bytes.NewReader(trailing), "MQTT", 4); err == nil {
		t.Fatal("decodeConnect() error = nil for trailing payload bytes")
	}
}

func mqttConnectPacket(level byte, flags byte, clientID string, properties []byte, payload []byte) []byte {
	return mqttConnectPacketWithName("MQTT", level, flags, clientID, properties, payload)
}

func mqttConnectPacketWithName(
	protocolName string,
	level byte,
	flags byte,
	clientID string,
	properties []byte,
	payload []byte,
) []byte {
	variableHeader := make([]byte, 0, 16+len(properties))
	variableHeader = appendMQTTUTF8(variableHeader, protocolName)
	variableHeader = append(variableHeader, level, flags, 0, 60)
	if level == 5 {
		variableHeader = appendMQTTVariableInteger(variableHeader, len(properties))
		variableHeader = append(variableHeader, properties...)
	}
	body := appendMQTTUTF8(variableHeader, clientID)
	body = append(body, payload...)
	packet := []byte{0x10}
	packet = appendMQTTVariableInteger(packet, len(body))
	return append(packet, body...)
}

func appendMQTTUTF8(dst []byte, value string) []byte {
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(value)))
	dst = append(dst, length[:]...)
	return append(dst, value...)
}

func appendMQTTVariableInteger(dst []byte, value int) []byte {
	for {
		encoded := byte(value % 128)
		value /= 128
		if value > 0 {
			encoded |= 0x80
		}
		dst = append(dst, encoded)
		if value == 0 {
			return dst
		}
	}
}
