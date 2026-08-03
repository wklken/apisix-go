package pluginintegration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol"
	"github.com/segmentio/kafka-go/protocol/apiversions"
	"github.com/segmentio/kafka-go/protocol/metadata"
	"github.com/segmentio/kafka-go/protocol/produce"
	"github.com/segmentio/kafka-go/protocol/saslauthenticate"
	"github.com/segmentio/kafka-go/protocol/saslhandshake"
	"github.com/segmentio/kafka-go/sasl"
	kafkaplain "github.com/segmentio/kafka-go/sasl/plain"
	kafkascram "github.com/segmentio/kafka-go/sasl/scram"
	xdgscram "github.com/xdg-go/scram"
)

type kafkaFixture struct {
	listener      net.Listener
	expect        []NetworkAssertion
	config        *KafkaFixtureConfig
	received      chan []byte
	records       chan kafkaFixtureRecord
	errors        chan error
	done          chan struct{}
	closeOnce     sync.Once
	sequence      sync.Mutex
	next          int
	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	wg            sync.WaitGroup
}

type kafkaFixtureRecord struct {
	payload   []byte
	timestamp time.Time
	partition int32
}

func startKafkaFixture(spec FixtureSpec) (namedFixture, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen Kafka fixture: %w", err)
	}
	recordExpectCount := 0
	if spec.Kafka != nil {
		recordExpectCount = len(spec.Kafka.RecordExpect)
	}
	fixture := &kafkaFixture{
		listener:    listener,
		expect:      spec.NetworkExpect,
		config:      spec.Kafka,
		received:    make(chan []byte, len(spec.NetworkExpect)+1),
		records:     make(chan kafkaFixtureRecord, recordExpectCount+1),
		errors:      make(chan error, len(spec.NetworkExpect)+recordExpectCount+1),
		done:        make(chan struct{}),
		connections: make(map[net.Conn]struct{}),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func (f *kafkaFixture) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.errors <- fmt.Errorf("accept Kafka fixture connection: %w", err)
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

func (f *kafkaFixture) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(connection)
	session := &kafkaSASLSession{}
	for {
		version, correlation, _, message, err := protocol.ReadRequest(reader)
		if err != nil {
			if err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
				f.errors <- fmt.Errorf("read Kafka request: %w", err)
			}
			return
		}
		response, payload, err := f.responseFor(message, session)
		if err != nil {
			f.errors <- err
			return
		}
		if len(payload) > 0 && (len(f.expect) > 0 || !f.hasRecordExpectations()) {
			f.received <- payload
		}
		if err := protocol.WriteResponse(connection, version, correlation, response); err != nil {
			f.errors <- fmt.Errorf("write Kafka response: %w", err)
			return
		}
	}
}

type kafkaSASLSession struct {
	mechanism string
	scram     *xdgscram.ServerConversation
}

func (f *kafkaFixture) responseFor(
	message protocol.Message,
	session *kafkaSASLSession,
) (protocol.Message, []byte, error) {
	switch request := message.(type) {
	case *apiversions.Request:
		return &apiversions.Response{ApiKeys: []apiversions.ApiKeyResponse{
			{ApiKey: int16(protocol.Produce), MinVersion: 0, MaxVersion: 2},
			{ApiKey: int16(protocol.Metadata), MinVersion: 0, MaxVersion: 4},
			{ApiKey: int16(protocol.ApiVersions), MinVersion: 0, MaxVersion: 2},
			{ApiKey: int16(protocol.SaslHandshake), MinVersion: 0, MaxVersion: 1},
			{ApiKey: int16(protocol.SaslAuthenticate), MinVersion: 0, MaxVersion: 1},
		}}, nil, nil
	case *saslhandshake.Request:
		return f.saslHandshakeResponse(request, session)
	case *saslauthenticate.Request:
		return f.saslAuthenticateResponse(request, session)
	case *metadata.Request:
		topics := request.TopicNames
		if len(topics) == 0 {
			topics = f.metadataTopics()
		}
		responseTopics := make([]metadata.ResponseTopic, 0, len(topics))
		for _, topic := range topics {
			if topic == "" {
				continue
			}
			partitions := make([]metadata.ResponsePartition, 0, f.partitionCount())
			for partition := range f.partitionCount() {
				partitions = append(partitions, metadata.ResponsePartition{
					ErrorCode:      f.metadataErrorCode(),
					PartitionIndex: int32(partition),
					LeaderID:       0,
					ReplicaNodes:   []int32{0},
					IsrNodes:       []int32{0},
				})
			}
			responseTopics = append(responseTopics, metadata.ResponseTopic{
				ErrorCode:  f.metadataErrorCode(),
				Name:       topic,
				Partitions: partitions,
			})
		}
		return &metadata.Response{
			Brokers:      []metadata.ResponseBroker{{NodeID: 0, Host: f.host(), Port: int32(mustPort(f.port()))}},
			ClusterID:    "fixture",
			ControllerID: 0,
			Topics:       responseTopics,
		}, nil, nil
	case *produce.Request:
		records, err := kafkaProduceRecords(request)
		if err != nil {
			return nil, nil, err
		}
		if f.hasRecordExpectations() {
			for _, record := range records {
				f.records <- record
			}
		}
		payloads := make([][]byte, 0, len(records))
		for _, record := range records {
			payloads = append(payloads, record.payload)
		}
		payload := bytes.Join(payloads, nil)
		response := &produce.Response{}
		for _, topic := range request.Topics {
			partitions := make([]produce.ResponsePartition, 0, len(topic.Partitions))
			for _, partition := range topic.Partitions {
				partitions = append(partitions, produce.ResponsePartition{
					Partition:  partition.Partition,
					BaseOffset: int64(f.nextOffset()),
				})
			}
			response.Topics = append(response.Topics, produce.ResponseTopic{Topic: topic.Topic, Partitions: partitions})
		}
		return response, payload, nil
	default:
		return nil, nil, fmt.Errorf("Kafka fixture received unsupported request %T", message)
	}
}

