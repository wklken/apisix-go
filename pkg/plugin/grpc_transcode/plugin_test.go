package grpc_transcode

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerTranscodesGETRequestAndResponse(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID:  "echo-proto",
		Service:  "echo.EchoService",
		Method:   "Echo",
		Deadline: 250,
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?msg=Hello", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("upstream method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/echo.EchoService/Echo" {
			t.Fatalf("upstream path = %s, want /echo.EchoService/Echo", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("upstream raw query = %q, want empty", r.URL.RawQuery)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Fatalf("upstream Content-Type = %q, want application/grpc", got)
		}
		if got := r.Header.Get("grpc-timeout"); got != "250m" {
			t.Fatalf("grpc-timeout = %q, want 250m", got)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		msg := decodeEchoMessage(t, unframeGRPCMessageForTest(t, body))
		if got := msg.Get(msg.Descriptor().Fields().ByName("msg")).String(); got != "Hello" {
			t.Fatalf("decoded upstream msg = %q, want Hello", got)
		}

		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "0")
		_, _ = w.Write(frameGRPCMessageForTest(t, encodeEchoMessage(t, "Hello")))
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("response Content-Type = %q, want application/json", got)
	}
	if got := res.Body.String(); got != `{"msg":"Hello"}` {
		t.Fatalf("response body = %q, want JSON echo", got)
	}
}

func TestHandlerReadsPOSTJSONBody(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader([]byte(`{"msg":"from-body"}`)))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		msg := decodeEchoMessage(t, unframeGRPCMessageForTest(t, body))
		if got := msg.Get(msg.Descriptor().Fields().ByName("msg")).String(); got != "from-body" {
			t.Fatalf("decoded upstream msg = %q, want from-body", got)
		}

		w.Header().Set("Grpc-Status", "0")
		_, _ = w.Write(frameGRPCMessageForTest(t, encodeEchoMessage(t, "ok")))
	})).ServeHTTP(res, req)

	if got := res.Body.String(); got != `{"msg":"ok"}` {
		t.Fatalf("response body = %q, want JSON ok", got)
	}
}

func TestHandlerMapsGRPCStatusToHTTPStatus(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?msg=denied", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Grpc-Status", "7")
		w.Header().Set("Grpc-Message", "permission denied")
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if got := res.Body.String(); got != "" {
		t.Fatalf("response body = %q, want empty error body by default", got)
	}
}

func TestHandlerRejectsMissingProtoResource(t *testing.T) {
	restore := stubProtoContent(t, "other-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?msg=Hello", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.Code)
	}
}

func TestConfigAcceptsNumericProtoID(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"proto_id": 123,
		"service":  "echo.EchoService",
		"method":   "Echo",
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.ProtoID != "123" {
		t.Fatalf("ProtoID = %q, want 123", cfg.ProtoID)
	}
}

func stubProtoContent(t *testing.T, id string, content string) func() {
	t.Helper()

	previous := fetchProtoContent
	fetchProtoContent = func(got string) (string, error) {
		if got != id {
			return "", errProtoNotFound
		}
		return content, nil
	}
	return func() {
		fetchProtoContent = previous
	}
}

func testDescriptorContent(t *testing.T) string {
	t.Helper()

	fd := echoFileDescriptor()
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}}
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func echoFileDescriptor() *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:    proto.String("echo.proto"),
		Package: proto.String("echo"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("EchoMsg"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("msg"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("msg"),
					},
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: proto.String("EchoService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       proto.String("Echo"),
						InputType:  proto.String(".echo.EchoMsg"),
						OutputType: proto.String(".echo.EchoMsg"),
					},
				},
			},
		},
	}
}

func encodeEchoMessage(t *testing.T, value string) []byte {
	t.Helper()

	msg := dynamicpb.NewMessage(echoMessageDescriptor(t))
	msg.Set(msg.Descriptor().Fields().ByName("msg"), protoreflect.ValueOfString(value))
	out, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal echo message: %v", err)
	}
	return out
}

func decodeEchoMessage(t *testing.T, body []byte) *dynamicpb.Message {
	t.Helper()

	msg := dynamicpb.NewMessage(echoMessageDescriptor(t))
	if err := proto.Unmarshal(body, msg); err != nil {
		t.Fatalf("unmarshal echo message: %v", err)
	}
	return msg
}

func echoMessageDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()

	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		echoFileDescriptor(),
	}})
	if err != nil {
		t.Fatalf("build descriptors: %v", err)
	}
	desc, err := files.FindDescriptorByName("echo.EchoMsg")
	if err != nil {
		t.Fatalf("find echo message: %v", err)
	}
	msg, ok := desc.(protoreflect.MessageDescriptor)
	if !ok {
		t.Fatalf("descriptor type = %T, want message", desc)
	}
	return msg
}

func frameGRPCMessageForTest(t *testing.T, payload []byte) []byte {
	t.Helper()

	out := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func unframeGRPCMessageForTest(t *testing.T, frame []byte) []byte {
	t.Helper()

	if len(frame) < 5 {
		t.Fatalf("frame length = %d, want at least 5", len(frame))
	}
	if frame[0] != 0 {
		t.Fatalf("compressed flag = %d, want 0", frame[0])
	}
	size := int(binary.BigEndian.Uint32(frame[1:5]))
	if size != len(frame)-5 {
		t.Fatalf("frame payload length = %d, want %d", len(frame)-5, size)
	}
	return frame[5:]
}

func jsonFromProto(t *testing.T, msg proto.Message) string {
	t.Helper()

	out, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal proto json: %v", err)
	}
	return string(out)
}

var _ = protoregistry.NotFound
