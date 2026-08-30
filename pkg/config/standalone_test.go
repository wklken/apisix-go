package config

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func testStandaloneDataEncryption(
	t *testing.T,
	enabled bool,
	keyring []string,
) data_encryption.Service {
	t.Helper()
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return data_encryption.NewService(enabled, keyring, catalog)
}

type standaloneApplierFunc func(
	context.Context,
	generation.DesiredBatch,
) (generation.Acknowledgement, error)

func (f standaloneApplierFunc) Apply(
	ctx context.Context,
	batch generation.DesiredBatch,
) (generation.Acknowledgement, error) {
	return f(ctx, batch)
}

type recordingStandaloneApplier struct {
	mu      sync.Mutex
	batches []generation.DesiredBatch
	apply   standaloneApplierFunc
}

func (a *recordingStandaloneApplier) Apply(
	ctx context.Context,
	batch generation.DesiredBatch,
) (generation.Acknowledgement, error) {
	a.mu.Lock()
	a.batches = append(a.batches, cloneStandaloneTestBatch(batch))
	call := len(a.batches)
	apply := a.apply
	a.mu.Unlock()
	if apply != nil {
		return apply(ctx, batch)
	}
	return standaloneAcknowledgement(batch, uint64(call), nil), nil
}

func (a *recordingStandaloneApplier) snapshot() []generation.DesiredBatch {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]generation.DesiredBatch, len(a.batches))
	for index, batch := range a.batches {
		result[index] = cloneStandaloneTestBatch(batch)
	}
	return result
}

func cloneStandaloneTestBatch(batch generation.DesiredBatch) generation.DesiredBatch {
	clone := batch
	clone.RequiredDomains = slices.Clone(batch.RequiredDomains)
	clone.Mutations = make([]generation.Mutation, len(batch.Mutations))
	for index, mutation := range batch.Mutations {
		clone.Mutations[index] = mutation
		clone.Mutations[index].Value = slices.Clone(mutation.Value)
	}
	return clone
}

func standaloneAcknowledgement(
	batch generation.DesiredBatch,
	desired uint64,
	dispositions map[generation.ResourceKey]generation.ResourceDisposition,
) generation.Acknowledgement {
	ack := generation.Acknowledgement{
		Cursor: batch.Cursor,
		Revisions: generation.RevisionSet{
			Desired: desired,
		},
		Decisions: make(map[generation.Domain][]generation.ResourceDecision),
	}
	for _, domain := range batch.RequiredDomains {
		switch domain {
		case generation.DomainHTTP:
			ack.Revisions.HTTP = desired
		case generation.DomainStream:
			ack.Revisions.Stream = desired
		}
		ack.Decisions[domain] = nil
	}
	for _, mutation := range batch.Mutations {
		for _, domain := range generation.DomainsForResourceKind(mutation.Key.Kind) {
			if !slices.Contains(batch.RequiredDomains, domain) {
				continue
			}
			disposition := generation.DispositionPublished
			if configured, ok := dispositions[mutation.Key]; ok {
				disposition = configured
			}
			ack.Decisions[domain] = append(ack.Decisions[domain], generation.ResourceDecision{
				Key:         mutation.Key,
				Disposition: disposition,
				Code:        "standalone-test-" + string(disposition),
			})
		}
	}
	return ack
}

func TestStandaloneEncryptionServiceIsRequiredAtEntryPoints(t *testing.T) {
	assertStandaloneCatalogPanic(t, func() {
		_ = NewStandaloneFileWatcher(
			"",
			standaloneProviderYAML,
			standaloneApplierFunc(nil),
			testutil.UnconfiguredDataEncryptionService(),
		)
	})
	if _, err := readStandaloneSnapshot(
		"",
		standaloneProviderYAML,
		testutil.UnconfiguredDataEncryptionService(),
	); !errors.Is(err, data_encryption.ErrDeclarationCatalogUnavailable) {
		t.Fatalf("readStandaloneSnapshot() error = %v, want catalog error", err)
	}
	if _, _, err := normalizeStandaloneResource(
		"routes",
		json.RawMessage(`{"id":"r1"}`),
		testutil.UnconfiguredDataEncryptionService(),
	); !errors.Is(err, data_encryption.ErrDeclarationCatalogUnavailable) {
		t.Fatalf("normalizeStandaloneResource() error = %v, want catalog error", err)
	}
}

func assertStandaloneCatalogPanic(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != data_encryption.ErrDeclarationCatalogUnavailable {
			t.Fatalf("panic = %v, want %v", recovered, data_encryption.ErrDeclarationCatalogUnavailable)
		}
	}()
	call()
}

