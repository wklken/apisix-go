package compiler

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
)

func buildDomainCandidate(
	domain generation.Domain,
	desired generation.Snapshot,
	input normalizedInput,
	issues []resourceIssue,
	previous generation.PublishedGeneration,
	hasPrevious bool,
	manifest *capability.Manifest,
) (generation.PublicationCandidate, error) {
	return buildDomainCandidateContext(
		context.Background(), domain, desired, input, issues, previous, hasPrevious, manifest,
	)
}

func TestDispositionUsesExplicitPredecessorPresenceAndPreservesLastGoodBytes(t *testing.T) {
	oldRoute := []byte(`{"id":"r1","upstream_id":"old-u","uri":"/old"}`)
	previousSnapshot := mustGenerationSnapshot(t, 4, []generation.Resource{
		{Key: generation.ResourceKey{Kind: "routes", ID: "r1"}, Value: oldRoute},
		resourceValue("upstreams", "old-u", `{"id":"old-u"}`),
	}, nil)
	previous := publishedForDomain(generation.DomainHTTP, previousSnapshot)
	desired := mustGenerationSnapshot(t, 11, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"wrong","upstream_id":"missing"}`),
		resourceValue("routes", "first-invalid", `{"id":"first-wrong"}`),
		resourceValue("upstreams", "old-u", `{"id":"old-u"}`),
		resourceValue("upstreams", "bad-u", `{not-json}`),
	}, []generation.Tombstone{{Key: generation.ResourceKey{Kind: "services", ID: "gone"}, Revision: 11}})

	candidate := compileDomain(t, generation.DomainHTTP, desired, previous, true)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		generation.DispositionLastGood,
		"id-mismatch",
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "routes", ID: "first-invalid"},
		generation.DispositionFailClosed,
		"id-mismatch",
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "upstreams", ID: "bad-u"},
		generation.DispositionQuarantined,
		"decode-invalid",
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "services", ID: "gone"},
		generation.DispositionDeleted,
		"explicit-delete",
	)
	got, ok := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "r1"})
	if !ok || !bytes.Equal(got, oldRoute) {
		t.Fatalf("last-good route = %q/%v, want exact %q", got, ok, oldRoute)
	}
	if _, ok := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "first-invalid"}); ok {
		t.Fatal("fail-closed route leaked into candidate")
	}
}

func TestDispositionPrioritizesIntrinsicInvalidityOverMissingDependencyOnFirstStart(t *testing.T) {
	desired := mustGenerationSnapshot(t, 25, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"wrong","upstream_id":"missing"}`),
	}, nil)

	candidate := compileDomain(t, generation.DomainHTTP, desired, generation.PublishedGeneration{}, false)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		generation.DispositionFailClosed,
		"id-mismatch",
	)
}

func TestDependencyClosureRebuildsFromEffectiveBytesAndQuarantinesUnavailableOwner(t *testing.T) {
	previousSnapshot := mustGenerationSnapshot(t, 5, []generation.Resource{
		resourceValue("routes", "last-good", `{"id":"last-good","upstream_id":"kept"}`),
		resourceValue("upstreams", "kept", `{"id":"kept"}`),
	}, nil)
	desired := mustGenerationSnapshot(t, 12, []generation.Resource{
		resourceValue("routes", "last-good", `{"id":"wrong","upstream_id":"rejected-only"}`),
		resourceValue("routes", "unavailable", `{"id":"unavailable","upstream_id":"bad"}`),
		resourceValue("upstreams", "kept", `{"id":"kept"}`),
		resourceValue("upstreams", "bad", `{bad}`),
		resourceValue("upstreams", "rejected-only", `{"id":"rejected-only"}`),
	}, nil)

	candidate := compileDomain(
		t,
		generation.DomainHTTP,
		desired,
		publishedForDomain(generation.DomainHTTP, previousSnapshot),
		true,
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "routes", ID: "last-good"},
		generation.DispositionLastGood,
		"id-mismatch",
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "routes", ID: "unavailable"},
		generation.DispositionQuarantined,
		"dependency-unavailable",
	)
	if _, ok := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "unavailable"}); ok {
		t.Fatal("route retaining unavailable dependency leaked into candidate")
	}
	if _, ok := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "upstreams", ID: "kept"}); !ok {
		t.Fatal("effective last-good dependency was omitted")
	}
}

