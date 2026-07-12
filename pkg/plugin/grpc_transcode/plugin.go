package grpc_transcode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bufbuild/protocompile"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
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

	bindingMu      sync.Mutex
	bindingContent string
	binding        *methodBinding
	bindingErr     error
	bindingLoaded  bool
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
	files  *protoregistry.Files
}

type protoJSONResolver interface {
	protoregistry.ExtensionTypeResolver
	protoregistry.MessageTypeResolver
}

type int64JSONMode uint8

const (
	int64AsNumber int64JSONMode = iota
	int64AsString
	int64AsHexString
)

type grpcStatusResolver struct {
	dynamic *dynamicpb.Types
}

func (r grpcStatusResolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if r.dynamic != nil {
		if message, err := r.dynamic.FindMessageByName(name); err == nil {
			return message, nil
		}
	}
	return protoregistry.GlobalTypes.FindMessageByName(name)
}

func (r grpcStatusResolver) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	if r.dynamic != nil {
		if message, err := r.dynamic.FindMessageByURL(url); err == nil {
			return message, nil
		}
	}
	return protoregistry.GlobalTypes.FindMessageByURL(url)
}

func (r grpcStatusResolver) FindExtensionByName(name protoreflect.FullName) (protoreflect.ExtensionType, error) {
	if r.dynamic != nil {
		if extension, err := r.dynamic.FindExtensionByName(name); err == nil {
			return extension, nil
		}
	}
	return protoregistry.GlobalTypes.FindExtensionByName(name)
}

func (r grpcStatusResolver) FindExtensionByNumber(
	message protoreflect.FullName,
	number protoreflect.FieldNumber,
) (protoreflect.ExtensionType, error) {
	if r.dynamic != nil {
		if extension, err := r.dynamic.FindExtensionByNumber(message, number); err == nil {
			return extension, nil
		}
	}
	return protoregistry.GlobalTypes.FindExtensionByNumber(message, number)
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

func (p *Plugin) Config() any {
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

	p.bindingMu.Lock()
	defer p.bindingMu.Unlock()
	if p.bindingLoaded && p.bindingContent == content {
		return p.binding, p.bindingErr
	}

	binding, err := loadBinding(content, p.config.ProtoID, p.config.Service, p.config.Method)
	p.bindingContent = content
	p.binding = binding
	p.bindingErr = err
	p.bindingLoaded = true
	return binding, err
}

func loadBinding(content string, rootName string, serviceName string, methodName string) (*methodBinding, error) {
	descriptorSet, err := decodeDescriptorSet(content, rootName)
	if err != nil {
		return nil, err
	}
	files, err := protodesc.NewFiles(descriptorSet)
	if err != nil {
		return nil, err
	}

	desc, err := files.FindDescriptorByName(protoreflect.FullName(serviceName))
	if err != nil {
		if errors.Is(err, protoregistry.NotFound) {
			return nil, fmt.Errorf("undefined service: %s", serviceName)
		}
		return nil, err
	}
	service, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("descriptor %s is not a service", serviceName)
	}
	method := service.Methods().ByName(protoreflect.Name(methodName))
	if method == nil {
		return nil, fmt.Errorf("undefined service method: %s/%s", serviceName, methodName)
	}
	if method.IsStreamingClient() || method.IsStreamingServer() {
		return nil, fmt.Errorf("grpc-transcode streaming methods are unsupported")
	}
	return &methodBinding{method: method, files: files}, nil
}

func decodeDescriptorSet(content string, rootName string) (*descriptorpb.FileDescriptorSet, error) {
	content = strings.TrimSpace(content)
	raw, err := base64.StdEncoding.DecodeString(content)
	if err == nil {
		var set descriptorpb.FileDescriptorSet
		if unmarshalErr := proto.Unmarshal(raw, &set); unmarshalErr == nil && len(set.File) > 0 {
			return &set, nil
		} else if unmarshalErr != nil {
			err = fmt.Errorf("decode FileDescriptorSet: %w", unmarshalErr)
		} else {
			err = fmt.Errorf("empty FileDescriptorSet")
		}
	}

	compiled, sourceErr := compileProtoSource(content, rootName)
	if sourceErr == nil {
		return compiled, nil
	}
	return nil, fmt.Errorf(
		"grpc-transcode only supports base64 FileDescriptorSet proto content or valid .proto source: %v; source compile: %w",
		err,
		sourceErr,
	)
}

