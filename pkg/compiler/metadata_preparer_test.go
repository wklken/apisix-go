package compiler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type metadataPreparationBroker struct {
	resolveCalls  int
	scopes        []secret.Scope
	raws          []string
	resolved      string
	resolvedByRaw map[string]string
	resolveErr    error
}

func (broker *metadataPreparationBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.resolveCalls++
	broker.scopes = append(broker.scopes, scope)
	broker.raws = append(broker.raws, raw)
	if broker.resolveErr != nil {
		return "", broker.resolveErr
	}
	if broker.resolved != "" {
		return broker.resolved, nil
	}
	if resolved, ok := broker.resolvedByRaw[raw]; ok {
		return resolved, nil
	}
	return "resolved:" + raw, nil
}

func TestMetadataPreparerUsesExactFinalPublishedDocuments(t *testing.T) {
	compiler := newTestCompiler(t)
	raw := []byte(` { "nodes": [{"host":"127.0.0.1","port":8000}], "mode":"monitor" } `)
	snapshot := mustGenerationSnapshot(t, 101, []generation.Resource{
		resourceValue("plugin_metadata", "chaitin-waf", string(raw)),
	}, []generation.Tombstone{{
		Key: generation.ResourceKey{Kind: "plugin_metadata", ID: "retired"}, Revision: 100,
	}})
	ticket, set := metadataPublicationSet(snapshot)
	broker := &metadataPreparationBroker{}
	attempt, registration := registerMetadataCandidate(t, compiler, broker, ticket, set)
	t.Cleanup(func() { closeMetadataRegistration(t, registration) })

	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	view, err := preparer.PrepareMetadata(context.Background(), attempt)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	found, err := view.Decode("chaitin-waf", &got)
	if err != nil || !found {
		t.Fatalf("Decode() = (%v, %v), want metadata document", found, err)
	}
	if got["mode"] != "monitor" {
		t.Fatalf("decoded mode = %#v, want monitor", got["mode"])
	}
	if found, err := view.Decode("retired", &got); err != nil || found {
		t.Fatalf("tombstoned metadata Decode() = (%v, %v), want absent", found, err)
	}
	if broker.resolveCalls != 0 {
		t.Fatalf("secret resolver calls = %d, want zero for chaitin metadata", broker.resolveCalls)
	}
	candidate, ok := attempt.Candidate(generation.DomainHTTP)
	if !ok {
		t.Fatal("HTTP candidate is missing")
	}
	gotRaw, ok := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "plugin_metadata", ID: "chaitin-waf"})
	if !ok || !bytes.Equal(gotRaw, raw) {
		t.Fatalf("candidate metadata bytes = %q, want original %q", gotRaw, raw)
	}
}

func TestMetadataPreparerRejectsMissingDuplicateOrForeignOccurrence(t *testing.T) {
	compiler := newTestCompiler(t)
	snapshot := metadataSnapshot(t, 102, `{"nodes":[{"host":"127.0.0.1","port":8000}]}`)
	ticket := ticketForSnapshot(snapshot, generation.DomainHTTP)
	set, err := compiler.PreparePublication(context.Background(), ticket, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(PreparationGeneration) PreparationGeneration{
		"missing": func(preparation PreparationGeneration) PreparationGeneration {
			preparation.occurrences = nil
			return preparation
		},
		"duplicate": func(preparation PreparationGeneration) PreparationGeneration {
			occurrence := preparation.Occurrences(capability.SecretPluginMetadata)[0]
			preparation.occurrences = append(preparation.occurrences, occurrence)
			return preparation
		},
		"foreign resource": func(preparation PreparationGeneration) PreparationGeneration {
			occurrence := preparation.Occurrences(capability.SecretPluginMetadata)[0]
			occurrence.resource.ID = "foreign"
			preparation.occurrences = []FactoryOccurrence{occurrence}
			return preparation
		},
		"stream tamper": func(preparation PreparationGeneration) PreparationGeneration {
			occurrence := preparation.Occurrences(capability.SecretPluginMetadata)[0]
			occurrence.domain = generation.DomainStream
			preparation.occurrences = []FactoryOccurrence{occurrence}
			return preparation
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			broker := &metadataPreparationBroker{}
			attempt, registration := registerMetadataCandidate(t, compiler, broker, ticket, set)
			defer closeMetadataRegistration(t, registration)
			attempt = mutate(attempt)
			if _, err := preparer.PrepareMetadata(
				context.Background(),
				attempt,
			); !errors.Is(
				err,
				errMetadataPreparationFailed,
			) {
				t.Fatalf("PrepareMetadata() error = %v, want stable preparation failure", err)
			}
			if broker.resolveCalls != 0 {
				t.Fatalf("resolver calls = %d, want zero before occurrence validation", broker.resolveCalls)
			}
		})
	}
}

