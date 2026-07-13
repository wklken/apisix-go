package kafka_proxy

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// PubSubCommand identifies the command carried by a PubSubReq oneof.
type PubSubCommand uint8

const (
	CmdEmpty PubSubCommand = iota + 1
	CmdPing
	CmdKafkaFetch
	CmdKafkaListOffset
)

// PubSubResponseKind identifies the response carried by a PubSubResp oneof.
type PubSubResponseKind uint8

const (
	RespError PubSubResponseKind = iota + 1
	RespPong
	RespKafkaFetch
	RespKafkaListOffset
)

var ErrMalformedPubSub = errors.New("malformed APISIX PubSub message")

// PubSubRequest is the APISIX PubSub request envelope. Position is the Kafka
// offset for fetch commands and the timestamp for list-offset commands.
type PubSubRequest struct {
	Sequence  int64
	Command   PubSubCommand
	Topic     string
	Partition int32
	Position  int64
	State     []byte
}

type KafkaMessage struct {
	Offset    int64
	Timestamp int64
	Key       []byte
	Value     []byte
}

type PubSubResponse struct {
	Sequence int64
	Kind     PubSubResponseKind
	Code     int32
	Message  string
	State    []byte
	Offset   int64
	Messages []KafkaMessage
}

func ParsePubSubRequest(data []byte) (PubSubRequest, error) {
	var request PubSubRequest
	var commandSeen bool
	for len(data) > 0 {
		number, typ, size := protowire.ConsumeTag(data)
		if size < 0 {
			return PubSubRequest{}, malformedWire(size)
		}
		data = data[size:]
		switch number {
		case 1:
			value, consumed, err := consumeInt64(data, typ)
			if err != nil {
				return PubSubRequest{}, fmt.Errorf("%w: sequence: %v", ErrMalformedPubSub, err)
			}
			request.Sequence = value
			data = data[consumed:]
		case 31, 32, 33, 34:
			if typ != protowire.BytesType {
				return PubSubRequest{}, fmt.Errorf("%w: command %d has wire type %d", ErrMalformedPubSub, number, typ)
			}
			payload, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return PubSubRequest{}, malformedWire(consumed)
			}
			if commandSeen {
				return PubSubRequest{}, fmt.Errorf("%w: multiple commands", ErrMalformedPubSub)
			}
			commandSeen = true
			command, err := parseRequestCommand(number, payload, &request)
			if err != nil {
				return PubSubRequest{}, err
			}
			request.Command = command
			data = data[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(number, typ, data)
			if consumed < 0 {
				return PubSubRequest{}, malformedWire(consumed)
			}
			data = data[consumed:]
		}
	}
	if !commandSeen {
		return PubSubRequest{}, fmt.Errorf("%w: missing command", ErrMalformedPubSub)
	}
	return request, nil
}

func MarshalPubSubRequest(request PubSubRequest) ([]byte, error) {
	if !validRequestCommand(request.Command) {
		return nil, fmt.Errorf("unsupported PubSub command %d", request.Command)
	}
	var data []byte
	if request.Sequence != 0 {
		data = protowire.AppendTag(data, 1, protowire.VarintType)
		data = protowire.AppendVarint(data, uint64(request.Sequence))
	}
	payload := marshalRequestCommand(request)
	data = protowire.AppendTag(data, requestFieldNumber(request.Command), protowire.BytesType)
	data = protowire.AppendBytes(data, payload)
	return data, nil
}