func compileProtoSource(content string, rootName string) (*descriptorpb.FileDescriptorSet, error) {
	if strings.TrimSpace(rootName) == "" {
		rootName = "root.proto"
	}
	resolver := &protocompile.SourceResolver{
		Accessor: func(path string) (io.ReadCloser, error) {
			if path == rootName {
				return io.NopCloser(strings.NewReader(content)), nil
			}
			imported, err := fetchProtoContent(path)
			if err != nil {
				return nil, fmt.Errorf("resolve imported proto %q: %w", path, err)
			}
			return io.NopCloser(strings.NewReader(imported)), nil
		},
	}
	compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(resolver)}
	files, err := compiler.Compile(context.Background(), rootName)
	if err != nil {
		return nil, err
	}
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	for _, file := range files {
		appendDescriptorFile(file, seen, &set.File)
	}
	if len(set.File) == 0 {
		return nil, fmt.Errorf("compiled .proto source produced no descriptors")
	}
	return set, nil
}

func appendDescriptorFile(
	file protoreflect.FileDescriptor,
	seen map[string]bool,
	dest *[]*descriptorpb.FileDescriptorProto,
) {
	if file == nil || seen[file.Path()] {
		return
	}
	seen[file.Path()] = true
	for index := 0; index < file.Imports().Len(); index++ {
		appendDescriptorFile(file.Imports().Get(index), seen, dest)
	}
	*dest = append(*dest, protodesc.ToFileDescriptorProto(file))
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
			return normalizeHashInt64JSON(body, desc), nil
		}
	}

	values, err := queryMessageValues(desc, r.URL.Query(), "")
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return normalizeHashInt64JSON(payload, desc), nil
}

func queryMessageValues(
	desc protoreflect.MessageDescriptor,
	query map[string][]string,
	prefix string,
) (map[string]any, error) {
	values := make(map[string]any)
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		jsonKey := prefix + string(field.JSONName())
		protoKey := prefix + string(field.Name())
		rawValues, ok := query[jsonKey]
		matchedKey := jsonKey
		if !ok {
			rawValues, ok = query[protoKey]
			matchedKey = protoKey
		}
		if field.IsMap() {
			if ok {
				return nil, fmt.Errorf("map query field %s requires dotted keys", field.FullName())
			}
			mapValues, err := queryMapValues(field, query, jsonKey+".")
			if err != nil {
				return nil, err
			}
			if len(mapValues) == 0 && jsonKey != protoKey {
				mapValues, err = queryMapValues(field, query, protoKey+".")
				if err != nil {
					return nil, err
				}
			}
			if len(mapValues) > 0 {
				values[field.JSONName()] = mapValues
			}
			continue
		}
		if field.IsList() && (field.Kind() == protoreflect.MessageKind || field.Kind() == protoreflect.GroupKind) {
			messageValues, found, err := queryRepeatedMessageValues(field, query, jsonKey, protoKey)
			if err != nil {
				return nil, err
			}
			if found {
				values[field.JSONName()] = messageValues
			}
			continue
		}
		if ok {
			if field.Kind() == protoreflect.MessageKind || field.Kind() == protoreflect.GroupKind {
				return nil, fmt.Errorf("message query field %s requires dotted fields", field.FullName())
			}
			value, err := coerceQueryValues(field, rawValues)
			if err != nil {
				return nil, err
			}
			values[field.JSONName()] = value
			continue
		}

		if field.IsList() || field.IsMap() ||
			(field.Kind() != protoreflect.MessageKind && field.Kind() != protoreflect.GroupKind) {
			continue
		}
		nested, err := queryMessageValues(field.Message(), query, matchedKey+".")
		if err != nil {
			return nil, err
		}
		if len(nested) == 0 && jsonKey != protoKey {
			nested, err = queryMessageValues(field.Message(), query, protoKey+".")
			if err != nil {
				return nil, err
			}
		}
		if len(nested) > 0 {
			values[field.JSONName()] = nested
		}
	}
	return values, nil
}