func TestMetadataPreparerReturnsEmptyViewForStreamOnlyAttempt(t *testing.T) {
	compiler := newTestCompiler(t)
	snapshot := mustGenerationSnapshot(t, 103, []generation.Resource{
		resourceValue("stream_routes", "stream-1", `{"id":"stream-1"}`),
	}, nil)
	streamCandidate := publishedForDomain(generation.DomainStream, snapshot)
	registration := &metadataTestRegistration{}
	attempt := newMetadataPreparationGeneration(
		t,
		compiler,
		snapshot.Revision(),
		map[generation.Domain]generation.PublicationCandidate{
			generation.DomainStream: generation.PublicationCandidate(streamCandidate),
		},
		registration,
	)
	t.Cleanup(func() { closeMetadataRegistration(t, registration) })
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	view, err := preparer.PrepareMetadata(context.Background(), attempt)
	if err != nil {
		t.Fatal(err)
	}
	var target map[string]any
	if found, err := view.Decode("stream-1", &target); err != nil || found {
		t.Fatalf("stream-only metadata Decode() = (%v, %v), want empty view", found, err)
	}
	if registration.materializeCalls != 0 {
		t.Fatalf("stream-only materialization calls = %d, want zero", registration.materializeCalls)
	}
}

func TestMetadataPreparerMaterializesAzureMetadataWithExactOccurrence(t *testing.T) {
	compiler := newTestCompiler(t)
	raw := `{"master_apikey":"$ENV://AZURE_MASTER_APIKEY","master_clientid":"client-n"}`
	snapshot := metadataSnapshot(t, 104, raw)
	ticket, set := metadataPublicationSet(snapshot)
	broker := &metadataPreparationBroker{resolved: "azure-secret"}
	attempt, registration := registerMetadataCandidate(t, compiler, broker, ticket, set)
	t.Cleanup(func() { closeMetadataRegistration(t, registration) })
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	view, err := preparer.PrepareMetadata(context.Background(), attempt)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	found, err := view.Decode("azure-functions", &got)
	if err != nil || !found {
		t.Fatalf("Decode() = (%v, %v), want Azure metadata", found, err)
	}
	if got["master_apikey"] != "azure-secret" || got["master_clientid"] != "client-n" {
		t.Fatalf("resolved Azure metadata = %#v", got)
	}
	var sibling map[string]any
	if found, err := view.ForFactory("chaitin-waf").Decode("azure-functions", &sibling); err != nil || found {
		t.Fatalf("foreign factory Decode() = (%v, %v), want scoped metadata hidden", found, err)
	}
	candidate, ok := attempt.Candidate(generation.DomainHTTP)
	if !ok {
		t.Fatal("Azure candidate is missing")
	}
	gotRaw, ok := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "plugin_metadata", ID: "azure-functions"})
	if !ok || !bytes.Equal(gotRaw, []byte(raw)) {
		t.Fatalf("Azure candidate bytes = %q, want original %q", gotRaw, raw)
	}
	if broker.resolveCalls != 1 || len(broker.scopes) != 1 || len(broker.raws) != 1 {
		t.Fatalf(
			"materialization calls = %d scopes=%#v raws=%#v, want one exact call",
			broker.resolveCalls,
			broker.scopes,
			broker.raws,
		)
	}
	scope := broker.scopes[0]
	if scope.Generation != snapshot.Revision() || scope.Generation != attempt.Generation() ||
		scope.Domain != generation.DomainHTTP ||
		scope.Resource != (generation.ResourceKey{Kind: "plugin_metadata", ID: "azure-functions"}) ||
		scope.Plugin != "azure-functions" || scope.Source != capability.SecretPluginMetadata ||
		scope.Field != "master_apikey" || broker.raws[0] != "$ENV://AZURE_MASTER_APIKEY" {
		t.Fatalf("materialization scope/raw = %#v/%q", scope, broker.raws[0])
	}
}