func TestDependencyClosurePropagatesUnavailableAcrossLongChain(t *testing.T) {
	const chainLength = 64
	resources := make([]generation.Resource, 0, chainLength)
	for index := range chainLength {
		id := fmt.Sprintf("vault/%02d", index)
		dependency := fmt.Sprintf("vault/%02d", index+1)
		if index == chainLength-1 {
			dependency = "vault/missing"
		}
		resources = append(
			resources,
			resourceValue("secrets", id, fmt.Sprintf(`{"next":"$secret://%s/path/value"}`, dependency)),
		)
	}
	desired := mustGenerationSnapshot(t, 24, resources, nil)
	candidate := compileDomain(t, generation.DomainHTTP, desired, generation.PublishedGeneration{}, false)
	for index := range chainLength {
		assertDecision(
			t,
			candidate,
			generation.ResourceKey{Kind: "secrets", ID: fmt.Sprintf("vault/%02d", index)},
			generation.DispositionQuarantined,
			"dependency-unavailable",
		)
	}
	if got := len(candidate.Snapshot.Resources()); got != 0 {
		t.Fatalf("effective resources = %d, want complete chain removal", got)
	}
}

func TestDependencyClosureUsesLastGoodBeforeRebuildingMissingDependencyView(t *testing.T) {
	previousSnapshot := mustGenerationSnapshot(t, 7, []generation.Resource{
		resourceValue("routes", "route", `{"id":"route","upstream_id":"kept"}`),
		resourceValue("upstreams", "kept", `{"id":"kept"}`),
	}, nil)
	desired := mustGenerationSnapshot(t, 16, []generation.Resource{
		resourceValue("routes", "route", `{"id":"route","upstream_id":"missing"}`),
		resourceValue("upstreams", "kept", `{"id":"kept"}`),
	}, nil)

	candidate := compileDomain(
		t, generation.DomainHTTP, desired,
		publishedForDomain(generation.DomainHTTP, previousSnapshot), true,
	)
	assertDecision(
		t, candidate, generation.ResourceKey{Kind: "routes", ID: "route"},
		generation.DispositionLastGood, "dependency-missing",
	)
	raw, found := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "route"})
	if !found || !bytes.Contains(raw, []byte(`"kept"`)) {
		t.Fatalf("last-good route = %q/%v, want predecessor dependency", raw, found)
	}
}

func TestDispositionDefersStreamClientSSLWithoutCrossDomainClosureLeak(t *testing.T) {
	desired := mustGenerationSnapshot(t, 13, []generation.Resource{
		resourceValue("stream_routes", "stream", `{"id":"stream","upstream_id":"tls-upstream"}`),
		resourceValue("upstreams", "tls-upstream", `{"id":"tls-upstream","tls":{"client_cert_id":"http-only-cert"}}`),
		resourceValue("ssls", "http-only-cert", `{"id":"http-only-cert"}`),
	}, nil)

	candidate := compileDomain(t, generation.DomainStream, desired, generation.PublishedGeneration{}, false)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "upstreams", ID: "tls-upstream"},
		generation.DispositionQuarantined,
		"stream-client-ssl-deferred",
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "stream_routes", ID: "stream"},
		generation.DispositionQuarantined,
		"dependency-unavailable",
	)
	for _, key := range candidate.Closure {
		if key.Kind == "ssls" {
			t.Fatalf("HTTP-only SSL leaked into stream closure: %v", key)
		}
	}
}

func TestDispositionPropagatesDeferredStreamInlineClientSSL(t *testing.T) {
	desired := mustGenerationSnapshot(t, 17, []generation.Resource{
		resourceValue(
			"stream_routes",
			"direct",
			`{"id":"direct","upstream":{"tls":{"client_cert_id":"http-only-cert"}}}`,
		),
		resourceValue("stream_routes", "via-service", `{"id":"via-service","service_id":"shared"}`),
		resourceValue("services", "shared", `{"id":"shared","upstream":{"tls":{"client_cert_id":"http-only-cert"}}}`),
		resourceValue("ssls", "http-only-cert", `{"id":"http-only-cert"}`),
	}, nil)

	candidate := compileDomain(t, generation.DomainStream, desired, generation.PublishedGeneration{}, false)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "stream_routes", ID: "direct"},
		generation.DispositionQuarantined,
		"stream-client-ssl-deferred",
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionQuarantined,
		"stream-client-ssl-deferred",
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "stream_routes", ID: "via-service"},
		generation.DispositionQuarantined,
		"dependency-unavailable",
	)
}

