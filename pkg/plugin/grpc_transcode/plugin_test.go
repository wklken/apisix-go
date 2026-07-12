package grpc_transcode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	stdjson "encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
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
		PBOption: []string{"no_default_values"},
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

func TestHandlerRejectsStreamingMethodDescriptor(t *testing.T) {
	restore := stubProtoContent(t, "streaming-proto", streamingDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "streaming-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?msg=Hello", nil)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for an unsupported streaming method")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "streaming") {
		t.Fatalf("response body = %q, want explicit streaming rejection", res.Body.String())
	}
}

func TestHandlerHonorsEnumAsValueOption(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID:  "echo-proto",
		Service:  "echo.EchoService",
		Method:   "Echo",
		PBOption: []string{"enum_as_value"},
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?msg=Hello&mode=MODE_ACTIVE", nil)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frame, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request frame: %v", err)
		}
		message := decodeEchoMessage(t, unframeGRPCMessageForTest(t, frame))
		mode := message.Get(message.Descriptor().Fields().ByName("mode")).Enum()
		if mode != 1 {
			t.Fatalf("decoded mode = %d, want 1", mode)
		}
		w.Header().Set("Content-Type", defaultContentType)
		w.Header().Set("Grpc-Status", "0")
		_, _ = w.Write(frame)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	var body map[string]any
	if err := stdjson.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v; body=%q", err, res.Body.String())
	}
	if got := body["msg"]; got != "Hello" {
		t.Fatalf("response msg = %#v, want Hello", got)
	}
	if got := body["mode"]; got != float64(1) {
		t.Fatalf("response mode = %#v, want numeric 1", got)
	}
}

func TestHandlerHonorsInt64OutputOptions(t *testing.T) {
	const rawID = "9007199254740993"
	for _, test := range []struct {
		name   string
		option string
		wantID any
	}{
		{name: "number", option: "int64_as_number", wantID: stdjson.Number(rawID)},
		{name: "string", option: "int64_as_string", wantID: "#" + rawID},
		{name: "hexstring", option: "int64_as_hexstring", wantID: "#0x20000000000001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
			defer restore()

			p := newTestPlugin(t, Config{
				ProtoID:  "echo-proto",
				Service:  "echo.EchoService",
				Method:   "Echo",
				PBOption: []string{test.option},
			})
			req := httptest.NewRequest(http.MethodGet, "/echo?msg=Hello&id="+rawID, nil)
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				frame, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read request frame: %v", err)
				}
				message := decodeEchoMessage(t, unframeGRPCMessageForTest(t, frame))
				id := message.Get(message.Descriptor().Fields().ByName("id")).Int()
				if id != 9007199254740993 {
					t.Fatalf("decoded id = %d, want %s", id, rawID)
				}
				w.Header().Set("Content-Type", defaultContentType)
				w.Header().Set("Grpc-Status", "0")
				_, _ = w.Write(frame)
			})).ServeHTTP(res, req)

			if res.Code != http.StatusOK {
				t.Fatalf("response status = %d, want 200; body=%q", res.Code, res.Body.String())
			}
			var body map[string]any
			decoder := stdjson.NewDecoder(bytes.NewReader(res.Body.Bytes()))
			decoder.UseNumber()
			if err := decoder.Decode(&body); err != nil {
				t.Fatalf("decode response body: %v; body=%q", err, res.Body.String())
			}
			if got := body["id"]; got != test.wantID {
				t.Fatalf("response id = %#v, want %#v", got, test.wantID)
			}
		})
	}
}

