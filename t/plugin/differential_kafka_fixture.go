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
	"time"

	"github.com/segmentio/kafka-go/protocol"
	"github.com/segmentio/kafka-go/protocol/apiversions"
	"github.com/segmentio/kafka-go/protocol/fetch"
	"github.com/segmentio/kafka-go/protocol/listoffsets"
	"github.com/segmentio/kafka-go/protocol/metadata"
	"github.com/segmentio/kafka-go/protocol/produce"
)

const (
	differentialFixtureWireHTTPKafka       = "http-kafka"
	differentialKafkaMethod                = "KAFKA"
	differentialKafkaListOffsetsMethod     = "KAFKA_LIST_OFFSETS"
	differentialKafkaFetchMethod           = "KAFKA_FETCH"
	differentialOracleHostGateway          = "host.containers.internal"
	differentialKafkaPubSubTopic           = "test-consumer"
	differentialKafkaPubSubPartition       = int32(0)
	differentialKafkaPubSubRecordOffset    = int64(14)
	differentialKafkaPubSubHighWatermark   = int64(15)
	differentialKafkaPubSubRecordTimestamp = int64(1_700_000_000_123)
	differentialKafkaPubSubRecordKey       = "key14"
	differentialKafkaPubSubRecordValue     = "testmsg15"
)

type differentialKafkaRecord struct {
	method string
	topic  string
	key    string
	value  string
}

func differentialFixtureUsesHostOracle(fixture DifferentialFixture) bool {
	return fixture.WireProtocol == differentialFixtureWireHTTPKafka ||
		fixture.WireProtocol == differentialFixtureWireGRPCH2C ||
		fixture.WireProtocol == differentialFixtureWireSSEHTTP ||
		differentialHTTPDubboFixtureUsesHostOracle(fixture) ||
		differentialDubboProxyFixtureUsesHostOracle(fixture) ||
		fixture.WireProtocol == differentialFixtureWireMQTTCONNECT ||
		fixture.WireProtocol == differentialFixtureWireHTTPRocketMQ
}

func differentialOracleFixtureEndpoint(fixture DifferentialFixture, hostPort int) string {
	if differentialFixtureUsesHostOracle(fixture) {
		return net.JoinHostPort(differentialOracleHostAddress(), strconv.Itoa(hostPort))
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(differentialOracleFixturePort))
}

func differentialOracleHostAddress() string {
	hostGateway, err := differentialOracleHostGatewayAddress()
	if err == nil && hostGateway != "" {
		return hostGateway
	}
	return differentialOracleHostGateway
}

func startDifferentialHTTPKafkaFixture(
	spec DifferentialFixture,
) (*differentialFixtureServer, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic HTTP/Kafka fixture: %w", err)
	}
	fixture, err := newDifferentialRawFixture(spec, listener)
	if err != nil {
		return nil, err
	}
	fixture.serveWG.Add(1)
	go fixture.serveHTTPKafka()
	return fixture, nil
}

func (fixture *differentialFixtureServer) serveHTTPKafka() {
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
					fixture.reportError(fmt.Errorf("sniff HTTP/Kafka fixture connection: %w", peekErr))
				}
				return
			}
			if first[0] >= 'A' && first[0] <= 'Z' {
				fixture.captureHTTPRequest(reader, connection)
				return
			}
			fixture.serveKafkaConnection(reader, connection)
		})
	}
}

func (fixture *differentialFixtureServer) serveKafkaConnection(
	reader *bufio.Reader,
	connection net.Conn,
) {
	for {
		version, correlation, _, message, err := protocol.ReadRequest(reader)
		if err != nil {
			var networkErr net.Error
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
				errors.As(err, &networkErr) && networkErr.Timeout() {
				return
			}
			fixture.reportError(fmt.Errorf("read Kafka fixture request: %w", err))
			return
		}
		response, records, err := differentialKafkaResponse(
			message,
			differentialKafkaAdvertisedHost(connection.RemoteAddr(), fixture.oracleSide.Load()),
			fixture.port(),
		)
		if err != nil {
			fixture.reportError(err)
			return
		}
		for _, record := range records {
			method := record.method
			if method == "" {
				method = differentialKafkaMethod
			}
			fixture.capture(differentialCapturedRequest{
				Method: method,
				Path:   record.topic,
				Host:   record.key,
				Body:   record.value,
			})
		}
		if response == nil {
			continue
		}
		if err := writeDifferentialKafkaResponse(connection, version, correlation, response); err != nil {
			fixture.reportError(fmt.Errorf("write Kafka fixture response: %w", err))
			return
		}
	}
}

