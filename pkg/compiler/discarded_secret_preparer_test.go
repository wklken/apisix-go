package compiler

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type discardPreparationBroker struct {
	mu      sync.Mutex
	failRaw string
	scopes  []secret.Scope
	raws    []string
	revoked []secret.AttemptID
}

func (*discardPreparationBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (broker *discardPreparationBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.scopes = append(broker.scopes, scope)
	broker.raws = append(broker.raws, raw)
	if raw == broker.failRaw {
		return "", fmt.Errorf("resolver exposed %s and resolved-plaintext", raw)
	}
	return "resolved:" + raw, nil
}

func (broker *discardPreparationBroker) RevokeAttempt(
	_ context.Context,
	attempt secret.AttemptID,
) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.revoked = append(broker.revoked, attempt)
	return nil
}

func (broker *discardPreparationBroker) calls() ([]secret.Scope, []string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]secret.Scope(nil), broker.scopes...), append([]string(nil), broker.raws...)
}

func TestPrepareCompilerDiscardSecretsUsesExactRawFinalOccurrences(t *testing.T) {
	compiler := newTestCompiler(t)
	desired := discardPreparationSnapshot(t, 61, "candidate")
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	set, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		generation uint64
		register   func(*discardPreparationBroker) (PreparationAttempt, secret.AttemptRegistration)
	}{
		{
			name:       "candidate",
			generation: desired.Revision(),
			register: func(broker *discardPreparationBroker) (PreparationAttempt, secret.AttemptRegistration) {
				return candidateDiscardPreparationAttempt(t, compiler, broker, ticket, set)
			},
		},
		{
			name:       "recovery",
			generation: 62,
			register: func(broker *discardPreparationBroker) (PreparationAttempt, secret.AttemptRegistration) {
				return snapshotDiscardPreparationAttempt(t, compiler, broker, 62, desired)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker := &discardPreparationBroker{}
			attempt, registration := tt.register(broker)
			t.Cleanup(func() {
				if err := registration.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			before, ok := attempt.Candidate(generation.DomainHTTP)
			if !ok {
				t.Fatal("HTTP candidate is missing")
			}
			resourceKey := generation.ResourceKey{Kind: "routes", ID: "discard-route"}
			beforeRaw, ok := before.Snapshot.Lookup(resourceKey)
			if !ok {
				t.Fatalf("discard route is missing: resources=%#v decisions=%#v",
					before.Snapshot.Resources(), before.Decisions)
			}

			if err := prepareCompilerDiscardSecrets(
				context.Background(), attempt, compiler.schemas.catalog,
			); err != nil {
				t.Fatal(err)
			}

			scopes, raws := broker.calls()
			got := make(map[string]string, len(scopes))
			for index, scope := range scopes {
				if scope.Generation != tt.generation || scope.Attempt != attempt.AttemptID() ||
					scope.Domain != generation.DomainHTTP || scope.Resource != resourceKey ||
					scope.Source != capability.SecretPluginConfig {
					t.Fatalf("scope[%d] = %#v", index, scope)
				}
				got[scope.Plugin+"/"+scope.Field] = raws[index]
			}
			if want := discardPreparationReferences("candidate"); !reflect.DeepEqual(got, want) {
				t.Fatalf("materialized references = %#v, want %#v", got, want)
			}

			after, ok := attempt.Candidate(generation.DomainHTTP)
			if !ok {
				t.Fatal("HTTP candidate disappeared")
			}
			afterRaw, ok := after.Snapshot.Lookup(resourceKey)
			if !ok || !bytes.Equal(afterRaw, beforeRaw) {
				t.Fatalf("candidate bytes changed: before=%s after=%s", beforeRaw, afterRaw)
			}
		})
	}
}

func TestPrepareCompilerDiscardSecretsRejectsNonStringAndRedactsFailure(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		failRaw string
	}{
		{
			name: "non-string",
			raw:  `{"id":"discard-route","plugins":{"basic-auth":{"password":{"nested":"value"}}}}`,
		},
		{
			name: "empty object",
			raw:  `{"id":"discard-route","plugins":{"basic-auth":{"password":{}}}}`,
		},
		{
			name: "empty array",
			raw:  `{"id":"discard-route","plugins":{"basic-auth":{"password":[]}}}`,
		},
		{
			name: "number",
			raw:  `{"id":"discard-route","plugins":{"basic-auth":{"password":7}}}`,
		},
		{
			name: "boolean",
			raw:  `{"id":"discard-route","plugins":{"basic-auth":{"password":true}}}`,
		},
		{
			name: "null",
			raw:  `{"id":"discard-route","plugins":{"basic-auth":{"password":null}}}`,
		},
		{
			name: "nested empty composite",
			raw:  `{"id":"discard-route","plugins":{"basic-auth":{"password":{"nested":{}}}}}`,
		},
		{
			name:    "resolver",
			raw:     `{"id":"discard-route","plugins":{"basic-auth":{"password":"$ENV://SENSITIVE_NAME"}}}`,
			failRaw: "$ENV://SENSITIVE_NAME",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compiler := newTestCompiler(t)
			broker := &discardPreparationBroker{failRaw: tt.failRaw}
			snapshot := mustGenerationSnapshot(t, 63, []generation.Resource{
				resourceValue("routes", "discard-route", tt.raw),
			}, nil)
			attempt, registration := snapshotDiscardPreparationAttempt(t, compiler, broker, 64, snapshot)
			t.Cleanup(func() {
				if err := registration.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})

			err := prepareCompilerDiscardSecrets(context.Background(), attempt, compiler.schemas.catalog)
			if err == nil || err.Error() != errCompilerDiscardPreparationFailed.Error() {
				t.Fatalf(
					"prepareCompilerDiscardSecrets() error = %v, want %v",
					err,
					errCompilerDiscardPreparationFailed,
				)
			}
			for _, sensitive := range []string{"SENSITIVE_NAME", "$ENV://", "resolved-plaintext"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatalf("error leaked %q: %v", sensitive, err)
				}
			}
			if tt.failRaw == "" {
				if scopes, _ := broker.calls(); len(scopes) != 0 {
					t.Fatalf("malformed compiler-discard value reached resolver: %#v", scopes)
				}
			}
		})
	}
}

