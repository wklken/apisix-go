package pluginintegration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
)

const (
	rocketMQGetRouteInfoCode  = int16(105)
	rocketMQSendMessageCode   = int16(10)
	rocketMQSendMessageV2Code = int16(310)
	rocketMQSendBatchCode     = int16(320)
	rocketMQResponseFlag      = int32(1)
	rocketMQTopicMissingCode  = int16(17)
)

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

type capturedRocketMQMessage struct {
	topic     string
	key       string
	tag       string
	body      string
	queueID   int
	accessKey string
}

type rocketMQFixture struct {
	listener      net.Listener
	addressValue  string
	config        *RocketMQFixtureAssertion
	received      chan capturedRocketMQMessage
	errors        chan error
	done          chan struct{}
	closeOnce     sync.Once
	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	wg            sync.WaitGroup
}

func startRocketMQFixture(spec FixtureSpec) (namedFixture, error) {
	if spec.RocketMQ == nil {
		return nil, errors.New("RocketMQ fixture requires protocol assertions")
	}
	fixture := &rocketMQFixture{
		config:      spec.RocketMQ,
		received:    make(chan capturedRocketMQMessage, spec.RocketMQ.ExpectMessages+1),
		errors:      make(chan error, spec.RocketMQ.ExpectMessages+4),
		done:        make(chan struct{}),
		connections: make(map[net.Conn]struct{}),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen RocketMQ fixture: %w", err)
	}
	fixture.addressValue = listener.Addr().String()
	if spec.RocketMQ.Unavailable {
		_ = listener.Close()
		return fixture, nil
	}
	fixture.listener = listener
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func (f *rocketMQFixture) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.reportError(fmt.Errorf("accept RocketMQ fixture connection: %w", err))
			return
		}
		f.connectionsMu.Lock()
		f.connections[connection] = struct{}{}
		f.connectionsMu.Unlock()
		f.wg.Go(func() {
			defer func() {
				f.connectionsMu.Lock()
				delete(f.connections, connection)
				f.connectionsMu.Unlock()
			}()
			f.serveConnection(connection)
		})
	}
}

func (f *rocketMQFixture) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	for {
		command, err := readRocketMQCommand(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				f.reportError(fmt.Errorf("read RocketMQ request: %w", err))
			}
			return
		}
		response, err := f.responseFor(command)
		if err != nil {
			f.reportError(err)
			return
		}
		if err := writeRocketMQCommand(connection, response); err != nil {
			f.reportError(fmt.Errorf("write RocketMQ response: %w", err))
			return
		}
	}
}

func (f *rocketMQFixture) responseFor(command *rocketMQCommand) (*rocketMQCommand, error) {
	response := &rocketMQCommand{
		Code:      0,
		Language:  "GO",
		Version:   command.Version,
		Opaque:    command.Opaque,
		Flag:      rocketMQResponseFlag,
		ExtFields: map[string]string{},
	}
	switch command.Code {
	case rocketMQGetRouteInfoCode:
		if f.config.TopicMissing {
			response.Code = rocketMQTopicMissingCode
			response.Remark = "No topic route info in name server for the topic: " + command.ExtFields["topic"]
			return response, nil
		}
		partitions := f.config.Partitions
		if partitions == 0 {
			partitions = 1
		}
		body, err := json.Marshal(map[string]any{
			"queueDatas": []any{map[string]any{
				"brokerName":     "fixture-broker",
				"readQueueNums":  partitions,
				"writeQueueNums": partitions,
				"perm":           6,
				"topicSynFlag":   0,
			}},
			"brokerDatas": []any{map[string]any{
				"cluster":    "fixture-cluster",
				"brokerName": "fixture-broker",
				"brokerAddrs": map[string]string{
					"0": f.address(),
				},
			}},
		})
		if err != nil {
			return nil, fmt.Errorf("encode RocketMQ topic route: %w", err)
		}
		response.Body = body
	case rocketMQSendMessageCode, rocketMQSendBatchCode:
		if f.config.Credentials != nil {
			if err := verifyRocketMQCredentials(command, f.config.Credentials); err != nil {
				return nil, err
			}
		}
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
			return nil, fmt.Errorf("decode RocketMQ queue id %q: %w", queueIDValue, err)
		}
		properties := command.ExtFields["properties"]
		if properties == "" {
			properties = command.ExtFields["i"]
		}
		decodedProperties := decodeRocketMQProperties(properties)
		f.received <- capturedRocketMQMessage{
			topic:     topic,
			key:       decodedProperties["KEYS"],
			tag:       decodedProperties["TAGS"],
			body:      string(command.Body),
			queueID:   queueID,
			accessKey: command.ExtFields["AccessKey"],
		}
		response.ExtFields = map[string]string{
			"queueId":     strconv.Itoa(queueID),
			"queueOffset": "0",
			"msgId":       fmt.Sprintf("fixture-%d", command.Opaque),
			"MSG_REGION":  "DefaultRegion",
			"TRACE_ON":    "false",
		}
	default:
		// Producer registration and heartbeat commands only need a successful
		// correlated response for this focused protocol fixture.
	}
	return response, nil
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