func TestStandaloneBucketsExcludeSingletonPlugins(t *testing.T) {
	buckets := StandaloneBuckets()
	if len(buckets) != 12 {
		t.Fatalf("len(StandaloneBuckets()) = %d, want 12", len(buckets))
	}
	if slices.Contains(buckets, "plugins") {
		t.Fatalf("StandaloneBuckets() = %v, want singleton plugins excluded", buckets)
	}
	for _, bucket := range buckets {
		if !generation.IsManagedResourceKind(bucket) {
			t.Errorf("standalone bucket %q is not managed", bucket)
		}
	}
}

func TestDesiredBatchFromStandaloneUsesContentDigestCursor(t *testing.T) {
	batch := desiredBatchFromStandalone(standaloneSnapshot{
		"routes": {"r1": []byte(`{"id":"r1","uri":"/"}`)},
	})
	if !batch.ReplaceManaged || batch.Cursor.Provider != "standalone/v1" ||
		batch.Cursor.Revision != "sha256:12bf10e04f88b65767a860dc08d95d6295c4b578d98fb1930564ec4c040b0c6b" {
		t.Fatalf("batch cursor = %+v, replace = %t", batch.Cursor, batch.ReplaceManaged)
	}
	if len(batch.Mutations) != 1 || batch.Mutations[0].Type != generation.MutationPut ||
		batch.Mutations[0].Key != (generation.ResourceKey{Kind: "routes", ID: "r1"}) {
		t.Fatalf("batch mutations = %+v", batch.Mutations)
	}
}

func TestDesiredBatchFromStandaloneSortsMutationsAndConservativelyRequiresDomains(t *testing.T) {
	snapshot := standaloneSnapshot{
		"stream_routes": {"z": []byte(`{"id":"z"}`)},
		"services": {
			"s2": []byte(`{"id":"s2"}`),
			"s1": []byte(`{"id":"s1"}`),
		},
		"routes": {
			"r2": []byte(`{"id":"r2"}`),
			"r1": []byte(`{"id":"r1"}`),
		},
	}
	batch := desiredBatchFromStandalone(snapshot)
	wantKeys := []generation.ResourceKey{
		{Kind: "routes", ID: "r1"},
		{Kind: "routes", ID: "r2"},
		{Kind: "services", ID: "s1"},
		{Kind: "services", ID: "s2"},
		{Kind: "stream_routes", ID: "z"},
	}
	gotKeys := make([]generation.ResourceKey, 0, len(batch.Mutations))
	for _, mutation := range batch.Mutations {
		gotKeys = append(gotKeys, mutation.Key)
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("mutation keys = %+v, want %+v", gotKeys, wantKeys)
	}
	if !reflect.DeepEqual(batch.RequiredDomains, []generation.Domain{
		generation.DomainHTTP,
		generation.DomainStream,
	}) {
		t.Fatalf("required domains = %v, want http and stream", batch.RequiredDomains)
	}
}

func TestDesiredBatchFromStandaloneEncryptedRetryBindsCursorToTranslatedState(t *testing.T) {
	const key = "qeddd145sfvddff3"
	raw := json.RawMessage(`{
		"id":"r1",
		"uri":"/",
		"plugins":{"key-auth":{"key":"plaintext-secret"}}
	}`)
	encryption := testStandaloneDataEncryption(t, true, []string{key})
	firstID, firstValue, err := normalizeStandaloneResource("routes", raw, encryption)
	if err != nil {
		t.Fatal(err)
	}
	secondID, secondValue, err := normalizeStandaloneResource("routes", raw, encryption)
	if err != nil {
		t.Fatal(err)
	}
	if firstID != secondID || string(firstValue) == string(secondValue) {
		t.Fatalf("encrypted normalizations unexpectedly equal: %q and %q", firstValue, secondValue)
	}
	first := desiredBatchFromStandalone(standaloneSnapshot{"routes": {firstID: firstValue}})
	second := desiredBatchFromStandalone(standaloneSnapshot{"routes": {secondID: secondValue}})
	if first.Cursor == second.Cursor {
		t.Fatalf("distinct translated states share cursor %q", first.Cursor.Revision)
	}
}

