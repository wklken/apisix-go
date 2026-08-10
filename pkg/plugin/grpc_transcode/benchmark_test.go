package grpc_transcode

import (
	"encoding/base64"
	"testing"

	"github.com/wklken/apisix-go/pkg/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// BenchmarkVerifiedHotPath measures the per-request descriptor binding lookup
// against the real store: proto content fetch plus cached-binding validation.
func BenchmarkVerifiedHotPath(b *testing.B) {
	events := make(chan *store.Event, 4)
	storage, err := store.GetStore(b.TempDir()+"/grpc-bench.db", events)
	if err != nil {
		b.Fatalf("get store: %v", err)
	}
	storage.Start()
	b.Cleanup(func() { _ = storage.Stop() })

	content := benchmarkDescriptorContent(b)
	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/protos/bench-proto")
	event.Value = []byte(`{"id":"bench-proto","content":"` + content + `"}`)
	events <- event
	if err := storage.Sync(); err != nil {
		b.Fatalf("Sync() error = %v", err)
	}

	p := &Plugin{config: Config{
		ProtoID: "bench-proto",
		Service: "echo.EchoService",
		Method:  "Echo",
	}}
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
