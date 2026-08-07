package mqtt_proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/eclipse/paho.golang/packets"
)

const DefaultMaxConnectPacketSize = 64 * 1024

var (
	ErrNeedMoreData     = errors.New("mqtt CONNECT packet needs more data")
	ErrMalformedConnect = errors.New("malformed MQTT CONNECT packet")
)

type ConnectInfo struct {
	ProtocolName  string
	ProtocolLevel byte
	ClientID      string
	PacketLength  int
}

// decodeConnect reads exactly one CONNECT packet from the reader and validates
// the protocol name, level and connect flags. MQTT 5 packets are decoded with
// the Eclipse Paho codec; MQTT 3.1/3.1.1 packets keep the local decoder
// because Paho's CONNECT codec is MQTT-5-only.
func decodeConnect(
	reader io.Reader,
	expectedProtocolName string,
	expectedProtocolLevel int,
) (ConnectInfo, []byte, error) {
	packet, err := readConnectPacketBytes(reader)
	if err != nil {
		return ConnectInfo{}, nil, err
	}
	level, err := protocolLevelOf(packet)
	if err != nil {
		return ConnectInfo{}, nil, err
	}
	if level == 5 {
		info, err := decodeConnectV5(packet, expectedProtocolName, expectedProtocolLevel)
		if err != nil {
			return ConnectInfo{}, nil, err
		}
		return info, packet, nil
	}
	info, err := ParseConnectPacket(packet, expectedProtocolName, expectedProtocolLevel)
	if err != nil {
		return ConnectInfo{}, nil, err
	}
	return info, packet, nil
}

// readConnectPacketBytes reads the fixed header and the declared remaining
// bytes of one CONNECT packet, bounded by the 64 KiB packet limit. Reads are
// byte-exact so the captured bytes contain exactly one packet.
func readConnectPacketBytes(reader io.Reader) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: DefaultMaxConnectPacketSize + 1}
	var raw bytes.Buffer
	source := io.TeeReader(limited, &raw)

	packetType, err := readByteExact(source)
	if err != nil {
		return nil, mapConnectDecodeError(err, raw.Len())
	}
	if packetType != 0x10 {
		return nil, fmt.Errorf("%w: packet is not CONNECT", ErrMalformedConnect)
	}
	remaining, err := readVariableIntegerFromReader(source)
	if err != nil {
		return nil, mapConnectDecodeError(err, raw.Len())
	}
	if remaining > DefaultMaxConnectPacketSize {
		return nil, fmt.Errorf(
			"%w: CONNECT packet exceeds %d bytes",
			ErrMalformedConnect,
			DefaultMaxConnectPacketSize,
		)
	}
	body := make([]byte, remaining)
	if _, err := io.ReadFull(source, body); err != nil {
		return nil, mapConnectDecodeError(err, raw.Len())
	}
	return raw.Bytes(), nil
}

func readByteExact(reader io.Reader) (byte, error) {
	var value [1]byte
	if _, err := io.ReadFull(reader, value[:]); err != nil {
		return 0, err
	}
	return value[0], nil
}