func (f *kafkaFixture) metadataTopics() []string {
	if f.config != nil && len(f.config.Topics) > 0 {
		return append([]string(nil), f.config.Topics...)
	}
	return []string{"apisix", "integration"}
}

func (f *kafkaFixture) hasRecordExpectations() bool {
	return f.config != nil && len(f.config.RecordExpect) > 0
}

func (f *kafkaFixture) metadataErrorCode() int16 {
	if f.config == nil {
		return 0
	}
	return f.config.MetadataErrorCode
}

func (f *kafkaFixture) partitionCount() int {
	if f.config != nil && f.config.Partitions > 0 {
		return f.config.Partitions
	}
	return 1
}

func (f *kafkaFixture) saslHandshakeResponse(
	request *saslhandshake.Request,
	session *kafkaSASLSession,
) (protocol.Message, []byte, error) {
	mechanisms := []string{"PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512"}
	if f.config == nil || f.config.SASL == nil {
		session.mechanism = request.Mechanism
		return &saslhandshake.Response{Mechanisms: mechanisms}, nil, nil
	}
	if request.Mechanism != f.config.SASL.Mechanism {
		return &saslhandshake.Response{ErrorCode: 33, Mechanisms: []string{f.config.SASL.Mechanism}}, nil, nil
	}
	session.mechanism = request.Mechanism
	if request.Mechanism == "SCRAM-SHA-256" || request.Mechanism == "SCRAM-SHA-512" {
		conversation, err := newKafkaSCRAMConversation(f.config.SASL)
		if err != nil {
			return nil, nil, err
		}
		session.scram = conversation
	}
	return &saslhandshake.Response{Mechanisms: []string{request.Mechanism}}, nil, nil
}

func (f *kafkaFixture) saslAuthenticateResponse(
	request *saslauthenticate.Request,
	session *kafkaSASLSession,
) (protocol.Message, []byte, error) {
	if f.config == nil || f.config.SASL == nil {
		return &saslauthenticate.Response{}, nil, nil
	}
	switch session.mechanism {
	case "PLAIN":
		parts := bytes.Split(request.AuthBytes, []byte{0})
		if len(parts) != 3 ||
			string(parts[1]) != f.config.SASL.Username ||
			string(parts[2]) != f.config.SASL.Password {
			return kafkaSASLFailure(), nil, nil
		}
		return &saslauthenticate.Response{}, nil, nil
	case "SCRAM-SHA-256", "SCRAM-SHA-512":
		if session.scram == nil {
			return kafkaSASLFailure(), nil, nil
		}
		response, err := session.scram.Step(string(request.AuthBytes))
		if err != nil {
			return kafkaSASLFailure(), nil, nil
		}
		return &saslauthenticate.Response{AuthBytes: []byte(response)}, nil, nil
	default:
		return kafkaSASLFailure(), nil, nil
	}
}