func queryRepeatedMessageValues(
	field protoreflect.FieldDescriptor,
	query map[string][]string,
	jsonKey string,
	protoKey string,
) ([]any, bool, error) {
	keys := []string{jsonKey}
	if protoKey != jsonKey {
		keys = append(keys, protoKey)
	}
	for _, key := range keys {
		if rawValues, ok := query[key]; ok {
			values, err := parseRepeatedMessageJSONValues(rawValues)
			if err != nil {
				return nil, true, fmt.Errorf("invalid repeated message query field %s: %w", field.FullName(), err)
			}
			return values, true, nil
		}
	}

	indexed, found, err := indexedRepeatedMessageQueries(query, keys)
	if err != nil {
		return nil, found, fmt.Errorf("invalid repeated message query field %s: %w", field.FullName(), err)
	}
	if !found {
		return nil, false, nil
	}

	indices := make([]int, 0, len(indexed))
	for index := range indexed {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	values := make([]any, 0, len(indices))
	for want, index := range indices {
		if index != want {
			return nil, true, fmt.Errorf("repeated message query indexes must be contiguous from 0")
		}
		nested, err := queryMessageValues(field.Message(), indexed[index], "")
		if err != nil {
			return nil, true, err
		}
		values = append(values, nested)
	}
	return values, true, nil
}

func parseRepeatedMessageJSONValues(rawValues []string) ([]any, error) {
	values := make([]any, 0, len(rawValues))
	for _, raw := range rawValues {
		if strings.HasPrefix(strings.TrimSpace(raw), "[") {
			var objects []map[string]any
			if err := json.Unmarshal([]byte(raw), &objects); err != nil {
				return nil, err
			}
			for _, object := range objects {
				if object == nil {
					return nil, fmt.Errorf("message value must be a JSON object")
				}
				values = append(values, object)
			}
			continue
		}

		var object map[string]any
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			return nil, err
		}
		if object == nil {
			return nil, fmt.Errorf("message value must be a JSON object")
		}
		values = append(values, object)
	}
	return values, nil
}

func indexedRepeatedMessageQueries(
	query map[string][]string,
	keys []string,
) (map[int]map[string][]string, bool, error) {
	queries := make(map[int]map[string][]string)
	found := false
	for key, rawValues := range query {
		for _, root := range keys {
			index, nestedKey, matched, err := parseIndexedMessageQueryKey(key, root)
			if !matched {
				continue
			}
			found = true
			if err != nil {
				return nil, true, err
			}
			if queries[index] == nil {
				queries[index] = make(map[string][]string)
			}
			queries[index][nestedKey] = append(queries[index][nestedKey], rawValues...)
			break
		}
	}
	return queries, found, nil
}

func parseIndexedMessageQueryKey(key string, root string) (int, string, bool, error) {
	if !strings.HasPrefix(key, root) {
		return 0, "", false, nil
	}
	suffix := strings.TrimPrefix(key, root)
	if suffix == "" || (suffix[0] != '.' && suffix[0] != '[') {
		return 0, "", false, nil
	}

	var digits string
	var remainder string
	if suffix[0] == '.' {
		suffix = suffix[1:]
		separator := strings.IndexByte(suffix, '.')
		if separator < 1 {
			return 0, "", true, fmt.Errorf("indexed message key %q must contain a nested field", key)
		}
		digits = suffix[:separator]
		remainder = suffix[separator+1:]
	} else {
		closing := strings.IndexByte(suffix, ']')
		if closing < 2 || closing+2 > len(suffix) || suffix[closing+1] != '.' || closing+2 == len(suffix) {
			return 0, "", true, fmt.Errorf("indexed message key %q must use [index].field syntax", key)
		}
		digits = suffix[1:closing]
		remainder = suffix[closing+2:]
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, "", true, fmt.Errorf("indexed message key %q has a non-numeric index", key)
		}
	}
	index, err := strconv.Atoi(digits)
	if err != nil {
		return 0, "", true, fmt.Errorf("indexed message key %q has an invalid index", key)
	}
	if remainder == "" {
		return 0, "", true, fmt.Errorf("indexed message key %q must contain a nested field", key)
	}
	return index, remainder, true, nil
}

