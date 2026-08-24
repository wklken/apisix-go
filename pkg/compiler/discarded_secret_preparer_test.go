package compiler

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
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

func (*discardPreparationBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
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
				return recoveryDiscardPreparationAttempt(t, compiler, broker, 62, desired)
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
			attempt, registration := recoveryDiscardPreparationAttempt(t, compiler, broker, 64, snapshot)
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

	attemptN, registrationN := recoveryDiscardPreparationAttempt(t, compiler, broker, 73, first)
	if err := prepareCompilerDiscardSecrets(context.Background(), attemptN, compiler.schemas.catalog); err != nil {
		t.Fatal(err)
	}
	if err := registrationN.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	attemptN1, registrationN1 := recoveryDiscardPreparationAttempt(t, compiler, broker, 74, second)
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

	foreignBroker := &discardPreparationBroker{}
	validAttempt, foreignRegistration := recoveryDiscardPreparationAttempt(t, compiler, foreignBroker, 75, second)
	t.Cleanup(func() {
		if err := foreignRegistration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	foreignAttempt, err := newPreparationAttempt(
		validAttempt.Generation(),
		validAttempt.candidates,
		validAttempt.capability,
		[]factoryOccurrenceSpec{{
			domain:   generation.DomainHTTP,
			resource: generation.ResourceKey{Kind: "routes", ID: "foreign-route"},
			source:   capability.SecretPluginConfig,
			factory:  "basic-auth",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareCompilerDiscardSecrets(
		context.Background(), foreignAttempt, compiler.schemas.catalog,
	); err == nil {
		t.Fatal("foreign occurrence unexpectedly succeeded")
	}
	if scopes, _ := foreignBroker.calls(); len(scopes) != 0 {
		t.Fatalf("foreign occurrence reached resolver: %#v", scopes)
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
	materializer := secret.NewScopedMaterializer(broker, compiler.schemas.catalog)
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	return newDiscardPreparationAttempt(t, set.DesiredRevision, set.Domains, registration, compiler)
}

func recoveryDiscardPreparationAttempt(
	t *testing.T,
	compiler *Compiler,
	broker *discardPreparationBroker,
	generationNumber uint64,
	snapshot generation.Snapshot,
) (PreparationAttempt, secret.AttemptRegistration) {
	t.Helper()
	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, snapshot),
	}
	revisions := generation.RevisionSet{Desired: generationNumber, HTTP: snapshot.Revision()}
	materializer := secret.NewScopedMaterializer(broker, compiler.schemas.catalog)
	registration, err := materializer.RegisterRecovery(context.Background(), revisions, published)
	if err != nil {
		t.Fatal(err)
	}
	candidates := map[generation.Domain]generation.PublicationCandidate{
		generation.DomainHTTP: generation.PublicationCandidate(published[generation.DomainHTTP]),
	}
	return newDiscardPreparationAttempt(t, generationNumber, candidates, registration, compiler)
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

var _ secret.ScopedAttemptBroker = (*discardPreparationBroker)(nil)