func TestStandaloneWatcherAppliesCanonicalFullSnapshotThroughDesiredApplier(t *testing.T) {
	path := writeStandaloneTestConfig(t, `services:
  - id: s2
    upstream_id: u1
routes:
  - id: r2
    uri: /two
  - id: r1
    uri: /one
stream_routes:
  - id: tcp-1
    server_addr: 127.0.0.1
    server_port: 9000
#END
`)
	applier := &recordingStandaloneApplier{}
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	batches := applier.snapshot()
	if len(batches) != 1 {
		t.Fatalf("Apply calls = %d, want 1", len(batches))
	}
	batch := batches[0]
	if !batch.ReplaceManaged || !slices.Equal(batch.RequiredDomains, []generation.Domain{
		generation.DomainHTTP,
		generation.DomainStream,
	}) {
		t.Fatalf("batch = %+v", batch)
	}
	wantKeys := []generation.ResourceKey{
		{Kind: "routes", ID: "r1"},
		{Kind: "routes", ID: "r2"},
		{Kind: "services", ID: "s2"},
		{Kind: "stream_routes", ID: "tcp-1"},
	}
	gotKeys := make([]generation.ResourceKey, 0, len(batch.Mutations))
	for _, mutation := range batch.Mutations {
		gotKeys = append(gotKeys, mutation.Key)
	}
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("mutation keys = %+v, want %+v", gotKeys, wantKeys)
	}
	if watcher.acknowledgedCursor != batch.Cursor || watcher.acknowledgedRevisions.Desired != 1 {
		t.Fatalf("acknowledged state = %+v/%+v", watcher.acknowledgedCursor, watcher.acknowledgedRevisions)
	}
}

func TestStandaloneWatcherFailedApplyRetainsAcknowledgedCursorAndDecisions(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	wantErr := errors.New("publication failed")
	calls := 0
	applier := &recordingStandaloneApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		calls++
		if calls == 2 {
			return generation.Acknowledgement{}, wantErr
		}
		return standaloneAcknowledgement(batch, 7, nil), nil
	}}
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	before := snapshotStandaloneAcknowledgedState(watcher)
	writeStandaloneConfig(t, path, "routes:\n  - id: r1\n    uri: /two\n#END\n")
	if err := watcher.Reload(); !errors.Is(err, wantErr) {
		t.Fatalf("Reload() error = %v, want %v", err, wantErr)
	}
	after := snapshotStandaloneAcknowledgedState(watcher)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("acknowledged state changed after failure:\n before=%+v\n after=%+v", before, after)
	}
}

func TestStandaloneWatcherAcknowledgementAtomicallyAdvancesRequiredReadiness(t *testing.T) {
	resetStandaloneConfigApplyMetrics(t, true)
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		&recordingStandaloneApplier{},
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	if !metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("provider/http/stream acknowledgement did not establish readiness")
	}
}

func TestStandaloneWatcherInvalidAcknowledgementPreservesLastReadyState(t *testing.T) {
	resetStandaloneConfigApplyMetrics(t, true)
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	calls := 0
	applier := standaloneApplierFunc(func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		calls++
		ack := standaloneAcknowledgement(batch, uint64(calls), nil)
		if calls == 2 {
			ack.Cursor.Revision = "sha256:invalid"
		}
		return ack, nil
	})
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	if !metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("initial acknowledgement did not establish readiness")
	}
	writeStandaloneConfig(t, path, "routes:\n  - id: r1\n    uri: /two\n#END\n")
	if err := watcher.Reload(); !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("Reload() error = %v, want ErrIntegrity", err)
	}
	if !metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("invalid acknowledgement overwrote the last ready state")
	}
}

func TestStandaloneWatcherSameContentReplaysCommittedCursor(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	var committed generation.Acknowledgement
	applier := &recordingStandaloneApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		if committed.Revisions.Desired == 0 {
			committed = standaloneAcknowledgement(batch, 11, nil)
		}
		return committed, nil
	}}
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	batches := applier.snapshot()
	if len(batches) != 2 || batches[0].Cursor != batches[1].Cursor {
		t.Fatalf("replayed cursors = %+v", batches)
	}
}

func TestStandaloneWatcherSameCursorReplaysLastGoodImplicitDeleteDecisions(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	calls := 0
	var deletedAck generation.Acknowledgement
	applier := standaloneApplierFunc(func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		calls++
		if calls == 1 {
			return standaloneAcknowledgement(batch, 1, nil), nil
		}
		if deletedAck.Revisions.Desired == 0 {
			deletedAck = standaloneAcknowledgement(batch, 2, nil)
			deletedAck.Decisions[generation.DomainHTTP] = []generation.ResourceDecision{{
				Key:         generation.ResourceKey{Kind: "routes", ID: "r1"},
				Disposition: generation.DispositionLastGood,
				Code:        "standalone-test-last-good",
			}}
		}
		return deletedAck, nil
	})
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	writeStandaloneConfig(t, path, "{}\n#END\n")
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Reload(); err != nil {
		t.Fatalf("same-cursor Replay() error = %v", err)
	}
}