func queryMapValues(
	field protoreflect.FieldDescriptor,
	query map[string][]string,
	prefix string,
) (map[string]any, error) {
	entry := field.Message()
	keyField := entry.Fields().ByName("key")
	valueField := entry.Fields().ByName("value")
	if keyField == nil || valueField == nil {
		return nil, fmt.Errorf("map field %s has an invalid entry descriptor", field.FullName())
	}
	if valueField.Kind() == protoreflect.MessageKind || valueField.Kind() == protoreflect.GroupKind {
		return nil, fmt.Errorf("message-valued map query field %s is not supported", field.FullName())
	}

	values := make(map[string]any)
	for key, rawValues := range query {
		if !strings.HasPrefix(key, prefix) || key == prefix {
			continue
		}
		mapKey := strings.TrimPrefix(key, prefix)
		if len(rawValues) != 1 {
			return nil, fmt.Errorf("map query key %s must have one value", key)
		}
		if _, err := coerceQueryValue(keyField, mapKey); err != nil {
			return nil, fmt.Errorf("invalid map query key %s: %w", key, err)
		}
		value, err := coerceQueryValue(valueField, rawValues[0])
		if err != nil {
			return nil, fmt.Errorf("invalid map query value %s: %w", key, err)
		}
		values[mapKey] = value
	}
	return values, nil
}

