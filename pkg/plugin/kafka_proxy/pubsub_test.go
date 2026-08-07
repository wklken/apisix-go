package kafka_proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
)

func TestPubSubRequestRoundTrip(t *testing.T) {
	wire := []byte{
		0x08, 0x2a,
		0x8a, 0x02, 0x09,
		0x0a, 0x03, 't', 'o', 'p',
		0x10, 0x02,
		0x18, 0x07,
	}
	request, err := ParsePubSubRequest(wire)
	if err != nil {
		t.Fatalf("ParsePubSubRequest() error = %v", err)
	}
	if request.Sequence != 42 {
		t.Fatalf("sequence = %d, want 42", request.Sequence)
	}
	if request.Command != CmdKafkaFetch {
		t.Fatalf("command = %v, want %v", request.Command, CmdKafkaFetch)
	}
	if request.Topic != "top" || request.Partition != 2 || request.Position != 7 {
		t.Fatalf("request payload = %#v, want topic/top partition/2 position/7", request)
	}
	encoded, err := MarshalPubSubRequest(request)
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	if !bytes.Equal(encoded, wire) {
		t.Fatalf("encoded request = %x, want %x", encoded, wire)
	}
}

func TestPubSubResponseRoundTrip(t *testing.T) {
	response := PubSubResponse{
		Sequence: 9,
		Kind:     RespKafkaFetch,
		Messages: []KafkaMessage{{
			Offset:    17,
			Timestamp: 1234,
			Key:       []byte("key"),
			Value:     []byte("value"),
		}},
	}
	wire, err := MarshalPubSubResponse(response)
	if err != nil {
		t.Fatalf("MarshalPubSubResponse() error = %v", err)
	}
	decoded, err := ParsePubSubResponse(wire)
	if err != nil {
		t.Fatalf("ParsePubSubResponse() error = %v", err)
	}
	if decoded.Sequence != response.Sequence || decoded.Kind != response.Kind {
		t.Fatalf("response header = %#v, want %#v", decoded, response)
	}
	if len(decoded.Messages) != 1 || decoded.Messages[0].Offset != 17 ||
		decoded.Messages[0].Timestamp != 1234 || !bytes.Equal(decoded.Messages[0].Key, []byte("key")) ||
		!bytes.Equal(decoded.Messages[0].Value, []byte("value")) {
		t.Fatalf("decoded messages = %#v, want %#v", decoded.Messages, response.Messages)
	}
}

func TestParsePubSubRequestRejectsMultipleCommands(t *testing.T) {
	wire, err := MarshalPubSubRequest(PubSubRequest{Command: CmdKafkaFetch, Topic: "topic", Partition: 1})
	if err != nil {
		t.Fatalf("MarshalPubSubRequest() error = %v", err)
	}
	wire = append(wire, 0x8a, 0x02, 0x00)
	if _, err := ParsePubSubRequest(wire); err == nil {
		t.Fatal("ParsePubSubRequest() error = nil, want duplicate command rejection")
	}
}

func TestPubSubCodecRejectsUnsupportedCommand(t *testing.T) {
	if _, err := MarshalPubSubRequest(PubSubRequest{Command: PubSubCommand(99)}); err == nil {
		t.Fatal("MarshalPubSubRequest() error = nil, want unsupported command rejection")
	}
	if _, err := MarshalPubSubResponse(PubSubResponse{Kind: PubSubResponseKind(99)}); err == nil {
		t.Fatal("MarshalPubSubResponse() error = nil, want unsupported response rejection")
	}
}

type fakeKafkaConsumer struct {
	topics     []string
	partitions []int32
	positions  []int64
	listOffset int64
	listErr    error
	messages   []KafkaMessage
	fetchErr   error
}

func (f *fakeKafkaConsumer) ListOffset(_ context.Context, topic string, partition int32, timestamp int64) (int64, error) {
	f.topics = append(f.topics, topic)
	f.partitions = append(f.partitions, partition)
	f.positions = append(f.positions, timestamp)
	if f.listErr != nil {
		return 0, f.listErr
	}
	return f.listOffset, nil
}

func (f *fakeKafkaConsumer) Fetch(_ context.Context, topic string, partition int32, offset int64) ([]KafkaMessage, error) {
	f.topics = append(f.topics, topic)
	f.partitions = append(f.partitions, partition)
	f.positions = append(f.positions, offset)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.messages, nil
}

