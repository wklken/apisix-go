package compiler

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
)

func TestValidateRecoveryRejectsInvalidRevisionCoverage(t *testing.T) {
	compiler := newTestCompiler(t)
	base := validRecoveryPublished(t)

	tests := []struct {
		name      string
		revisions generation.RevisionSet
		published map[generation.Domain]generation.PublishedGeneration
	}{
		{
			name:      "no published domain",
			revisions: generation.RevisionSet{Desired: 9},
			published: nil,
		},
		{
			name:      "partial",
			revisions: generation.RevisionSet{Desired: 9, HTTP: 5, Stream: 6},
			published: map[generation.Domain]generation.PublishedGeneration{
				generation.DomainHTTP: base[generation.DomainHTTP],
			},
		},
		{
			name:      "extra",
			revisions: generation.RevisionSet{Desired: 9, HTTP: 5},
			published: base,
		},
		{
			name:      "unknown",
			revisions: generation.RevisionSet{Desired: 9, HTTP: 5},
			published: map[generation.Domain]generation.PublishedGeneration{
				generation.DomainHTTP:        base[generation.DomainHTTP],
				generation.Domain("unknown"): base[generation.DomainHTTP],
			},
		},
		{
			name:      "revision mismatch",
			revisions: generation.RevisionSet{Desired: 9, HTTP: 4},
			published: map[generation.Domain]generation.PublishedGeneration{
				generation.DomainHTTP: base[generation.DomainHTTP],
			},
		},
		{
			name:      "future revision",
			revisions: generation.RevisionSet{Desired: 4, HTTP: 5},
			published: map[generation.Domain]generation.PublishedGeneration{
				generation.DomainHTTP: base[generation.DomainHTTP],
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateRecovery(
				context.Background(), test.revisions, test.published, compiler.manifest, compiler.schemas,
			); !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("validateRecovery() error = %v, want generation.ErrIntegrity", err)
			}
		})
	}
}

func TestValidateRecoveryRejectsInvalidPublishedGeneration(t *testing.T) {
	compiler := newTestCompiler(t)
	base := validRecoveryPublished(t)
	for name, mutate := range map[string]func(*generation.PublishedGeneration){
		"artifact digest": func(published *generation.PublishedGeneration) {
			published.Artifact.Digest[0]++
		},
		"closure gap": func(published *generation.PublishedGeneration) {
			published.Closure = nil
		},
		"decision gap": func(published *generation.PublishedGeneration) {
			published.Decisions = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			published := base[generation.DomainHTTP]
			mutate(&published)
			if _, err := validateRecovery(
				context.Background(),
				generation.RevisionSet{Desired: 9, HTTP: 5},
				map[generation.Domain]generation.PublishedGeneration{
					generation.DomainHTTP: published,
				},
				compiler.manifest,
				compiler.schemas,
			); !errors.Is(err, generation.ErrIntegrity) && !errors.Is(err, generation.ErrInvalidClosure) {
				t.Fatalf("validateRecovery() error = %v, want generation integrity or closure error", err)
			}
		})
	}
}

func TestValidateRecoveryRejectsSchemaAndDependencyIssuesWithRedactedError(t *testing.T) {
	tests := map[string]generation.Snapshot{
		"schema": mustGenerationSnapshot(t, 5, []generation.Resource{
			resourceValue(
				"routes", "r1",
				`{"id":"r1","plugins":{"request-id":{"algorithm":"invalid-recovery-value"}}}`,
			),
		}, nil),
		"dependency": mustGenerationSnapshot(t, 5, []generation.Resource{
			resourceValue("routes", "r1", `{"id":"r1","upstream_id":"missing"}`),
		}, nil),
	}
	for name, snapshot := range tests {
		t.Run(name, func(t *testing.T) {
			compiler := newTestCompiler(t)
			_, err := validateRecovery(
				context.Background(),
				generation.RevisionSet{Desired: 9, HTTP: 5},
				map[generation.Domain]generation.PublishedGeneration{
					generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, snapshot),
				},
				compiler.manifest,
				compiler.schemas,
			)
			if !errors.Is(err, errRecoverySnapshotInvalid) {
				t.Fatalf("validateRecovery() error = %v, want redacted recovery validation error", err)
			}
			if bytes.Contains([]byte(err.Error()), []byte("invalid-recovery-value")) ||
				bytes.Contains([]byte(err.Error()), []byte("missing")) {
				t.Fatalf("validateRecovery() leaked committed value: %v", err)
			}
		})
	}
}

