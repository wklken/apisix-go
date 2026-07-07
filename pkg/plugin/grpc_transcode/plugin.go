package grpc_transcode

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 506
	name     = "grpc-transcode"

	defaultContentType = "application/grpc"
	jsonContentType    = "application/json"
)

const schema = `
{
  "type": "object",
  "properties": {
    "proto_id": {
      "anyOf": [
        {"type": "string"},
        {"type": "integer"}
      ]
    },
    "service": {
      "type": "string"
    },
    "method": {
      "type": "string"
    },
    "deadline": {
      "type": "number",
      "default": 0
    },
    "pb_option": {
      "type": "array",
      "items": {
        "type": "string",
        "enum": [
          "enum_as_name",
          "enum_as_value",
          "int64_as_number",
          "int64_as_string",
          "int64_as_hexstring",
          "auto_default_values",
          "no_default_values",
          "use_default_values",
          "use_default_metatable",
          "enable_hooks",
          "disable_hooks"
        ]
      },
      "minItems": 1
    },
    "show_status_in_body": {
      "type": "boolean",
      "default": false
    },
    "status_detail_type": {
      "type": "string"
    }
  },
  "additionalProperties": true,
  "required": ["proto_id", "service", "method"]
}
`

var (
	errProtoNotFound  = errors.New("proto not found")
	fetchProtoContent = func(id string) (string, error) {
		protoResource, err := store.GetProto(id)
		if err != nil {
			return "", err
		}
		return protoResource.Content, nil
	}
)

type Config struct {
	ProtoID          string   `json:"proto_id"`
	Service          string   `json:"service"`
	Method           string   `json:"method"`
	Deadline         float64  `json:"deadline,omitempty"`
	PBOption         []string `json:"pb_option,omitempty"`
	ShowStatusInBody bool     `json:"show_status_in_body,omitempty"`
	StatusDetailType string   `json:"status_detail_type,omitempty"`
}