func TestDispositionReappliesStreamClientSSLDeferralToLastGoodBytes(t *testing.T) {
	previousSnapshot := mustGenerationSnapshot(t, 14, []generation.Resource{
		resourceValue(
			"services",
			"shared",
			`{"id":"shared","upstream":{"nodes":{},"tls":{"client_cert_id":"http-only-cert"}}}`,
		),
		resourceValue("stream_routes", "stream", `{"id":"stream","service_id":"shared"}`),
	}, nil)
	desired := mustGenerationSnapshot(t, 21, []generation.Resource{
		resourceValue("services", "shared", `{"id":"wrong","upstream":{"nodes":{}}}`),
		resourceValue("stream_routes", "stream", `{"id":"stream","service_id":"shared"}`),
	}, nil)
	candidate := compileDomain(
		t,
		generation.DomainStream,
		desired,
		publishedForDomain(generation.DomainStream, previousSnapshot),
		true,
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionQuarantined,
		"stream-client-ssl-deferred",
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "stream_routes", ID: "stream"},
		generation.DispositionQuarantined,
		"dependency-unavailable",
	)
	if _, found := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "services", ID: "shared"}); found {
		t.Fatal("last-good service with stream client SSL leaked into candidate")
	}
	if _, found := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "ssls", ID: "http-only-cert"}); found {
		t.Fatal("HTTP-only SSL leaked into stream candidate")
	}
}

