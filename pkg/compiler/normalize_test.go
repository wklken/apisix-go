package compiler

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
)

func normalize(snapshot generation.Snapshot) (normalizedInput, []resourceIssue, error) {
	return normalizeContext(context.Background(), snapshot)
}

func TestNormalizePreservesRawBytesAndExactNumbersForEveryManagedKind(t *testing.T) {
	resources := []generation.Resource{
		resourceValue("routes", "r1", ` {"id":"r1","priority":9007199254740993,"uri":"/"} `),
		resourceValue("services", "svc1", `{"id":"svc1"}`),
		resourceValue("upstreams", "u1", `{"id":"u1","nodes":{"127.0.0.1:80":1}}`),
		resourceValue("global_rules", "g1", `{"id":"g1","plugins":{}}`),
		resourceValue("plugin_configs", "pc1", `{"plugins":{}}`),
		resourceValue("plugin_metadata", "request-id", `{"value":1}`),
		resourceValue("consumers", "alice", `{"username":"alice","plugins":{}}`),
		resourceValue("consumer_groups", "staff", `{"plugins":{}}`),
		resourceValue("plugins", "plugins", `[{"name":"request-id","stream":false}]`),
		resourceValue("protos", "p1", `{"id":"p1","content":"syntax = proto3;"}`),
		resourceValue("ssls", "cert1", `{"id":"cert1"}`),
		resourceValue("stream_routes", "sr1", `{"id":"sr1","server_port":9000}`),
		resourceValue("secrets", "vault/team", `{"uri":"https://vault.invalid","prefix":"kv","token":"$ENV://TOKEN"}`),
	}
	snapshot := mustGenerationSnapshot(t, 7, resources, nil)
	original := bytes.Clone(resources[0].Value)

	input, issues, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("normalize issues = %v, want none", issues)
	}
	if got, want := input.keys(), []generation.ResourceKey{
		{Kind: "consumer_groups", ID: "staff"},
		{Kind: "consumers", ID: "alice"},
		{Kind: "global_rules", ID: "g1"},
		{Kind: "plugin_configs", ID: "pc1"},
		{Kind: "plugin_metadata", ID: "request-id"},
		{Kind: "plugins", ID: "plugins"},
		{Kind: "protos", ID: "p1"},
		{Kind: "routes", ID: "r1"},
		{Kind: "secrets", ID: "vault/team"},
		{Kind: "services", ID: "svc1"},
		{Kind: "ssls", ID: "cert1"},
		{Kind: "stream_routes", ID: "sr1"},
		{Kind: "upstreams", ID: "u1"},
	}; !slices.Equal(got, want) {
		t.Fatalf("normalized keys = %v, want %v", got, want)
	}
	route := input.resources[generation.ResourceKey{Kind: "routes", ID: "r1"}]
	if !bytes.Equal(route.raw, original) {
		t.Fatalf("normalized raw = %q, want exact %q", route.raw, original)
	}
	object, ok := route.document.(map[string]any)
	if !ok {
		t.Fatalf("exact document type = %T, want map", route.document)
	}
	if number, ok := object["priority"].(apisixjson.Number); !ok || number.String() != "9007199254740993" {
		t.Fatalf("priority = %#v (%T), want exact json.Number", object["priority"], object["priority"])
	}
	resources[0].Value[1] = 'x'
	if !bytes.Equal(route.raw, original) {
		t.Fatal("normalized raw aliases caller input")
	}
}

func TestNormalizeCollectsStableIdentityShapeAndKindIssues(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 8, []generation.Resource{
		resourceValue("routes", "a", `{"id":"same"}`),
		resourceValue("routes", "b", `{"id":"same"}`),
		resourceValue("plugins", "plugins", `{"request-id":true}`),
		resourceValue("secrets", "vault", `{}`),
		resourceValue("unknown", "x", `{}`),
	}, nil)

	_, issues, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := []resourceIssue{
		{Key: generation.ResourceKey{Kind: "plugins", ID: "plugins"}, Code: "malformed-singleton"},
		{Key: generation.ResourceKey{Kind: "routes", ID: "a"}, Code: "duplicate-typed-id"},
		{Key: generation.ResourceKey{Kind: "routes", ID: "a"}, Code: "id-mismatch"},
		{Key: generation.ResourceKey{Kind: "routes", ID: "b"}, Code: "duplicate-typed-id"},
		{Key: generation.ResourceKey{Kind: "routes", ID: "b"}, Code: "id-mismatch"},
		{Key: generation.ResourceKey{Kind: "secrets", ID: "vault"}, Code: "malformed-secret-id"},
		{Key: generation.ResourceKey{Kind: "unknown", ID: "x"}, Code: "unsupported-kind"},
	}
	assertIssueIdentities(t, issues, want)
}