func TestMetadataPreparerMaterializationFailureIsRedacted(t *testing.T) {
	compiler := newTestCompiler(t)
	snapshot := mustGenerationSnapshot(t, 106, []generation.Resource{
		resourceValue("plugin_metadata", "azure-functions", `{"master_apikey":"$secret://vault/private/path"}`),
		resourceValue("secrets", "vault/private", `{}`),
	}, nil)
	ticket, set := metadataPublicationSet(snapshot)
	registration := &metadataTestRegistration{
		materializeErr: fmt.Errorf("resolver exposed VAULT_PATH_DO_NOT_LEAK and plaintext-should-not-leak"),
	}
	attempt := newMetadataPreparationGeneration(t, compiler, snapshot.Revision(), set.Domains, registration)
	t.Cleanup(func() { closeMetadataRegistration(t, registration) })
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	_, err = preparer.PrepareMetadata(context.Background(), attempt)
	if !errors.Is(err, errMetadataPreparationFailed) {
		t.Fatalf("PrepareMetadata() error = %v, want stable preparation failure", err)
	}
	for _, sensitive := range []string{"VAULT_PATH_DO_NOT_LEAK", "$secret://vault/private/path", "plaintext-should-not-leak"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("preparation error leaked %q: %v", sensitive, err)
		}
	}

	t.Run("scoped broker", func(t *testing.T) {
		broker := &metadataPreparationBroker{
			resolveErr: fmt.Errorf("resolver exposed VAULT_PATH_DO_NOT_LEAK and plaintext-should-not-leak"),
		}
		brokerAttempt, brokerRegistration := registerMetadataCandidate(t, compiler, broker, ticket, set)
		defer closeMetadataRegistration(t, brokerRegistration)
		_, brokerErr := preparer.PrepareMetadata(context.Background(), brokerAttempt)
		if !errors.Is(brokerErr, errMetadataPreparationFailed) {
			t.Fatalf("scoped broker error = %v, want stable preparation failure", brokerErr)
		}
		if broker.resolveCalls != 1 || !slices.Equal(broker.raws, []string{"$secret://vault/private/path"}) {
			t.Fatalf(
				"scoped broker calls = %d raws=%#v, want one exact sensitive reference",
				broker.resolveCalls,
				broker.raws,
			)
		}
		for _, sensitive := range []string{"VAULT_PATH_DO_NOT_LEAK", "$secret://vault/private/path", "plaintext-should-not-leak"} {
			if strings.Contains(brokerErr.Error(), sensitive) {
				t.Fatalf("scoped broker error leaked %q: %v", sensitive, brokerErr)
			}
		}
	})
}

func TestMetadataPreparerReturnsEmptyViewForNoPublishedMetadata(t *testing.T) {
	compiler := newTestCompiler(t)
	snapshot := mustGenerationSnapshot(t, 107, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1"}`),
	}, nil)
	ticket := ticketForSnapshot(snapshot, generation.DomainHTTP)
	set, err := compiler.PreparePublication(context.Background(), ticket, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	registration := &metadataTestRegistration{}
	attempt := newMetadataPreparationGeneration(t, compiler, snapshot.Revision(), set.Domains, registration)
	t.Cleanup(func() { closeMetadataRegistration(t, registration) })
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	view, err := preparer.PrepareMetadata(context.Background(), attempt)
	if err != nil {
		t.Fatal(err)
	}
	var target map[string]any
	if found, err := view.Decode("chaitin-waf", &target); err != nil || found {
		t.Fatalf("no-metadata Decode() = (%v, %v), want empty view", found, err)
	}
	if registration.materializeCalls != 0 {
		t.Fatalf("no-metadata materialization calls = %d, want zero", registration.materializeCalls)
	}
}