func TestStandaloneWatcherDoesNotCommitAcknowledgementAfterCancellation(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	var watcher *StandaloneFileWatcher
	applier := standaloneApplierFunc(func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		watcher.cancel()
		return standaloneAcknowledgement(batch, 1, nil), nil
	})
	watcher = NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reload() error = %v, want context.Canceled", err)
	}
	if watcher.acknowledgedRevisions != (generation.RevisionSet{}) {
		t.Fatalf("acknowledged revisions = %+v, want zero", watcher.acknowledgedRevisions)
	}
}

func TestStandaloneWatcherParseFailureDoesNotCallApplier(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "missing end marker", content: "routes:\n  - id: r1\n"},
		{name: "missing id", content: "routes:\n  - uri: /one\n#END\n"},
		{name: "unknown section", content: "routes:\n  - id: r1\nunknown:\n  - id: x\n#END\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeStandaloneTestConfig(t, test.content)
			applier := &recordingStandaloneApplier{}
			watcher := NewStandaloneFileWatcher(
				path,
				standaloneProviderYAML,
				applier,
				testStandaloneDataEncryption(t, false, nil),
			)
			if err := watcher.Reload(); err == nil {
				t.Fatal("Reload() error = nil, want parse/normalization failure")
			}
			if calls := len(applier.snapshot()); calls != 0 {
				t.Fatalf("Apply calls = %d, want 0", calls)
			}
		})
	}
}

func TestStandaloneWatcherDoesNotRetainInMemoryLastGoodSnapshot(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: a\n    uri: /a\n#END\n")
	applier := &recordingStandaloneApplier{}
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	writeStandaloneConfig(t, path, `routes:
  - uri: /invalid-a
  - id: b
    uri: /b
#END
`)
	if err := watcher.Reload(); err == nil {
		t.Fatal("Reload() error = nil, want whole-file normalization failure")
	}
	batches := applier.snapshot()
	if len(batches) != 1 {
		t.Fatalf("Apply calls = %d, want only acknowledged A", len(batches))
	}
	if got := string(batches[0].Mutations[0].Value); !strings.Contains(got, `"id":"a"`) {
		t.Fatalf("first batch = %s, want A", got)
	}
}

func TestStandaloneWatcherStructDoesNotCacheRawSnapshots(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "standalone.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var watcherStruct *ast.StructType
	methods := make(map[string]struct{})
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.GenDecl:
			for _, specification := range declaration.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if ok && typeSpec.Name.Name == "StandaloneFileWatcher" {
					watcherStruct, _ = typeSpec.Type.(*ast.StructType)
				}
			}
		case *ast.FuncDecl:
			if declaration.Recv != nil {
				methods[declaration.Name.Name] = struct{}{}
			}
		}
	}
	if watcherStruct == nil {
		t.Fatal("StandaloneFileWatcher struct not found")
	}
	for _, field := range watcherStruct.Fields.List {
		if containsStandaloneRawBytes(field.Type) {
			t.Fatalf("StandaloneFileWatcher retains raw snapshot field %s", astTypeString(field.Type))
		}
	}
	for _, removed := range []string{
		"SeedCurrentSnapshot",
		"ReloadSnapshot",
		"SetReloadCallback",
		"SetAcknowledgedReloadCallback",
	} {
		if _, exists := methods[removed]; exists {
			t.Errorf("removed provider-local publication method %s still exists", removed)
		}
	}
}

func containsStandaloneRawBytes(expression ast.Expr) bool {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name == "standaloneSnapshot"
	case *ast.ArrayType:
		identifier, ok := expression.Elt.(*ast.Ident)
		return ok && identifier.Name == "byte"
	case *ast.MapType:
		return containsStandaloneRawBytes(expression.Value)
	case *ast.StarExpr:
		return containsStandaloneRawBytes(expression.X)
	}
	return false
}

func astTypeString(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.ArrayType:
		return "[]" + astTypeString(expression.Elt)
	case *ast.MapType:
		return "map[" + astTypeString(expression.Key) + "]" + astTypeString(expression.Value)
	case *ast.StarExpr:
		return "*" + astTypeString(expression.X)
	case *ast.SelectorExpr:
		return astTypeString(expression.X) + "." + expression.Sel.Name
	default:
		return reflect.TypeOf(expression).String()
	}
}

