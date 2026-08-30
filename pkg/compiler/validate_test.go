package compiler

import (
	"context"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
)

func validate(input normalizedInput) validationResult {
	result, err := validateContext(context.Background(), input, nil)
	if err != nil {
		panic(err)
	}
	return result
}

func TestValidateUsesEffectiveUpstreamPrecedenceAndFindsProtocolDependencies(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 9, []generation.Resource{
		resourceValue(
			"routes",
			"r-inline",
			`{"id":"r-inline","service_id":"svc","upstream_id":"route-u","upstream":{"nodes":{"inline:80":1}},"plugin_config_id":"pc"}`,
		),
		resourceValue(
			"routes",
			"r-empty-inline",
			`{"id":"r-empty-inline","service_id":"svc","upstream_id":"route-u","upstream":{}}`,
		),
		resourceValue("routes", "r-service", `{"id":"r-service","service_id":"svc"}`),
		resourceValue("services", "svc", `{"id":"svc","upstream_id":"service-u"}`),
		resourceValue("upstreams", "service-u", `{"id":"service-u","tls":{"client_cert_id":"ssl-service"}}`),
		resourceValue(
			"plugin_configs",
			"pc",
			`{"plugins":{"grpc-transcode":{"proto_id":"proto-1"},"traffic-split":{"rules":[{"weighted_upstreams":[{"upstream_id":"split-u"},{"upstream":{"tls":{"client_cert_id":"ssl-inline"}}}] }]}}}`,
		),
		resourceValue("upstreams", "split-u", `{"id":"split-u"}`),
		resourceValue("protos", "proto-1", `{"id":"proto-1"}`),
		resourceValue("ssls", "ssl-service", `{"id":"ssl-service"}`),
		resourceValue("ssls", "ssl-inline", `{"id":"ssl-inline"}`),
		resourceValue("consumers", "alice", `{"username":"alice","group_id":"staff"}`),
		resourceValue("consumer_groups", "staff", `{"plugins":{}}`),
		resourceValue("secrets", "vault/a", `{"token":"$secret://vault/b/path/token"}`),
		resourceValue("secrets", "vault/b", `{}`),
		resourceValue(
			"plugin_metadata",
			"opaque",
			`{"plugins":{"token":"$secret://vault/b/path/token"}}`,
		),
	}, nil)
	input, _, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	result := validate(input)
	if len(result.issues) != 0 {
		t.Fatalf("validation issues = %v", result.issues)
	}
	assertEdges(t, result.graph.edges[generation.ResourceKey{Kind: "routes", ID: "r-inline"}], []generation.ResourceKey{
		{Kind: "plugin_configs", ID: "pc"},
		{Kind: "services", ID: "svc"},
	})
	assertEdges(
		t,
		result.graph.edges[generation.ResourceKey{Kind: "routes", ID: "r-service"}],
		[]generation.ResourceKey{
			{Kind: "services", ID: "svc"},
			{Kind: "upstreams", ID: "service-u"},
		},
	)
	assertEdges(
		t,
		result.graph.edges[generation.ResourceKey{Kind: "routes", ID: "r-empty-inline"}],
		[]generation.ResourceKey{
			{Kind: "services", ID: "svc"},
		},
	)
	assertEdges(
		t,
		result.graph.edges[generation.ResourceKey{Kind: "plugin_metadata", ID: "opaque"}],
		[]generation.ResourceKey{{Kind: "secrets", ID: "vault/b"}},
	)
	assertEdges(
		t,
		result.graph.edges[generation.ResourceKey{Kind: "plugin_configs", ID: "pc"}],
		[]generation.ResourceKey{
			{Kind: "protos", ID: "proto-1"},
			{Kind: "ssls", ID: "ssl-inline"},
			{Kind: "upstreams", ID: "split-u"},
		},
	)
	assertEdges(
		t,
		result.graph.edges[generation.ResourceKey{Kind: "upstreams", ID: "service-u"}],
		[]generation.ResourceKey{
			{Kind: "ssls", ID: "ssl-service"},
		},
	)
	assertEdges(t, result.graph.edges[generation.ResourceKey{Kind: "consumers", ID: "alice"}], []generation.ResourceKey{
		{Kind: "consumer_groups", ID: "staff"},
	})
	assertEdges(t, result.graph.edges[generation.ResourceKey{Kind: "secrets", ID: "vault/a"}], []generation.ResourceKey{
		{Kind: "secrets", ID: "vault/b"},
	})
}