func TestMetadataPreparerHonorsCancellationBeforeResolverAccess(t *testing.T) {
	compiler := newTestCompiler(t)
	snapshot := metadataSnapshot(t, 108, `{"master_apikey":"$ENV://CANCELED"}`)
	ticket, set := metadataPublicationSet(snapshot)
	broker := &metadataPreparationBroker{resolved: "must-not-resolve"}
	attempt, registration := registerMetadataCandidate(t, compiler, broker, ticket, set)
	t.Cleanup(func() { closeMetadataRegistration(t, registration) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.PrepareMetadata(ctx, attempt); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled PrepareMetadata() error = %v, want context.Canceled", err)
	}
	if broker.resolveCalls != 0 {
		t.Fatalf("canceled resolver calls = %d, want zero", broker.resolveCalls)
	}
}

func TestMetadataPreparerRejectsNonStringDeclaredTerminalsBeforeResolver(t *testing.T) {
	compiler := newTestCompiler(t)
	compiled, err := util.CompileSchema(`{
		"type":"object",
		"properties":{"master_apikey":{}}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	entry := compiler.schemas.factories["azure-functions"]
	entry.metadata = compiled
	compiler.schemas.factories["azure-functions"] = entry
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []string{
		`{"master_apikey":{}}`,
		`{"master_apikey":[]}`,
		`{"master_apikey":{"nested":{}}}`,
	} {
		t.Run(document, func(t *testing.T) {
			snapshot := metadataSnapshot(t, 109, document)
			registration := &metadataTestRegistration{}
			attempt := newMetadataPreparationGeneration(
				t,
				compiler,
				snapshot.Revision(),
				map[generation.Domain]generation.PublicationCandidate{
					generation.DomainHTTP: generation.PublicationCandidate(
						publishedForDomain(generation.DomainHTTP, snapshot),
					),
				},
				registration,
			)
			defer closeMetadataRegistration(t, registration)
			if _, err := preparer.PrepareMetadata(
				context.Background(),
				attempt,
			); !errors.Is(
				err,
				errMetadataPreparationFailed,
			) {
				t.Fatalf("non-string metadata error = %v, want stable preparation failure", err)
			}
			if registration.materializeCalls != 0 {
				t.Fatalf("non-string metadata materialization calls = %d, want zero", registration.materializeCalls)
			}
		})
	}
}

func TestMetadataPreparerMaterializesErrorLogWildcardMetadataForArrayAndMap(t *testing.T) {
	compiler := newTestCompiler(t)
	installErrorLogMetadataSchema(t, compiler)
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		document   string
		rawRefs    []string
		wantValues []string
	}{
		{
			name: "array",
			document: `{"kafka":{"brokers":[
				{"sasl_config":{"password":"$ENV://ARRAY_A"}},
				{"sasl_config":{"password":"$ENV://ARRAY_B"}}
			]}}`,
			rawRefs:    []string{"$ENV://ARRAY_A", "$ENV://ARRAY_B"},
			wantValues: []string{"array-secret-a", "array-secret-b"},
		},
		{
			name:       "map",
			document:   `{"kafka":{"brokers":{"first":{"sasl_config":{"password":"$ENV://MAP_ONLY"}}}}}`,
			rawRefs:    []string{"$ENV://MAP_ONLY"},
			wantValues: []string{"map-secret"},
		},
		{
			name:       "case insensitive terminal",
			document:   `{"kafka":{"brokers":[{"sasl_config":{"PASSWORD":"$ENV://CASE_INSENSITIVE"}}]}}`,
			rawRefs:    []string{"$ENV://CASE_INSENSITIVE"},
			wantValues: []string{"case-secret"},
		},
	}
	for index, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := errorLogMetadataSnapshot(t, uint64(110+index), tt.document)
			ticket, set := metadataPublicationSet(snapshot)
			resolvedByRaw := make(map[string]string, len(tt.rawRefs))
			for index, raw := range tt.rawRefs {
				resolvedByRaw[raw] = tt.wantValues[index]
			}
			broker := &metadataPreparationBroker{resolvedByRaw: resolvedByRaw}
			attempt, registration := registerMetadataCandidate(t, compiler, broker, ticket, set)
			defer closeMetadataRegistration(t, registration)
			view, err := preparer.PrepareMetadata(context.Background(), attempt)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if found, err := view.Decode("error-log-logger", &got); err != nil || !found {
				t.Fatalf("metadata Decode() = (%v, %v), want present", found, err)
			}
			if gotValues := errorLogMetadataPasswords(got); !slices.Equal(gotValues, tt.wantValues) {
				t.Fatalf("resolved wildcard passwords = %#v, want %#v", gotValues, tt.wantValues)
			}
			if !slices.Equal(broker.raws, tt.rawRefs) {
				t.Fatalf("materialized raw references = %#v, want %#v", broker.raws, tt.rawRefs)
			}
			if len(broker.scopes) != len(tt.rawRefs) {
				t.Fatalf("materialization scopes = %#v, want %d calls", broker.scopes, len(tt.rawRefs))
			}
			for _, scope := range broker.scopes {
				if scope.Generation != snapshot.Revision() || scope.Generation != attempt.Generation() ||
					scope.Domain != generation.DomainHTTP ||
					scope.Resource != (generation.ResourceKey{Kind: "plugin_metadata", ID: "error-log-logger"}) ||
					scope.Plugin != "error-log-logger" || scope.Source != capability.SecretPluginMetadata ||
					scope.Field != "kafka.brokers.*.sasl_config.password" {
					t.Fatalf("wildcard materialization scope = %#v", scope)
				}
			}
		})
	}
}

func TestMetadataPreparerKeepsMissingAndIntermediateMetadataPathsAsNoOp(t *testing.T) {
	compiler := newTestCompiler(t)
	installErrorLogMetadataSchema(t, compiler)
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing brokers":             `{"kafka":{}}`,
		"missing wildcard elements":   `{"kafka":{"brokers":[]}}`,
		"missing sasl config":         `{"kafka":{"brokers":[{}]}}`,
		"missing password":            `{"kafka":{"brokers":[{"sasl_config":{}}]}}`,
		"intermediate shape mismatch": `{"kafka":{"brokers":"not-an-array"}}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := errorLogMetadataSnapshot(t, 120, document)
			_, set := metadataPublicationSet(snapshot)
			registration := &metadataTestRegistration{}
			attempt := newMetadataPreparationGeneration(t, compiler, snapshot.Revision(), set.Domains, registration)
			defer closeMetadataRegistration(t, registration)
			view, err := preparer.PrepareMetadata(context.Background(), attempt)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if found, err := view.Decode("error-log-logger", &got); err != nil || !found {
				t.Fatalf("metadata Decode() = (%v, %v), want present", found, err)
			}
			if registration.materializeCalls != 0 {
				t.Fatalf("no-op metadata materialization calls = %d, want zero", registration.materializeCalls)
			}
		})
	}
}