func TestNormalizeInt64JSONValueConvertsNestedListAndMap(t *testing.T) {
	desc, err := buildEchoMessageDescriptor()
	if err != nil {
		t.Fatalf("build echo descriptor: %v", err)
	}
	value := map[string]any{
		"id":  "9007199254740993",
		"ids": []any{"1", "2"},
		"options": map[string]any{
			"id": "3",
		},
		"counts": map[string]any{
			"first": "4",
		},
	}

	normalizeInt64JSONValue(value, desc, int64AsNumber)

	if got := value["id"]; got != stdjson.Number("9007199254740993") {
		t.Fatalf("id = %#v, want JSON number", got)
	}
	ids := value["ids"].([]any)
	if ids[0] != stdjson.Number("1") || ids[1] != stdjson.Number("2") {
		t.Fatalf("ids = %#v, want JSON numbers", ids)
	}
	options := value["options"].(map[string]any)
	if got := options["id"]; got != stdjson.Number("3") {
		t.Fatalf("nested id = %#v, want JSON number", got)
	}
	counts := value["counts"].(map[string]any)
	if got := counts["first"]; got != stdjson.Number("4") {
		t.Fatalf("map value = %#v, want JSON number", got)
	}
}

func TestNormalizeInt64JSONValuePreservesSmallNumbers(t *testing.T) {
	desc, err := buildEchoMessageDescriptor()
	if err != nil {
		t.Fatalf("build echo descriptor: %v", err)
	}
	idField := desc.Fields().ByName("id")
	for _, test := range []struct {
		name string
		mode int64JSONMode
		want any
	}{
		{name: "string small", mode: int64AsString, want: stdjson.Number("1")},
		{name: "hex small", mode: int64AsHexString, want: stdjson.Number("1")},
		{name: "string negative", mode: int64AsString, want: "#-2147483649"},
		{name: "hex negative", mode: int64AsHexString, want: "#-0x80000001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := map[string]any{"id": "1"}
			if strings.Contains(test.name, "negative") {
				value["id"] = "-2147483649"
			}
			normalizeInt64JSONValue(value, desc, test.mode)
			if got := value[idField.JSONName()]; got != test.want {
				t.Fatalf("id = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestHandlerUsesProto3DefaultValues(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?msg=Hello", nil)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frame, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request frame: %v", err)
		}
		w.Header().Set("Content-Type", defaultContentType)
		w.Header().Set("Grpc-Status", "0")
		_, _ = w.Write(frame)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	var body map[string]any
	decoder := stdjson.NewDecoder(bytes.NewReader(res.Body.Bytes()))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode response body: %v; body=%q", err, res.Body.String())
	}
	if got := body["msg"]; got != "Hello" {
		t.Fatalf("msg = %#v, want Hello", got)
	}
	if got := body["id"]; got != stdjson.Number("0") {
		t.Fatalf("id = %#v, want default 0", got)
	}
	if got := body["mode"]; got != "MODE_UNSPECIFIED" {
		t.Fatalf("mode = %#v, want MODE_UNSPECIFIED", got)
	}
	if got := body["tags"]; got == nil {
		t.Fatal("tags missing, want empty array")
	}
	if got := body["ids"]; got == nil {
		t.Fatal("ids missing, want empty array")
	}
	if got := body["counts"]; got == nil {
		t.Fatal("counts missing, want empty map")
	}
	if _, ok := body["options"]; ok {
		t.Fatal("options present, want absent message field")
	}
}

func TestHandlerAcceptsHashPrefixedInt64Inputs(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{name: "query decimal", path: "/echo?msg=Hello&id=%239007199254740993"},
		{name: "query hex", path: "/echo?msg=Hello&id=%23-0x80000001"},
		{name: "json hex", path: "/echo", body: `{"msg":"Hello","id":"#0x20000000000001"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
			defer restore()

			p := newTestPlugin(t, Config{
				ProtoID:  "echo-proto",
				Service:  "echo.EchoService",
				Method:   "Echo",
				PBOption: []string{"no_default_values"},
			})
			var bodyReader io.Reader
			if test.body != "" {
				bodyReader = strings.NewReader(test.body)
			}
			req := httptest.NewRequest(http.MethodGet, test.path, bodyReader)
			if test.body != "" {
				req.Method = http.MethodPost
				req.Header.Set("Content-Type", "application/json")
			}
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				frame, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read request frame: %v", err)
				}
				message := decodeEchoMessage(t, unframeGRPCMessageForTest(t, frame))
				id := message.Get(message.Descriptor().Fields().ByName("id")).Int()
				if test.name == "query hex" {
					if id != -2147483649 {
						t.Fatalf("decoded id = %d, want -2147483649", id)
					}
				} else if id != 9007199254740993 {
					t.Fatalf("decoded id = %d, want 9007199254740993", id)
				}
				w.Header().Set("Content-Type", defaultContentType)
				w.Header().Set("Grpc-Status", "0")
				_, _ = w.Write(frame)
			})).ServeHTTP(res, req)

			if res.Code != http.StatusOK {
				t.Fatalf("response status = %d, want 200; body=%q", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerTranscodesThroughInProcessGRPCServer(t *testing.T) {
	for _, test := range []struct {
		name       string
		serverErr  error
		wantStatus int
		wantBody   string
	}{
		{name: "success", wantStatus: http.StatusOK, wantBody: `{"msg":"echoed"}`},
		{
			name:       "unary error",
			serverErr:  status.Error(codes.PermissionDenied, "denied"),
			wantStatus: http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
			defer restore()

			address := startEchoGRPCServer(t, test.serverErr)
			conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("grpc.NewClient() error = %v", err)
			}
			t.Cleanup(func() { _ = conn.Close() })

			p := newTestPlugin(t, Config{
				ProtoID:  "echo-proto",
				Service:  "echo.EchoService",
				Method:   "Echo",
				PBOption: []string{"no_default_values"},
			})
			req := httptest.NewRequest(http.MethodGet, "/echo?msg=hello", nil)
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				frame, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read transformed frame: %v", err)
				}
				request := dynamicpb.NewMessage(echoMessageDescriptor(t))
				if err := proto.Unmarshal(unframeGRPCMessageForTest(t, frame), request); err != nil {
					t.Fatalf("decode transformed frame: %v", err)
				}
				response := dynamicpb.NewMessage(echoMessageDescriptor(t))
				if err := conn.Invoke(r.Context(), "/echo.EchoService/Echo", request, response); err != nil {
					grpcStatus, ok := status.FromError(err)
					if !ok {
						t.Fatalf("status.FromError() failed for %v", err)
					}
					w.Header().Set("Grpc-Status", strconv.Itoa(int(grpcStatus.Code())))
					w.Header().Set("Grpc-Message", grpcStatus.Message())
					return
				}
				w.Header().Set("Content-Type", defaultContentType)
				w.Header().Set("Grpc-Status", "0")
				_, _ = w.Write(frameGRPCMessageForTest(t, mustMarshalProto(t, response)))
			})).ServeHTTP(res, req)

			if res.Code != test.wantStatus {
				t.Fatalf("response status = %d, want %d; body=%q", res.Code, test.wantStatus, res.Body.String())
			}
			if test.wantBody != "" && res.Body.String() != test.wantBody {
				t.Fatalf("response body = %q, want %q", res.Body.String(), test.wantBody)
			}
		})
	}
}

func TestHandlerSupportsPlainProtoSourceAndImports(t *testing.T) {
	restore := stubProtoSources(t, map[string]string{
		"echo-source": `syntax = "proto3";
package echo;
import "common.proto";
service EchoService {
  rpc Echo (common.EchoMsg) returns (common.EchoMsg);
}`,
		"common.proto": `syntax = "proto3";
package common;
message EchoMsg {
  string msg = 1;
}`,
	})
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-source",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?msg=hello", nil)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frame, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed frame: %v", err)
		}
		_ = unframeGRPCMessageForTest(t, frame)
		w.Header().Set("Content-Type", defaultContentType)
		w.Header().Set("Grpc-Status", "0")
		_, _ = w.Write(frame)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	if got := res.Body.String(); got != `{"msg":"hello"}` {
		t.Fatalf("response body = %q, want %q", got, `{"msg":"hello"}`)
	}
}

func TestLoadBindingRejectsMissingPlainProtoImport(t *testing.T) {
	restore := stubProtoSources(t, map[string]string{
		"echo-source": `syntax = "proto3";
package echo;
import "missing.proto";
service EchoService {
  rpc Echo (Missing) returns (Missing);
}`,
	})
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-source",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	if _, err := p.loadBinding(); err == nil {
		t.Fatal("loadBinding() error = nil, want missing import error")
	}
}

func TestHandlerMapsRepeatedGETQueryValues(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?tags=one&tags=two", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		msg := decodeEchoMessage(t, unframeGRPCMessageForTest(t, body))
		field := msg.Descriptor().Fields().ByName("tags")
		values := msg.Get(field).List()
		if values.Len() != 2 {
			t.Fatalf("decoded tags length = %d, want 2", values.Len())
		}
		if got := values.Get(0).String(); got != "one" {
			t.Fatalf("decoded first tag = %q, want one", got)
		}
		if got := values.Get(1).String(); got != "two" {
			t.Fatalf("decoded second tag = %q, want two", got)
		}
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
}

func TestHandlerMapsNestedGETQueryValues(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?options.alias=deep", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		msg := decodeEchoMessage(t, unframeGRPCMessageForTest(t, body))
		optionsField := msg.Descriptor().Fields().ByName("options")
		aliasField := optionsField.Message().Fields().ByName("alias")
		if got := msg.Get(optionsField).Message().Get(aliasField).String(); got != "deep" {
			t.Fatalf("decoded nested alias = %q, want deep", got)
		}
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
}

func TestHandlerMapsRepeatedNestedGETQueryValues(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(
		http.MethodGet,
		"/echo?children.0.alias=first&children[1].alias=second&children[1].id=%237",
		nil,
	)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		msg := decodeEchoMessage(t, unframeGRPCMessageForTest(t, body))
		childrenField := msg.Descriptor().Fields().ByName("children")
		children := msg.Get(childrenField).List()
		if children.Len() != 2 {
			t.Fatalf("decoded children length = %d, want 2", children.Len())
		}
		aliasField := childrenField.Message().Fields().ByName("alias")
		idField := childrenField.Message().Fields().ByName("id")
		if got := children.Get(0).Message().Get(aliasField).String(); got != "first" {
			t.Fatalf("decoded first child alias = %q, want first", got)
		}
		if got := children.Get(1).Message().Get(aliasField).String(); got != "second" {
			t.Fatalf("decoded second child alias = %q, want second", got)
		}
		if got := children.Get(1).Message().Get(idField).Int(); got != 7 {
			t.Fatalf("decoded second child id = %d, want 7", got)
		}
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
}

func TestHandlerMapsRepeatedNestedGETJSONValues(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(
		http.MethodGet,
		"/echo?children=%7B%22alias%22%3A%22first%22%7D&children=%7B%22alias%22%3A%22second%22%7D",
		nil,
	)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		msg := decodeEchoMessage(t, unframeGRPCMessageForTest(t, body))
		children := msg.Get(msg.Descriptor().Fields().ByName("children")).List()
		if children.Len() != 2 {
			t.Fatalf("decoded children length = %d, want 2", children.Len())
		}
		aliasField := children.Get(0).Message().Descriptor().Fields().ByName("alias")
		if got := children.Get(0).Message().Get(aliasField).String(); got != "first" {
			t.Fatalf("decoded first child alias = %q, want first", got)
		}
		if got := children.Get(1).Message().Get(aliasField).String(); got != "second" {
			t.Fatalf("decoded second child alias = %q, want second", got)
		}
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsSparseRepeatedNestedGETQueryValues(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?children.1.alias=second", nil)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", res.Code, res.Body.String())
	}
}

func TestHandlerMapsGETQueryValuesIntoMapField(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?labels.env=prod&labels.region=us", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		msg := decodeEchoMessage(t, unframeGRPCMessageForTest(t, body))
		labelsField := msg.Descriptor().Fields().ByName("labels")
		labels := msg.Get(labelsField).Map()
		if got := labels.Get(protoreflect.ValueOfString("env").MapKey()).String(); got != "prod" {
			t.Fatalf("decoded env label = %q, want prod", got)
		}
		if got := labels.Get(protoreflect.ValueOfString("region").MapKey()).String(); got != "us" {
			t.Fatalf("decoded region label = %q, want us", got)
		}
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
}

func TestHandlerReadsPOSTJSONBody(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID:  "echo-proto",
		Service:  "echo.EchoService",
		Method:   "Echo",
		PBOption: []string{"no_default_values"},
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

func TestHandlerMapsRepeatedMessageJSONBody(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID:  "echo-proto",
		Service:  "echo.EchoService",
		Method:   "Echo",
		PBOption: []string{"no_default_values"},
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/echo",
		bytes.NewReader([]byte(`{"children":[{"alias":"first"},{"alias":"second"}]}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		msg := decodeEchoMessage(t, unframeGRPCMessageForTest(t, body))
		children := msg.Get(msg.Descriptor().Fields().ByName("children")).List()
		if children.Len() != 2 {
			t.Fatalf("decoded children length = %d, want 2", children.Len())
		}
		alias := children.Get(0).Message().Get(children.Get(0).Message().Descriptor().Fields().ByName("alias"))
		if got := alias.String(); got != "first" {
			t.Fatalf("first child alias = %q, want first", got)
		}
		w.Header().Set("Grpc-Status", "0")
		_, _ = w.Write(frameGRPCMessageForTest(t, encodeEchoMessage(t, "ok")))
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", res.Code, res.Body.String())
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

func TestHandlerShowsGRPCStatusDetailsInBody(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID:          "echo-proto",
		Service:          "echo.EchoService",
		Method:           "Echo",
		ShowStatusInBody: true,
	})
	statusDetails, err := proto.Marshal(&statuspb.Status{
		Code:    7,
		Message: "permission denied",
		Details: []*anypb.Any{{
			TypeUrl: "type.googleapis.com/google.rpc.ErrorInfo",
			Value: func() []byte {
				value, marshalErr := proto.Marshal(&errdetails.ErrorInfo{
					Reason: "AUTH_REQUIRED",
					Domain: "example.test",
				})
				if marshalErr != nil {
					t.Fatalf("marshal ErrorInfo: %v", marshalErr)
				}
				return value
			}(),
		}},
	})
	if err != nil {
		t.Fatalf("marshal status details: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/echo?msg=denied", nil)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Grpc-Status", "7")
		w.Header().Set("Grpc-Status-Details-Bin", base64.StdEncoding.EncodeToString(statusDetails))
	})).ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]any
	if err := stdjson.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v; body=%q", err, res.Body.String())
	}
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response error field = %#v, want object", body["error"])
	}
	if got := errorBody["code"]; got != float64(7) {
		t.Fatalf("error code = %#v, want 7", got)
	}
	if got := errorBody["message"]; got != "permission denied" {
		t.Fatalf("error message = %#v, want permission denied", got)
	}
	details, ok := errorBody["details"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("error details = %#v, want one detail", errorBody["details"])
	}
	detail, ok := details[0].(map[string]any)
	if !ok {
		t.Fatalf("error detail = %#v, want object", details[0])
	}
	if got := detail["reason"]; got != "AUTH_REQUIRED" {
		t.Fatalf("detail reason = %#v, want AUTH_REQUIRED", got)
	}
}