func verifyRocketMQCredentials(command *rocketMQCommand, credentials *RocketMQCredentialsAssertion) error {
	if command.ExtFields["AccessKey"] != credentials.AccessKey {
		return fmt.Errorf(
			"RocketMQ access key = %q, want %q",
			command.ExtFields["AccessKey"],
			credentials.AccessKey,
		)
	}
	signature := command.ExtFields["Signature"]
	if signature == "" {
		return errors.New("RocketMQ request has no signature")
	}
	fields := make(map[string]string, len(command.ExtFields)-1)
	keys := make([]string, 0, len(command.ExtFields)-1)
	for key, value := range command.ExtFields {
		if key == "Signature" {
			continue
		}
		fields[key] = value
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var signed bytes.Buffer
	for _, key := range keys {
		signed.WriteString(fields[key])
	}
	signed.Write(command.Body)
	mac := hmac.New(sha1.New, []byte(credentials.SecretKey))
	_, _ = mac.Write(signed.Bytes())
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(want)) {
		return errors.New("RocketMQ request signature does not match configured secret key")
	}
	return nil
}

func (f *rocketMQFixture) address() string { return f.addressValue }

func (f *rocketMQFixture) host() string {
	host, _, _ := net.SplitHostPort(f.address())
	return host
}

func (f *rocketMQFixture) port() string {
	_, port, _ := net.SplitHostPort(f.address())
	return port
}

func (f *rocketMQFixture) url() string { return "rocketmq://" + f.address() }

func (f *rocketMQFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		if f.listener != nil {
			_ = f.listener.Close()
		}
		f.connectionsMu.Lock()
		for connection := range f.connections {
			_ = connection.Close()
		}
		f.connectionsMu.Unlock()
		f.wg.Wait()
	})
}

func (f *rocketMQFixture) reportError(err error) {
	select {
	case f.errors <- err:
	default:
	}
}

func (f *rocketMQFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
	config := spec.RocketMQ
	messages := make([]capturedRocketMQMessage, 0, config.ExpectMessages)
	for len(messages) < config.ExpectMessages {
		select {
		case message := <-f.received:
			messages = append(messages, message)
		case err := <-f.errors:
			t.Fatalf("fixture %s: %v", spec.Name, err)
		case <-time.After(3 * time.Second):
			t.Fatalf(
				"fixture %s received %d RocketMQ messages, want %d",
				spec.Name,
				len(messages),
				config.ExpectMessages,
			)
		}
	}
	for index, actual := range messages {
		expectedIndex := index
		if len(config.Messages) == 1 {
			expectedIndex = 0
		}
		expected := config.Messages[expectedIndex]
		for field, valueMatcher := range map[string]struct {
			value   string
			present bool
			matcher *Matcher
		}{
			"topic": {actual.topic, actual.topic != "", &expected.Topic},
			"key":   {actual.key, actual.key != "", expected.Key},
			"tag":   {actual.tag, actual.tag != "", expected.Tag},
			"body":  {actual.body, true, &expected.Body},
		} {
			if valueMatcher.matcher != nil {
				if err := valueMatcher.matcher.match(valueMatcher.value, valueMatcher.present); err != nil {
					t.Errorf("fixture %s message %d %s: %v", spec.Name, index+1, field, err)
				}
			}
		}
		if expected.KeyAbsent && actual.key != "" {
			t.Errorf("fixture %s message %d key = %q, want absent", spec.Name, index+1, actual.key)
		}
		if expected.QueueID != nil && actual.queueID != *expected.QueueID {
			t.Errorf(
				"fixture %s message %d queue_id = %d, want %d",
				spec.Name,
				index+1,
				actual.queueID,
				*expected.QueueID,
			)
		}
	}
	if config.DistinctQueueCount > 0 {
		queues := make(map[int]struct{}, len(messages))
		for _, message := range messages {
			queues[message.queueID] = struct{}{}
		}
		if len(queues) != config.DistinctQueueCount {
			t.Errorf(
				"fixture %s distinct queues = %d, want %d",
				spec.Name,
				len(queues),
				config.DistinctQueueCount,
			)
		}
	}
	if config.ExpectMessages == 0 {
		select {
		case message := <-f.received:
			t.Errorf("fixture %s received unexpected RocketMQ message for topic %q", spec.Name, message.topic)
		case err := <-f.errors:
			t.Errorf("fixture %s: %v", spec.Name, err)
		case <-time.After(250 * time.Millisecond):
		}
	}
	select {
	case err := <-f.errors:
		t.Errorf("fixture %s: %v", spec.Name, err)
	default:
	}
	if extra := len(f.received); extra > 0 {
		t.Errorf("fixture %s received %d unexpected extra RocketMQ messages", spec.Name, extra)
	}
}