func kafkaSASLFailure() *saslauthenticate.Response {
	return &saslauthenticate.Response{
		ErrorCode:    58,
		ErrorMessage: "SASL authentication failed",
	}
}

func newKafkaSCRAMConversation(config *KafkaSASLFixtureConfig) (*xdgscram.ServerConversation, error) {
	hash := xdgscram.SHA256
	if config.Mechanism == "SCRAM-SHA-512" {
		hash = xdgscram.SHA512
	}
	client, err := hash.NewClient(config.Username, config.Password, "")
	if err != nil {
		return nil, err
	}
	credentials := client.GetStoredCredentials(xdgscram.KeyFactors{
		Salt:  "apisix-go-kafka-fixture",
		Iters: 4096,
	})
	server, err := hash.NewServer(func(username string) (xdgscram.StoredCredentials, error) {
		if username != config.Username {
			return xdgscram.StoredCredentials{}, fmt.Errorf("unknown SCRAM username %q", username)
		}
		return credentials, nil
	})
	if err != nil {
		return nil, err
	}
	return server.NewConversation(), nil
}

func kafkaProduceRecords(request *produce.Request) ([]kafkaFixtureRecord, error) {
	var records []kafkaFixtureRecord
	for _, topic := range request.Topics {
		for _, partition := range topic.Partitions {
			if partition.RecordSet.Records == nil {
				continue
			}
			for {
				record, err := partition.RecordSet.Records.ReadRecord()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, fmt.Errorf("read Kafka record: %w", err)
				}
				value, err := protocol.ReadAll(record.Value)
				if err != nil {
					return nil, fmt.Errorf("read Kafka record value: %w", err)
				}
				records = append(records, kafkaFixtureRecord{
					payload:   value,
					timestamp: record.Time,
					partition: partition.Partition,
				})
			}
		}
	}
	return records, nil
}

func (f *kafkaFixture) nextOffset() int {
	f.sequence.Lock()
	defer f.sequence.Unlock()
	index := f.next
	f.next++
	return index
}

func (f *kafkaFixture) address() string { return f.listener.Addr().String() }

func (f *kafkaFixture) host() string {
	host, _, _ := net.SplitHostPort(f.address())
	return host
}

func (f *kafkaFixture) port() string {
	_, port, _ := net.SplitHostPort(f.address())
	return port
}

func (f *kafkaFixture) url() string { return "kafka://" + f.address() }

func (f *kafkaFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.listener.Close()
		f.connectionsMu.Lock()
		for connection := range f.connections {
			_ = connection.Close()
		}
		f.connectionsMu.Unlock()
		f.wg.Wait()
	})
}

func (f *kafkaFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
	for i, expected := range spec.NetworkExpect {
		select {
		case payload := <-f.received:
			if err := matchNetworkAssertion(expected, payload); err != nil {
				t.Errorf("fixture %s payload %d: %v", spec.Name, i+1, err)
			}
		case <-time.After(3 * time.Second):
			select {
			case err := <-f.errors:
				t.Errorf("fixture %s: %v", spec.Name, err)
			default:
			}
			t.Errorf("fixture %s did not receive expected payload %d", spec.Name, i+1)
		}
	}
	if spec.Kafka != nil {
		observedPartitions := make(map[int32]struct{}, len(spec.Kafka.RecordExpect))
		for i, expected := range spec.Kafka.RecordExpect {
			select {
			case record := <-f.records:
				if err := matchNetworkAssertion(expected.NetworkAssertion, record.payload); err != nil {
					t.Errorf("fixture %s record %d: %v", spec.Name, i+1, err)
				}
				if expected.TimestampPositive && record.timestamp.UnixMilli() <= 0 {
					t.Errorf(
						"fixture %s record %d timestamp = %s, want positive",
						spec.Name,
						i+1,
						record.timestamp,
					)
				}
				if expected.Partition != nil && record.partition != int32(*expected.Partition) {
					t.Errorf(
						"fixture %s record %d partition = %d, want %d",
						spec.Name,
						i+1,
						record.partition,
						*expected.Partition,
					)
				}
				observedPartitions[record.partition] = struct{}{}
			case <-time.After(3 * time.Second):
				t.Errorf("fixture %s did not receive expected record %d", spec.Name, i+1)
			}
		}
		if extra := len(f.records); extra > 0 {
			t.Errorf("fixture %s received %d unexpected extra records", spec.Name, extra)
		}
		if want := spec.Kafka.DistinctPartitionCount; want > 0 && len(observedPartitions) != want {
			t.Errorf(
				"fixture %s distinct record partitions = %d, want %d",
				spec.Name,
				len(observedPartitions),
				want,
			)
		}
	}
	select {
	case err := <-f.errors:
		t.Errorf("fixture %s: %v", spec.Name, err)
	default:
	}
	if extra := len(f.received); extra > 0 {
		t.Errorf("fixture %s received %d unexpected extra payloads", spec.Name, extra)
	}
}

