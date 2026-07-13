package mqtt_proxy

import (
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
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
	if level == 5 {
		if err := skipProperties(data, &cursor, end); err != nil {
			return ConnectInfo{}, err
		}
	}

	clientID, err := readUTF8(data, &cursor, end)
	if err != nil {
		return ConnectInfo{}, err
	}
	if flags&0x04 != 0 {
		if level == 5 {
			if err := skipProperties(data, &cursor, end); err != nil {
				return ConnectInfo{}, err
			}
		}
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

func ClientIDOrPeer(info ConnectInfo, peer string) string {
	if info.ClientID != "" {
		return info.ClientID
	}
	return peer
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
	if bytesContainsZero(value) || !utf8.Valid(value) {
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

func skipProperties(data []byte, cursor *int, end int) error {
	length, consumed, err := readVariableInteger(data[*cursor:end])
	if err != nil {
		return err
	}
	*cursor += consumed
	if length > end-*cursor {
		return ErrNeedMoreData
	}
	propertyEnd := *cursor + length
	for *cursor < propertyEnd {
		propertyID, err := readByte(data, cursor, propertyEnd)
		if err != nil {
			return err
		}
		switch propertyID {
		case 0x01, 0x17, 0x19, 0x23, 0x24, 0x28, 0x29, 0x2a:
			if _, err := readByte(data, cursor, propertyEnd); err != nil {
				return err
			}
		case 0x02, 0x11, 0x18, 0x27:
			if propertyEnd-*cursor < 4 {
				return ErrNeedMoreData
			}
			*cursor += 4
		case 0x13, 0x21, 0x22:
			if _, err := readUint16(data, cursor, propertyEnd); err != nil {
				return err
			}
		case 0x03, 0x08, 0x12, 0x15, 0x1a, 0x1c, 0x1f:
			if _, err := readUTF8(data, cursor, propertyEnd); err != nil {
				return err
			}
		case 0x09, 0x16:
			if err := skipBinary(data, cursor, propertyEnd); err != nil {
				return err
			}
		case 0x0b:
			_, consumed, err := readVariableInteger(data[*cursor:propertyEnd])
			if err != nil {
				return err
			}
			*cursor += consumed
		case 0x26:
			if _, err := readUTF8(data, cursor, propertyEnd); err != nil {
				return err
			}
			if _, err := readUTF8(data, cursor, propertyEnd); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported MQTT property 0x%02x", ErrMalformedConnect, propertyID)
		}
	}
	*cursor = propertyEnd
	return nil
}

func bytesContainsZero(data []byte) bool {
	return slices.Contains(data, 0)
}
