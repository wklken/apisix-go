package compiler

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func TestFinalCandidateCompositeChildOccurrences(t *testing.T) {
	compiler := newTestCompiler(t)
	desired := mustGenerationSnapshot(t, 901, []generation.Resource{
		resourceValue("routes", "composite-route", `{
			"id":"composite-route",
			"plugins":{
				"workflow":{
					"_meta":{"priority":1006},
					"rules":[{"actions":[
						["limit-conn",{"conn":1,"burst":0,"default_conn_delay":0,"key":"remote_addr"}],
						["return",{"code":204}],
						["limit-count",{"count":1,"time_window":60,"key":"$ENV://WORKFLOW_KEY"}]
					]}]
				},
				"multi-auth":{"auth_plugins":[
					{"key-auth":{"header":"X-API-Key"},"basic-auth":{}},
					{"jwt-auth":{}}
				]}
			}
		}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	set, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := compositeSpecsForTest(t, compiler, set.Domains)
	if err != nil {
		t.Fatal(err)
	}
	wantFactories := []string{"basic-auth", "key-auth", "jwt-auth", "limit-conn", "limit-count"}
	wantPositions := []string{
		"auth_plugins[0].basic-auth",
		"auth_plugins[0].key-auth",
		"auth_plugins[1].jwt-auth",
		"rules[0].actions[0]",
		"rules[0].actions[2]",
	}
	if len(got) != len(wantFactories) {
		t.Fatalf("composite children = %#v, want %d", got, len(wantFactories))
	}
	resourceKey := generation.ResourceKey{Kind: "routes", ID: "composite-route"}
	for index, child := range got {
		if child.factory != wantFactories[index] || child.position != wantPositions[index] ||
			child.outer.domain != generation.DomainHTTP || child.outer.resource != resourceKey ||
			child.outer.source != capability.SecretPluginConfig {
			t.Fatalf("child[%d] = %#v", index, child)
		}
	}
	if got[0].outer.factory != "multi-auth" || got[3].outer.factory != "workflow" {
		t.Fatalf("outer factories = %q/%q", got[0].outer.factory, got[3].outer.factory)
	}

	got[1].config["header"] = "mutated"
	again, err := compositeSpecsForTest(t, compiler, set.Domains)
	if err != nil {
		t.Fatal(err)
	}
	if again[1].config["header"] != "X-API-Key" {
		t.Fatalf("child config was not defensively copied: %#v", again[1].config)
	}
}

func TestFinalCandidateCompositeChildOccurrencesRespectOuterMetadataAndFinalState(t *testing.T) {
	tests := map[string]struct {
		raw     string
		wantErr bool
	}{
		"disabled": {
			raw: `{"id":"r1","plugins":{"workflow":{"_meta":{"disable":true},"rules":[{"actions":[["limit-count",{"count":1,"time_window":60}]]}]}}}`,
		},
		"invalid disable": {
			raw:     `{"id":"r1","plugins":{"workflow":{"_meta":{"disable":"yes"},"rules":[{"actions":[["limit-count",{"count":1,"time_window":60}]]}]}}}`,
			wantErr: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			compiler := newTestCompiler(t)
			desired := mustGenerationSnapshot(t, 902, []generation.Resource{
				resourceValue("routes", "r1", test.raw),
			}, nil)
			set, err := compiler.PreparePublication(
				context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			got, err := compositeSpecsForTest(t, compiler, set.Domains)
			if test.wantErr {
				if err == nil {
					t.Fatalf("composite extraction = %#v/nil, want error", got)
				}
				return
			}
			if err != nil || len(got) != 0 {
				t.Fatalf("disabled composite extraction = %#v/%v, want empty", got, err)
			}
		})
	}

	empty := publishedForDomain(
		generation.DomainHTTP,
		mustGenerationSnapshot(t, 903, nil, []generation.Tombstone{{
			Key: generation.ResourceKey{Kind: "routes", ID: "retired"}, Revision: 903,
		}}),
	)
	compiler := newTestCompiler(t)
	got, err := compositeSpecsForTest(
		t,
		compiler,
		map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: generation.PublicationCandidate(empty),
		},
	)
	if err != nil || len(got) != 0 {
		t.Fatalf("tombstone composite extraction = %#v/%v, want empty", got, err)
	}
}

func TestCompositeChildOccurrencesPreserveRouteServiceOrder(t *testing.T) {
	compiler := newTestCompiler(t)
	snapshot := mustGenerationSnapshot(t, 908, []generation.Resource{
		resourceValue("services", "composite-service", `{
			"id":"composite-service",
			"plugins":{"multi-auth":{"auth_plugins":[
				{"wolf-rbac":{"server":"https://wolf.example.com"}},
				{"ldap-auth":{"base_dn":"dc=example,dc=org","ldap_uri":"ldap://127.0.0.1"}},
				{"jwe-decrypt":{"header":"Authorization","forward_header":"X-JWE"}},
				{"hmac-auth":{}},
				{"key-auth":{}},
				{"jwt-auth":{}},
				{"basic-auth":{}}
			]}}
		}`),
		resourceValue("routes", "composite-route", `{
			"id":"composite-route",
			"plugins":{"workflow":{"rules":[{"actions":[
				["limit-count",{"count":1,"time_window":60}],
				["limit-req",{"rate":1,"burst":0,"key":"remote_addr"}]
			]}]}}
		}`),
	}, nil)
	candidates := map[generation.Domain]generation.PublicationCandidate{
		generation.DomainHTTP: generation.PublicationCandidate(
			publishedForDomain(generation.DomainHTTP, snapshot),
		),
	}

	got, err := compositeSpecsForTest(t, compiler, candidates)
	if err != nil {
		t.Fatal(err)
	}
	wantFactories := []string{
		"limit-count", "limit-req",
		"wolf-rbac", "ldap-auth", "jwe-decrypt", "hmac-auth",
		"key-auth", "jwt-auth", "basic-auth",
	}
	wantResources := []generation.ResourceKey{
		{Kind: "routes", ID: "composite-route"},
		{Kind: "routes", ID: "composite-route"},
		{Kind: "services", ID: "composite-service"},
		{Kind: "services", ID: "composite-service"},
		{Kind: "services", ID: "composite-service"},
		{Kind: "services", ID: "composite-service"},
		{Kind: "services", ID: "composite-service"},
		{Kind: "services", ID: "composite-service"},
		{Kind: "services", ID: "composite-service"},
	}
	if len(got) != len(wantFactories) {
		t.Fatalf("composite children = %#v, want %d", got, len(wantFactories))
	}
	for index, child := range got {
		if child.factory != wantFactories[index] || child.outer.resource != wantResources[index] ||
			child.outer.domain != generation.DomainHTTP {
			t.Fatalf("child[%d] = %#v", index, child)
		}
	}
	got[2].config["server"] = "mutated"
	again, err := compositeSpecsForTest(t, compiler, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if again[2].config["server"] != "https://wolf.example.com" {
		t.Fatalf("child config was not defensively copied: %#v", again[2].config)
	}
}

func TestAttemptFactoryIgnoresHTTPOnlyCompositeChildrenInStreamDomain(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 909, []generation.Resource{
		resourceValue("stream_routes", "stream-composite", `{
			"id":"stream-composite",
			"plugins":{"workflow":{"rules":[{"actions":[
				["limit-count",{"count":1,"time_window":60}]
			]}]}}
		}`),
	}, nil)

	t.Run("candidate", func(t *testing.T) {
		broker := &countingScopedBroker{}
		factory, materializer, _, _ := newScopedAttemptFactory(t, broker)
		prepared, err := factory.prepareCandidateAttempt(
			context.Background(), ticketForSnapshot(snapshot, generation.DomainStream), snapshot, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if materializer.candidateCalls != 1 || broker.candidateAuthorizations != 1 {
			t.Fatalf(
				"candidate registrations = %d authorizations = %d, want 1/1",
				materializer.candidateCalls,
				broker.candidateAuthorizations,
			)
		}
		if err := prepared.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBindCompositeChildOccurrencesRequiresExactOuterAuthority(t *testing.T) {
	compiler := newTestCompiler(t)
	desired := mustGenerationSnapshot(t, 904, []generation.Resource{
		resourceValue(
			"routes",
			"r1",
			`{"id":"r1","plugins":{"workflow":{"rules":[{"actions":[["limit-count",{"count":1,"time_window":60}]]}]}}}`,
		),
	}, nil)
	set, err := compiler.PreparePublication(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	topLevel, err := factoryOccurrencesFromCandidates(context.Background(), set.Domains, compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	children, err := compositeChildOccurrenceSpecsFromCandidates(
		context.Background(), set.Domains, topLevel,
	)
	if err != nil {
		t.Fatal(err)
	}
	trace := []string{}
	registration := &recordingFactoryRegistration{id: secret.AttemptID{4}, trace: &trace}
	capabilityValue, err := secret.NewGenerationCapability(registration, 904)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := newPreparationAttempt(904, set.Domains, capabilityValue, topLevel)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindCompositeChildOccurrences(attempt, children)
	if err != nil {
		t.Fatal(err)
	}
	if len(bound) != 1 || !attempt.owns(bound[0].outer) || bound[0].factory != "limit-count" {
		t.Fatalf("bound children = %#v", bound)
	}
	bound[0].config["count"] = 99
	if children[0].config["count"] != jsonNumberForTest(t, "1") {
		t.Fatalf("bound config aliases source: source=%#v bound=%#v", children[0].config, bound[0].config)
	}

	foreign := children[0]
	foreign.outer.resource.ID = "foreign"
	if _, err := bindCompositeChildOccurrences(attempt, []compositeChildOccurrenceSpec{foreign}); err == nil {
		t.Fatal("foreign outer authority bound successfully")
	}
	duplicate := []compositeChildOccurrenceSpec{children[0], children[0]}
	if _, err := bindCompositeChildOccurrences(attempt, duplicate); err == nil {
		t.Fatal("duplicate composite position bound successfully")
	}
}

func TestValidateScopedSecretSupportRejectsNestedBeforeRegistration(t *testing.T) {
	compiler := newUnsupportedNestedPluginTargetTestCompiler(t)
	broker := &countingScopedBroker{}
	factory, materializer, _, trace := newScopedAttemptFactoryWithCompiler(t, compiler, broker)
	snapshot := mustGenerationSnapshot(t, 905, []generation.Resource{
		resourceValue(
			"routes",
			"r1",
			`{"id":"r1","plugins":{"workflow":{"rules":[{"actions":[["limit-req",{"rate":1,"burst":0,"key":"$ENV://NESTED_KEY"}]]}]}}}`,
		),
	}, nil)

	_, candidateErr := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(snapshot, generation.DomainHTTP), snapshot, nil,
	)
	if !errors.Is(candidateErr, errAttemptPreparationFailed) || materializer.candidateCalls != 0 {
		t.Fatalf("candidate nested support = %v registrations=%d", candidateErr, materializer.candidateCalls)
	}
	if broker.candidateAuthorizations != 0 || broker.recoveryAuthorizations != 0 ||
		broker.resolveCalls != 0 || broker.revokeCalls != 0 || len(*trace) != 0 {
		t.Fatalf(
			"nested support reached side effects: broker=%#v trace=%v",
			broker,
			*trace,
		)
	}
}

func TestPrepareCompilerDiscardSecretsOwnsNestedMultiAuthCompatibilityFields(t *testing.T) {
	compiler := newTestCompiler(t)
	broker := &discardPreparationBroker{}
	desired := mustGenerationSnapshot(t, 907, []generation.Resource{
		resourceValue("routes", "nested-auth", `{"id":"nested-auth","plugins":{"multi-auth":{"auth_plugins":[
			{"basic-auth":{"password":"$ENV://BASIC_NESTED"}},
			{"key-auth":{"key":"$ENV://KEY_NESTED"}},
			{"jwt-auth":{"secret":"$ENV://JWT_NESTED","private_key":"$ENV://JWT_KEY_NESTED"}},
			{"hmac-auth":{"secret":"$ENV://HMAC_NESTED"}},
			{"ldap-auth":{"base_dn":"dc=example,dc=org","ldap_uri":"ldap://127.0.0.1","user_dn":"$ENV://LDAP_NESTED"}},
			{"jwe-decrypt":{"header":"Authorization","forward_header":"X-JWE","key":"$ENV://JWE_KEY_NESTED","secret":"$ENV://JWE_SECRET_NESTED"}},
			{"wolf-rbac":{"server":"https://wolf.example.com"}}
		]}}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	set, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	materializer := testutil.NewSecretMaterializer(broker, compiler.schemas.catalog)
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Close(context.Background()) })
	topLevel, err := factoryOccurrencesFromCandidates(context.Background(), set.Domains, compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	childSpecs, err := compositeChildOccurrenceSpecsFromCandidates(
		context.Background(), set.Domains, topLevel,
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, desired.Revision())
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := newPreparationAttempt(desired.Revision(), set.Domains, capabilityValue, topLevel)
	if err != nil {
		t.Fatal(err)
	}
	boundChildren, err := bindCompositeChildOccurrences(attempt, childSpecs)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := attempt.Candidate(generation.DomainHTTP)
	beforeRaw, _ := before.Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "nested-auth"})
	if err := prepareCompilerDiscardSecrets(
		context.Background(), attempt, compiler.schemas.catalog, boundChildren...,
	); err != nil {
		t.Fatal(err)
	}

	scopes, raws := broker.calls()
	wantRaw := []string{
		"$ENV://BASIC_NESTED", "$ENV://KEY_NESTED", "$ENV://JWT_KEY_NESTED", "$ENV://JWT_NESTED",
		"$ENV://HMAC_NESTED", "$ENV://LDAP_NESTED", "$ENV://JWE_KEY_NESTED", "$ENV://JWE_SECRET_NESTED",
	}
	if !slices.Equal(raws, wantRaw) {
		t.Fatalf("nested discard raws = %#v, want %#v", raws, wantRaw)
	}
	wantFactories := []string{
		"basic-auth", "key-auth", "jwt-auth", "jwt-auth",
		"hmac-auth", "ldap-auth", "jwe-decrypt", "jwe-decrypt",
	}
	wantFields := []string{
		"password", "key", "private_key", "secret",
		"secret", "user_dn", "key", "secret",
	}
	if len(scopes) != len(wantFactories) {
		t.Fatalf("nested discard scopes = %#v, want %d", scopes, len(wantFactories))
	}
	for index, scope := range scopes {
		if scope.Resource != (generation.ResourceKey{Kind: "routes", ID: "nested-auth"}) ||
			scope.Source != capability.SecretPluginConfig || scope.Domain != generation.DomainHTTP ||
			scope.Attempt != attempt.AttemptID() || scope.Generation != desired.Revision() ||
			scope.Plugin != wantFactories[index] || scope.Field != wantFields[index] {
			t.Fatalf("nested discard scope[%d] = %#v", index, scope)
		}
	}
	after, _ := attempt.Candidate(generation.DomainHTTP)
	afterRaw, _ := after.Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "nested-auth"})
	if !bytes.Equal(beforeRaw, afterRaw) {
		t.Fatalf("nested discard mutated final candidate: before=%s after=%s", beforeRaw, afterRaw)
	}
}

func newUnsupportedNestedPluginTargetTestCompiler(t *testing.T) *Compiler {
	t.Helper()
	manifest := mustManifest(t)
	found := false
	for index := range manifest.Plugins {
		entry := &manifest.Plugins[index]
		for _, factory := range entry.Factories {
			if factory.Key != "limit-req" {
				continue
			}
			entry.SecretDeclarations = append(entry.SecretDeclarations, capability.SecretDeclaration{
				Factory: "limit-req", Source: capability.SecretPluginConfig, Field: "key",
			})
			found = true
		}
	}
	if !found {
		t.Fatal("limit-req manifest entry is unavailable")
	}
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return compiler
}

func jsonNumberForTest(t *testing.T, value string) any {
	t.Helper()
	document, err := decodeExactDocument([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func compositeSpecsForTest(
	t *testing.T,
	compiler *Compiler,
	candidates map[generation.Domain]generation.PublicationCandidate,
) ([]compositeChildOccurrenceSpec, error) {
	t.Helper()
	occurrences, err := factoryOccurrencesFromCandidates(context.Background(), candidates, compiler.schemas)
	if err != nil {
		return nil, err
	}
	return compositeChildOccurrenceSpecsFromCandidates(context.Background(), candidates, occurrences)
}
