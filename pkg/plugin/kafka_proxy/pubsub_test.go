package kafka_proxy

import (
	"bytes"
	"testing"
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