func TestStandaloneWatcherStopWaitsForApplyAndDoesNotAdvanceAfterCancellation(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	applyStarted := make(chan struct{})
	applyCanceled := make(chan struct{})
	releaseApply := make(chan struct{})
	applier := standaloneApplierFunc(func(
		ctx context.Context,
		_ generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		close(applyStarted)
		<-ctx.Done()
		close(applyCanceled)
		<-releaseApply
		return generation.Acknowledgement{}, ctx.Err()
	})
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- watcher.Reload() }()
	<-applyStarted
	stopDone := make(chan error, 1)
	go func() { stopDone <- watcher.Stop() }()
	<-applyCanceled
	select {
	case err := <-stopDone:
		t.Fatalf("Stop() returned before Apply exited: %v", err)
	default:
	}
	close(releaseApply)
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := <-reloadDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Reload() error = %v, want context.Canceled", err)
	}
	if watcher.acknowledgedRevisions != (generation.RevisionSet{}) {
		t.Fatalf("acknowledged revisions = %+v, want zero", watcher.acknowledgedRevisions)
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
}

func TestStandaloneWatcherRejectsIncompleteAcknowledgementsAtomically(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*generation.Acknowledgement)
	}{
		{name: "cursor mismatch", mutate: func(ack *generation.Acknowledgement) {
			ack.Cursor.Revision = "sha256:mismatch"
		}},
		{name: "zero desired revision", mutate: func(ack *generation.Acknowledgement) {
			ack.Revisions = generation.RevisionSet{}
		}},
		{name: "missing stream revision", mutate: func(ack *generation.Acknowledgement) {
			ack.Revisions.Stream = 0
		}},
		{name: "missing http decisions", mutate: func(ack *generation.Acknowledgement) {
			delete(ack.Decisions, generation.DomainHTTP)
		}},
		{name: "unknown decision key", mutate: func(ack *generation.Acknowledgement) {
			ack.Decisions[generation.DomainHTTP][0].Key.ID = "unknown"
		}},
		{name: "non-managed external delete", mutate: func(ack *generation.Acknowledgement) {
			ack.Decisions[generation.DomainHTTP] = append(
				ack.Decisions[generation.DomainHTTP],
				generation.ResourceDecision{
					Key:         generation.ResourceKey{Kind: "unknown", ID: "gone"},
					Disposition: generation.DispositionDeleted,
					Code:        "standalone-test-deleted",
				},
			)
		}},
		{name: "wrong-domain external delete", mutate: func(ack *generation.Acknowledgement) {
			ack.Decisions[generation.DomainStream] = append(
				ack.Decisions[generation.DomainStream],
				generation.ResourceDecision{
					Key:         generation.ResourceKey{Kind: "routes", ID: "gone"},
					Disposition: generation.DispositionDeleted,
					Code:        "standalone-test-deleted",
				},
			)
		}},
		{name: "partial cross-domain external delete", mutate: func(ack *generation.Acknowledgement) {
			ack.Decisions[generation.DomainHTTP] = append(
				ack.Decisions[generation.DomainHTTP],
				generation.ResourceDecision{
					Key:         generation.ResourceKey{Kind: "services", ID: "gone"},
					Disposition: generation.DispositionDeleted,
					Code:        "standalone-test-deleted",
				},
			)
		}},
		{name: "duplicate decision", mutate: func(ack *generation.Acknowledgement) {
			ack.Decisions[generation.DomainHTTP] = append(
				ack.Decisions[generation.DomainHTTP],
				ack.Decisions[generation.DomainHTTP][0],
			)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
			applier := standaloneApplierFunc(func(
				_ context.Context,
				batch generation.DesiredBatch,
			) (generation.Acknowledgement, error) {
				ack := standaloneAcknowledgement(batch, 3, nil)
				test.mutate(&ack)
				return ack, nil
			})
			watcher := NewStandaloneFileWatcher(
				path,
				standaloneProviderYAML,
				applier,
				testStandaloneDataEncryption(t, false, nil),
			)
			if err := watcher.Reload(); !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("Reload() error = %v, want ErrIntegrity", err)
			}
			if watcher.acknowledgedRevisions != (generation.RevisionSet{}) || len(watcher.knownKeys) != 0 {
				t.Fatalf(
					"invalid acknowledgement advanced state: %+v %v",
					watcher.acknowledgedRevisions,
					watcher.knownKeys,
				)
			}
		})
	}
}