func coerceQueryValues(field protoreflect.FieldDescriptor, rawValues []string) (any, error) {
	if field.IsList() {
		values := make([]any, 0, len(rawValues))
		for _, raw := range rawValues {
			value, err := coerceQueryValue(field, raw)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	}
	return coerceQueryValue(field, rawValues[0])
}

func coerceQueryValue(field protoreflect.FieldDescriptor, raw string) (any, error) {
	switch field.Kind() {
	case protoreflect.BoolKind:
		return strconv.ParseBool(raw)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		value, err := strconv.ParseInt(raw, 10, 32)
		return value, err
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		value, err := parseInt64Input(raw, false)
		return int64(value), err
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		value, err := strconv.ParseUint(raw, 10, 32)
		return value, err
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		value, err := parseInt64Input(raw, true)
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

func parseInt64Input(raw string, unsigned bool) (uint64, error) {
	if !strings.HasPrefix(raw, "#") {
		if unsigned {
			return strconv.ParseUint(raw, 10, 64)
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		return uint64(value), err
	}

	literal := raw[1:]
	negative := strings.HasPrefix(literal, "-")
	if negative {
		if unsigned {
			return 0, fmt.Errorf("unsigned integer cannot be negative")
		}
		literal = literal[1:]
	}
	base := 10
	if strings.HasPrefix(literal, "0x") || strings.HasPrefix(literal, "0X") {
		base = 16
		literal = literal[2:]
	}
	if literal == "" {
		return 0, fmt.Errorf("empty integer")
	}
	magnitude, err := strconv.ParseUint(literal, base, 64)
	if err != nil {
		return 0, err
	}
	if !negative {
		return magnitude, nil
	}
	if magnitude > uint64(1)<<63 {
		return 0, fmt.Errorf("signed integer overflows int64")
	}
	return ^magnitude + 1, nil
}

func normalizeHashInt64JSON(body []byte, desc protoreflect.MessageDescriptor) []byte {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return body
	}
	normalizeHashInt64JSONValue(value, desc)
	normalized, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return normalized
}

func normalizeHashInt64JSONValue(value any, desc protoreflect.MessageDescriptor) {
	object, ok := value.(map[string]any)
	if !ok || desc == nil {
		return
	}
	fields := desc.Fields()
	for index := 0; index < fields.Len(); index++ {
		field := fields.Get(index)
		fieldValue, ok := object[field.JSONName()]
		if !ok {
			continue
		}
		if field.IsMap() {
			normalizeHashInt64MapValue(fieldValue, field.MapValue())
			continue
		}
		if field.IsList() {
			if items, ok := fieldValue.([]any); ok {
				for itemIndex, item := range items {
					items[itemIndex] = normalizeHashInt64FieldValue(item, field)
				}
			}
			continue
		}
		object[field.JSONName()] = normalizeHashInt64FieldValue(fieldValue, field)
	}
}

func normalizeHashInt64MapValue(value any, valueField protoreflect.FieldDescriptor) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, item := range object {
		if valueField.Kind() == protoreflect.MessageKind || valueField.Kind() == protoreflect.GroupKind {
			normalizeHashInt64JSONValue(item, valueField.Message())
			continue
		}
		object[key] = normalizeHashInt64FieldValue(item, valueField)
	}
}

func normalizeHashInt64FieldValue(value any, field protoreflect.FieldDescriptor) any {
	if field.Kind() == protoreflect.MessageKind || field.Kind() == protoreflect.GroupKind {
		normalizeHashInt64JSONValue(value, field.Message())
		return value
	}
	if !isInt64FieldKind(field.Kind()) {
		return value
	}
	raw, ok := value.(string)
	if !ok || !strings.HasPrefix(raw, "#") {
		return value
	}
	unsigned := field.Kind() == protoreflect.Uint64Kind || field.Kind() == protoreflect.Fixed64Kind
	parsed, err := parseInt64Input(raw, unsigned)
	if err != nil {
		return value
	}
	if unsigned {
		return strconv.FormatUint(parsed, 10)
	}
	return strconv.FormatInt(int64(parsed), 10)
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
		if p.config.ShowStatusInBody {
			if encodedStatus := resp.header.Get("Grpc-Status-Details-Bin"); encodedStatus != "" {
				body, err := p.decodeStatusDetails(encodedStatus, binding)
				if err != nil {
					return err
				}
				resp.body.Reset()
				_, _ = resp.body.Write(body)
				resp.header.Set("Content-Type", jsonContentType)
				return nil
			}
		} else {
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
	out, err := p.marshalProtoJSON(msg, nil)
	if err != nil {
		return fmt.Errorf("encode protobuf JSON response: %w", err)
	}
	resp.body.Reset()
	_, _ = resp.body.Write(out)
	resp.header.Set("Content-Type", jsonContentType)
	resp.header.Del("Content-Length")
	return nil
}

func (p *Plugin) decodeStatusDetails(encoded string, binding *methodBinding) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode grpc-status-details-bin: %w", err)
	}

	var status statuspb.Status
	if err := proto.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("decode grpc status details: %w", err)
	}

	resolver := grpcStatusResolver{}
	if binding != nil && binding.files != nil {
		resolver.dynamic = dynamicpb.NewTypes(binding.files)
	}
	if p.config.StatusDetailType == "" {
		statusJSON, err := p.marshalProtoJSON(&status, resolver)
		if err != nil {
			statusJSON, err = marshalGRPCStatusWithoutResolver(&status)
			if err != nil {
				return nil, fmt.Errorf("encode grpc status details: %w", err)
			}
		}
		return wrapGRPCStatusJSON(statusJSON)
	}

	detailType, err := findMessageDescriptor(binding, p.config.StatusDetailType)
	if err != nil {
		return nil, fmt.Errorf("resolve grpc status detail type %q: %w", p.config.StatusDetailType, err)
	}
	details := make([]any, 0, len(status.Details))
	for _, detail := range status.Details {
		message := dynamicpb.NewMessage(detailType)
		if err := proto.Unmarshal(detail.Value, message); err != nil {
			return nil, fmt.Errorf("decode grpc status detail %q: %w", p.config.StatusDetailType, err)
		}
		messageJSON, err := p.marshalProtoJSON(message, resolver)
		if err != nil {
			return nil, fmt.Errorf("encode grpc status detail %q: %w", p.config.StatusDetailType, err)
		}
		var value any
		if err := json.Unmarshal(messageJSON, &value); err != nil {
			return nil, fmt.Errorf("decode encoded grpc status detail %q: %w", p.config.StatusDetailType, err)
		}
		details = append(details, value)
	}

	statusWithoutDetails := proto.Clone(&status).(*statuspb.Status)
	statusWithoutDetails.Details = nil
	statusJSON, err := p.marshalProtoJSON(statusWithoutDetails, resolver)
	if err != nil {
		return nil, fmt.Errorf("encode grpc status: %w", err)
	}
	var statusValue map[string]any
	if err := json.Unmarshal(statusJSON, &statusValue); err != nil {
		return nil, fmt.Errorf("decode encoded grpc status: %w", err)
	}
	statusValue["details"] = details
	statusJSON, err = json.Marshal(statusValue)
	if err != nil {
		return nil, fmt.Errorf("encode grpc status body: %w", err)
	}
	return wrapGRPCStatusJSON(statusJSON)
}

