package grpc_transcode

import (
	"encoding/base64"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// BenchmarkVerifiedHotPath measures the per-request descriptor binding lookup
// after generation-local proto resolution and binding precompilation.
func BenchmarkVerifiedHotPath(b *testing.B) {
	content := benchmarkDescriptorContent(b)
	p := &Plugin{config: Config{
		ProtoID: "bench-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	}}
	p.SetProtoResolver(func(id string) (string, error) {
		if id != "bench-proto" {
			return "", errProtoNotFound
		}
		return content, nil
	})
	if err := p.Init(); err != nil {
		b.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatal(err)
	}
	b.Run("descriptor-lookup", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := p.loadBinding(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func benchmarkDescriptorContent(b *testing.B) string {
	b.Helper()
	fd := &descriptorpb.FileDescriptorProto{
		Name:    new("echo.proto"),
		Package: new("echo"),
		Syntax:  new("proto3"),
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
				},
			},
		},
	}
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}}
	raw, err := proto.Marshal(set)
	if err != nil {
		b.Fatalf("marshal descriptor set: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}