func TestStandaloneWatcherDerivesQuarantineFromEveryAffectedDomain(t *testing.T) {
	path := writeStandaloneTestConfig(t, "services:\n  - id: s1\n    upstream_id: u1\n#END\n")
	key := generation.ResourceKey{Kind: "services", ID: "s1"}
	applier := standaloneApplierFunc(func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		ack := standaloneAcknowledgement(batch, 4, nil)
		ack.Decisions[generation.DomainStream][0].Disposition = generation.DispositionLastGood
		ack.Decisions[generation.DomainStream][0].Code = "standalone-test-last-good"
		return ack, nil
	})
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, exists := watcher.quarantine[key]; !exists || len(watcher.quarantine) != 1 {
		t.Fatalf("quarantine = %v, want services/s1", watcher.quarantine)
	}
}

func TestStandaloneWatcherAcceptsImplicitDeleteDecisionFromAcknowledgedState(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	calls := 0
	applier := standaloneApplierFunc(func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		calls++
		if calls == 1 {
			return standaloneAcknowledgement(batch, 1, nil), nil
		}
		ack := standaloneAcknowledgement(batch, 2, nil)
		ack.Decisions[generation.DomainHTTP] = []generation.ResourceDecision{{
			Key:         generation.ResourceKey{Kind: "routes", ID: "r1"},
			Disposition: generation.DispositionDeleted,
			Code:        "standalone-test-deleted",
		}}
		return ack, nil
	})
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	writeStandaloneConfig(t, path, "{}\n#END\n")
	if err := watcher.Reload(); err != nil {
		t.Fatalf("delete Reload() error = %v", err)
	}
	if len(watcher.knownKeys) != 0 || len(watcher.quarantine) != 0 {
		t.Fatalf("post-delete state = keys:%v quarantine:%v", watcher.knownKeys, watcher.quarantine)
	}
}

func TestStandaloneWatcherReplaysImplicitDeleteForSameCursor(t *testing.T) {
	coordinator := generation.NewCoordinator(standaloneReplayEngine{})

	first := desiredBatchFromStandalone(standaloneSnapshot{"services": {
		"a": []byte(`{"id":"a","upstream_id":"u1"}`),
		"b": []byte(`{"id":"b","upstream_id":"u1"}`),
	}})
	if _, err := coordinator.Apply(context.Background(), first); err != nil {
		t.Fatalf("Apply(A+B) error = %v", err)
	}
	current := desiredBatchFromStandalone(standaloneSnapshot{"services": {
		"b": []byte(`{"id":"b","upstream_id":"u1"}`),
	}})
	committed, err := coordinator.Apply(context.Background(), current)
	if err != nil {
		t.Fatalf("Apply(B) error = %v", err)
	}
	deleted := generation.ResourceKey{Kind: "services", ID: "a"}
	for _, domain := range current.RequiredDomains {
		if !slices.Contains(committed.Decisions[domain], generation.ResourceDecision{
			Key:         deleted,
			Disposition: generation.DispositionDeleted,
			Code:        "standalone-replay-deleted",
		}) {
			t.Fatalf("committed %s decisions = %+v, want Deleted(services/a)", domain, committed.Decisions[domain])
		}
	}

	path := writeStandaloneTestConfig(t, "services:\n  - id: b\n    upstream_id: u1\n#END\n")
	restarted := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		coordinator,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := restarted.Reload(); err != nil {
		t.Fatalf("restarted durable replay error = %v", err)
	}
	if restarted.acknowledgedCursor != current.Cursor || restarted.acknowledgedRevisions != committed.Revisions {
		t.Fatalf(
			"restarted acknowledgement = %+v/%+v, want %+v/%+v",
			restarted.acknowledgedCursor,
			restarted.acknowledgedRevisions,
			current.Cursor,
			committed.Revisions,
		)
	}
}

type standaloneReplayEngine struct{}

func (standaloneReplayEngine) Publish(
	_ context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	_ map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error) {
	closure := make([]generation.ResourceKey, 0, len(desired.Resources())+len(desired.Tombstones()))
	decisions := make([]generation.ResourceDecision, 0, cap(closure))
	for _, resource := range desired.Resources() {
		closure = append(closure, resource.Key)
		decisions = append(decisions, generation.ResourceDecision{
			Key: resource.Key, Disposition: generation.DispositionPublished, Code: "standalone-replay-published",
		})
	}
	for _, tombstone := range desired.Tombstones() {
		closure = append(closure, tombstone.Key)
		decisions = append(decisions, generation.ResourceDecision{
			Key: tombstone.Key, Disposition: generation.DispositionDeleted, Code: "standalone-replay-deleted",
		})
	}
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(ticket.RequiredDomains)),
	}
	for _, domain := range ticket.RequiredDomains {
		set.Domains[domain] = generation.PublicationCandidate{
			Artifact: generation.GenerationArtifact{
				Domain: domain, Revision: ticket.DesiredRevision,
				Digest: desired.Digest(), Snapshot: desired.SnapshotID(),
			},
			Snapshot:  desired.Clone(),
			Closure:   slices.Clone(closure),
			Decisions: slices.Clone(decisions),
		}
	}
	return set, nil
}