type dubboFixture struct {
	listener  net.Listener
	expect    []NetworkAssertion
	respond   []NetworkResponse
	received  chan []byte
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
	sequence  sync.Mutex
	next      int
	wg        sync.WaitGroup
}

func startDubboFixture(spec FixtureSpec) (namedFixture, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen Dubbo fixture: %w", err)
	}
	fixture := &dubboFixture{
		listener: listener,
		expect:   spec.NetworkExpect,
		respond:  spec.NetworkRespond,
		received: make(chan []byte, len(spec.NetworkExpect)+1),
		errors:   make(chan error, len(spec.NetworkExpect)+1),
		done:     make(chan struct{}),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func (f *dubboFixture) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.errors <- fmt.Errorf("accept Dubbo fixture connection: %w", err)
			return
		}
		f.wg.Go(func() {
			f.serveConnection(connection)
		})
	}
}

func (f *dubboFixture) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	for {
		frame, err := readDubboFrame(connection)
		if err != nil {
			if err != io.EOF {
				f.errors <- fmt.Errorf("read Dubbo frame: %w", err)
			}
			return
		}
		index := f.nextResponse()
		if index >= len(f.respond) {
			f.errors <- fmt.Errorf("Dubbo fixture received more than %d frames", len(f.expect))
			return
		}
		f.received <- frame
		response, err := networkResponseBytes(f.respond[index])
		if err != nil {
			f.errors <- fmt.Errorf("decode Dubbo response %d: %w", index+1, err)
			return
		}
		if f.respond[index].Delay > 0 {
			time.Sleep(f.respond[index].Delay)
		}
		if _, err := connection.Write(response); err != nil {
			f.errors <- fmt.Errorf("write Dubbo response %d: %w", index+1, err)
			return
		}
		if f.respond[index].Close || index == len(f.respond)-1 {
			return
		}
	}
}

func readDubboFrame(reader io.Reader) ([]byte, error) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	if header[0] != 0xda || header[1] != 0xbb {
		return nil, fmt.Errorf("unexpected Dubbo magic %x%02x", header[0], header[1])
	}
	payloadLength := int(binary.BigEndian.Uint32(header[12:16]))
	if payloadLength < 0 || payloadLength > 8<<20 {
		return nil, fmt.Errorf("Dubbo payload length %d is invalid", payloadLength)
	}
	frame := make([]byte, 16+payloadLength)
	copy(frame, header)
	if _, err := io.ReadFull(reader, frame[16:]); err != nil {
		return nil, err
	}
	return frame, nil
}

func (f *dubboFixture) nextResponse() int {
	f.sequence.Lock()
	defer f.sequence.Unlock()
	index := f.next
	f.next++
	return index
}

func (f *dubboFixture) address() string { return f.listener.Addr().String() }
func (f *dubboFixture) host() string {
	host, _, _ := net.SplitHostPort(f.address())
	return host
}

func (f *dubboFixture) port() string {
	_, port, _ := net.SplitHostPort(f.address())
	return port
}
func (f *dubboFixture) url() string { return "dubbo://" + f.address() }
func (f *dubboFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.listener.Close()
		f.wg.Wait()
	})
}

