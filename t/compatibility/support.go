package pluginintegration

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	rocketMQGetRouteInfoCode  = int16(105)
	rocketMQSendMessageCode   = int16(10)
	rocketMQSendMessageV2Code = int16(310)
	rocketMQSendBatchCode     = int16(320)
	rocketMQResponseFlag      = int32(1)
)

var integrationStartupMu sync.Mutex

type rocketMQExtFields map[string]string

func (fields *rocketMQExtFields) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	decoded := make(rocketMQExtFields, len(raw))
	for key, value := range raw {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("RocketMQ extField %q must not be null", key)
		}
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			decoded[key] = text
			continue
		}
		var boolean bool
		if err := json.Unmarshal(value, &boolean); err == nil {
			decoded[key] = strconv.FormatBool(boolean)
			continue
		}
		var number json.Number
		if err := json.Unmarshal(value, &number); err == nil {
			decoded[key] = number.String()
			continue
		}
		return fmt.Errorf("RocketMQ extField %q must be a JSON scalar", key)
	}
	*fields = decoded
	return nil
}

type rocketMQCommand struct {
	Code      int16             `json:"code"`
	Language  string            `json:"language"`
	Version   int16             `json:"version"`
	Opaque    int32             `json:"opaque"`
	Flag      int32             `json:"flag"`
	Remark    string            `json:"remark"`
	ExtFields rocketMQExtFields `json:"extFields"`
	Body      []byte            `json:"-"`
}

func readRocketMQCommand(reader io.Reader) (*rocketMQCommand, error) {
	var frameLength int32
	if err := binary.Read(reader, binary.BigEndian, &frameLength); err != nil {
		return nil, err
	}
	if frameLength < 4 || frameLength > 16<<20 {
		return nil, fmt.Errorf("RocketMQ frame length %d is invalid", frameLength)
	}
	frame := make([]byte, frameLength)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nil, err
	}
	headerMarker := binary.BigEndian.Uint32(frame[:4])
	codec := byte(headerMarker >> 24)
	headerLength := int(headerMarker & 0x00ffffff)
	if headerLength < 0 || headerLength > len(frame)-4 {
		return nil, fmt.Errorf("RocketMQ header length %d is invalid", headerLength)
	}
	if codec != 0 {
		return nil, fmt.Errorf("RocketMQ codec %d is not supported by the fixture", codec)
	}
	command := &rocketMQCommand{}
	if err := json.Unmarshal(frame[4:4+headerLength], command); err != nil {
		return nil, fmt.Errorf("decode RocketMQ JSON header: %w", err)
	}
	command.Body = append([]byte(nil), frame[4+headerLength:]...)
	return command, nil
}

func writeRocketMQCommand(writer io.Writer, command *rocketMQCommand) error {
	header, err := json.Marshal(command)
	if err != nil {
		return err
	}
	frameLength := 4 + len(header) + len(command.Body)
	if err := binary.Write(writer, binary.BigEndian, int32(frameLength)); err != nil {
		return err
	}
	if err := binary.Write(writer, binary.BigEndian, uint32(len(header))); err != nil {
		return err
	}
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err = writer.Write(command.Body)
	return err
}

func decodeRocketMQProperties(properties string) map[string]string {
	result := make(map[string]string)
	for item := range strings.SplitSeq(properties, "\x02") {
		key, value, ok := strings.Cut(item, "\x01")
		if ok {
			result[key] = value
		}
	}
	return result
}

func reservePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForInitialGeneration(
	address string,
	readLogs func() (string, error),
	timeout time.Duration,
) (bool, error) {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + address + "/status/ready")
		if err == nil {
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			switch {
			case readErr != nil:
				lastErr = readErr
			case closeErr != nil:
				lastErr = closeErr
			case response.StatusCode == http.StatusOK:
				return true, nil
			case response.StatusCode != http.StatusServiceUnavailable:
				lastErr = fmt.Errorf("readiness status = %d", response.StatusCode)
			}
		} else {
			lastErr = err
		}

		if readLogs != nil {
			logs, logsErr := readLogs()
			if logsErr == nil &&
				(strings.Contains(logs, "reconcile standalone config") ||
					strings.Contains(logs, "reload standalone config")) &&
				strings.Contains(logs, " failed") {
				return false, nil
			}
			if logsErr != nil {
				lastErr = logsErr
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("initial standalone generation did not settle")
	}
	return false, fmt.Errorf(
		"wait for initial standalone generation within %s: %w",
		timeout,
		lastErr,
	)
}