func TestHandlerDecodesConfiguredGRPCStatusDetailType(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID:          "echo-proto",
		Service:          "echo.EchoService",
		Method:           "Echo",
		ShowStatusInBody: true,
		StatusDetailType: "google.rpc.ErrorInfo",
	})
	detail, err := proto.Marshal(&errdetails.ErrorInfo{Reason: "AUTH_REQUIRED"})
	if err != nil {
		t.Fatalf("marshal ErrorInfo: %v", err)
	}
	statusDetails, err := proto.Marshal(&statuspb.Status{
		Code:    7,
		Message: "permission denied",
		Details: []*anypb.Any{{Value: detail}},
	})
	if err != nil {
		t.Fatalf("marshal status details: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/echo?msg=denied", nil)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Grpc-Status", "7")
		w.Header().Set("Grpc-Status-Details-Bin", base64.StdEncoding.EncodeToString(statusDetails))
	})).ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	var body map[string]any
	if err := stdjson.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v; body=%q", err, res.Body.String())
	}
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response error field = %#v, want object", body["error"])
	}
	details, ok := errorBody["details"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("error details = %#v, want one detail", errorBody["details"])
	}
	detailBody, ok := details[0].(map[string]any)
	if !ok {
		t.Fatalf("error detail = %#v, want object", details[0])
	}
	if got := detailBody["reason"]; got != "AUTH_REQUIRED" {
		t.Fatalf("detail reason = %#v, want AUTH_REQUIRED", got)
	}
	if _, ok := detailBody["@type"]; ok {
		t.Fatalf("configured detail should not include @type: %#v", detailBody)
	}
}

