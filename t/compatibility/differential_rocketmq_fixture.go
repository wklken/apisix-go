package pluginintegration

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	differentialFixtureWireHTTPRocketMQ = "http-rocketmq"
	differentialRocketMQMethod          = "ROCKETMQ"
	differentialRocketMQTagHeader       = "X-Rocketmq-Tag"
	differentialRocketMQQueueIDHeader   = "X-Rocketmq-Queue-Id"
)

func startDifferentialHTTPRocketMQFixture(
	spec DifferentialFixture,
) (*differentialFixtureServer, error) {
	if spec.WireProtocol != differentialFixtureWireHTTPRocketMQ {
		return nil, fmt.Errorf("RocketMQ fixture wire protocol = %q", spec.WireProtocol)
	}
	if spec.ExpectedCalls != 2 || !spec.CaptureAllCalls {
		return nil, errors.New("RocketMQ fixture requires exactly two captured calls with capture_all_calls")
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic HTTP/RocketMQ fixture: %w", err)
	}
	fixture, err := newDifferentialRawFixture(spec, listener)
	if err != nil {
		return nil, err
	}
	fixture.serveWG.Add(1)
	go fixture.serveDifferentialHTTPRocketMQ()
	return fixture, nil
}

func (fixture *differentialFixtureServer) serveDifferentialHTTPRocketMQ() {
	defer fixture.serveWG.Done()
	for {
		connection, err := fixture.listener.Accept()
		if err != nil {
			return
		}
		fixture.connectionWG.Go(func() {
			defer func() { _ = connection.Close() }()
			_ = connection.SetDeadline(time.Now().Add(8 * time.Second))
			reader := bufio.NewReader(connection)
			first, peekErr := reader.Peek(1)
			if peekErr != nil {
				if !errors.Is(peekErr, io.EOF) {
					fixture.reportError(fmt.Errorf("sniff HTTP/RocketMQ fixture connection: %w", peekErr))
				}
				return
			}
			if first[0] >= 'A' && first[0] <= 'Z' {
				fixture.captureHTTPRequest(reader, connection)
				return
			}
			fixture.serveDifferentialRocketMQConnection(reader, connection)
		})
	}
}

func (fixture *differentialFixtureServer) serveDifferentialRocketMQConnection(
	reader *bufio.Reader,
	connection net.Conn,
) {
	for {
		command, err := readRocketMQCommand(reader)
		if err != nil {
			var networkErr net.Error
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
				errors.As(err, &networkErr) && networkErr.Timeout() {
				return
			}
			fixture.reportError(fmt.Errorf("read differential RocketMQ request: %w", err))
			return
		}
		response, captured, err := fixture.differentialRocketMQResponse(command, connection.RemoteAddr())
		if err != nil {
			fixture.reportError(err)
			return
		}
		if captured != nil {
			fixture.capture(*captured)
		}
		if err := writeRocketMQCommand(connection, response); err != nil {
			fixture.reportError(fmt.Errorf("write differential RocketMQ response: %w", err))
			return
		}
	}
}

func (fixture *differentialFixtureServer) differentialRocketMQResponse(
	command *rocketMQCommand,
	remote net.Addr,
) (*rocketMQCommand, *differentialCapturedRequest, error) {
	response := &rocketMQCommand{
		Code: 0, Language: "GO", Version: command.Version, Opaque: command.Opaque,
		Flag: rocketMQResponseFlag, ExtFields: map[string]string{},
	}
	switch command.Code {
	case rocketMQGetRouteInfoCode:
		body, err := differentialRocketMQRouteBody(
			differentialRocketMQAdvertisedHost(remote, fixture.oracleSide.Load()),
			fixture.port(),
		)
		if err != nil {
			return nil, nil, err
		}
		response.Body = body
		return response, nil, nil
	case rocketMQSendMessageCode, rocketMQSendMessageV2Code, rocketMQSendBatchCode:
		topic := command.ExtFields["topic"]
		if topic == "" {
			topic = command.ExtFields["b"]
		}
		queueIDValue := command.ExtFields["queueId"]
		if queueIDValue == "" {
			queueIDValue = command.ExtFields["e"]
		}
		queueID, err := strconv.Atoi(queueIDValue)
		if err != nil {
			return nil, nil, fmt.Errorf("decode differential RocketMQ queue id %q: %w", queueIDValue, err)
		}
		properties := command.ExtFields["properties"]
		if properties == "" {
			properties = command.ExtFields["i"]
		}
		decoded := decodeRocketMQProperties(properties)
		headers := make(http.Header, 2)
		headers.Set(differentialRocketMQTagHeader, decoded["TAGS"])
		headers.Set(differentialRocketMQQueueIDHeader, strconv.Itoa(queueID))
		captured := &differentialCapturedRequest{
			Method:  differentialRocketMQMethod,
			Path:    topic,
			Host:    decoded["KEYS"],
			Headers: headers,
			Body:    string(command.Body),
		}
		response.ExtFields = map[string]string{
			"queueId": strconv.Itoa(queueID), "queueOffset": "0",
			"msgId":      fmt.Sprintf("differential-%d", command.Opaque),
			"MSG_REGION": "DefaultRegion", "TRACE_ON": "false",
		}
		return response, captured, nil
	default:
		// Producer registration and heartbeat commands require only a
		// successful correlated response and are deliberately not evidence.
		return response, nil, nil
	}
}

func differentialRocketMQAdvertisedHost(remote net.Addr, oracleSide bool) string {
	if oracleSide {
		return differentialOracleHostAddress()
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err == nil {
		if address := net.ParseIP(host); address != nil && !address.IsLoopback() {
			return differentialOracleHostAddress()
		}
	}
	return "127.0.0.1"
}

func differentialRocketMQRouteBody(host string, port int) ([]byte, error) {
	if host == "" || strings.TrimSpace(host) != host || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid differential RocketMQ broker endpoint %q:%d", host, port)
	}
	body, err := json.Marshal(map[string]any{
		"queueDatas": []any{map[string]any{
			"brokerName": "differential-broker", "readQueueNums": 1,
			"writeQueueNums": 1, "perm": 6, "topicSynFlag": 0,
		}},
		"brokerDatas": []any{map[string]any{
			"cluster": "differential-cluster", "brokerName": "differential-broker",
			"brokerAddrs": map[string]string{"0": net.JoinHostPort(host, strconv.Itoa(port))},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("encode differential RocketMQ route: %w", err)
	}
	return body, nil
}