func (p *Plugin) protoJSONMarshalOptions() protojson.MarshalOptions {
	return protojson.MarshalOptions{
		UseProtoNames:  false,
		UseEnumNumbers: p.enumAsValue(),
	}
}

func (p *Plugin) protoJSONMarshalOptionsWithResolver(resolver protoJSONResolver) protojson.MarshalOptions {
	options := p.protoJSONMarshalOptions()
	options.Resolver = resolver
	return options
}

func (p *Plugin) enumAsValue() bool {
	for _, option := range p.config.PBOption {
		if option == "enum_as_value" {
			return true
		}
		if option == "enum_as_name" {
			return false
		}
	}
	return false
}

func (p *Plugin) int64JSONMode() int64JSONMode {
	for _, option := range p.config.PBOption {
		switch option {
		case "int64_as_number":
			return int64AsNumber
		case "int64_as_string":
			return int64AsString
		case "int64_as_hexstring":
			return int64AsHexString
		}
	}
	return int64AsNumber
}

func (p *Plugin) emitDefaultValues(desc protoreflect.MessageDescriptor) bool {
	for _, option := range p.config.PBOption {
		switch option {
		case "no_default_values":
			return false
		case "use_default_values":
			return true
		case "use_default_metatable":
			// Lua metatable-backed defaults are not JSON-visible through the Go
			// protobuf marshaler; keep the response sparse until an exact
			// metatable contract exists.
			return false
		case "auto_default_values":
			return desc != nil && desc.ParentFile().Syntax() == protoreflect.Proto3
		}
	}
	return false
}

func (p *Plugin) marshalProtoJSON(message proto.Message, resolver protoJSONResolver) ([]byte, error) {
	options := p.protoJSONMarshalOptions()
	if resolver != nil {
		options = p.protoJSONMarshalOptionsWithResolver(resolver)
	}
	options.EmitDefaultValues = p.emitDefaultValues(message.ProtoReflect().Descriptor())
	out, err := options.Marshal(message)
	if err != nil {
		return out, err
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode protobuf JSON for int64 conversion: %w", err)
	}
	normalizeInt64JSONValue(value, message.ProtoReflect().Descriptor(), p.int64JSONMode())
	return json.Marshal(value)
}

func normalizeInt64JSONValue(value any, desc protoreflect.MessageDescriptor, mode int64JSONMode) {
	object, ok := value.(map[string]any)
	if !ok || desc == nil {
		return
	}
	fields := desc.Fields()
	for index := 0; index < fields.Len(); index++ {
		field := fields.Get(index)
		fieldValue, ok := object[field.JSONName()]
		if !ok {
			continue
		}
		if field.IsMap() {
			normalizeInt64MapValue(fieldValue, field.MapValue(), mode)
			continue
		}
		if field.IsList() {
			if items, ok := fieldValue.([]any); ok {
				for itemIndex, item := range items {
					items[itemIndex] = normalizeInt64FieldValue(item, field, mode)
				}
			}
			continue
		}
		object[field.JSONName()] = normalizeInt64FieldValue(fieldValue, field, mode)
	}
}