func (c *Config) UnmarshalJSON(data []byte) error {
	var raw struct {
		ProtoID          any      `json:"proto_id"`
		Service          string   `json:"service"`
		Method           string   `json:"method"`
		Deadline         float64  `json:"deadline,omitempty"`
		PBOption         []string `json:"pb_option,omitempty"`
		ShowStatusInBody bool     `json:"show_status_in_body,omitempty"`
		StatusDetailType string   `json:"status_detail_type,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	protoID, err := normalizeProtoID(raw.ProtoID)
	if err != nil {
		return err
	}
	*c = Config{
		ProtoID:          protoID,
		Service:          raw.Service,
		Method:           raw.Method,
		Deadline:         raw.Deadline,
		PBOption:         raw.PBOption,
		ShowStatusInBody: raw.ShowStatusInBody,
		StatusDetailType: raw.StatusDetailType,
	}
	return nil
}

func normalizeProtoID(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case float64:
		if math.Trunc(v) != v {
			return "", fmt.Errorf("proto_id must be string or integer")
		}
		return strconv.FormatInt(int64(v), 10), nil
	default:
		return "", fmt.Errorf("proto_id must be string or integer")
	}
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

type methodBinding struct {
	method protoreflect.MethodDescriptor
}

var grpcStatusToHTTPStatus = map[string]int{
	"1":  499,
	"2":  http.StatusInternalServerError,
	"3":  http.StatusBadRequest,
	"4":  http.StatusGatewayTimeout,
	"5":  http.StatusNotFound,
	"6":  http.StatusConflict,
	"7":  http.StatusForbidden,
	"8":  http.StatusTooManyRequests,
	"9":  http.StatusBadRequest,
	"10": http.StatusConflict,
	"11": http.StatusBadRequest,
	"12": http.StatusNotImplemented,
	"13": http.StatusInternalServerError,
	"14": http.StatusServiceUnavailable,
	"15": http.StatusInternalServerError,
	"16": http.StatusUnauthorized,
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if len(p.config.PBOption) == 0 {
		p.config.PBOption = []string{
			"enum_as_name",
			"int64_as_number",
			"auto_default_values",
			"disable_hooks",
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		binding, err := p.loadBinding()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		if err := p.transformRequest(r, binding); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)
		if err := p.transformResponse(recorder, binding); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		recorder.writeTo(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) loadBinding() (*methodBinding, error) {
	content, err := fetchProtoContent(p.config.ProtoID)
	if err != nil {
		return nil, err
	}
	descriptorSet, err := decodeDescriptorSet(content)
	if err != nil {
		return nil, err
	}
	files, err := protodesc.NewFiles(descriptorSet)
	if err != nil {
		return nil, err
	}

	desc, err := files.FindDescriptorByName(protoreflect.FullName(p.config.Service))
	if err != nil {
		if errors.Is(err, protoregistry.NotFound) {
			return nil, fmt.Errorf("undefined service: %s", p.config.Service)
		}
		return nil, err
	}
	service, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("descriptor %s is not a service", p.config.Service)
	}
	method := service.Methods().ByName(protoreflect.Name(p.config.Method))
	if method == nil {
		return nil, fmt.Errorf("undefined service method: %s/%s", p.config.Service, p.config.Method)
	}
	return &methodBinding{method: method}, nil
}

func decodeDescriptorSet(content string) (*descriptorpb.FileDescriptorSet, error) {
	content = strings.TrimSpace(content)
	raw, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return nil, fmt.Errorf("grpc-transcode only supports base64 FileDescriptorSet proto content: %w", err)
	}

	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("decode FileDescriptorSet: %w", err)
	}
	if len(set.File) == 0 {
		return nil, fmt.Errorf("empty FileDescriptorSet")
	}
	return &set, nil
}

func (p *Plugin) transformRequest(r *http.Request, binding *methodBinding) error {
	msg := dynamicpb.NewMessage(binding.method.Input())
	payload, err := p.requestPayload(r, msg.Descriptor())
	if err != nil {
		return err
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(payload, msg); err != nil {
		return fmt.Errorf("decode HTTP request as protobuf JSON: %w", err)
	}

	encoded, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode protobuf request: %w", err)
	}
	framed := frameGRPCMessage(encoded)

	r.Method = http.MethodPost
	r.URL.Path = "/" + p.config.Service + "/" + p.config.Method
	r.URL.RawQuery = ""
	r.Body = io.NopCloser(bytes.NewReader(framed))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(framed)), nil
	}
	r.ContentLength = int64(len(framed))
	r.Header.Set("Content-Type", defaultContentType)
	r.Header.Set("Content-Length", strconv.Itoa(len(framed)))
	r.Header.Set("TE", "trailers")
	if p.config.Deadline > 0 {
		r.Header.Set("grpc-timeout", fmt.Sprintf("%gm", p.config.Deadline))
	}
	return nil
}

func (p *Plugin) requestPayload(r *http.Request, desc protoreflect.MessageDescriptor) ([]byte, error) {
	if r.Body != nil && r.Body != http.NoBody && strings.Contains(r.Header.Get("Content-Type"), jsonContentType) {
		body, err := readBody(r)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(body)) > 0 {
			return body, nil
		}
	}

	values := map[string]any{}
	query := r.URL.Query()
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		raw := query.Get(string(field.JSONName()))
		if raw == "" {
			raw = query.Get(string(field.Name()))
		}
		if raw == "" {
			continue
		}
		value, err := coerceQueryValue(field, raw)
		if err != nil {
			return nil, err
		}
		values[field.JSONName()] = value
	}
	return json.Marshal(values)
}

func coerceQueryValue(field protoreflect.FieldDescriptor, raw string) (any, error) {
	if field.IsList() {
		return []string{raw}, nil
	}
	switch field.Kind() {
	case protoreflect.BoolKind:
		return strconv.ParseBool(raw)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		value, err := strconv.ParseInt(raw, 10, 32)
		return value, err
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		value, err := strconv.ParseInt(raw, 10, 64)
		return value, err
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		value, err := strconv.ParseUint(raw, 10, 32)
		return value, err
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		value, err := strconv.ParseUint(raw, 10, 64)
		return value, err
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid floating-point value for %s", field.FullName())
		}
		return value, nil
	case protoreflect.EnumKind:
		if _, err := strconv.ParseInt(raw, 10, 32); err == nil {
			return raw, nil
		}
		return raw, nil
	default:
		return raw, nil
	}
}

func (p *Plugin) transformResponse(resp *responseRecorder, binding *methodBinding) error {
	grpcStatus := resp.header.Get("Grpc-Status")
	if grpcStatus != "" && grpcStatus != "0" {
		if status, ok := grpcStatusToHTTPStatus[grpcStatus]; ok {
			resp.statusCode = status
		} else {
			resp.statusCode = 599
		}
		resp.header.Del("Content-Length")
		if !p.config.ShowStatusInBody {
			resp.body.Reset()
			return nil
		}
	}

	if resp.statusCode >= 300 && !p.config.ShowStatusInBody {
		resp.body.Reset()
		return nil
	}
	if resp.body.Len() == 0 {
		resp.header.Set("Content-Type", jsonContentType)
		resp.header.Del("Content-Length")
		return nil
	}

	payload, err := unframeGRPCMessage(resp.body.Bytes())
	if err != nil {
		return err
	}
	msg := dynamicpb.NewMessage(binding.method.Output())
	if err := proto.Unmarshal(payload, msg); err != nil {
		return fmt.Errorf("decode protobuf response: %w", err)
	}
	out, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode protobuf JSON response: %w", err)
	}
	resp.body.Reset()
	_, _ = resp.body.Write(out)
	resp.header.Set("Content-Type", jsonContentType)
	resp.header.Del("Content-Length")
	return nil
}

func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func frameGRPCMessage(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func unframeGRPCMessage(frame []byte) ([]byte, error) {
	if len(frame) < 5 {
		return nil, fmt.Errorf("invalid grpc frame: length %d", len(frame))
	}
	if frame[0] != 0 {
		return nil, fmt.Errorf("compressed grpc frames are not supported")
	}
	size := int(binary.BigEndian.Uint32(frame[1:5]))
	if size != len(frame)-5 {
		return nil, fmt.Errorf("grpc frame payload length %d does not match %d", len(frame)-5, size)
	}
	return frame[5:], nil
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		if strings.EqualFold(field, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = w.Write(r.body.Bytes())
}