func TestStandaloneWatcherRejectsRevisionRegression(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	calls := 0
	applier := standaloneApplierFunc(func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		calls++
		return standaloneAcknowledgement(batch, uint64(3-calls), nil), nil
	})
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	writeStandaloneConfig(t, path, "routes:\n  - id: r1\n    uri: /two\n#END\n")
	if err := watcher.Reload(); !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("Reload() error = %v, want ErrIntegrity", err)
	}
	if watcher.acknowledgedRevisions.Desired != 2 {
		t.Fatalf("acknowledged desired = %d, want 2", watcher.acknowledgedRevisions.Desired)
	}
}

func TestStandaloneStartAndReconcileClosesRegistrationGap(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	applied := make(chan generation.DesiredBatch, 4)
	calls := uint64(0)
	applier := standaloneApplierFunc(func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		calls++
		applied <- cloneStandaloneTestBatch(batch)
		return standaloneAcknowledgement(batch, calls, nil), nil
	})
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	t.Cleanup(func() { _ = watcher.Stop() })
	if err := watcher.StartAndReconcile(); err != nil {
		t.Fatalf("StartAndReconcile() error = %v", err)
	}
	select {
	case <-applied:
	case <-time.After(time.Second):
		t.Fatal("initial full snapshot was not applied")
	}
	writeStandaloneConfig(t, path, "routes:\n  - id: r1\n    uri: /two\n#END\n")
	select {
	case <-applied:
	case <-time.After(3 * time.Second):
		t.Fatal("post-registration file event was not applied")
	}
}

func TestStandaloneStartAndReconcileReturnsInitialParseFailure(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: r1\n")
	applied := make(chan struct{}, 1)
	applier := standaloneApplierFunc(func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		applied <- struct{}{}
		return standaloneAcknowledgement(batch, 1, nil), nil
	})
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, false, nil),
	)
	t.Cleanup(func() { _ = watcher.Stop() })
	if err := watcher.StartAndReconcile(); err == nil {
		t.Fatal("StartAndReconcile() error = nil for invalid initial config")
	}
	select {
	case <-applied:
		t.Fatal("invalid initial file reached applier")
	default:
	}
	writeStandaloneConfig(t, path, "routes:\n  - id: r1\n    uri: /one\n#END\n")
	select {
	case <-applied:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not recover after transient parse failure")
	}
}