func normalizeInt64MapValue(
	value any,
	valueField protoreflect.FieldDescriptor,
	mode int64JSONMode,
) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, item := range object {
		if valueField.Kind() == protoreflect.MessageKind || valueField.Kind() == protoreflect.GroupKind {
			normalizeInt64JSONValue(item, valueField.Message(), mode)
			continue
		}
		object[key] = normalizeInt64FieldValue(item, valueField, mode)
	}
}

func normalizeInt64FieldValue(
	value any,
	field protoreflect.FieldDescriptor,
	mode int64JSONMode,
) any {
	if field.Kind() == protoreflect.MessageKind || field.Kind() == protoreflect.GroupKind {
		normalizeInt64JSONValue(value, field.Message(), mode)
		return value
	}
	if !isInt64FieldKind(field.Kind()) {
		return value
	}
	raw, ok := value.(string)
	if !ok {
		return value
	}
	if field.Kind() == protoreflect.Uint64Kind || field.Kind() == protoreflect.Fixed64Kind {
		unsigned, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return value
		}
		return formatInt64JSONValue(raw, unsigned, false, mode)
	}
	signed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return value
	}
	return formatInt64JSONValue(raw, uint64(signed), true, mode)
}

func formatInt64JSONValue(raw string, value uint64, signed bool, mode int64JSONMode) any {
	if mode == int64AsNumber || fitsInt64JSONNumber(value, signed) {
		return stdjson.Number(raw)
	}
	if mode == int64AsString {
		return "#" + raw
	}
	if signed && int64(value) < 0 {
		value = ^value + 1
		return "#-0x" + strings.ToUpper(strconv.FormatUint(value, 16))
	}
	return "#0x" + strings.ToUpper(strconv.FormatUint(value, 16))
}

func fitsInt64JSONNumber(value uint64, signed bool) bool {
	if !signed {
		return value <= uint64(^uint32(0))
	}
	signedValue := int64(value)
	return signedValue >= -1<<31 && signedValue <= int64(^uint32(0))
}

func isInt64FieldKind(kind protoreflect.Kind) bool {
	switch kind {
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return true
	default:
		return false
	}
}

func findMessageDescriptor(binding *methodBinding, name string) (protoreflect.MessageDescriptor, error) {
	name = strings.TrimPrefix(name, ".")
	if binding != nil && binding.files != nil {
		if descriptor, err := binding.files.FindDescriptorByName(protoreflect.FullName(name)); err == nil {
			if message, ok := descriptor.(protoreflect.MessageDescriptor); ok {
				return message, nil
			}
		}
	}
	message, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(name))
	if err != nil {
		return nil, err
	}
	return message.Descriptor(), nil
}

func marshalGRPCStatusWithoutResolver(status *statuspb.Status) ([]byte, error) {
	value := map[string]any{
		"code":    status.GetCode(),
		"message": status.GetMessage(),
	}
	if len(status.GetDetails()) > 0 {
		details := make([]any, 0, len(status.GetDetails()))
		for _, detail := range status.GetDetails() {
			details = append(details, map[string]any{
				"type_url": detail.GetTypeUrl(),
				"value":    base64.StdEncoding.EncodeToString(detail.GetValue()),
			})
		}
		value["details"] = details
	}
	return json.Marshal(value)
}

func wrapGRPCStatusJSON(statusJSON []byte) ([]byte, error) {
	var statusValue map[string]any
	if err := json.Unmarshal(statusJSON, &statusValue); err != nil {
		return nil, fmt.Errorf("decode grpc status JSON: %w", err)
	}
	return json.Marshal(map[string]any{"error": statusValue})
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
