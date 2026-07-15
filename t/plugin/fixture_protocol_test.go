package pluginintegration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
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
)

type kafkaFixture struct {
	listener      net.Listener
	expect        []NetworkAssertion
	received      chan []byte
	errors        chan error
	done          chan struct{}
	closeOnce     sync.Once
	sequence      sync.Mutex
	next          int
	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	wg            sync.WaitGroup
}

func startKafkaFixture(spec FixtureSpec) (namedFixture, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen Kafka fixture: %w", err)
	}
	fixture := &kafkaFixture{
		listener:    listener,
		expect:      spec.NetworkExpect,
		received:    make(chan []byte, len(spec.NetworkExpect)+1),
		errors:      make(chan error, len(spec.NetworkExpect)+1),
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
	for {
		version, correlation, _, message, err := protocol.ReadRequest(reader)
		if err != nil {
			if err != io.EOF {
				f.errors <- fmt.Errorf("read Kafka request: %w", err)
			}
			return
		}
		response, payload, err := f.responseFor(message)
		if err != nil {
			f.errors <- err
			return
		}
		if len(payload) > 0 {
			f.received <- payload
		}
		if err := protocol.WriteResponse(connection, version, correlation, response); err != nil {
			f.errors <- fmt.Errorf("write Kafka response: %w", err)
			return
		}
		if len(f.expect) > 0 && len(f.received) >= len(f.expect) {
			return
		}
	}
}

func (f *kafkaFixture) responseFor(message protocol.Message) (protocol.Message, []byte, error) {
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
		return &saslhandshake.Response{Mechanisms: []string{"PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512"}}, nil, nil
	case *saslauthenticate.Request:
		return &saslauthenticate.Response{}, nil, nil
	case *metadata.Request:
		topic := "apisix"
		if len(request.TopicNames) > 0 && request.TopicNames[0] != "" {
			topic = request.TopicNames[0]
		}
		return &metadata.Response{
			Brokers:      []metadata.ResponseBroker{{NodeID: 0, Host: f.host(), Port: int32(mustPort(f.port()))}},
			ClusterID:    "fixture",
			ControllerID: 0,
			Topics: []metadata.ResponseTopic{{
				Name: topic,
				Partitions: []metadata.ResponsePartition{{
					PartitionIndex: 0,
					LeaderID:       0,
					ReplicaNodes:   []int32{0},
					IsrNodes:       []int32{0},
				}},
			}},
		}, nil, nil
	case *produce.Request:
		payload, err := kafkaProducePayload(request)
		if err != nil {
			return nil, nil, err
		}
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

func kafkaProducePayload(request *produce.Request) ([]byte, error) {
	var payload []byte
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
				payload = append(payload, value...)
			}
		}
	}
	return payload, nil
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
			t.Errorf("fixture %s did not receive expected payload %d", spec.Name, i+1)
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
		Topic:        "apisix",
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
