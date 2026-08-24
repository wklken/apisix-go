package compiler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	authz_casbin "github.com/wklken/apisix-go/pkg/plugin/authz_casbin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

const specialCasbinModel = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && r.obj == p.obj && r.act == p.act
`

const specialCasbinPolicy = `
p, alice, /orders/123, GET
p, anonymous, /public, GET
`

type specialMetadataBroker struct {
	mu       sync.Mutex
	values   map[string]string
	scopes   []secret.Scope
	raws     []string
	revoked  []secret.AttemptID
	resolved int
}

func (broker *specialMetadataBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (broker *specialMetadataBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *specialMetadataBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.resolved++
	broker.scopes = append(broker.scopes, scope)
	broker.raws = append(broker.raws, raw)
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return "resolved:" + raw, nil
}

func (broker *specialMetadataBroker) RevokeAttempt(_ context.Context, attempt secret.AttemptID) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.revoked = append(broker.revoked, attempt)
	return nil
}

type specialConsumerPreparer struct {
	bindings *runtime.ConsumerBindings
	calls    int
}

func (preparer *specialConsumerPreparer) PrepareConsumers(
	context.Context,
	PreparationAttempt,
	runtime.MetadataView,
) (*runtime.ConsumerBindings, error) {
	preparer.calls++
	return preparer.bindings, nil
}

type specialPluginPreparer struct {
	calls int
}

func (preparer *specialPluginPreparer) PreparePlugins(
	context.Context,
	PreparationAttempt,
	runtime.MetadataView,
	base.ConsumerLookup,
) (PreparedPlugins, error) {
	preparer.calls++
	return specialPreparedPlugins{}, nil
}

type specialPreparedPlugins struct{}

func (specialPreparedPlugins) Close(context.Context) error { return nil }

func newSpecialMetadataAttemptFactory(
	t *testing.T,
	broker *specialMetadataBroker,
) (*attemptFactory, *Compiler, *countingMaterializer, *specialConsumerPreparer, *specialPluginPreparer) {
	t.Helper()
	compiler := newTestCompiler(t)
	metadata, err := newMetadataPreparer(compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := runtime.NewConsumerBindings(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	materializer := &countingMaterializer{
		delegate: secret.NewScopedMaterializer(broker, compiler.schemas.catalog),
	}
	consumer := &specialConsumerPreparer{bindings: bindings}
	plugins := &specialPluginPreparer{}
	factory, err := newAttemptFactory(compiler, materializer, metadata, consumer, plugins)
	if err != nil {
		t.Fatal(err)
	}
	return factory, compiler, materializer, consumer, plugins
}

func specialCasbinMetadata(model, policy string) string {
	return fmt.Sprintf(`{"model":%q,"policy":%q}`, model, policy)
}

func assertSpecialCasbinFixtureConstructs(t *testing.T, document string) {
	t.Helper()
	view, err := runtime.NewMetadataView(map[string][]byte{
		"authz-casbin": []byte(document),
	})
	if err != nil {
		t.Fatal(err)
	}
	plugin := &authz_casbin.Plugin{}
	plugin.SetDependencies(base.Dependencies{Metadata: view})
	if err := plugin.Init(); err != nil {
		t.Fatalf("authz-casbin Init() error = %v", err)
	}
	config, ok := plugin.Config().(*authz_casbin.Config)
	if !ok {
		t.Fatalf("authz-casbin Config() type = %T", plugin.Config())
	}
	config.Username = "X-User"
	if err := plugin.PostInit(); err != nil {
		t.Fatalf("authz-casbin PostInit() failed to construct the enforcer: %v", err)
	}
}

func TestSpecialMetadataSecretScopesUseRealAttemptFactory(t *testing.T) {
	const (
		azureRaw    = `{"master_apikey":"$ENV://AZURE_MASTER_APIKEY","master_clientid":"client-n"}`
		errorLogRaw = `{"clickhouse":{"endpoint_addr":"http://127.0.0.1:8123","user":"default","password":"$ENV://ERROR_LOG_CLICKHOUSE_PASSWORD","database":"apisix","logtable":"error_log"},"kafka":{"brokers":[{"host":"kafka-a","port":9092,"sasl_config":{"user":"user-a","password":"$ENV://KAFKA_A_PASSWORD"}},{"host":"kafka-b","port":9092,"sasl_config":{"user":"user-b","password":"$ENV://KAFKA_B_PASSWORD"}}],"kafka_topic":"error-log"}}`
	)
	broker := &specialMetadataBroker{values: map[string]string{
		"$ENV://AZURE_MASTER_APIKEY":           "azure-secret",
		"$ENV://ERROR_LOG_CLICKHOUSE_PASSWORD": "clickhouse-secret",
		"$ENV://KAFKA_A_PASSWORD":              "kafka-secret-a",
		"$ENV://KAFKA_B_PASSWORD":              "kafka-secret-b",
	}}
	factory, _, materializer, consumer, plugins := newSpecialMetadataAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 601, []generation.Resource{
		resourceValue("plugin_metadata", "azure-functions", azureRaw),
		resourceValue("plugin_metadata", "error-log-logger", errorLogRaw),
	}, nil)
	prepared, err := factory.prepareCandidateAttempt(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	var azure map[string]any
	if found, err := prepared.metadata.Decode("azure-functions", &azure); err != nil || !found {
		t.Fatalf("Azure metadata Decode() = (%v, %v), want present", found, err)
	}
	if azure["master_apikey"] != "azure-secret" || azure["master_clientid"] != "client-n" {
		t.Fatalf("resolved Azure metadata = %#v", azure)
	}
	var errorLog map[string]any
	if found, err := prepared.metadata.Decode("error-log-logger", &errorLog); err != nil || !found {
		t.Fatalf("error-log metadata Decode() = (%v, %v), want present", found, err)
	}
	clickhouse := errorLog["clickhouse"].(map[string]any)
	if clickhouse["password"] != "clickhouse-secret" {
		t.Fatalf("resolved ClickHouse password = %#v", clickhouse["password"])
	}
	brokers := errorLog["kafka"].(map[string]any)["brokers"].([]any)
	if brokers[0].(map[string]any)["sasl_config"].(map[string]any)["password"] != "kafka-secret-a" ||
		brokers[1].(map[string]any)["sasl_config"].(map[string]any)["password"] != "kafka-secret-b" {
		t.Fatalf("resolved Kafka passwords = %#v", brokers)
	}

	candidate, ok := prepared.attempt.Candidate(generation.DomainHTTP)
	if !ok {
		t.Fatal("HTTP candidate is missing")
	}
	for _, want := range []struct {
		key generation.ResourceKey
		raw string
	}{
		{generation.ResourceKey{Kind: "plugin_metadata", ID: "azure-functions"}, azureRaw},
		{generation.ResourceKey{Kind: "plugin_metadata", ID: "error-log-logger"}, errorLogRaw},
	} {
		got, found := candidate.Snapshot.Lookup(want.key)
		if !found || !bytes.Equal(got, []byte(want.raw)) {
			t.Fatalf("candidate %v bytes = %q/%v, want original %q", want.key, got, found, want.raw)
		}
	}

	if materializer.candidateCalls != 1 || consumer.calls != 1 || plugins.calls != 1 {
		t.Fatalf(
			"factory calls = registration %d consumer %d plugin %d, want one each",
			materializer.candidateCalls,
			consumer.calls,
			plugins.calls,
		)
	}
	wantScopes := map[string]struct{}{
		"azure-functions|master_apikey|$ENV://AZURE_MASTER_APIKEY":                      {},
		"error-log-logger|clickhouse.password|$ENV://ERROR_LOG_CLICKHOUSE_PASSWORD":     {},
		"error-log-logger|kafka.brokers.*.sasl_config.password|$ENV://KAFKA_A_PASSWORD": {},
		"error-log-logger|kafka.brokers.*.sasl_config.password|$ENV://KAFKA_B_PASSWORD": {},
	}
	if len(broker.scopes) != len(wantScopes) {
		t.Fatalf("materialization scopes = %#v, want %d", broker.scopes, len(wantScopes))
	}
	for index, scope := range broker.scopes {
		key := scope.Plugin + "|" + scope.Field + "|" + broker.raws[index]
		if _, ok := wantScopes[key]; !ok {
			t.Fatalf("unexpected metadata scope/raw = %#v/%q", scope, broker.raws[index])
		}
		delete(wantScopes, key)
		if scope.Generation != desired.Revision() || scope.Attempt != prepared.attempt.AttemptID() ||
			scope.Domain != generation.DomainHTTP || scope.Source != capability.SecretPluginMetadata {
			t.Fatalf("metadata scope = %#v, want candidate HTTP authority", scope)
		}
		if scope.Plugin == "azure-functions" &&
			scope.Resource != (generation.ResourceKey{Kind: "plugin_metadata", ID: "azure-functions"}) {
			t.Fatalf("Azure scope resource = %#v", scope.Resource)
		}
		if scope.Plugin == "error-log-logger" &&
			scope.Resource != (generation.ResourceKey{Kind: "plugin_metadata", ID: "error-log-logger"}) {
			t.Fatalf("error-log scope resource = %#v", scope.Resource)
		}
	}
	if len(wantScopes) != 0 {
		t.Fatalf("missing metadata scopes = %v", wantScopes)
	}

	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(broker.revoked) != 1 || broker.revoked[0] != prepared.attempt.AttemptID() {
		t.Fatalf("revoked attempts = %x, want one candidate attempt", broker.revoked)
	}
}

func TestSpecialMetadataCandidateAndRecoveryScopesAreDistinct(t *testing.T) {
	const (
		candidateAzure = `{"master_apikey":"$ENV://AZURE_CANDIDATE","master_clientid":"candidate"}`
		candidateError = `{"clickhouse":{"endpoint_addr":"http://127.0.0.1:8123","user":"default","password":"$ENV://ERROR_CANDIDATE","database":"apisix","logtable":"error_log"}}`
		recoveryAzure  = `{"master_apikey":"$ENV://AZURE_RECOVERY","master_clientid":"recovery"}`
		recoveryError  = `{"clickhouse":{"endpoint_addr":"http://127.0.0.1:8123","user":"default","password":"$ENV://ERROR_RECOVERY","database":"apisix","logtable":"error_log"}}`
	)
	broker := &specialMetadataBroker{values: map[string]string{
		"$ENV://AZURE_CANDIDATE": "azure-candidate",
		"$ENV://ERROR_CANDIDATE": "error-candidate",
		"$ENV://AZURE_RECOVERY":  "azure-recovery",
		"$ENV://ERROR_RECOVERY":  "error-recovery",
	}}
	factory, _, _, _, _ := newSpecialMetadataAttemptFactory(t, broker)
	candidateSnapshot := mustGenerationSnapshot(t, 602, []generation.Resource{
		resourceValue("plugin_metadata", "azure-functions", candidateAzure),
		resourceValue("plugin_metadata", "error-log-logger", candidateError),
	}, nil)
	candidate, err := factory.prepareCandidateAttempt(
		context.Background(),
		ticketForSnapshot(candidateSnapshot, generation.DomainHTTP),
		candidateSnapshot,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	committedSnapshot := mustGenerationSnapshot(t, 603, []generation.Resource{
		resourceValue("plugin_metadata", "azure-functions", recoveryAzure),
		resourceValue("plugin_metadata", "error-log-logger", recoveryError),
	}, nil)
	revisions := generation.RevisionSet{Desired: 900, HTTP: committedSnapshot.Revision()}
	recovery, err := factory.prepareRecoveryAttempt(
		context.Background(), revisions, map[generation.Domain]generation.PublishedGeneration{
			generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, committedSnapshot),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.attempt.AttemptID() == recovery.attempt.AttemptID() {
		t.Fatal("candidate and recovery attempts unexpectedly share identity")
	}

	if len(broker.scopes) != 4 {
		t.Fatalf("materialization scopes = %#v, want candidate and recovery Azure/error-log fields", broker.scopes)
	}
	for index, scope := range broker.scopes {
		if scope.Domain != generation.DomainHTTP || scope.Source != capability.SecretPluginMetadata ||
			scope.Resource.Kind != "plugin_metadata" || scope.Resource.ID != scope.Plugin {
			t.Fatalf("scope[%d] = %#v, want HTTP plugin metadata scope", index, scope)
		}
		switch scope.Attempt {
		case candidate.attempt.AttemptID():
			if scope.Generation != candidateSnapshot.Revision() {
				t.Fatalf("candidate scope generation = %d, want %d", scope.Generation, candidateSnapshot.Revision())
			}
			if broker.raws[index] != map[string]string{
				"azure-functions":  "$ENV://AZURE_CANDIDATE",
				"error-log-logger": "$ENV://ERROR_CANDIDATE",
			}[scope.Plugin] {
				t.Fatalf("candidate scope[%d] = %#v/%q, want exact source field", index, scope, broker.raws[index])
			}
		case recovery.attempt.AttemptID():
			if scope.Generation != revisions.Desired {
				t.Fatalf("recovery scope generation = %d, want desired %d", scope.Generation, revisions.Desired)
			}
			if broker.raws[index] != map[string]string{
				"azure-functions":  "$ENV://AZURE_RECOVERY",
				"error-log-logger": "$ENV://ERROR_RECOVERY",
			}[scope.Plugin] {
				t.Fatalf("recovery scope[%d] = %#v/%q, want exact source field", index, scope, broker.raws[index])
			}
		default:
			t.Fatalf("scope[%d] uses unknown attempt %x", index, scope.Attempt)
		}
		wantField := map[string]string{
			"azure-functions":  "master_apikey",
			"error-log-logger": "clickhouse.password",
		}[scope.Plugin]
		if scope.Field != wantField {
			t.Fatalf("scope[%d] field = %q, want %q", index, scope.Field, wantField)
		}
	}

	var candidateAzureMetadata map[string]any
	if found, err := candidate.metadata.Decode("azure-functions", &candidateAzureMetadata); err != nil || !found {
		t.Fatalf("candidate Azure metadata Decode() = (%v, %v)", found, err)
	}
	var recoveryAzureMetadata map[string]any
	if found, err := recovery.metadata.Decode("azure-functions", &recoveryAzureMetadata); err != nil || !found {
		t.Fatalf("recovery Azure metadata Decode() = (%v, %v)", found, err)
	}
	if candidateAzureMetadata["master_apikey"] != "azure-candidate" ||
		recoveryAzureMetadata["master_apikey"] != "azure-recovery" {
		t.Fatalf("candidate/recovery Azure views = %#v/%#v", candidateAzureMetadata, recoveryAzureMetadata)
	}

	if err := candidate.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(broker.revoked) != 1 || broker.revoked[0] != candidate.attempt.AttemptID() {
		t.Fatalf("after candidate close revoked = %x, want candidate only", broker.revoked)
	}
	var stillLive map[string]any
	if found, err := recovery.metadata.Decode(
		"azure-functions",
		&stillLive,
	); err != nil || !found ||
		stillLive["master_apikey"] != "azure-recovery" {
		t.Fatalf("recovery metadata after candidate close = (%v, %v, %#v)", found, err, stillLive)
	}
	if err := recovery.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(broker.revoked) != 2 || broker.revoked[1] != recovery.attempt.AttemptID() {
		t.Fatalf("after recovery close revoked = %x, want both distinct attempts", broker.revoked)
	}
}

func TestSpecialMetadataLastGoodAndFailClosedStayInCompiler(t *testing.T) {
	rows := []struct {
		name    string
		factory string
		invalid string
		valid   string
	}{
		{
			name:    "chaitin-waf",
			factory: "chaitin-waf",
			invalid: `{"nodes":[]}`,
			valid:   `{"nodes":[{"host":"127.0.0.1","port":80}],"mode":"monitor"}`,
		},
		{
			name:    "authz-casbin",
			factory: "authz-casbin",
			invalid: `{"model":"model-without-policy"}`,
			valid:   specialCasbinMetadata(specialCasbinModel, specialCasbinPolicy),
		},
		{
			name:    "batch-requests",
			factory: "batch-requests",
			invalid: `{"max_concurrency":0}`,
			valid:   `{"max_concurrency":8,"max_timeout":30000}`,
		},
		{name: "error-log-logger", factory: "error-log-logger", invalid: `{"level":"WARN"}`, valid: `{}`},
		{
			name:    "opentelemetry",
			factory: "opentelemetry",
			invalid: `{"collector":{"request_timeout":"bad"}}`,
			valid:   `{"trace_id_source":"random"}`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			previousSnapshot := mustGenerationSnapshot(t, 700, []generation.Resource{
				resourceValue("plugin_metadata", row.factory, row.valid),
			}, nil)
			desired := mustGenerationSnapshot(t, 701, []generation.Resource{
				resourceValue("plugin_metadata", row.factory, row.invalid),
			}, nil)
			previous := map[generation.Domain]generation.PublishedGeneration{
				generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, previousSnapshot),
			}
			factory, compiler, materializer, consumer, plugins := newSpecialMetadataAttemptFactory(
				t, &specialMetadataBroker{},
			)
			ticket := ticketForSnapshot(desired, generation.DomainHTTP)
			set, err := compiler.PreparePublication(context.Background(), ticket, desired, previous)
			if err != nil {
				t.Fatal(err)
			}
			candidate := set.Domains[generation.DomainHTTP]
			assertDecision(
				t,
				candidate,
				generation.ResourceKey{Kind: "plugin_metadata", ID: row.factory},
				generation.DispositionLastGood,
				"plugin-metadata-schema-invalid",
			)
			got, found := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "plugin_metadata", ID: row.factory})
			if !found || !bytes.Equal(got, []byte(row.valid)) {
				t.Fatalf("last-good %s bytes = %q/%v, want predecessor %q", row.factory, got, found, row.valid)
			}
			if row.factory == "authz-casbin" {
				assertSpecialCasbinFixtureConstructs(t, row.valid)
			}
			prepared, err := factory.prepareCandidateAttempt(context.Background(), ticket, desired, previous)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepared.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if materializer.candidateCalls != 1 || consumer.calls != 1 || plugins.calls != 1 {
				t.Fatalf(
					"last-good factory calls = registration %d consumer %d plugin %d, want one each",
					materializer.candidateCalls,
					consumer.calls,
					plugins.calls,
				)
			}

			noPredecessorBroker := &specialMetadataBroker{}
			noPredecessorFactory, noPredecessorCompiler, noPredecessorMaterializer, noPredecessorConsumer, noPredecessorPlugins := newSpecialMetadataAttemptFactory(
				t,
				noPredecessorBroker,
			)
			noPredecessorSet, err := noPredecessorCompiler.PreparePublication(
				context.Background(), ticket, desired, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			noPredecessorCandidate := noPredecessorSet.Domains[generation.DomainHTTP]
			assertDecision(
				t,
				noPredecessorCandidate,
				generation.ResourceKey{Kind: "plugin_metadata", ID: row.factory},
				generation.DispositionFailClosed,
				"plugin-metadata-schema-invalid",
			)
			if _, found := noPredecessorCandidate.Snapshot.Lookup(
				generation.ResourceKey{Kind: "plugin_metadata", ID: row.factory},
			); found {
				t.Fatal("fail-closed metadata resource leaked into candidate")
			}
			if prepared, err := noPredecessorFactory.prepareCandidateAttempt(
				context.Background(), ticket, desired, nil,
			); prepared != nil || !errors.Is(err, errAttemptPreparationFailed) {
				t.Fatalf("no-predecessor preparation = %#v/%v, want fail-closed before registration", prepared, err)
			}
			if noPredecessorMaterializer.candidateCalls != 0 || noPredecessorBroker.resolved != 0 ||
				noPredecessorConsumer.calls != 0 || noPredecessorPlugins.calls != 0 {
				t.Fatalf(
					"fail-closed factory calls = registration %d resolution %d consumer %d plugin %d, want zero",
					noPredecessorMaterializer.candidateCalls,
					noPredecessorBroker.resolved,
					noPredecessorConsumer.calls,
					noPredecessorPlugins.calls,
				)
			}
		})
	}
}

func TestSpecialMetadataOTelAliasUsesRealAttemptFactory(t *testing.T) {
	broker := &specialMetadataBroker{}
	factory, compiler, _, _, _ := newSpecialMetadataAttemptFactory(t, broker)
	aliasSchema, ok := compiler.schemas.factories["otel"]
	if !ok || aliasSchema.metadata == nil {
		t.Fatal("manifest/compiler schema does not expose the otel metadata factory")
	}
	canonicalSchema, ok := compiler.schemas.factories["opentelemetry"]
	if !ok || canonicalSchema.metadata == nil {
		t.Fatal("manifest/compiler schema does not expose the canonical opentelemetry metadata factory")
	}

	nSnapshot := mustGenerationSnapshot(t, 705, []generation.Resource{
		resourceValue(
			"plugin_metadata",
			"otel",
			`{"trace_id_source":"random","collector":{"address":"otel-n:4318"}}`,
		),
	}, nil)
	n1Snapshot := mustGenerationSnapshot(t, 706, []generation.Resource{
		resourceValue(
			"plugin_metadata",
			"otel",
			`{"trace_id_source":"x-request-id","collector":{"address":"otel-n1:4318"}}`,
		),
	}, nil)
	n, err := factory.prepareCandidateAttempt(
		context.Background(),
		ticketForSnapshot(nSnapshot, generation.DomainHTTP),
		nSnapshot,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	n1, err := factory.prepareCandidateAttempt(
		context.Background(),
		ticketForSnapshot(n1Snapshot, generation.DomainHTTP),
		n1Snapshot,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name        string
		prepared    *registeredAttempt
		wantSource  string
		wantAddress string
	}{
		{name: "N", prepared: n, wantSource: "random", wantAddress: "otel-n:4318"},
		{name: "N+1", prepared: n1, wantSource: "x-request-id", wantAddress: "otel-n1:4318"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var metadata map[string]any
			found, err := test.prepared.metadata.Decode("otel", &metadata)
			if err != nil || !found {
				t.Fatalf("alias metadata Decode() = (%v, %v), want alias document", found, err)
			}
			if metadata["trace_id_source"] != test.wantSource ||
				metadata["collector"].(map[string]any)["address"] != test.wantAddress {
				t.Fatalf("alias metadata = %#v, want source/address %q/%q", metadata, test.wantSource, test.wantAddress)
			}
			if found, err := test.prepared.metadata.Decode("opentelemetry", &metadata); err != nil || found {
				t.Fatalf("canonical metadata Decode() = (%v, %v), want canonical key absent", found, err)
			}
			candidate, ok := test.prepared.attempt.Candidate(generation.DomainHTTP)
			if !ok {
				t.Fatal("HTTP candidate is missing")
			}
			if _, ok := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "plugin_metadata", ID: "otel"}); !ok {
				t.Fatal("alias plugin_metadata resource is missing from candidate")
			}
			if _, ok := candidate.Snapshot.Lookup(
				generation.ResourceKey{Kind: "plugin_metadata", ID: "opentelemetry"},
			); ok {
				t.Fatal("canonical plugin_metadata resource unexpectedly exists")
			}
		})
	}

	if err := n.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := n1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSpecialMetadataViewsRemainIndependentUnderRace(t *testing.T) {
	broker := &specialMetadataBroker{values: map[string]string{
		"$ENV://SPECIAL_N_PASSWORD":  "special-n-secret",
		"$ENV://SPECIAL_N1_PASSWORD": "special-n1-secret",
	}}
	factory, _, _, _, _ := newSpecialMetadataAttemptFactory(t, broker)
	nSnapshot := specialFiveMetadataSnapshot(t, 710, "n", "$ENV://SPECIAL_N_PASSWORD")
	n1Snapshot := specialFiveMetadataSnapshot(t, 711, "n1", "$ENV://SPECIAL_N1_PASSWORD")
	wantN := specialFiveMetadataExpected("n", "special-n-secret")
	wantN1 := specialFiveMetadataExpected("n1", "special-n1-secret")
	n, err := factory.prepareCandidateAttempt(
		context.Background(),
		ticketForSnapshot(nSnapshot, generation.DomainHTTP),
		nSnapshot,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	n1, err := factory.prepareCandidateAttempt(
		context.Background(),
		ticketForSnapshot(n1Snapshot, generation.DomainHTTP),
		n1Snapshot,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertSpecialMetadataView(t, n.metadata, wantN)
	assertSpecialMetadataView(t, n1.metadata, wantN1)

	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	decodeAll := func(view runtime.MetadataView, expected map[string]string) {
		defer wg.Done()
		for range 1000 {
			for _, factoryName := range specialMetadataFactoryNames {
				var document map[string]any
				found, err := view.Decode(factoryName, &document)
				if err != nil || !found {
					errorsCh <- fmt.Errorf("Decode(%q) = (%v, %v)", factoryName, found, err)
					return
				}
				got, ok := specialMetadataObservable(factoryName, document)
				if !ok || got != expected[factoryName] {
					errorsCh <- fmt.Errorf(
						"Decode(%q) observable = %q/%v, want %q",
						factoryName,
						got,
						ok,
						expected[factoryName],
					)
					return
				}
			}
		}
	}
	wg.Add(2)
	go decodeAll(n.metadata, wantN)
	go decodeAll(n1.metadata, wantN1)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}

	if err := n.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSpecialMetadataView(t, n1.metadata, wantN1)
	if err := n1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

var specialMetadataFactoryNames = []string{
	"chaitin-waf", "authz-casbin", "batch-requests", "error-log-logger", "opentelemetry",
}

func specialFiveMetadataExpected(marker, resolvedPassword string) map[string]string {
	batchConcurrency := 8
	if marker == "n1" {
		batchConcurrency = 9
	}
	traceIDSource := "random"
	if marker == "n1" {
		traceIDSource = "x-request-id"
	}
	return map[string]string{
		"chaitin-waf":      marker,
		"authz-casbin":     specialCasbinPolicyForMarker(marker),
		"batch-requests":   fmt.Sprintf("%d", batchConcurrency),
		"error-log-logger": resolvedPassword,
		"opentelemetry":    traceIDSource,
	}
}

func specialCasbinPolicyForMarker(marker string) string {
	return fmt.Sprintf("p, %s, /orders/123, GET\n", marker)
}

func specialMetadataObservable(factory string, document map[string]any) (string, bool) {
	switch factory {
	case "chaitin-waf":
		value, ok := document["marker"].(string)
		return value, ok
	case "authz-casbin":
		model, modelOK := document["model"].(string)
		policy, policyOK := document["policy"].(string)
		return policy, modelOK && policyOK && model == specialCasbinModel
	case "batch-requests":
		value, ok := document["max_concurrency"]
		return fmt.Sprint(value), ok
	case "error-log-logger":
		clickhouse, ok := document["clickhouse"].(map[string]any)
		if !ok {
			return "", false
		}
		value, ok := clickhouse["password"].(string)
		return value, ok
	case "opentelemetry":
		value, ok := document["trace_id_source"].(string)
		return value, ok
	default:
		return "", false
	}
}

func assertSpecialMetadataView(t *testing.T, view runtime.MetadataView, expected map[string]string) {
	t.Helper()
	for _, factory := range specialMetadataFactoryNames {
		var document map[string]any
		found, err := view.Decode(factory, &document)
		if err != nil || !found {
			t.Fatalf("metadata Decode(%q) = (%v, %v), want document", factory, found, err)
		}
		got, ok := specialMetadataObservable(factory, document)
		if !ok || got != expected[factory] {
			t.Fatalf("metadata %q observable = %q/%v, want %q", factory, got, ok, expected[factory])
		}
	}
}

func specialFiveMetadataSnapshot(t *testing.T, revision uint64, marker, password string) generation.Snapshot {
	t.Helper()
	batchConcurrency := 8
	if marker == "n1" {
		batchConcurrency = 9
	}
	return mustGenerationSnapshot(t, revision, []generation.Resource{
		resourceValue(
			"plugin_metadata",
			"chaitin-waf",
			fmt.Sprintf(`{"nodes":[{"host":"127.0.0.1","port":80}],"mode":"monitor","marker":%q}`, marker),
		),
		resourceValue(
			"plugin_metadata",
			"authz-casbin",
			specialCasbinMetadata(specialCasbinModel, specialCasbinPolicyForMarker(marker)),
		),
		resourceValue(
			"plugin_metadata",
			"batch-requests",
			fmt.Sprintf(`{"max_concurrency":%d,"max_timeout":30000}`, batchConcurrency),
		),
		resourceValue(
			"plugin_metadata",
			"error-log-logger",
			fmt.Sprintf(
				`{"clickhouse":{"endpoint_addr":"http://127.0.0.1:8123","user":"default","password":%q,"database":"apisix","logtable":"error_log"}}`,
				password,
			),
		),
		resourceValue(
			"plugin_metadata",
			"opentelemetry",
			fmt.Sprintf(`{"trace_id_source":"%s"}`, map[string]string{"n": "random", "n1": "x-request-id"}[marker]),
		),
	}, nil)
}

var _ secret.ScopedAttemptBroker = (*specialMetadataBroker)(nil)