func TestMetadataPreparerRejectsErrorLogWildcardContainersBeforeResolver(t *testing.T) {
	compiler := newTestCompiler(t)
	installErrorLogMetadataSchema(t, compiler)
	preparer, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"array object": `{"kafka":{"brokers":[{"sasl_config":{"password":{}}}]}}`,
		"array array":  `{"kafka":{"brokers":[{"sasl_config":{"password":[]}}]}}`,
		"map object":   `{"kafka":{"brokers":{"first":{"sasl_config":{"password":{}}}}}}`,
		"map array":    `{"kafka":{"brokers":{"first":{"sasl_config":{"password":[]}}}}}`,
		"mixed array":  `{"kafka":{"brokers":[{"sasl_config":{"password":"$ENV://VALID"}},{"sasl_config":{"password":{}}}]}}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := errorLogMetadataSnapshot(t, 130, document)
			_, set := metadataPublicationSet(snapshot)
			registration := &metadataTestRegistration{}
			attempt := newMetadataPreparationGeneration(t, compiler, snapshot.Revision(), set.Domains, registration)
			defer closeMetadataRegistration(t, registration)
			if _, err := preparer.PrepareMetadata(
				context.Background(),
				attempt,
			); !errors.Is(
				err,
				errMetadataPreparationFailed,
			) {
				t.Fatalf("wildcard container error = %v, want stable preparation failure", err)
			}
			if registration.materializeCalls != 0 {
				t.Fatalf("wildcard container materialization calls = %d, want zero", registration.materializeCalls)
			}
		})
	}
}

type metadataTestRegistration struct {
	owner            secret.GenerationMaterialization
	materializeCalls int
	materializeErr   error
}

func (registration *metadataTestRegistration) ResolveScoped(
	_ context.Context,
	_ secret.Scope,
	raw string,
) (string, error) {
	registration.materializeCalls++
	if registration.materializeErr != nil {
		return "", registration.materializeErr
	}
	return raw, nil
}

func (registration *metadataTestRegistration) Secrets() secret.GenerationSecrets {
	return registration.owner.Secrets()
}

func (registration *metadataTestRegistration) Close(ctx context.Context) error {
	return registration.owner.Close(ctx)
}

func registerMetadataCandidate(
	t *testing.T,
	compiler *Compiler,
	broker *metadataPreparationBroker,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (PreparationGeneration, secret.GenerationMaterialization) {
	t.Helper()
	materializer := testutil.NewSecretMaterializer(broker, compiler.schemas.catalog)
	registration, err := materializer.PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	attempt := newMetadataPreparationGeneration(t, compiler, set.DesiredRevision, set.Domains, registration)
	return attempt, registration
}

func metadataPublicationSet(snapshot generation.Snapshot) (generation.ApplyTicket, generation.PublicationSet) {
	published := publishedForDomain(generation.DomainHTTP, snapshot)
	candidate := generation.PublicationCandidate(published)
	ticket := ticketForSnapshot(snapshot, generation.DomainHTTP)
	return ticket, generation.PublicationSet{
		DesiredRevision: snapshot.Revision(),
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
}

func newMetadataPreparationGeneration(
	t *testing.T,
	compiler *Compiler,
	generationNumber uint64,
	candidates map[generation.Domain]generation.PublicationCandidate,
	registration secret.GenerationMaterialization,
) PreparationGeneration {
	t.Helper()
	specs, err := factoryOccurrencesFromCandidates(context.Background(), candidates, compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	set := generation.PublicationSet{DesiredRevision: generationNumber, Domains: candidates}
	if testRegistration, ok := registration.(*metadataTestRegistration); ok {
		testRegistration.owner, err = testutil.NewSecretMaterializer(
			testRegistration, compiler.schemas.catalog,
		).PrepareGeneration(context.Background(), set)
		if err != nil {
			t.Fatal(err)
		}
	}
	secrets := registration.Secrets()
	attempt, err := newPreparationGeneration(generationNumber, candidates, secrets, specs)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func closeMetadataRegistration(t *testing.T, registration secret.GenerationMaterialization) {
	t.Helper()
	if registration != nil {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}
}

func metadataSnapshot(t *testing.T, revision uint64, document string) generation.Snapshot {
	t.Helper()
	return mustGenerationSnapshot(t, revision, []generation.Resource{
		resourceValue("plugin_metadata", metadataFactoryForDocument(document), document),
	}, nil)
}

func metadataFactoryForDocument(document string) string {
	if strings.Contains(document, "master_apikey") {
		return "azure-functions"
	}
	return "chaitin-waf"
}

func errorLogMetadataSnapshot(t *testing.T, revision uint64, document string) generation.Snapshot {
	t.Helper()
	return mustGenerationSnapshot(t, revision, []generation.Resource{
		resourceValue("plugin_metadata", "error-log-logger", document),
	}, nil)
}

func errorLogMetadataPasswords(document map[string]any) []string {
	kafka := document["kafka"].(map[string]any)
	passwords := make([]string, 0)
	switch brokers := kafka["brokers"].(type) {
	case []any:
		for _, item := range brokers {
			broker := item.(map[string]any)
			saslConfig := broker["sasl_config"].(map[string]any)
			passwords = append(passwords, metadataStringField(saslConfig, "password"))
		}
	case map[string]any:
		for _, item := range brokers {
			broker := item.(map[string]any)
			saslConfig := broker["sasl_config"].(map[string]any)
			passwords = append(passwords, metadataStringField(saslConfig, "password"))
		}
	}
	return passwords
}

func metadataStringField(document map[string]any, want string) string {
	for key, value := range document {
		if strings.EqualFold(key, want) {
			return value.(string)
		}
	}
	return ""
}

func installErrorLogMetadataSchema(t *testing.T, compiler *Compiler) {
	t.Helper()
	compiled, err := util.CompileSchema(`{
		"type":"object",
		"properties":{
			"clickhouse":{"type":"object","properties":{"password":{}}},
			"kafka":{"type":"object","properties":{"brokers":{}}}
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := compiler.schemas.factories["error-log-logger"]
	if !ok {
		t.Fatal("error-log-logger schema entry is missing")
	}
	entry.metadata = compiled
	compiler.schemas.factories["error-log-logger"] = entry
}

var (
	_ testutil.SecretResolver          = (*metadataPreparationBroker)(nil)
	_ secret.GenerationMaterialization = (*metadataTestRegistration)(nil)
)