func TestValidateRecoveryRejectsCanceledOrNilContext(t *testing.T) {
	compiler := newTestCompiler(t)
	base := validRecoveryPublished(t)
	revisions := generation.RevisionSet{Desired: 9, HTTP: 5, Stream: 6}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := validateRecovery(
		ctx,
		revisions,
		base,
		compiler.manifest,
		compiler.schemas,
	); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("canceled context error = %v, want context.Canceled", err)
	}
	//nolint:staticcheck // This verifies the fail-closed nil-context boundary.
	if _, err := validateRecovery(
		nil,
		revisions,
		base,
		compiler.manifest,
		compiler.schemas,
	); !errors.Is(
		err,
		ErrInvalidInput,
	) {
		t.Fatalf("nil context error = %v, want ErrInvalidInput", err)
	}
}

func TestValidateRecoveryPreservesIndependentDomainRevisionsAndClonesInputs(t *testing.T) {
	compiler := newTestCompiler(t)
	committed := validRecoveryPublished(t)
	wantHTTP := committed[generation.DomainHTTP]
	wantStream := committed[generation.DomainStream]
	got, err := validateRecovery(
		context.Background(),
		generation.RevisionSet{Desired: 9, HTTP: 5, Stream: 6},
		committed,
		compiler.manifest,
		compiler.schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("verified domain count = %d, want 2", len(got))
	}
	assertRecoveryIdentity(t, got[generation.DomainHTTP], wantHTTP)
	assertRecoveryIdentity(t, got[generation.DomainStream], wantStream)

	committed[generation.DomainHTTP].Closure[0].ID = "mutated-input"
	committed[generation.DomainHTTP].Decisions[0].Code = "mutated-input"
	delete(committed, generation.DomainStream)
	if got[generation.DomainHTTP].Closure[0].ID == "mutated-input" ||
		got[generation.DomainHTTP].Decisions[0].Code == "mutated-input" {
		t.Fatal("verified recovery aliases mutable input slices")
	}
	if _, ok := got[generation.DomainStream]; !ok {
		t.Fatal("verified recovery aliases input map")
	}

	wantResourceValue := mustSnapshotResourceValue(t, got[generation.DomainHTTP].Snapshot)
	resources := got[generation.DomainHTTP].Snapshot.Resources()
	resources[0].Value[0]++
	if !bytes.Equal(wantResourceValue, mustSnapshotResourceValue(t, got[generation.DomainHTTP].Snapshot)) {
		t.Fatal("verified recovery snapshot exposes mutable resource bytes")
	}
}

