package pluginintegration

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	differentialMQTTProbePrefix    = "APISIX-GO-MQTT-PROBE:"
	differentialMQTTMaxPacketBytes = 64 << 10
)

type differentialMQTTCONNECTInfo struct {
	ProtocolName  string
	ProtocolLevel byte
	ConnectFlags  byte
	KeepAlive     uint16
	ClientID      string
}

func startDifferentialMQTTFixture(
	spec DifferentialFixture,
) (*differentialFixtureServer, error) {
	if spec.WireProtocol != differentialFixtureWireMQTTCONNECT {
		return nil, fmt.Errorf("MQTT fixture wire protocol = %q", spec.WireProtocol)
	}
	if spec.ExpectedCalls != 1 {
		return nil, fmt.Errorf("MQTT fixture expected_calls = %d, want 1", spec.ExpectedCalls)
	}
	if spec.Response.Status != 0 || len(spec.Response.Headers) != 0 || spec.Response.DelayMillis != 0 {
		return nil, errors.New("MQTT fixture response must contain only raw body bytes")
	}
	if spec.Response.Body == "" {
		return nil, errors.New("MQTT fixture response body is empty")
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic MQTT fixture: %w", err)
	}
	fixture, err := newDifferentialRawFixture(spec, listener)
	if err != nil {
		return nil, err
	}
	fixture.serveWG.Add(1)
	go fixture.serveDifferentialMQTT()
	return fixture, nil
}

func (fixture *differentialFixtureServer) serveDifferentialMQTT() {
	defer fixture.serveWG.Done()
	for {
		connection, err := fixture.listener.Accept()
		if err != nil {
			return
		}
		fixture.connectionWG.Go(func() {
			defer func() { _ = connection.Close() }()
			_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
			if err := fixture.handleDifferentialMQTTConnection(connection); err != nil {
				fixture.reportError(err)
			}
		})
	}
}

func (fixture *differentialFixtureServer) handleDifferentialMQTTConnection(
	connection net.Conn,
) error {
	var first [1]byte
	if _, err := io.ReadFull(connection, first[:]); err != nil {
		return fmt.Errorf("read MQTT fixture first byte: %w", err)
	}
	if first[0] != 0x10 {
		reader := bufio.NewReader(io.MultiReader(bytes.NewReader(first[:]), connection))
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read MQTT fixture probe or packet: %w", err)
		}
		want := differentialMQTTProbePrefix + fixture.probeToken + "\n"
		if line != want {
			return fmt.Errorf("MQTT fixture received unexpected packet type 0x%02x", first[0]>>4)
		}
		if _, err := io.WriteString(connection, want); err != nil {
			return fmt.Errorf("write MQTT fixture probe response: %w", err)
		}
		return nil
	}

	packet, err := readDifferentialMQTTCONNECTFrame(
		io.MultiReader(bytes.NewReader(first[:]), connection),
	)
	if err != nil {
		return fmt.Errorf("read MQTT fixture CONNECT: %w", err)
	}
	if _, err := parseDifferentialMQTTCONNECT(packet); err != nil {
		return fmt.Errorf("validate MQTT fixture CONNECT: %w", err)
	}
	fixture.capture(differentialCapturedRequest{
		Method: "MQTT", Path: "CONNECT", Body: string(packet),
	})
	if _, err := io.WriteString(connection, fixture.response.Body); err != nil {
		return fmt.Errorf("write MQTT fixture response: %w", err)
	}
	return nil
}

func probeDifferentialMQTTFixture(fixture *differentialFixtureServer) error {
	if fixture == nil || fixture.port() <= 0 || fixture.probeToken == "" {
		return errors.New("MQTT fixture probe requires a fixture port and token")
	}
	connection, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())),
		500*time.Millisecond,
	)
	if err != nil {
		return fmt.Errorf("dial MQTT fixture probe: %w", err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(500 * time.Millisecond))
	want := differentialMQTTProbePrefix + fixture.probeToken + "\n"
	if _, err := io.WriteString(connection, want); err != nil {
		return fmt.Errorf("write MQTT fixture probe: %w", err)
	}
	response := make([]byte, len(want))
	if _, err := io.ReadFull(connection, response); err != nil {
		return fmt.Errorf("read MQTT fixture probe: %w", err)
	}
	if string(response) != want {
		return errors.New("MQTT fixture probe identity mismatch")
	}
	return nil
}

func readDifferentialMQTTCONNECTFrame(reader io.Reader) ([]byte, error) {
	var fixed [1]byte
	if _, err := io.ReadFull(reader, fixed[:]); err != nil {
		return nil, err
	}
	if fixed[0] != 0x10 {
		return nil, fmt.Errorf("packet type+flags = 0x%02x, want CONNECT", fixed[0])
	}
	header := []byte{fixed[0]}
	remaining := 0
	multiplier := 1
	for range 4 {
		var encoded [1]byte
		if _, err := io.ReadFull(reader, encoded[:]); err != nil {
			return nil, err
		}
		header = append(header, encoded[0])
		remaining += int(encoded[0]&0x7f) * multiplier
		if encoded[0]&0x80 == 0 {
			if remaining+len(header) > differentialMQTTMaxPacketBytes {
				return nil, fmt.Errorf("MQTT CONNECT exceeds %d bytes", differentialMQTTMaxPacketBytes)
			}
			body := make([]byte, remaining)
			if _, err := io.ReadFull(reader, body); err != nil {
				return nil, err
			}
			return append(header, body...), nil
		}
		multiplier *= 128
	}
	return nil, errors.New("MQTT remaining length uses more than four bytes")
}