func writeDifferentialKafkaResponse(
	connection io.Writer,
	version int16,
	correlation int32,
	response protocol.Message,
) error {
	if _, isFetch := response.(*fetch.Response); !isFetch {
		return protocol.WriteResponse(connection, version, correlation, response)
	}
	var encoded bytes.Buffer
	if err := protocol.WriteResponse(&encoded, version, correlation, response); err != nil {
		return err
	}
	packet := encoded.Bytes()
	offsetPosition, err := differentialKafkaFetchRecordOffsetPosition(packet, version)
	if err != nil {
		return err
	}
	binary.BigEndian.PutUint64(packet[offsetPosition:], uint64(differentialKafkaPubSubRecordOffset))
	_, err = connection.Write(packet)
	return err
}

func differentialKafkaFetchRecordOffsetPosition(packet []byte, version int16) (int, error) {
	if version < 0 || version > 2 {
		return 0, fmt.Errorf("kafka fixture cannot patch Fetch response version %d", version)
	}
	position := 8 // frame size and correlation id
	if version >= 1 {
		position += 4 // throttle time
	}
	if len(packet) < position+6 || binary.BigEndian.Uint32(packet[position:]) != 1 {
		return 0, errors.New("kafka fixture Fetch response must contain one topic")
	}
	position += 4
	topicLength := int(binary.BigEndian.Uint16(packet[position:]))
	position += 2
	if topicLength <= 0 || len(packet) < position+topicLength+4 {
		return 0, errors.New("kafka fixture Fetch response has an invalid topic")
	}
	position += topicLength
	if binary.BigEndian.Uint32(packet[position:]) != 1 {
		return 0, errors.New("kafka fixture Fetch response must contain one partition")
	}
	position += 4 + 4 + 2 + 8 // partition, error code, high watermark
	if len(packet) < position+12 {
		return 0, errors.New("kafka fixture Fetch response has no record set")
	}
	recordSetSize := int(binary.BigEndian.Uint32(packet[position:]))
	position += 4
	if recordSetSize < 12 || len(packet) < position+recordSetSize {
		return 0, errors.New("kafka fixture Fetch response record set is truncated")
	}
	return position, nil
}

