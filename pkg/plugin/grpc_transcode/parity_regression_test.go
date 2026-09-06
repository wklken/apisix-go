package grpc_transcode

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestParityPreserveProtoFieldNames(t *testing.T) {
	binding, err := loadBinding(
		`syntax="proto3";package test;message Reply{string full_name=1;}service Echo{rpc Say(Reply)returns(Reply);}`,
		"test.proto",
		"test.Echo",
		"Say",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	msg := dynamicpb.NewMessage(binding.method.Output())
	msg.Set(msg.Descriptor().Fields().ByName("full_name"), protoreflect.ValueOfString("Alice"))
	p := &Plugin{}
	body, err := p.marshalProtoJSON(msg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"full_name"`) {
		t.Fatalf("response=%s, want original proto field full_name", body)
	}
}