func TestValidateRecoveryRejectsNilSchemaInputs(t *testing.T) {
	compiler := newTestCompiler(t)
	base := validRecoveryPublished(t)
	revisions := generation.RevisionSet{Desired: 9, HTTP: 5, Stream: 6}
	for name, inputs := range map[string]struct {
		manifest *capability.Manifest
		schemas  *schemaSet
	}{
		"nil manifest": {manifest: nil, schemas: compiler.schemas},
		"nil schemas":  {manifest: compiler.manifest, schemas: nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateRecovery(
				context.Background(), revisions, base, inputs.manifest, inputs.schemas,
			); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validateRecovery() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestAttemptFactoryRegistersExactRecoveryAndUsesDistinctAttemptIdentity(t *testing.T) {
	factory, materializer, _, trace := newRecordingAttemptFactory(t)
	committed := validRecoveryPublished(t)
	want := cloneRecoveryMapForFactoryTest(committed)
	revisions := generation.RevisionSet{Desired: 9, HTTP: 5, Stream: 6}
	prepared, err := factory.prepareRecoveryAttempt(context.Background(), revisions, committed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(materializer.recoveryState, want) {
		t.Fatalf("registered recovery = %#v, want %#v", materializer.recoveryState, want)
	}
	if got, wantTrace := *trace, []string{"register-recovery"}; !slices.Equal(
		got,
		wantTrace,
	) {
		t.Fatalf("recovery trace = %v, want %v", got, wantTrace)
	}
	if prepared.attempt.Generation() != revisions.Desired ||
		prepared.attempt.AttemptID() != secret.RecoveryAttemptID(revisions, want) {
		t.Fatalf("recovery attempt identity = %d/%x", prepared.attempt.Generation(), prepared.attempt.AttemptID())
	}

	desired := mustGenerationSnapshot(t, revisions.Desired, []generation.Resource{
		resourceValue("routes", "candidate", `{"id":"candidate"}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	candidateSet, err := factory.compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.attempt.AttemptID() == secret.CandidateAttemptID(ticket, candidateSet) {
		t.Fatal("candidate and recovery attempt identities alias")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptFactoryRejectsInvalidRecoveryBeforeRegistration(t *testing.T) {
	factory, materializer, _, _ := newRecordingAttemptFactory(t)
	snapshot := mustGenerationSnapshot(t, 5, []generation.Resource{
		resourceValue(
			"routes", "r1",
			`{"id":"r1","plugins":{"request-id":{"algorithm":"invalid-recovery-value"}}}`,
		),
	}, nil)
	_, err := factory.prepareRecoveryAttempt(
		context.Background(),
		generation.RevisionSet{Desired: 9, HTTP: 5},
		map[generation.Domain]generation.PublishedGeneration{
			generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, snapshot),
		},
	)
	if !errors.Is(err, errRecoverySnapshotInvalid) {
		t.Fatalf("invalid recovery error = %v, want redacted schema error", err)
	}
	if materializer.recoveryCalls != 0 {
		t.Fatalf("invalid recovery registrations = %d, want zero", materializer.recoveryCalls)
	}
}

func validRecoveryPublished(t *testing.T) map[generation.Domain]generation.PublishedGeneration {
	t.Helper()
	return map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: publishedForDomain(
			generation.DomainHTTP,
			mustGenerationSnapshot(t, 5, []generation.Resource{
				resourceValue("routes", "http-route", `{"id":"http-route"}`),
			}, nil),
		),
		generation.DomainStream: publishedForDomain(
			generation.DomainStream,
			mustGenerationSnapshot(t, 6, []generation.Resource{
				resourceValue("stream_routes", "stream-route", `{"id":"stream-route"}`),
			}, nil),
		),
	}
}

func assertRecoveryIdentity(
	t *testing.T,
	got generation.PublishedGeneration,
	want generation.PublishedGeneration,
) {
	t.Helper()
	if got.Artifact != want.Artifact || !slices.Equal(got.Closure, want.Closure) ||
		!reflect.DeepEqual(got.Decisions, want.Decisions) {
		t.Fatalf("verified publication metadata changed: got %#v, want %#v", got, want)
	}
	gotBytes, err := got.Snapshot.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := want.Snapshot.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) || got.Snapshot.Digest() != want.Snapshot.Digest() {
		t.Fatalf(
			"verified snapshot identity changed: got %x/%x, want %x/%x",
			gotBytes,
			got.Snapshot.Digest(),
			wantBytes,
			want.Snapshot.Digest(),
		)
	}
}

func mustSnapshotResourceValue(t *testing.T, snapshot generation.Snapshot) []byte {
	t.Helper()
	resources := snapshot.Resources()
	if len(resources) == 0 {
		t.Fatal("snapshot has no resources")
	}
	return resources[0].Value
}