func TestValidateReportsMissingMalformedSecretAndDeterministicCycles(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 10, []generation.Resource{
		resourceValue("routes", "missing", `{"id":"missing","upstream_id":"absent","plugins":{"request-id":{}}}`),
		resourceValue("secrets", "vault/a", `{"token":"$secret://vault/b/path/token"}`),
		resourceValue("secrets", "vault/b", `{"token":"$secret://vault/a/path/token"}`),
		resourceValue("plugin_metadata", "request-id", `{"bad":"$secret://vault-only"}`),
	}, nil)
	input, _, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	result := validate(input)
	want := []resourceIssue{
		{Key: generation.ResourceKey{Kind: "plugin_metadata", ID: "request-id"}, Code: "secret-reference-invalid"},
		{Key: generation.ResourceKey{Kind: "routes", ID: "missing"}, Code: "dependency-missing"},
		{Key: generation.ResourceKey{Kind: "secrets", ID: "vault/a"}, Code: "dependency-cycle"},
		{Key: generation.ResourceKey{Kind: "secrets", ID: "vault/b"}, Code: "dependency-cycle"},
	}
	assertIssueIdentities(t, result.issues, want)
}

func TestValidateRejectsMalformedNestedStructuralReferenceIDs(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 11, []generation.Resource{
		resourceValue("plugin_configs", "grpc", `{"plugins":{"grpc-transcode":{"proto_id":""}}}`),
		resourceValue(
			"plugin_configs",
			"traffic",
			`{"plugins":{"traffic-split":{"rules":[{"weighted_upstreams":[{"upstream_id":0}]}]}}}`,
		),
		resourceValue(
			"plugin_configs",
			"traffic-string-zero",
			`{"plugins":{"traffic-split":{"rules":[{"weighted_upstreams":[{"upstream_id":"0"}]}]}}}`,
		),
		resourceValue("upstreams", "tls", `{"id":"tls","tls":{"client_cert_id":[]}}`),
		resourceValue("upstreams", "0", `{"id":"0"}`),
		resourceValue("upstreams", "trimmed-tls", `{"id":"trimmed-tls","tls":{"client_cert_id":" cert "}}`),
		resourceValue("ssls", "cert", `{"id":"cert"}`),
	}, nil)
	input, _, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	result := validate(input)
	want := []resourceIssue{
		{Key: generation.ResourceKey{Kind: "plugin_configs", ID: "grpc"}, Code: "reference-invalid"},
		{Key: generation.ResourceKey{Kind: "plugin_configs", ID: "traffic"}, Code: "reference-invalid"},
		{Key: generation.ResourceKey{Kind: "upstreams", ID: "tls"}, Code: "reference-invalid"},
	}
	assertIssueIdentities(t, result.issues, want)
	assertEdges(
		t,
		result.graph.edges[generation.ResourceKey{Kind: "plugin_configs", ID: "traffic-string-zero"}],
		[]generation.ResourceKey{{Kind: "upstreams", ID: "0"}},
	)
	assertEdges(
		t,
		result.graph.edges[generation.ResourceKey{Kind: "upstreams", ID: "trimmed-tls"}],
		[]generation.ResourceKey{{Kind: "ssls", ID: "cert"}},
	)
}

func TestValidateCanonicalizesSameCodePluginIssuesByErrorMessage(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 12, []generation.Resource{
		resourceValue(
			"plugin_configs",
			"both",
			`{"plugins":{"traffic-split":{"rules":[{"weighted_upstreams":[{"upstream_id":0}]}]},"grpc-transcode":{"proto_id":[]}}}`,
		),
	}, nil)
	input, _, err := normalize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	result := validate(input)
	if len(result.issues) != 1 || result.issues[0].Code != "reference-invalid" ||
		result.issues[0].Err.Error() != "grpc-transcode proto_id is invalid" {
		t.Fatalf("canonical same-code issue = %#v", result.issues)
	}
}

func assertEdges(t *testing.T, got, want []generation.ResourceKey) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}
}