func TestHandlerRejectsMalformedGRPCStatusDetails(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID:          "echo-proto",
		Service:          "echo.EchoService",
		Method:           "Echo",
		ShowStatusInBody: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/echo?msg=denied", nil)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Grpc-Status", "7")
		w.Header().Set("Grpc-Status-Details-Bin", "not-base64!")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.Code)
	}
	if !strings.Contains(res.Body.String(), "grpc-status-details-bin") {
		t.Fatalf("error body = %q, want status-details decode error", res.Body.String())
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

func TestHandlerRejectsInvalidDescriptorResource(t *testing.T) {
	restore := stubProtoContent(t, "bad-proto", "not-a-file-descriptor-set")
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "bad-proto",
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
	if !strings.Contains(res.Body.String(), "FileDescriptorSet") {
		t.Fatalf("error body = %q, want descriptor-set error", res.Body.String())
	}
}

func TestLoadBindingCachesUnchangedDescriptor(t *testing.T) {
	restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
	defer restore()

	p := newTestPlugin(t, Config{
		ProtoID: "echo-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	})
	first, err := p.loadBinding()
	if err != nil {
		t.Fatalf("first loadBinding() error = %v", err)
	}
	second, err := p.loadBinding()
	if err != nil {
		t.Fatalf("second loadBinding() error = %v", err)
	}
	if first != second {
		t.Fatal("loadBinding() returned different bindings for unchanged descriptor")
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

func stubProtoSources(t *testing.T, sources map[string]string) func() {
	t.Helper()
	previous := fetchProtoContent
	fetchProtoContent = func(id string) (string, error) {
		content, ok := sources[id]
		if !ok {
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

func streamingDescriptorContent(t *testing.T) string {
	t.Helper()
	fd := echoFileDescriptor()
	fd.Service[0].Method[0].ServerStreaming = new(true)
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}}
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal streaming descriptor set: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func echoFileDescriptor() *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:    new("echo.proto"),
		Package: new("echo"),
		Syntax:  new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: new("EchoMsg"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     new("msg"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: new("msg"),
					},
					{
						Name:     new("tags"),
						Number:   proto.Int32(2),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: new("tags"),
					},
					{
						Name:     new("options"),
						Number:   proto.Int32(3),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: new(".echo.EchoOptions"),
						JsonName: new("options"),
					},
					{
						Name:     new("labels"),
						Number:   proto.Int32(4),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: new(".echo.EchoMsg.LabelsEntry"),
						JsonName: new("labels"),
					},
					{
						Name:     new("mode"),
						Number:   proto.Int32(5),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
						TypeName: new(".echo.Mode"),
						JsonName: new("mode"),
					},
					{
						Name:     new("id"),
						Number:   proto.Int32(6),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
						JsonName: new("id"),
					},
					{
						Name:     new("ids"),
						Number:   proto.Int32(7),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
						JsonName: new("ids"),
					},
					{
						Name:     new("counts"),
						Number:   proto.Int32(8),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: new(".echo.EchoMsg.CountsEntry"),
						JsonName: new("counts"),
					},
					{
						Name:     new("children"),
						Number:   proto.Int32(9),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: new(".echo.EchoOptions"),
						JsonName: new("children"),
					},
				},
				NestedType: []*descriptorpb.DescriptorProto{
					{
						Name: new("LabelsEntry"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:     new("key"),
								Number:   proto.Int32(1),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
								JsonName: new("key"),
							},
							{
								Name:     new("value"),
								Number:   proto.Int32(2),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
								JsonName: new("value"),
							},
						},
						Options: &descriptorpb.MessageOptions{MapEntry: new(true)},
					},
					{
						Name: new("CountsEntry"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:     new("key"),
								Number:   proto.Int32(1),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
								JsonName: new("key"),
							},
							{
								Name:     new("value"),
								Number:   proto.Int32(2),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
								JsonName: new("value"),
							},
						},
						Options: &descriptorpb.MessageOptions{MapEntry: new(true)},
					},
				},
			},
			{
				Name: new("EchoOptions"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     new("alias"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: new("alias"),
					},
					{
						Name:     new("id"),
						Number:   proto.Int32(2),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
						JsonName: new("id"),
					},
				},
			},
		},
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{
				Name: new("Mode"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: new("MODE_UNSPECIFIED"), Number: proto.Int32(0)},
					{Name: new("MODE_ACTIVE"), Number: proto.Int32(1)},
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: new("EchoService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       new("Echo"),
						InputType:  new(".echo.EchoMsg"),
						OutputType: new(".echo.EchoMsg"),
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

	desc, err := buildEchoMessageDescriptor()
	if err != nil {
		t.Fatalf("build echo message descriptor: %v", err)
	}
	return desc
}

func buildEchoMessageDescriptor() (protoreflect.MessageDescriptor, error) {
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		echoFileDescriptor(),
	}})
	if err != nil {
		return nil, err
	}
	desc, err := files.FindDescriptorByName("echo.EchoMsg")
	if err != nil {
		return nil, err
	}
	msg, ok := desc.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("descriptor type = %T, want message", desc)
	}
	return msg, nil
}