func TestPrepareCompilerDiscardSecretsIsAtomicAcrossAttempts(t *testing.T) {
	compiler := newTestCompiler(t)
	broker := &discardPreparationBroker{}
	first := mustGenerationSnapshot(t, 71, []generation.Resource{
		resourceValue(
			"routes", "discard-route",
			`{"id":"discard-route","plugins":{"basic-auth":{"password":"$ENV://FIRST"}}}`,
		),
	}, nil)
	second := mustGenerationSnapshot(t, 72, []generation.Resource{
		resourceValue(
			"routes", "discard-route",
			`{"id":"discard-route","plugins":{"basic-auth":{"password":"$ENV://SECOND"}}}`,
		),
	}, nil)

	attemptN, registrationN := snapshotDiscardPreparationAttempt(t, compiler, broker, 73, first)
	if err := prepareCompilerDiscardSecrets(context.Background(), attemptN, compiler.schemas.catalog); err != nil {
		t.Fatal(err)
	}
	if err := registrationN.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	attemptN1, registrationN1 := snapshotDiscardPreparationAttempt(t, compiler, broker, 74, second)
	t.Cleanup(func() {
		if err := registrationN1.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := prepareCompilerDiscardSecrets(context.Background(), attemptN1, compiler.schemas.catalog); err != nil {
		t.Fatal(err)
	}

	scopes, raws := broker.calls()
	if len(scopes) != 2 || scopes[0].Generation != 73 || scopes[1].Generation != 74 ||
		raws[0] != "$ENV://FIRST" || raws[1] != "$ENV://SECOND" ||
		scopes[0].Attempt == scopes[1].Attempt {
		t.Fatalf("attempt calls = %#v / %#v", scopes, raws)
	}

	sourceBroker := &discardPreparationBroker{}
	sourceAttempt, sourceRegistration := snapshotDiscardPreparationAttempt(t, compiler, sourceBroker, 75, second)
	t.Cleanup(func() {
		if err := sourceRegistration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	var sourceOccurrence FactoryOccurrence
	for _, occurrence := range sourceAttempt.Occurrences(capability.SecretPluginConfig) {
		if occurrence.Factory() == "basic-auth" &&
			occurrence.Resource() == (generation.ResourceKey{Kind: "routes", ID: "discard-route"}) {
			sourceOccurrence = occurrence
			break
		}
	}
	if sourceOccurrence.authority == nil {
		t.Fatal("source preparation attempt did not expose the discard occurrence")
	}
	targetBroker := &discardPreparationBroker{}
	targetAttempt, targetRegistration := snapshotDiscardPreparationAttempt(t, compiler, targetBroker, 76, second)
	t.Cleanup(func() {
		if err := targetRegistration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	targetAttempt.occurrences = []FactoryOccurrence{sourceOccurrence}
	if targetAttempt.owns(sourceOccurrence) {
		t.Fatal("cross-attempt occurrence unexpectedly has target authority")
	}
	if err := prepareCompilerDiscardSecrets(
		context.Background(), targetAttempt, compiler.schemas.catalog,
	); err == nil {
		t.Fatal("cross-attempt occurrence unexpectedly succeeded")
	}
	if scopes, _ := targetBroker.calls(); len(scopes) != 0 {
		t.Fatalf("cross-attempt occurrence reached resolver: %#v", scopes)
	}

	crossDomainOccurrence := sourceOccurrence
	crossDomainOccurrence.domain = generation.DomainStream
	crossDomainOccurrence.resource = generation.ResourceKey{Kind: "stream_routes", ID: "foreign-route"}
	targetAttempt.occurrences = []FactoryOccurrence{crossDomainOccurrence}
	if targetAttempt.owns(crossDomainOccurrence) {
		t.Fatal("cross-domain occurrence unexpectedly has target authority")
	}
	if err := prepareCompilerDiscardSecrets(
		context.Background(), targetAttempt, compiler.schemas.catalog,
	); err == nil {
		t.Fatal("cross-domain occurrence unexpectedly succeeded")
	}
	if scopes, _ := targetBroker.calls(); len(scopes) != 0 {
		t.Fatalf("cross-domain occurrence reached resolver: %#v", scopes)
	}
}

func TestPrepareCompilerDiscardSecretsSkipsPluginTargetCandidates(t *testing.T) {
	compiler := newTestCompiler(t)
	declaration, ok := compiler.schemas.catalog.Lookup(
		"response-rewrite", capability.SecretPluginConfig, "body",
	)
	if !ok || declaration.EffectiveTarget() != capability.SecretMaterializationPlugin {
		t.Fatalf("response-rewrite body declaration = %#v/%v, want plugin target", declaration, ok)
	}
	snapshot := mustGenerationSnapshot(t, 76, []generation.Resource{
		resourceValue("routes", "r1", "{"),
	}, nil)
	published := publishedForDomain(generation.DomainHTTP, snapshot)
	registration := &recordingFactoryRegistration{
		id:    secret.AttemptID{1},
		trace: new([]string),
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, 76)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := newPreparationAttempt(
		76,
		map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: generation.PublicationCandidate(published),
		},
		capabilityValue,
		[]factoryOccurrenceSpec{{
			domain:   generation.DomainHTTP,
			resource: generation.ResourceKey{Kind: "routes", ID: "r1"},
			source:   capability.SecretPluginConfig,
			factory:  "response-rewrite",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareCompilerDiscardSecrets(context.Background(), attempt, compiler.schemas.catalog); err != nil {
		t.Fatalf("plugin-target-only preparation = %v, want nil", err)
	}
}

func TestPrepareCompilerDiscardSecretsMatchesTerminalKeysCaseInsensitively(t *testing.T) {
	compiler := newTestCompiler(t)
	broker := &discardPreparationBroker{}
	snapshot := mustGenerationSnapshot(t, 78, []generation.Resource{
		resourceValue(
			"routes", "discard-route",
			`{"id":"discard-route","plugins":{"basic-auth":{"PASSWORD":"$ENV://CASE_INSENSITIVE"}}}`,
		),
	}, nil)
	attempt, registration := snapshotDiscardPreparationAttempt(t, compiler, broker, 79, snapshot)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := prepareCompilerDiscardSecrets(context.Background(), attempt, compiler.schemas.catalog); err != nil {
		t.Fatal(err)
	}
	_, raws := broker.calls()
	if !slices.Equal(raws, []string{"$ENV://CASE_INSENSITIVE"}) {
		t.Fatalf("case-insensitive materialization raws = %#v, want one reference", raws)
	}
}

func candidateDiscardPreparationAttempt(
	t *testing.T,
	compiler *Compiler,
	broker *discardPreparationBroker,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (PreparationAttempt, secret.AttemptRegistration) {
	t.Helper()
	materializer := testutil.NewSecretMaterializer(broker, compiler.schemas.catalog)
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	return newDiscardPreparationAttempt(t, set.DesiredRevision, set.Domains, registration, compiler)
}

func snapshotDiscardPreparationAttempt(
	t *testing.T,
	compiler *Compiler,
	broker *discardPreparationBroker,
	generationNumber uint64,
	snapshot generation.Snapshot,
) (PreparationAttempt, secret.AttemptRegistration) {
	t.Helper()
	owned, err := generation.NewSnapshot(generationNumber, snapshot.Resources(), snapshot.Tombstones())
	if err != nil {
		t.Fatal(err)
	}
	published := publishedForDomain(generation.DomainHTTP, owned)
	set := generation.PublicationSet{
		DesiredRevision: generationNumber,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: generation.PublicationCandidate(published),
		},
	}
	materializer := testutil.NewSecretMaterializer(broker, compiler.schemas.catalog)
	registration, err := materializer.RegisterCandidate(
		context.Background(), ticketForSnapshot(owned, generation.DomainHTTP), set,
	)
	if err != nil {
		t.Fatal(err)
	}
	return newDiscardPreparationAttempt(t, generationNumber, set.Domains, registration, compiler)
}

func newDiscardPreparationAttempt(
	t *testing.T,
	generationNumber uint64,
	candidates map[generation.Domain]generation.PublicationCandidate,
	registration secret.AttemptRegistration,
	compiler *Compiler,
) (PreparationAttempt, secret.AttemptRegistration) {
	t.Helper()
	specs, err := factoryOccurrencesFromCandidates(context.Background(), candidates, compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, generationNumber)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := newPreparationAttempt(generationNumber, candidates, capabilityValue, specs)
	if err != nil {
		t.Fatal(err)
	}
	return attempt, registration
}

func discardPreparationSnapshot(t *testing.T, revision uint64, suffix string) generation.Snapshot {
	t.Helper()
	return mustGenerationSnapshot(t, revision, []generation.Resource{
		resourceValue(
			"routes",
			"discard-route",
			fmt.Sprintf(`{"id":"discard-route","plugins":{
				"basic-auth":{"password":"$ENV://BASIC_%[1]s"},
				"key-auth":{"key":"$ENV://KEY_%[1]s"},
				"jwt-auth":{"secret":"$ENV://JWT_SECRET_%[1]s","private_key":"$ENV://JWT_KEY_%[1]s"},
				"hmac-auth":{"secret":"$ENV://HMAC_%[1]s"},
				"ldap-auth":{"base_dn":"dc=example,dc=org","ldap_uri":"ldap://127.0.0.1","user_dn":"$ENV://LDAP_%[1]s"},
				"jwe-decrypt":{"header":"Authorization","forward_header":"X-JWE","key":"$ENV://JWE_KEY_%[1]s","secret":"$ENV://JWE_SECRET_%[1]s"}
			}}`, suffix),
		),
		resourceValue(
			"routes",
			"missing-route",
			`{"id":"missing-route","plugins":{
				"basic-auth":{"password":""},"key-auth":{},"jwt-auth":{},"hmac-auth":{},
				"ldap-auth":{"base_dn":"dc=example,dc=org","ldap_uri":"ldap://127.0.0.1"},
				"jwe-decrypt":{"header":"Authorization","forward_header":"X-JWE"}
			}}`,
		),
	}, nil)
}

func discardPreparationReferences(suffix string) map[string]string {
	return map[string]string{
		"basic-auth/password":  "$ENV://BASIC_" + suffix,
		"hmac-auth/secret":     "$ENV://HMAC_" + suffix,
		"jwe-decrypt/key":      "$ENV://JWE_KEY_" + suffix,
		"jwe-decrypt/secret":   "$ENV://JWE_SECRET_" + suffix,
		"jwt-auth/private_key": "$ENV://JWT_KEY_" + suffix,
		"jwt-auth/secret":      "$ENV://JWT_SECRET_" + suffix,
		"key-auth/key":         "$ENV://KEY_" + suffix,
		"ldap-auth/user_dn":    "$ENV://LDAP_" + suffix,
	}
}

var _ testutil.SecretAttemptBroker = (*discardPreparationBroker)(nil)