func TestDispatchPubSubRequest(t *testing.T) {
	t.Run("ping returns pong with state", func(t *testing.T) {
		response, err := dispatchPubSubRequest(context.Background(), &fakeKafkaConsumer{}, PubSubRequest{
			Sequence: 3, Command: CmdPing, State: []byte("session"),
		})
		if err != nil {
			t.Fatalf("dispatchPubSubRequest() error = %v", err)
		}
		if response.Kind != RespPong || response.Sequence != 3 || !bytes.Equal(response.State, []byte("session")) {
			t.Fatalf("response = %#v, want pong with echoed state", response)
		}
	})

	t.Run("list offset records topic partition position", func(t *testing.T) {
		consumer := &fakeKafkaConsumer{listOffset: 77}
		response, err := dispatchPubSubRequest(context.Background(), consumer, PubSubRequest{
			Sequence: 4, Command: CmdKafkaListOffset, Topic: "orders", Partition: 2, Position: -1,
		})
		if err != nil {
			t.Fatalf("dispatchPubSubRequest() error = %v", err)
		}
		if response.Kind != RespKafkaListOffset || response.Offset != 77 {
			t.Fatalf("response = %#v, want list offset 77", response)
		}
		if len(consumer.topics) != 1 || consumer.topics[0] != "orders" ||
			consumer.partitions[0] != 2 || consumer.positions[0] != -1 {
			t.Fatalf("consumer call = %v/%v/%v, want orders/2/-1", consumer.topics, consumer.partitions, consumer.positions)
		}
	})

	t.Run("fetch returns messages", func(t *testing.T) {
		consumer := &fakeKafkaConsumer{messages: []KafkaMessage{{Offset: 9, Value: []byte("v")}}}
		response, err := dispatchPubSubRequest(context.Background(), consumer, PubSubRequest{
			Command: CmdKafkaFetch, Topic: "orders", Partition: 0, Position: 5,
		})
		if err != nil {
			t.Fatalf("dispatchPubSubRequest() error = %v", err)
		}
		if response.Kind != RespKafkaFetch || len(response.Messages) != 1 || response.Messages[0].Offset != 9 {
			t.Fatalf("response = %#v, want one fetched message", response)
		}
		if consumer.positions[0] != 5 {
			t.Fatalf("fetch position = %d, want 5", consumer.positions[0])
		}
	})

	t.Run("empty command rejected", func(t *testing.T) {
		if _, err := dispatchPubSubRequest(context.Background(), &fakeKafkaConsumer{}, PubSubRequest{Command: CmdEmpty}); err == nil {
			t.Fatal("dispatchPubSubRequest() error = nil for an empty command")
		}
	})

	t.Run("unsupported command rejected", func(t *testing.T) {
		if _, err := dispatchPubSubRequest(context.Background(), &fakeKafkaConsumer{}, PubSubRequest{Command: PubSubCommand(99)}); err == nil {
			t.Fatal("dispatchPubSubRequest() error = nil for an unsupported command")
		}
	})

	t.Run("consumer error propagated", func(t *testing.T) {
		cause := errors.New("broker unavailable")
		consumer := &fakeKafkaConsumer{listErr: cause}
		_, err := dispatchPubSubRequest(context.Background(), consumer, PubSubRequest{
			Command: CmdKafkaListOffset, Topic: "orders", Partition: 0, Position: -1,
		})
		if !errors.Is(err, cause) {
			t.Fatalf("dispatchPubSubRequest() error = %v, want propagated broker error", err)
		}
	})
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "operation timed out" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestPubSubErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int32
	}{
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: 504},
		{name: "timeout net error", err: timeoutError{}, want: 504},
		{name: "ordinary error", err: errors.New("boom"), want: 502},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pubSubErrorCode(test.err); got != test.want {
				t.Fatalf("pubSubErrorCode() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestPubSubErrorMessage(t *testing.T) {
	tests := []struct {
		name    string
		command PubSubCommand
		err     error
		want    string
	}{
		{name: "auth", command: CmdKafkaFetch, err: kafka.SASLAuthenticationFailed, want: "Kafka authentication failed"},
		{name: "list offset", command: CmdKafkaListOffset, err: errors.New("boom"), want: "Kafka list offset failed"},
		{name: "fetch", command: CmdKafkaFetch, err: errors.New("boom"), want: "Kafka fetch failed"},
		{name: "default", command: CmdPing, err: errors.New("boom"), want: "Kafka PubSub command failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pubSubErrorMessage(test.command, test.err); got != test.want {
				t.Fatalf("pubSubErrorMessage() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPubSubCodecRejectsMalformedWire(t *testing.T) {
	tests := []struct {
		name  string
		data  []byte
		parse func([]byte) error
	}{
		{name: "truncated sequence varint", data: []byte{0x08, 0x80}, parse: func(data []byte) error {
			_, err := ParsePubSubRequest(data)
			return err
		}},
		{name: "sequence wrong wire type", data: []byte{0x0a, 0x01, 0x00}, parse: func(data []byte) error {
			_, err := ParsePubSubRequest(data)
			return err
		}},
		{name: "command wrong wire type", data: []byte{0x88, 0x02, 0x01}, parse: func(data []byte) error {
			_, err := ParsePubSubRequest(data)
			return err
		}},
		{name: "truncated response varint", data: []byte{0x08, 0x80}, parse: func(data []byte) error {
			_, err := ParsePubSubResponse(data)
			return err
		}},
		{name: "multiple responses", data: append(mustMarshalPubSubResponse(t, PubSubResponse{Kind: RespPong, State: []byte("x")}), 0x82, 0x02, 0x00), parse: func(data []byte) error {
			_, err := ParsePubSubResponse(data)
			return err
		}},
		{name: "invalid response kind", data: []byte{0x96, 0x02, 0x00}, parse: func(data []byte) error {
			_, err := ParsePubSubResponse(data)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.parse(test.data); err == nil {
				t.Fatalf("parse = nil, want malformed wire rejection for %x", test.data)
			}
		})
	}
}

func mustMarshalPubSubResponse(t *testing.T, response PubSubResponse) []byte {
	t.Helper()
	data, err := MarshalPubSubResponse(response)
	if err != nil {
		t.Fatalf("MarshalPubSubResponse() error = %v", err)
	}
	return data
}