func TestDispositionFailsClosedWhenSelectedPredecessorBytesCannotDecode(t *testing.T) {
	previousSnapshot := mustGenerationSnapshot(t, 6, []generation.Resource{
		resourceValue("routes", "r1", `{broken-predecessor}`),
	}, nil)
	desired := mustGenerationSnapshot(t, 14, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"wrong"}`),
	}, nil)

	candidate := compileDomain(
		t,
		generation.DomainHTTP,
		desired,
		publishedForDomain(generation.DomainHTTP, previousSnapshot),
		true,
	)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		generation.DispositionFailClosed,
		"effective-invalid",
	)
	if _, ok := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "r1"}); ok {
		t.Fatal("undecodable predecessor bytes leaked into candidate")
	}
}

func TestDependencyClosureDoesNotApplyHTTPOnlyPluginDependenciesToStream(t *testing.T) {
	desired := mustGenerationSnapshot(t, 15, []generation.Resource{
		resourceValue(
			"services",
			"shared",
			`{"id":"shared","plugins":{"grpc-transcode":{"proto_id":"http-only-proto","service":"test.Service","method":"Call"}}}`,
		),
		resourceValue("stream_routes", "stream", `{"id":"stream","service_id":"shared","upstream":{"nodes":{}}}`),
	}, nil)
	compiler := newTestCompiler(t)
	set, err := compiler.PreparePublication(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(
		t,
		set.Domains[generation.DomainHTTP],
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionQuarantined,
		"dependency-unavailable",
	)
	assertDecision(
		t,
		set.Domains[generation.DomainStream],
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionPublished,
		"validated",
	)
	assertDecision(
		t,
		set.Domains[generation.DomainStream],
		generation.ResourceKey{Kind: "stream_routes", ID: "stream"},
		generation.DispositionPublished,
		"validated",
	)
}

func TestDependencyClosureDoesNotLeakHTTPOnlyTrafficSplitEdgesIntoStream(t *testing.T) {
	desired := mustGenerationSnapshot(t, 18, []generation.Resource{
		resourceValue(
			"services",
			"shared",
			`{"id":"shared","plugins":{"traffic-split":{"rules":[{"weighted_upstreams":[{"upstream_id":"missing"}]}]}}}`,
		),
		resourceValue("stream_routes", "stream", `{"id":"stream","service_id":"shared","upstream":{"nodes":{}}}`),
	}, nil)
	set, err := newTestCompiler(t).PreparePublication(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(
		t,
		set.Domains[generation.DomainHTTP],
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionQuarantined,
		"dependency-unavailable",
	)
	assertDecision(
		t,
		set.Domains[generation.DomainStream],
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionPublished,
		"validated",
	)
	assertDecision(
		t,
		set.Domains[generation.DomainStream],
		generation.ResourceKey{Kind: "stream_routes", ID: "stream"},
		generation.DispositionPublished,
		"validated",
	)
}

func TestDependencyClosureDoesNotLeakHTTPOnlyPluginSecretEdgesIntoStream(t *testing.T) {
	desired := mustGenerationSnapshot(t, 19, []generation.Resource{
		resourceValue(
			"services",
			"shared",
			`{"id":"shared","plugins":{"traffic-split":{"credential":"$secret://vault/missing/path/token"}}}`,
		),
		resourceValue("stream_routes", "stream", `{"id":"stream","service_id":"shared","upstream":{"nodes":{}}}`),
	}, nil)
	set, err := newTestCompiler(t).PreparePublication(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(
		t,
		set.Domains[generation.DomainHTTP],
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionQuarantined,
		"dependency-unavailable",
	)
	assertDecision(
		t,
		set.Domains[generation.DomainStream],
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionPublished,
		"validated",
	)
	assertDecision(
		t,
		set.Domains[generation.DomainStream],
		generation.ResourceKey{Kind: "stream_routes", ID: "stream"},
		generation.DispositionPublished,
		"validated",
	)
}

func TestDependencyClosureDoesNotLeakHTTPOnlyPluginIssuesIntoStream(t *testing.T) {
	desired := mustGenerationSnapshot(t, 22, []generation.Resource{
		resourceValue(
			"services",
			"shared",
			`{"id":"shared","plugins":{"traffic-split":{"credential":"$secret://invalid","rules":[]}}}`,
		),
		resourceValue("stream_routes", "stream", `{"id":"stream","service_id":"shared","upstream":{"nodes":{}}}`),
	}, nil)
	set, err := newTestCompiler(t).PreparePublication(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(
		t,
		set.Domains[generation.DomainHTTP],
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionFailClosed,
		"secret-reference-invalid",
	)
	assertDecision(
		t,
		set.Domains[generation.DomainStream],
		generation.ResourceKey{Kind: "services", ID: "shared"},
		generation.DispositionPublished,
		"validated",
	)
	assertDecision(
		t,
		set.Domains[generation.DomainStream],
		generation.ResourceKey{Kind: "stream_routes", ID: "stream"},
		generation.DispositionPublished,
		"validated",
	)
}

func TestDependencyClosureIgnoresLegacyStreamPluginConfigID(t *testing.T) {
	desired := mustGenerationSnapshot(t, 20, []generation.Resource{
		resourceValue(
			"stream_routes",
			"stream",
			`{"id":"stream","plugin_config_id":[],"upstream":{"nodes":{"127.0.0.1:9000":1}}}`,
		),
	}, nil)
	candidate := compileDomain(t, generation.DomainStream, desired, generation.PublishedGeneration{}, false)
	assertDecision(
		t,
		candidate,
		generation.ResourceKey{Kind: "stream_routes", ID: "stream"},
		generation.DispositionPublished,
		"validated",
	)
}

func compileDomain(
	t *testing.T,
	domain generation.Domain,
	desired generation.Snapshot,
	previous generation.PublishedGeneration,
	found bool,
) generation.PublicationCandidate {
	t.Helper()
	input, normalizationIssues, err := normalize(desired)
	if err != nil {
		t.Fatal(err)
	}
	validation := validate(input, mustManifest(t))
	issues := append(normalizationIssues, validation.issuesForDomain(domain)...)
	candidate, err := buildDomainCandidate(domain, desired, input, issues, previous, found, mustManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func publishedForDomain(domain generation.Domain, snapshot generation.Snapshot) generation.PublishedGeneration {
	return generation.PublishedGeneration{
		Artifact: generation.GenerationArtifact{
			Domain: domain, Revision: snapshot.Revision(), Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
	}
}

func assertDecision(
	t *testing.T,
	candidate generation.PublicationCandidate,
	key generation.ResourceKey,
	disposition generation.ResourceDisposition,
	code string,
) {
	t.Helper()
	for _, decision := range candidate.Decisions {
		if decision.Key == key {
			if decision.Disposition != disposition || decision.Code != code {
				t.Fatalf("decision for %v = %#v, want %s/%s", key, decision, disposition, code)
			}
			return
		}
	}
	t.Fatalf("decision for %v is missing", key)
}