func TestStandaloneSnapshotDecodeFailures(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		content  string
		want     string
	}{
		{name: "unsupported provider", provider: "toml", content: "", want: "unsupported"},
		{name: "invalid yaml", provider: "yaml", content: "routes: [\n#END\n", want: "parse"},
		{name: "invalid json", provider: "json", content: "{", want: "parse"},
		{name: "null document", provider: "json", content: "null", want: "expected object"},
		{name: "unknown section", provider: "json", content: `{"unknown":[]}`, want: "unknown root section"},
		{name: "null bucket", provider: "json", content: `{"routes":null}`, want: "expected array"},
		{name: "object bucket", provider: "json", content: `{"routes":{}}`, want: "decode standalone routes"},
		{name: "missing id", provider: "json", content: `{"routes":[{"uri":"/"}]}`, want: "missing id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeStandaloneTestConfig(t, test.content)
			_, err := readStandaloneSnapshot(
				path,
				test.provider,
				testStandaloneDataEncryption(t, false, nil),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("readStandaloneSnapshot() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestStandaloneFileWatcherLoadsYAMLAndJSON(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		content  string
	}{
		{name: "yaml", provider: "yaml", content: "routes:\n  - id: r1\n    uri: /one\n#END\n"},
		{name: "json", provider: "json", content: `{"routes":[{"id":"r1","uri":"/one"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeStandaloneTestConfig(t, test.content)
			applier := &recordingStandaloneApplier{}
			watcher := NewStandaloneFileWatcher(
				path,
				test.provider,
				applier,
				testStandaloneDataEncryption(t, false, nil),
			)
			if err := watcher.Reload(); err != nil {
				t.Fatal(err)
			}
			batch := applier.snapshot()[0]
			if len(batch.Mutations) != 1 || batch.Mutations[0].Key != (generation.ResourceKey{
				Kind: "routes", ID: "r1",
			}) {
				t.Fatalf("batch mutations = %+v", batch.Mutations)
			}
		})
	}
}

func TestStandaloneFileWatcherEncryptsDeclaredPluginFieldsBeforeApply(t *testing.T) {
	const plaintext = "plaintext-secret"
	path := writeStandaloneTestConfig(t, `routes:
  - id: r1
    uri: /one
    plugins:
      key-auth:
        key: plaintext-secret
#END
`)
	applier := &recordingStandaloneApplier{}
	watcher := NewStandaloneFileWatcher(
		path,
		standaloneProviderYAML,
		applier,
		testStandaloneDataEncryption(t, true, []string{"qeddd145sfvddff3"}),
	)
	if err := watcher.Reload(); err != nil {
		t.Fatal(err)
	}
	value := applier.snapshot()[0].Mutations[0].Value
	if strings.Contains(string(value), plaintext) || !strings.Contains(string(value), "$encrypted://") {
		t.Fatalf("translated value = %s, want encrypted secret", value)
	}
}

func TestStandaloneStopBeforeStartAndRepeatedStop(t *testing.T) {
	watcher := NewStandaloneFileWatcher(
		filepath.Join(t.TempDir(), "apisix.yaml"),
		standaloneProviderYAML,
		&recordingStandaloneApplier{},
		testStandaloneDataEncryption(t, false, nil),
	)
	if err := watcher.Stop(); err != nil {
		t.Fatalf("Stop() before Start error = %v", err)
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
	if err := watcher.Reload(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reload() after Stop error = %v, want context.Canceled", err)
	}
}

func TestStandaloneConfigFile(t *testing.T) {
	for _, test := range []struct {
		provider string
		want     string
	}{
		{provider: "yaml", want: "conf/apisix.yaml"},
		{provider: " YAML ", want: "conf/apisix.yaml"},
		{provider: "json", want: "conf/apisix.json"},
		{provider: "toml", want: ""},
	} {
		if got := StandaloneConfigFile(test.provider); got != test.want {
			t.Errorf("StandaloneConfigFile(%q) = %q, want %q", test.provider, got, test.want)
		}
	}
}

func TestStandaloneProviderFromPath(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "conf/apisix.yaml", want: "yaml"},
		{path: "conf/apisix.JSON", want: "json"},
		{path: "conf/apisix", want: ""},
	} {
		if got := standaloneProviderFromPath(test.path); got != test.want {
			t.Errorf("standaloneProviderFromPath(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func writeStandaloneTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	writeStandaloneConfig(t, path, content)
	return path
}

func writeStandaloneConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resetStandaloneConfigApplyMetrics(t *testing.T, streamRequired bool) {
	t.Helper()
	oldFailures := metrics.ConfigApplyFailures
	oldReady := metrics.ConfigApplyReady
	oldQuarantine := metrics.ConfigApplyQuarantined
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_standalone_config_apply_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_standalone_config_apply_ready",
	})
	metrics.ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_standalone_config_apply_quarantine",
	})
	metrics.SetConfigApplyStreamRequired(streamRequired)
	t.Cleanup(func() {
		metrics.SetConfigApplyStreamRequired(false)
		metrics.ConfigApplyFailures = oldFailures
		metrics.ConfigApplyReady = oldReady
		metrics.ConfigApplyQuarantined = oldQuarantine
	})
}

type standaloneAcknowledgedState struct {
	cursor     generation.ProviderCursor
	revisions  generation.RevisionSet
	decisions  map[generation.Domain][]generation.ResourceDecision
	knownKeys  map[generation.ResourceKey]struct{}
	quarantine map[generation.ResourceKey]struct{}
}

func snapshotStandaloneAcknowledgedState(watcher *StandaloneFileWatcher) standaloneAcknowledgedState {
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	state := standaloneAcknowledgedState{
		cursor:     watcher.acknowledgedCursor,
		revisions:  watcher.acknowledgedRevisions,
		decisions:  make(map[generation.Domain][]generation.ResourceDecision, len(watcher.acknowledgedDecisions)),
		knownKeys:  make(map[generation.ResourceKey]struct{}, len(watcher.knownKeys)),
		quarantine: make(map[generation.ResourceKey]struct{}, len(watcher.quarantine)),
	}
	for domain, decisions := range watcher.acknowledgedDecisions {
		state.decisions[domain] = slices.Clone(decisions)
	}
	for key := range watcher.knownKeys {
		state.knownKeys[key] = struct{}{}
	}
	for key := range watcher.quarantine {
		state.quarantine[key] = struct{}{}
	}
	return state
}