type echoGRPCService interface {
	Echo(context.Context, *dynamicpb.Message) (*dynamicpb.Message, error)
}

type echoGRPCServer struct {
	err error
}

func (s *echoGRPCServer) Echo(_ context.Context, request *dynamicpb.Message) (*dynamicpb.Message, error) {
	if s.err != nil {
		return nil, s.err
	}
	response := dynamicpb.NewMessage(request.Descriptor())
	field := response.Descriptor().Fields().ByName("msg")
	response.Set(field, protoreflect.ValueOfString("echoed"))
	return response, nil
}

var echoGRPCServiceDescription = grpc.ServiceDesc{
	ServiceName: "echo.EchoService",
	HandlerType: (*echoGRPCService)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Echo",
		Handler: func(
			srv any,
			ctx context.Context,
			decode func(any) error,
			interceptor grpc.UnaryServerInterceptor,
		) (any, error) {
			descriptor, err := buildEchoMessageDescriptor()
			if err != nil {
				return nil, err
			}
			request := dynamicpb.NewMessage(descriptor)
			if err := decode(request); err != nil {
				return nil, err
			}
			if interceptor == nil {
				return srv.(echoGRPCService).Echo(ctx, request)
			}
			info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/echo.EchoService/Echo"}
			handler := func(ctx context.Context, req any) (any, error) {
				return srv.(echoGRPCService).Echo(ctx, req.(*dynamicpb.Message))
			}
			return interceptor(ctx, request, info, handler)
		},
	}},
}

func startEchoGRPCServer(t *testing.T, serverErr error) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	server := grpc.NewServer()
	server.RegisterService(&echoGRPCServiceDescription, &echoGRPCServer{err: serverErr})
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String()
}

func mustMarshalProto(t *testing.T, message proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("marshal proto message: %v", err)
	}
	return data
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