func parseDifferentialMQTTCONNECT(packet []byte) (differentialMQTTCONNECTInfo, error) {
	if len(packet) < 2 || packet[0] != 0x10 {
		return differentialMQTTCONNECTInfo{}, fmt.Errorf("MQTT packet type+flags is not CONNECT")
	}
	remaining, lengthBytes, err := parseDifferentialMQTTVariableInteger(packet[1:])
	if err != nil {
		return differentialMQTTCONNECTInfo{}, err
	}
	packetLength := 1 + lengthBytes + remaining
	if len(packet) < packetLength {
		return differentialMQTTCONNECTInfo{}, errors.New("MQTT CONNECT is truncated")
	}
	if len(packet) > packetLength {
		return differentialMQTTCONNECTInfo{}, fmt.Errorf(
			"MQTT CONNECT has %d trailing bytes", len(packet)-packetLength,
		)
	}
	body := packet[1+lengthBytes:]
	cursor := 0
	protocolName, err := readDifferentialMQTTUTF8(body, &cursor)
	if err != nil {
		return differentialMQTTCONNECTInfo{}, fmt.Errorf("MQTT protocol name: %w", err)
	}
	if protocolName != "MQTT" {
		return differentialMQTTCONNECTInfo{}, fmt.Errorf("MQTT protocol name = %q", protocolName)
	}
	if cursor+4 > len(body) {
		return differentialMQTTCONNECTInfo{}, errors.New("MQTT CONNECT variable header is truncated")
	}
	level := body[cursor]
	flags := body[cursor+1]
	keepAlive := binary.BigEndian.Uint16(body[cursor+2 : cursor+4])
	cursor += 4
	if level != 4 && level != 5 {
		return differentialMQTTCONNECTInfo{}, fmt.Errorf("MQTT protocol level = %d, want 4 or 5", level)
	}
	if flags&0x01 != 0 || flags&0x04 == 0 && flags&0x18 != 0 || flags&0x40 != 0 && flags&0x80 == 0 {
		return differentialMQTTCONNECTInfo{}, fmt.Errorf("MQTT CONNECT flags = 0x%02x", flags)
	}
	if level == 5 {
		propertiesLength, size, parseErr := parseDifferentialMQTTVariableInteger(body[cursor:])
		if parseErr != nil {
			return differentialMQTTCONNECTInfo{}, fmt.Errorf("MQTT 5 properties: %w", parseErr)
		}
		cursor += size
		if propertiesLength > len(body)-cursor {
			return differentialMQTTCONNECTInfo{}, errors.New("MQTT 5 properties are truncated")
		}
		cursor += propertiesLength
	}
	clientID, err := readDifferentialMQTTUTF8(body, &cursor)
	if err != nil {
		return differentialMQTTCONNECTInfo{}, fmt.Errorf("MQTT client ID: %w", err)
	}
	if cursor != len(body) {
		return differentialMQTTCONNECTInfo{}, fmt.Errorf(
			"MQTT CONNECT payload has %d unsupported trailing bytes", len(body)-cursor,
		)
	}
	return differentialMQTTCONNECTInfo{
		ProtocolName: protocolName, ProtocolLevel: level,
		ConnectFlags: flags, KeepAlive: keepAlive, ClientID: clientID,
	}, nil
}

func parseDifferentialMQTTVariableInteger(data []byte) (int, int, error) {
	value := 0
	multiplier := 1
	for index := range 4 {
		if index >= len(data) {
			return 0, 0, errors.New("MQTT variable integer is truncated")
		}
		encoded := data[index]
		value += int(encoded&0x7f) * multiplier
		if encoded&0x80 == 0 {
			if index > 0 && encoded == 0 {
				return 0, 0, errors.New("MQTT variable integer is not minimally encoded")
			}
			return value, index + 1, nil
		}
		multiplier *= 128
	}
	return 0, 0, errors.New("MQTT variable integer uses more than four bytes")
}

func readDifferentialMQTTUTF8(data []byte, cursor *int) (string, error) {
	if *cursor+2 > len(data) {
		return "", errors.New("length is truncated")
	}
	length := int(binary.BigEndian.Uint16(data[*cursor : *cursor+2]))
	*cursor += 2
	if length > len(data)-*cursor {
		return "", errors.New("value is truncated")
	}
	value := string(data[*cursor : *cursor+length])
	*cursor += length
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", errors.New("value is not valid MQTT UTF-8")
	}
	return value, nil
}