func (f *dubboFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
	for i, expected := range spec.NetworkExpect {
		select {
		case payload := <-f.received:
			if err := matchNetworkAssertion(expected, payload); err != nil {
				t.Errorf("fixture %s frame %d: %v", spec.Name, i+1, err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("fixture %s did not receive expected frame %d", spec.Name, i+1)
		}
	}
	select {
	case err := <-f.errors:
		t.Errorf("fixture %s: %v", spec.Name, err)
	default:
	}
}

func TestKafkaFixtureAcceptsProduceMessage(t *testing.T) {
	spec := FixtureSpec{
		Name: "kafka",
		Kind: "kafka",
		NetworkExpect: []NetworkAssertion{{
			Payload: &Matcher{Equals: new("log-entry")},
		}},
		NetworkRespond: []NetworkResponse{{Payload: "ignored"}},
	}
	fixture, err := startKafkaFixture(spec)
	if err != nil {
		t.Fatalf("start Kafka fixture: %v", err)
	}
	defer fixture.close()
	writer := &kafka.Writer{
		Addr:         kafka.TCP(fixture.address()),
		Topic:        "integration",
		BatchSize:    1,
		RequiredAcks: 1,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	}
	if err := writer.WriteMessages(context.Background(), kafka.Message{Value: []byte("log-entry")}); err != nil {
		t.Fatalf("write Kafka message: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close Kafka writer: %v", err)
	}
	fixture.assert(t, spec)
}

func TestKafkaFixtureRecordAssertionsIgnoreProduceBatchBoundaries(t *testing.T) {
	for _, test := range []struct {
		name    string
		batches [][]string
	}{
		{name: "single batch", batches: [][]string{{"record-one", "record-two", "record-three"}}},
		{name: "two batches", batches: [][]string{{"record-one", "record-two"}, {"record-three"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := FixtureSpec{
				Name: "kafka",
				Kind: "kafka",
				Kafka: &KafkaFixtureConfig{RecordExpect: []KafkaRecordAssertion{
					{NetworkAssertion: NetworkAssertion{Payload: &Matcher{Equals: new("record-one")}}},
					{NetworkAssertion: NetworkAssertion{Payload: &Matcher{Equals: new("record-two")}}},
					{
						NetworkAssertion:  NetworkAssertion{Payload: &Matcher{Equals: new("record-three")}},
						TimestampPositive: true,
					},
				}},
			}
			fixture, err := startKafkaFixture(spec)
			if err != nil {
				t.Fatalf("start Kafka fixture: %v", err)
			}
			defer fixture.close()

			for _, batch := range test.batches {
				writer := &kafka.Writer{
					Addr:         kafka.TCP(fixture.address()),
					Topic:        "integration",
					BatchSize:    len(batch),
					RequiredAcks: 1,
					ReadTimeout:  2 * time.Second,
					WriteTimeout: 2 * time.Second,
				}
				messages := make([]kafka.Message, 0, len(batch))
				for _, record := range batch {
					messages = append(messages, kafka.Message{Value: []byte(record)})
				}
				if err := writer.WriteMessages(context.Background(), messages...); err != nil {
					t.Fatalf("write Kafka messages: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("close Kafka writer: %v", err)
				}
			}

			fixture.assert(t, spec)
		})
	}
}

func TestKafkaFixtureUsesConfiguredMetadataTopics(t *testing.T) {
	spec := FixtureSpec{
		Name:  "kafka",
		Kind:  "kafka",
		Kafka: &KafkaFixtureConfig{Topics: []string{"custom-topic"}},
		NetworkExpect: []NetworkAssertion{{
			Payload: &Matcher{Equals: new("custom-record")},
		}},
		NetworkRespond: []NetworkResponse{{}},
	}
	fixture, err := startKafkaFixture(spec)
	if err != nil {
		t.Fatalf("start Kafka fixture: %v", err)
	}
	defer fixture.close()
	writer := &kafka.Writer{
		Addr:         kafka.TCP(fixture.address()),
		Topic:        "custom-topic",
		BatchSize:    1,
		RequiredAcks: 1,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	}
	if err := writer.WriteMessages(context.Background(), kafka.Message{Value: []byte("custom-record")}); err != nil {
		t.Fatalf("write Kafka message: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close Kafka writer: %v", err)
	}
	fixture.assert(t, spec)
}

func TestKafkaFixtureReturnsConfiguredMetadataError(t *testing.T) {
	spec := FixtureSpec{
		Name: "kafka",
		Kind: "kafka",
		Kafka: &KafkaFixtureConfig{
			Topics:            []string{"integration"},
			MetadataErrorCode: 3,
		},
	}
	fixture, err := startKafkaFixture(spec)
	if err != nil {
		t.Fatalf("start Kafka fixture: %v", err)
	}
	defer fixture.close()
	writer := &kafka.Writer{
		Addr:         kafka.TCP(fixture.address()),
		Topic:        "integration",
		BatchSize:    1,
		RequiredAcks: 1,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
	}
	err = writer.WriteMessages(context.Background(), kafka.Message{Value: []byte("must-fail")})
	_ = writer.Close()
	if err == nil || !strings.Contains(err.Error(), "Unknown Topic Or Partition") {
		t.Fatalf("write Kafka message error = %v, want Unknown Topic Or Partition", err)
	}
}

func TestKafkaFixtureValidatesSASLCredentials(t *testing.T) {
	tests := []struct {
		name      string
		mechanism string
		client    func(password string) (sasl.Mechanism, error)
	}{
		{
			name:      "PLAIN",
			mechanism: "PLAIN",
			client: func(password string) (sasl.Mechanism, error) {
				return kafkaplain.Mechanism{Username: "admin", Password: password}, nil
			},
		},
		{
			name:      "SCRAM-SHA-256",
			mechanism: "SCRAM-SHA-256",
			client: func(password string) (sasl.Mechanism, error) {
				return kafkascram.Mechanism(kafkascram.SHA256, "admin", password)
			},
		},
		{
			name:      "SCRAM-SHA-512",
			mechanism: "SCRAM-SHA-512",
			client: func(password string) (sasl.Mechanism, error) {
				return kafkascram.Mechanism(kafkascram.SHA512, "admin", password)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, credential := range []struct {
				name      string
				password  string
				wantError bool
			}{
				{name: "correct", password: "secret"},
				{name: "incorrect", password: "wrong", wantError: true},
			} {
				t.Run(credential.name, func(t *testing.T) {
					spec := FixtureSpec{
						Name: "kafka",
						Kind: "kafka",
						Kafka: &KafkaFixtureConfig{
							Topics: []string{"integration"},
							SASL: &KafkaSASLFixtureConfig{
								Mechanism: test.mechanism,
								Username:  "admin",
								Password:  "secret",
							},
						},
					}
					if !credential.wantError {
						spec.NetworkExpect = []NetworkAssertion{{Payload: &Matcher{Equals: new("authenticated")}}}
						spec.NetworkRespond = []NetworkResponse{{}}
					}
					fixture, err := startKafkaFixture(spec)
					if err != nil {
						t.Fatalf("start Kafka fixture: %v", err)
					}
					defer fixture.close()
					mechanism, err := test.client(credential.password)
					if err != nil {
						t.Fatalf("create client mechanism: %v", err)
					}
					writer := &kafka.Writer{
						Addr:         kafka.TCP(fixture.address()),
						Topic:        "integration",
						BatchSize:    1,
						RequiredAcks: 1,
						ReadTimeout:  2 * time.Second,
						WriteTimeout: 2 * time.Second,
						Transport:    &kafka.Transport{SASL: mechanism},
					}
					err = writer.WriteMessages(context.Background(), kafka.Message{Value: []byte("authenticated")})
					_ = writer.Close()
					if credential.wantError {
						if err == nil {
							t.Fatal("write Kafka message with incorrect credential = nil, want error")
						}
						return
					}
					if err != nil {
						t.Fatalf("write Kafka message: %v", err)
					}
					fixture.assert(t, spec)
				})
			}
		})
	}
}

func TestDubboFixtureReturnsResponseFrame(t *testing.T) {
	request := dubboTestFrame("request")
	response := dubboTestFrame("1\nresponse\n")
	spec := FixtureSpec{
		Name: "dubbo",
		Kind: "dubbo",
		NetworkExpect: []NetworkAssertion{{
			PayloadBase64: &Matcher{Equals: new(base64.StdEncoding.EncodeToString(request))},
		}},
		NetworkRespond: []NetworkResponse{{
			PayloadBase64: base64.StdEncoding.EncodeToString(response),
		}},
	}
	fixture, err := startDubboFixture(spec)
	if err != nil {
		t.Fatalf("start Dubbo fixture: %v", err)
	}
	defer fixture.close()
	connection, err := net.Dial("tcp", fixture.address())
	if err != nil {
		t.Fatalf("dial Dubbo fixture: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.Write(request); err != nil {
		t.Fatalf("write Dubbo request: %v", err)
	}
	got, err := readDubboFrame(connection)
	if err != nil {
		t.Fatalf("read Dubbo response: %v", err)
	}
	if !bytes.Equal(got, response) {
		t.Fatalf("Dubbo response = %q, want %q", got, response)
	}
	fixture.assert(t, spec)
}

func dubboTestFrame(payload string) []byte {
	frame := make([]byte, 16+len(payload))
	frame[0], frame[1], frame[3] = 0xda, 0xbb, 20
	binary.BigEndian.PutUint64(frame[4:12], 1)
	binary.BigEndian.PutUint32(frame[12:16], uint32(len(payload)))
	copy(frame[16:], payload)
	return frame
}

func mustPort(port string) int {
	value, _ := strconv.Atoi(port)
	return value
}
