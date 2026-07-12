package mqtt_proxy

import (
	"encoding/binary"
	"errors"
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

func TestParseConnectPacketExtractsClientIDAndPreservesPacketLength(t *testing.T) {
	packet := mqttConnectPacket(4, 0x02, "client-1", nil, nil)
	packet = append(packet, []byte("next-packet")...)

	info, err := ParseConnectPacket(packet, "MQTT", 4)
	if err != nil {
		t.Fatalf("ParseConnectPacket() error = %v", err)
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
	if got := ClientIDOrPeer(info, "192.0.2.10:1234"); got != "client-1" {
		t.Fatalf("ClientIDOrPeer() = %q, want client-1", got)
	}
}

func TestParseConnectPacketSupportsMQTT5PropertiesAndEmptyClientIDFallback(t *testing.T) {
	properties := []byte{
		0x11, 0, 0, 0, 30,
		0x21, 0, 10,
		0x26, 0, 3, 'k', 'e', 'y', 0, 5, 'v', 'a', 'l', 'u', 'e',
	}
	packet := mqttConnectPacket(5, 0x02, "", properties, nil)

	info, err := ParseConnectPacket(packet, "MQTT", 5)
	if err != nil {
		t.Fatalf("ParseConnectPacket() error = %v", err)
	}
	if got := ClientIDOrPeer(info, "198.51.100.8:1883"); got != "198.51.100.8:1883" {
		t.Fatalf("ClientIDOrPeer() = %q, want peer fallback", got)
	}
}

func TestParseConnectPacketRejectsMalformedPackets(t *testing.T) {
	valid := mqttConnectPacket(4, 0x02, "client", nil, nil)
	tests := []struct {
		name  string
		data  []byte
		level int
		want  error
	}{
		{name: "partial remaining length", data: []byte{0x10, 0x80}, want: ErrNeedMoreData},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConnectPacket(tt.data, "MQTT", tt.level)
			if err == nil || !errors.Is(err, tt.want) {
				t.Fatalf("ParseConnectPacket() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParseConnectPacketRejectsInvalidWillAndTrailingBytes(t *testing.T) {
	invalidWillQoS := mqttConnectPacket(4, 0x1a, "client", nil, nil)
	if _, err := ParseConnectPacket(invalidWillQoS, "MQTT", 4); err == nil {
		t.Fatal("ParseConnectPacket() error = nil for invalid will QoS")
	}

	trailing := mqttConnectPacket(4, 0x02, "client", nil, []byte{0x01})
	if _, err := ParseConnectPacket(trailing, "MQTT", 4); err == nil {
		t.Fatal("ParseConnectPacket() error = nil for trailing payload bytes")
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