func TestNormalizeKeepsExactLargePositiveIntegerReferenceIDs(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 9, []generation.Resource{
		resourceValue(
			"routes",
			"r",
			`{"id":"r","service_id":1.0,"upstream_id":9.007199254740993e15}`,
		),
	}, nil)
	input, issues, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("normalize issues = %v", issues)
	}
	view := input.resources[generation.ResourceKey{Kind: "routes", ID: "r"}].view
	if view.serviceID != "1" || view.upstreamID != "9007199254740993" {
		t.Fatalf("exact reference IDs = %q/%q", view.serviceID, view.upstreamID)
	}
}

func TestReferenceIDRejectsNonPositiveNumericIDs(t *testing.T) {
	for _, raw := range []apisixjson.Number{"0", "-1", "-1.0"} {
		if got, valid := referenceID(raw); valid {
			t.Fatalf("referenceID(%q) = %q, true; want invalid numeric ID", raw, got)
		}
	}
}

func TestNormalizeRejectsNumericConsumerUsername(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 10, []generation.Resource{
		resourceValue("consumers", "1", `{"username":1,"plugins":{}}`),
	}, nil)
	_, issues, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	assertIssueIdentities(t, issues, []resourceIssue{{
		Key:  generation.ResourceKey{Kind: "consumers", ID: "1"},
		Code: "id-invalid",
	}})
}

func TestNormalizeValidatesOptionalPluginConfigAndConsumerGroupIDs(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 10, []generation.Resource{
		resourceValue("plugin_configs", "pc-a", `{"id":"shared","plugins":{}}`),
		resourceValue("plugin_configs", "pc-b", `{"id":"shared","plugins":{}}`),
		resourceValue("consumer_groups", "staff", `{"id":"other","plugins":{}}`),
		resourceValue("consumer_groups", "without-id", `{"plugins":{}}`),
	}, nil)
	_, issues, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := []resourceIssue{
		{Key: generation.ResourceKey{Kind: "consumer_groups", ID: "staff"}, Code: "id-mismatch"},
		{Key: generation.ResourceKey{Kind: "plugin_configs", ID: "pc-a"}, Code: "duplicate-typed-id"},
		{Key: generation.ResourceKey{Kind: "plugin_configs", ID: "pc-a"}, Code: "id-mismatch"},
		{Key: generation.ResourceKey{Kind: "plugin_configs", ID: "pc-b"}, Code: "duplicate-typed-id"},
		{Key: generation.ResourceKey{Kind: "plugin_configs", ID: "pc-b"}, Code: "id-mismatch"},
	}
	assertIssueIdentities(t, issues, want)
}

func resourceValue(kind, id, value string) generation.Resource {
	return generation.Resource{Key: generation.ResourceKey{Kind: kind, ID: id}, Value: []byte(value)}
}

func mustGenerationSnapshot(
	t *testing.T,
	revision uint64,
	resources []generation.Resource,
	tombstones []generation.Tombstone,
) generation.Snapshot {
	t.Helper()
	snapshot, err := generation.NewSnapshot(revision, resources, tombstones)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertIssueIdentities(t *testing.T, got, want []resourceIssue) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("issues = %v, want %v", got, want)
	}
	for index := range want {
		if got[index].Key != want[index].Key || got[index].Code != want[index].Code || got[index].Err == nil {
			t.Fatalf("issue[%d] = %#v, want key/code %#v", index, got[index], want[index])
		}
	}
}