func ParsePubSubResponse(data []byte) (PubSubResponse, error) {
	var response PubSubResponse
	var responseSeen bool
	for len(data) > 0 {
		number, typ, size := protowire.ConsumeTag(data)
		if size < 0 {
			return PubSubResponse{}, malformedWire(size)
		}
		data = data[size:]
		switch number {
		case 1:
			value, consumed, err := consumeInt64(data, typ)
			if err != nil {
				return PubSubResponse{}, fmt.Errorf("%w: sequence: %v", ErrMalformedPubSub, err)
			}
			response.Sequence = value
			data = data[consumed:]
		case 31, 32, 33, 34:
			if typ != protowire.BytesType {
				return PubSubResponse{}, fmt.Errorf("%w: response %d has wire type %d", ErrMalformedPubSub, number, typ)
			}
			payload, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return PubSubResponse{}, malformedWire(consumed)
			}
			if responseSeen {
				return PubSubResponse{}, fmt.Errorf("%w: multiple responses", ErrMalformedPubSub)
			}
			responseSeen = true
			kind, err := parseResponse(number, payload, &response)
			if err != nil {
				return PubSubResponse{}, err
			}
			response.Kind = kind
			data = data[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(number, typ, data)
			if consumed < 0 {
				return PubSubResponse{}, malformedWire(consumed)
			}
			data = data[consumed:]
		}
	}
	if !responseSeen {
		return PubSubResponse{}, fmt.Errorf("%w: missing response", ErrMalformedPubSub)
	}
	return response, nil
}

func MarshalPubSubResponse(response PubSubResponse) ([]byte, error) {
	if !validResponseKind(response.Kind) {
		return nil, fmt.Errorf("unsupported PubSub response %d", response.Kind)
	}
	var data []byte
	if response.Sequence != 0 {
		data = protowire.AppendTag(data, 1, protowire.VarintType)
		data = protowire.AppendVarint(data, uint64(response.Sequence))
	}
	payload, err := marshalResponse(response)
	if err != nil {
		return nil, err
	}
	data = protowire.AppendTag(data, responseFieldNumber(response.Kind), protowire.BytesType)
	data = protowire.AppendBytes(data, payload)
	return data, nil
}