func readVariableIntegerFromReader(reader io.Reader) (int, error) {
	value := 0
	multiplier := 1
	for range 4 {
		encoded, err := readByteExact(reader)
		if err != nil {
			return 0, err
		}
		value += int(encoded&0x7f) * multiplier
		if value > 268435455 {
			return 0, fmt.Errorf("%w: MQTT variable integer exceeds maximum", ErrMalformedConnect)
		}
		if encoded&0x80 == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, fmt.Errorf("%w: MQTT variable integer uses more than four bytes", ErrMalformedConnect)
}

// protocolLevelOf extracts the protocol level from the variable header prefix.
func protocolLevelOf(packet []byte) (byte, error) {
	body := packet[1+variableIntegerSize(packet[1:]):]
	if len(body) < 3 {
		return 0, ErrNeedMoreData
	}
	nameLength := int(body[0])<<8 | int(body[1])
	if len(body) < 3+nameLength {
		return 0, ErrNeedMoreData
	}
	return body[2+nameLength], nil
}

// decodeConnectV5 decodes a complete MQTT 5 CONNECT packet with the Eclipse
// Paho codec and applies the APISIX protocol checks on top.
func decodeConnectV5(
	packet []byte,
	expectedProtocolName string,
	expectedProtocolLevel int,
) (ConnectInfo, error) {
	cp, err := packets.ReadPacket(bytes.NewReader(packet))
	if err != nil {
		return ConnectInfo{}, mapConnectDecodeError(err, len(packet))
	}
	if cp.Type != packets.CONNECT {
		return ConnectInfo{}, fmt.Errorf("%w: packet is not CONNECT", ErrMalformedConnect)
	}
	connect, ok := cp.Content.(*packets.Connect)
	if !ok {
		return ConnectInfo{}, fmt.Errorf("%w: unexpected CONNECT payload", ErrMalformedConnect)
	}
	if expectedProtocolName == "" {
		expectedProtocolName = "MQTT"
	}
	if connect.ProtocolName != expectedProtocolName {
		return ConnectInfo{}, fmt.Errorf(
			"%w: protocol name %q, want %q",
			ErrMalformedConnect,
			connect.ProtocolName,
			expectedProtocolName,
		)
	}
	if expectedProtocolLevel != 0 && int(connect.ProtocolVersion) != expectedProtocolLevel {
		return ConnectInfo{}, fmt.Errorf(
			"%w: protocol level %d, want %d",
			ErrMalformedConnect,
			connect.ProtocolVersion,
			expectedProtocolLevel,
		)
	}
	if err := validateMQTTUTF8(connect.ProtocolName); err != nil {
		return ConnectInfo{}, err
	}
	if err := validateMQTTUTF8(connect.ClientID); err != nil {
		return ConnectInfo{}, err
	}

	body := packet[1+variableIntegerSize(packet[1:]):]
	flagsOffset := 3 + len(connect.ProtocolName)
	if len(body) <= flagsOffset {
		return ConnectInfo{}, ErrNeedMoreData
	}
	if err := validateConnectFlags(body[flagsOffset]); err != nil {
		return ConnectInfo{}, err
	}
	if consumed := connectBodyLength(connect); consumed != len(body) {
		return ConnectInfo{}, fmt.Errorf(
			"%w: CONNECT payload has %d trailing bytes",
			ErrMalformedConnect,
			len(body)-consumed,
		)
	}

	return ConnectInfo{
		ProtocolName:  connect.ProtocolName,
		ProtocolLevel: connect.ProtocolVersion,
		ClientID:      connect.ClientID,
		PacketLength:  len(packet),
	}, nil
}

// ClientIDOrPeer uses the parsed client ID, falling back to the peer address
// for MQTT 5 clients that deliberately omit a client ID.
func ClientIDOrPeer(info ConnectInfo, peer string) string {
	if info.ClientID != "" {
		return info.ClientID
	}
	return peer
}

func mapConnectDecodeError(err error, rawLength int) error {
	if rawLength > DefaultMaxConnectPacketSize {
		return fmt.Errorf(
			"%w: CONNECT packet exceeds %d bytes",
			ErrMalformedConnect,
			DefaultMaxConnectPacketSize,
		)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrNeedMoreData
	}
	return fmt.Errorf("%w: %v", ErrMalformedConnect, err)
}

func validateMQTTUTF8(value string) error {
	if strings.ContainsRune(value, '\x00') || !utf8.ValidString(value) {
		return fmt.Errorf("%w: invalid UTF-8 string", ErrMalformedConnect)
	}
	return nil
}

// connectBodyLength returns the byte length paho re-encodes for a parsed
// CONNECT; a shorter wire body means the packet declared trailing bytes.
func connectBodyLength(connect *packets.Connect) int {
	length := 0
	for _, buffer := range connect.Buffers() {
		length += len(buffer)
	}
	return length
}

// variableIntegerSize returns the encoded length of an MQTT variable integer.
func variableIntegerSize(data []byte) int {
	for size := 0; size < len(data) && size < 4; size++ {
		if data[size]&0x80 == 0 {
			return size + 1
		}
	}
	return len(data)
}

func ParseConnectPacket(data []byte, expectedProtocolName string, expectedProtocolLevel int) (ConnectInfo, error) {
	if expectedProtocolName == "" {
		expectedProtocolName = "MQTT"
	}
	if len(data) < 2 {
		return ConnectInfo{}, ErrNeedMoreData
	}
	if data[0] != 0x10 {
		return ConnectInfo{}, fmt.Errorf("%w: packet is not CONNECT", ErrMalformedConnect)
	}
	if data[0]&0x0f != 0 {
		return ConnectInfo{}, fmt.Errorf("%w: CONNECT flags must be zero", ErrMalformedConnect)
	}

	remaining, lengthBytes, err := readVariableInteger(data[1:])
	if err != nil {
		return ConnectInfo{}, err
	}
	packetLength := 1 + lengthBytes + remaining
	if packetLength > DefaultMaxConnectPacketSize {
		return ConnectInfo{}, fmt.Errorf(
			"%w: CONNECT packet exceeds %d bytes",
			ErrMalformedConnect,
			DefaultMaxConnectPacketSize,
		)
	}
	if len(data) < packetLength {
		return ConnectInfo{}, ErrNeedMoreData
	}
	end := packetLength
	cursor := 1 + lengthBytes

	protocolName, err := readUTF8(data, &cursor, end)
	if err != nil {
		return ConnectInfo{}, err
	}
	if protocolName != expectedProtocolName {
		return ConnectInfo{}, fmt.Errorf(
			"%w: protocol name %q, want %q",
			ErrMalformedConnect,
			protocolName,
			expectedProtocolName,
		)
	}
	level, err := readByte(data, &cursor, end)
	if err != nil {
		return ConnectInfo{}, err
	}
	if expectedProtocolLevel != 0 && int(level) != expectedProtocolLevel {
		return ConnectInfo{}, fmt.Errorf(
			"%w: protocol level %d, want %d",
			ErrMalformedConnect,
			level,
			expectedProtocolLevel,
		)
	}
	flags, err := readByte(data, &cursor, end)
	if err != nil {
		return ConnectInfo{}, err
	}
	if err := validateConnectFlags(flags); err != nil {
		return ConnectInfo{}, err
	}
	if _, err := readUint16(data, &cursor, end); err != nil {
		return ConnectInfo{}, err
	}

	clientID, err := readUTF8(data, &cursor, end)
	if err != nil {
		return ConnectInfo{}, err
	}
	if flags&0x04 != 0 {
		if _, err := readUTF8(data, &cursor, end); err != nil {
			return ConnectInfo{}, err
		}
		if err := skipBinary(data, &cursor, end); err != nil {
			return ConnectInfo{}, err
		}
	}
	if flags&0x80 != 0 {
		if _, err := readUTF8(data, &cursor, end); err != nil {
			return ConnectInfo{}, err
		}
	}
	if flags&0x40 != 0 {
		if err := skipBinary(data, &cursor, end); err != nil {
			return ConnectInfo{}, err
		}
	}
	if cursor != end {
		return ConnectInfo{}, fmt.Errorf("%w: CONNECT payload has %d trailing bytes", ErrMalformedConnect, end-cursor)
	}

	return ConnectInfo{
		ProtocolName:  protocolName,
		ProtocolLevel: level,
		ClientID:      clientID,
		PacketLength:  packetLength,
	}, nil
}

func readVariableInteger(data []byte) (int, int, error) {
	value := 0
	multiplier := 1
	for index := range 4 {
		if index >= len(data) {
			return 0, 0, ErrNeedMoreData
		}
		encoded := data[index]
		value += int(encoded&0x7f) * multiplier
		if value > 268435455 {
			return 0, 0, fmt.Errorf("%w: MQTT variable integer exceeds maximum", ErrMalformedConnect)
		}
		if encoded&0x80 == 0 {
			return value, index + 1, nil
		}
		multiplier *= 128
	}
	return 0, 0, fmt.Errorf("%w: MQTT variable integer uses more than four bytes", ErrMalformedConnect)
}

func readByte(data []byte, cursor *int, end int) (byte, error) {
	if *cursor >= end {
		return 0, ErrNeedMoreData
	}
	value := data[*cursor]
	*cursor += 1
	return value, nil
}

func readUint16(data []byte, cursor *int, end int) (uint16, error) {
	if end-*cursor < 2 {
		return 0, ErrNeedMoreData
	}
	value := uint16(data[*cursor])<<8 | uint16(data[*cursor+1])
	*cursor += 2
	return value, nil
}

func readUTF8(data []byte, cursor *int, end int) (string, error) {
	length, err := readUint16(data, cursor, end)
	if err != nil {
		return "", err
	}
	if int(length) > end-*cursor {
		return "", ErrNeedMoreData
	}
	value := data[*cursor : *cursor+int(length)]
	*cursor += int(length)
	if bytes.ContainsRune(value, 0) || !utf8.Valid(value) {
		return "", fmt.Errorf("%w: invalid UTF-8 string", ErrMalformedConnect)
	}
	return string(value), nil
}

func skipBinary(data []byte, cursor *int, end int) error {
	length, err := readUint16(data, cursor, end)
	if err != nil {
		return err
	}
	if int(length) > end-*cursor {
		return ErrNeedMoreData
	}
	*cursor += int(length)
	return nil
}

func validateConnectFlags(flags byte) error {
	if flags&0x01 != 0 {
		return fmt.Errorf("%w: CONNECT reserved flag is set", ErrMalformedConnect)
	}
	willFlag := flags&0x04 != 0
	willQoS := (flags >> 3) & 0x03
	willRetain := flags&0x20 != 0
	if !willFlag && (willQoS != 0 || willRetain) {
		return fmt.Errorf("%w: will QoS/retain set without will flag", ErrMalformedConnect)
	}
	if willQoS == 3 {
		return fmt.Errorf("%w: invalid will QoS", ErrMalformedConnect)
	}
	if flags&0x40 != 0 && flags&0x80 == 0 {
		return fmt.Errorf("%w: password flag requires username flag", ErrMalformedConnect)
	}
	return nil
}