func differentialKafkaAdvertisedHost(remote net.Addr, oracleSide bool) string {
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

func differentialKafkaResponse(
	message protocol.Message,
	advertisedHost string,
	port int,
) (protocol.Message, []differentialKafkaRecord, error) {
	switch request := message.(type) {
	case *apiversions.Request:
		return &apiversions.Response{ApiKeys: []apiversions.ApiKeyResponse{
			{ApiKey: int16(protocol.Produce), MinVersion: 0, MaxVersion: 2},
			{ApiKey: int16(protocol.Fetch), MinVersion: 0, MaxVersion: 2},
			{ApiKey: int16(protocol.ListOffsets), MinVersion: 1, MaxVersion: 2},
			{ApiKey: int16(protocol.Metadata), MinVersion: 0, MaxVersion: 4},
			{ApiKey: int16(protocol.ApiVersions), MinVersion: 0, MaxVersion: 2},
		}}, nil, nil
	case *metadata.Request:
		topics := request.TopicNames
		if len(topics) == 0 {
			topics = []string{"test2"}
		}
		response := &metadata.Response{
			Brokers: []metadata.ResponseBroker{{
				NodeID: 0, Host: advertisedHost, Port: int32(port),
			}},
			ClusterID: "apisix-go-differential", ControllerID: 0,
		}
		for _, topic := range topics {
			if topic == "" {
				continue
			}
			response.Topics = append(response.Topics, metadata.ResponseTopic{
				Name: topic,
				Partitions: []metadata.ResponsePartition{{
					PartitionIndex: 0, LeaderID: 0,
					ReplicaNodes: []int32{0}, IsrNodes: []int32{0},
				}},
			})
		}
		return response, nil, nil
	case *produce.Request:
		records, err := readDifferentialKafkaRecords(request)
		if err != nil {
			return nil, nil, err
		}
		if request.Acks == 0 {
			return nil, records, nil
		}
		response := &produce.Response{}
		for _, topic := range request.Topics {
			partitions := make([]produce.ResponsePartition, 0, len(topic.Partitions))
			for _, partition := range topic.Partitions {
				partitions = append(partitions, produce.ResponsePartition{
					Partition: partition.Partition, BaseOffset: 0,
				})
			}
			response.Topics = append(response.Topics, produce.ResponseTopic{
				Topic: topic.Topic, Partitions: partitions,
			})
		}
		return response, records, nil
	case *listoffsets.Request:
		return differentialKafkaListOffsetsResponse(request)
	case *fetch.Request:
		return differentialKafkaFetchResponse(request)
	default:
		return nil, nil, fmt.Errorf("kafka fixture received unsupported request %T", message)
	}
}

func differentialKafkaListOffsetsResponse(
	request *listoffsets.Request,
) (protocol.Message, []differentialKafkaRecord, error) {
	if len(request.Topics) != 1 || request.Topics[0].Topic != differentialKafkaPubSubTopic ||
		len(request.Topics[0].Partitions) != 1 {
		return nil, nil, fmt.Errorf("kafka fixture received unexpected ListOffsets request %#v", request.Topics)
	}
	partition := request.Topics[0].Partitions[0]
	if partition.Partition != differentialKafkaPubSubPartition {
		return nil, nil, fmt.Errorf("kafka fixture received ListOffsets partition %d", partition.Partition)
	}
	offset := differentialKafkaPubSubHighWatermark
	if partition.Timestamp == -2 {
		offset = 0
	}
	return &listoffsets.Response{Topics: []listoffsets.ResponseTopic{{
			Topic: differentialKafkaPubSubTopic,
			Partitions: []listoffsets.ResponsePartition{{
				Partition: partition.Partition,
				Timestamp: -1,
				Offset:    offset,
			}},
		}}}, []differentialKafkaRecord{{
			method: differentialKafkaListOffsetsMethod,
			topic:  differentialKafkaPubSubTopic,
			key:    strconv.FormatInt(int64(partition.Partition), 10),
			value:  strconv.FormatInt(partition.Timestamp, 10),
		}}, nil
}

func differentialKafkaFetchResponse(
	request *fetch.Request,
) (protocol.Message, []differentialKafkaRecord, error) {
	if len(request.Topics) != 1 || request.Topics[0].Topic != differentialKafkaPubSubTopic ||
		len(request.Topics[0].Partitions) != 1 {
		return nil, nil, fmt.Errorf("kafka fixture received unexpected Fetch request %#v", request.Topics)
	}
	partition := request.Topics[0].Partitions[0]
	if partition.Partition != differentialKafkaPubSubPartition ||
		partition.FetchOffset != differentialKafkaPubSubRecordOffset {
		return nil, nil, fmt.Errorf(
			"kafka fixture received Fetch partition/offset %d/%d",
			partition.Partition,
			partition.FetchOffset,
		)
	}
	return &fetch.Response{Topics: []fetch.ResponseTopic{{
			Topic: differentialKafkaPubSubTopic,
			Partitions: []fetch.ResponsePartition{{
				Partition:     partition.Partition,
				HighWatermark: differentialKafkaPubSubHighWatermark,
				RecordSet: protocol.RecordSet{
					Version: 1,
					Records: protocol.NewRecordReader(protocol.Record{
						Offset: differentialKafkaPubSubRecordOffset,
						Time:   time.UnixMilli(differentialKafkaPubSubRecordTimestamp),
						Key:    protocol.NewBytes([]byte(differentialKafkaPubSubRecordKey)),
						Value:  protocol.NewBytes([]byte(differentialKafkaPubSubRecordValue)),
					}),
				},
			}},
		}}}, []differentialKafkaRecord{{
			method: differentialKafkaFetchMethod,
			topic:  differentialKafkaPubSubTopic,
			key:    strconv.FormatInt(int64(partition.Partition), 10),
			value:  strconv.FormatInt(partition.FetchOffset, 10),
		}}, nil
}

func readDifferentialKafkaRecords(
	request *produce.Request,
) ([]differentialKafkaRecord, error) {
	var records []differentialKafkaRecord
	for _, topic := range request.Topics {
		for _, partition := range topic.Partitions {
			if partition.RecordSet.Records == nil {
				continue
			}
			for {
				record, err := partition.RecordSet.Records.ReadRecord()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return nil, fmt.Errorf("read Kafka record: %w", err)
				}
				key, err := protocol.ReadAll(record.Key)
				if err != nil {
					return nil, fmt.Errorf("read Kafka record key: %w", err)
				}
				value, err := protocol.ReadAll(record.Value)
				if err != nil {
					return nil, fmt.Errorf("read Kafka record value: %w", err)
				}
				records = append(records, differentialKafkaRecord{
					topic: topic.Topic, key: string(key), value: string(value),
				})
			}
		}
	}
	return records, nil
}