func TestRocketMQFixtureConfigurationValidatesProtocolAssertions(t *testing.T) {
	spec := FixtureSpec{
		Name: "rocketmq",
		Kind: "rocketmq",
		RocketMQ: &RocketMQFixtureAssertion{
			Partitions:     3,
			ExpectMessages: 1,
			Messages: []RocketMQMessageAssertion{{
				Topic: Matcher{Equals: new("integration")},
				Body:  Matcher{Matches: new(`"status":200`)},
			}},
		},
	}
	if err := spec.validate(); err != nil {
		t.Fatalf("validate RocketMQ fixture: %v", err)
	}
}

func TestRocketMQFixtureAcceptsRealProducerMessage(t *testing.T) {
	spec := FixtureSpec{
		Name: "rocketmq",
		Kind: "rocketmq",
		RocketMQ: &RocketMQFixtureAssertion{
			Partitions:     3,
			ExpectMessages: 1,
			Messages: []RocketMQMessageAssertion{{
				Topic: Matcher{Equals: new("integration")},
				Key:   &Matcher{Equals: new("route-key")},
				Tag:   &Matcher{Equals: new("access")},
				Body:  Matcher{Equals: new(`{"status":200}`)},
			}},
		},
	}
	fixture, err := startRocketMQFixture(spec)
	if err != nil {
		t.Fatalf("start RocketMQ fixture: %v", err)
	}
	defer fixture.close()

	client, err := rocketmq.NewProducer(
		producer.WithNameServer([]string{fixture.address()}),
		producer.WithSendMsgTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("new RocketMQ producer: %v", err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start RocketMQ producer: %v", err)
	}
	defer func() { _ = client.Shutdown() }()

	message := primitive.NewMessage("integration", []byte(`{"status":200}`))
	message.WithKeys([]string{"route-key"})
	message.WithTag("access")
	if _, err := client.SendSync(context.Background(), message); err != nil {
		t.Fatalf("send RocketMQ message: %v", err)
	}
	fixture.assert(t, spec)
}

func TestRocketMQFixtureAcceptsTLSNameServerAndBrokerConnections(t *testing.T) {
	spec := FixtureSpec{
		Name: "rocketmq-tls",
		Kind: "rocketmq",
		RocketMQ: &RocketMQFixtureAssertion{
			Partitions:     1,
			ExpectMessages: 1,
			Messages: []RocketMQMessageAssertion{{
				Topic: Matcher{Equals: new("integration-tls")},
				Body:  Matcher{Equals: new(`{"transport":"tls"}`)},
			}},
		},
	}
	certPEM, keyPEM, err := generateRedisFixtureCertificate()
	if err != nil {
		t.Fatalf("generate RocketMQ fixture certificate: %v", err)
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load RocketMQ fixture certificate: %v", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{ //nolint:gosec // test-only self-signed fixture
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listen TLS RocketMQ fixture: %v", err)
	}
	fixture := &rocketMQFixture{
		listener:     listener,
		addressValue: listener.Addr().String(),
		config:       spec.RocketMQ,
		received:     make(chan capturedRocketMQMessage, 2),
		errors:       make(chan error, 5),
		done:         make(chan struct{}),
		connections:  make(map[net.Conn]struct{}),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	defer fixture.close()

	client, err := rocketmq.NewProducer(
		producer.WithNameServer([]string{fixture.address()}),
		producer.WithSendMsgTimeout(2*time.Second),
		producer.WithTls(true),
	)
	if err != nil {
		t.Fatalf("new TLS RocketMQ producer: %v", err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start TLS RocketMQ producer: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
		reset, resetErr := rocketmq.NewProducer(
			producer.WithNameServer([]string{"127.0.0.1:1"}),
			producer.WithTls(false),
		)
		if resetErr != nil {
			t.Errorf("reset upstream RocketMQ TLS default: %v", resetErr)
			return
		}
		_ = reset.Shutdown()
	}()

	message := primitive.NewMessage("integration-tls", []byte(`{"transport":"tls"}`))
	if _, err := client.SendSync(context.Background(), message); err != nil {
		t.Fatalf("send TLS RocketMQ message: %v", err)
	}
	fixture.assert(t, spec)
}