func parseRequestCommand(number protowire.Number, payload []byte, request *PubSubRequest) (PubSubCommand, error) {
	var command PubSubCommand
	switch number {
	case 31:
		command = CmdEmpty
	case 32:
		command = CmdPing
	case 33:
		command = CmdKafkaFetch
	case 34:
		command = CmdKafkaListOffset
	default:
		return 0, fmt.Errorf("%w: unsupported command field %d", ErrMalformedPubSub, number)
	}
	for len(payload) > 0 {
		field, typ, size := protowire.ConsumeTag(payload)
		if size < 0 {
			return 0, malformedWire(size)
		}
		payload = payload[size:]
		switch field {
		case 1:
			if command == CmdPing {
				if typ != protowire.BytesType {
					return 0, fmt.Errorf("%w: ping state has wire type %d", ErrMalformedPubSub, typ)
				}
				state, consumed := protowire.ConsumeBytes(payload)
				if consumed < 0 {
					return 0, malformedWire(consumed)
				}
				request.State = append(request.State[:0], state...)
				payload = payload[consumed:]
				continue
			}
			if typ != protowire.BytesType {
				return 0, fmt.Errorf("%w: topic has wire type %d", ErrMalformedPubSub, typ)
			}
			topic, consumed := protowire.ConsumeBytes(payload)
			if consumed < 0 {
				return 0, malformedWire(consumed)
			}
			request.Topic = string(topic)
			payload = payload[consumed:]
		case 2:
			value, consumed, err := consumeInt64(payload, typ)
			if err != nil {
				return 0, fmt.Errorf("%w: partition: %v", ErrMalformedPubSub, err)
			}
			request.Partition = int32(value)
			payload = payload[consumed:]
		case 3:
			value, consumed, err := consumeInt64(payload, typ)
			if err != nil {
				return 0, fmt.Errorf("%w: position: %v", ErrMalformedPubSub, err)
			}
			request.Position = value
			payload = payload[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(field, typ, payload)
			if consumed < 0 {
				return 0, malformedWire(consumed)
			}
			payload = payload[consumed:]
		}
	}
	return command, nil
}

func parseResponse(number protowire.Number, payload []byte, response *PubSubResponse) (PubSubResponseKind, error) {
	var kind PubSubResponseKind
	switch number {
	case 31:
		kind = RespError
	case 32:
		kind = RespPong
	case 33:
		kind = RespKafkaFetch
	case 34:
		kind = RespKafkaListOffset
	default:
		return 0, fmt.Errorf("%w: unsupported response field %d", ErrMalformedPubSub, number)
	}
	for len(payload) > 0 {
		field, typ, size := protowire.ConsumeTag(payload)
		if size < 0 {
			return 0, malformedWire(size)
		}
		payload = payload[size:]
		switch field {
		case 1:
			switch kind {
			case RespError:
				value, consumed, err := consumeInt64(payload, typ)
				if err != nil {
					return 0, fmt.Errorf("%w: error code: %v", ErrMalformedPubSub, err)
				}
				response.Code = int32(value)
				payload = payload[consumed:]
			case RespPong:
				if typ != protowire.BytesType {
					return 0, fmt.Errorf("%w: pong state has wire type %d", ErrMalformedPubSub, typ)
				}
				state, consumed := protowire.ConsumeBytes(payload)
				if consumed < 0 {
					return 0, malformedWire(consumed)
				}
				response.State = append(response.State[:0], state...)
				payload = payload[consumed:]
			case RespKafkaFetch:
				if typ != protowire.BytesType {
					return 0, fmt.Errorf("%w: Kafka message has wire type %d", ErrMalformedPubSub, typ)
				}
				message, consumed := protowire.ConsumeBytes(payload)
				if consumed < 0 {
					return 0, malformedWire(consumed)
				}
				decoded, err := parseKafkaMessage(message)
				if err != nil {
					return 0, err
				}
				response.Messages = append(response.Messages, decoded)
				payload = payload[consumed:]
			case RespKafkaListOffset:
				value, consumed, err := consumeInt64(payload, typ)
				if err != nil {
					return 0, fmt.Errorf("%w: Kafka offset: %v", ErrMalformedPubSub, err)
				}
				response.Offset = value
				payload = payload[consumed:]
			}
		case 2:
			if kind != RespError || typ != protowire.BytesType {
				return 0, fmt.Errorf("%w: response field 2 is invalid for %d", ErrMalformedPubSub, kind)
			}
			message, consumed := protowire.ConsumeBytes(payload)
			if consumed < 0 {
				return 0, malformedWire(consumed)
			}
			response.Message = string(message)
			payload = payload[consumed:]
		case 3, 4:
			if kind != RespKafkaFetch {
				consumed := protowire.ConsumeFieldValue(field, typ, payload)
				if consumed < 0 {
					return 0, malformedWire(consumed)
				}
				payload = payload[consumed:]
				continue
			}
			consumed := protowire.ConsumeFieldValue(field, typ, payload)
			if consumed < 0 {
				return 0, malformedWire(consumed)
			}
			payload = payload[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(field, typ, payload)
			if consumed < 0 {
				return 0, malformedWire(consumed)
			}
			payload = payload[consumed:]
		}
	}
	return kind, nil
}

func parseKafkaMessage(data []byte) (KafkaMessage, error) {
	var message KafkaMessage
	for len(data) > 0 {
		field, typ, size := protowire.ConsumeTag(data)
		if size < 0 {
			return KafkaMessage{}, malformedWire(size)
		}
		data = data[size:]
		switch field {
		case 1, 2:
			value, consumed, err := consumeInt64(data, typ)
			if err != nil {
				return KafkaMessage{}, fmt.Errorf("%w: Kafka message field %d: %v", ErrMalformedPubSub, field, err)
			}
			if field == 1 {
				message.Offset = value
			} else {
				message.Timestamp = value
			}
			data = data[consumed:]
		case 3, 4:
			if typ != protowire.BytesType {
				return KafkaMessage{}, fmt.Errorf(
					"%w: Kafka message field %d has wire type %d",
					ErrMalformedPubSub,
					field,
					typ,
				)
			}
			value, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return KafkaMessage{}, malformedWire(consumed)
			}
			if field == 3 {
				message.Key = append(message.Key[:0], value...)
			} else {
				message.Value = append(message.Value[:0], value...)
			}
			data = data[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(field, typ, data)
			if consumed < 0 {
				return KafkaMessage{}, malformedWire(consumed)
			}
			data = data[consumed:]
		}
	}
	return message, nil
}

func marshalRequestCommand(request PubSubRequest) []byte {
	var data []byte
	switch request.Command {
	case CmdKafkaFetch, CmdKafkaListOffset:
		if request.Topic != "" {
			data = protowire.AppendTag(data, 1, protowire.BytesType)
			data = protowire.AppendString(data, request.Topic)
		}
		if request.Partition != 0 {
			data = protowire.AppendTag(data, 2, protowire.VarintType)
			data = protowire.AppendVarint(data, uint64(int64(request.Partition)))
		}
		if request.Position != 0 {
			data = protowire.AppendTag(data, 3, protowire.VarintType)
			data = protowire.AppendVarint(data, uint64(request.Position))
		}
	case CmdPing:
		if len(request.State) > 0 {
			data = protowire.AppendTag(data, 1, protowire.BytesType)
			data = protowire.AppendBytes(data, request.State)
		}
	}
	return data
}

func marshalResponse(response PubSubResponse) ([]byte, error) {
	var data []byte
	switch response.Kind {
	case RespError:
		if response.Code != 0 {
			data = protowire.AppendTag(data, 1, protowire.VarintType)
			data = protowire.AppendVarint(data, uint64(int64(response.Code)))
		}
		if response.Message != "" {
			data = protowire.AppendTag(data, 2, protowire.BytesType)
			data = protowire.AppendString(data, response.Message)
		}
	case RespPong:
		if len(response.State) > 0 {
			data = protowire.AppendTag(data, 1, protowire.BytesType)
			data = protowire.AppendBytes(data, response.State)
		}
	case RespKafkaFetch:
		for _, message := range response.Messages {
			payload := marshalKafkaMessage(message)
			data = protowire.AppendTag(data, 1, protowire.BytesType)
			data = protowire.AppendBytes(data, payload)
		}
	case RespKafkaListOffset:
		if response.Offset != 0 {
			data = protowire.AppendTag(data, 1, protowire.VarintType)
			data = protowire.AppendVarint(data, uint64(response.Offset))
		}
	default:
		return nil, fmt.Errorf("unsupported PubSub response %d", response.Kind)
	}
	return data, nil
}

func marshalKafkaMessage(message KafkaMessage) []byte {
	var data []byte
	if message.Offset != 0 {
		data = protowire.AppendTag(data, 1, protowire.VarintType)
		data = protowire.AppendVarint(data, uint64(message.Offset))
	}
	if message.Timestamp != 0 {
		data = protowire.AppendTag(data, 2, protowire.VarintType)
		data = protowire.AppendVarint(data, uint64(message.Timestamp))
	}
	if len(message.Key) > 0 {
		data = protowire.AppendTag(data, 3, protowire.BytesType)
		data = protowire.AppendBytes(data, message.Key)
	}
	if len(message.Value) > 0 {
		data = protowire.AppendTag(data, 4, protowire.BytesType)
		data = protowire.AppendBytes(data, message.Value)
	}
	return data
}

func consumeInt64(data []byte, typ protowire.Type) (int64, int, error) {
	if typ != protowire.VarintType {
		return 0, 0, fmt.Errorf("expected varint wire type, got %d", typ)
	}
	value, consumed := protowire.ConsumeVarint(data)
	if consumed < 0 {
		return 0, 0, malformedWire(consumed)
	}
	return int64(value), consumed, nil
}

func malformedWire(code int) error {
	return fmt.Errorf("%w: wire decode error %d", ErrMalformedPubSub, code)
}

func validRequestCommand(command PubSubCommand) bool {
	return command >= CmdEmpty && command <= CmdKafkaListOffset
}

func validResponseKind(kind PubSubResponseKind) bool {
	return kind >= RespError && kind <= RespKafkaListOffset
}

func requestFieldNumber(command PubSubCommand) protowire.Number {
	return protowire.Number(30 + command)
}

func responseFieldNumber(kind PubSubResponseKind) protowire.Number {
	return protowire.Number(30 + kind)
}
