package compiler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type consumerPreparationBroker struct {
	resolved map[string]string
	scopes   []secret.Scope
}

type consumerAttemptFactory struct {
	registration *generationFactory
	consumers    *consumerBindingPreparer
}

type preparedConsumerAttempt struct {
	*preparedSecretGeneration
	consumers *runtime.ConsumerBindings
}

func (factory *consumerAttemptFactory) prepareGenerationSecrets(
	ctx context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	previous map[generation.Domain]generation.PublishedGeneration,
) (*preparedConsumerAttempt, error) {
	registered, err := factory.registration.prepareGenerationSecrets(ctx, ticket, desired, previous)
	if err != nil {
		return nil, err
	}
	consumers, err := factory.consumers.PrepareConsumers(ctx, registered.preparation)
	if err != nil {
		return nil, errors.Join(err, registered.Close(context.WithoutCancel(ctx)))
	}
	return &preparedConsumerAttempt{preparedSecretGeneration: registered, consumers: consumers}, nil
}

func (prepared *preparedConsumerAttempt) Close(ctx context.Context) error {
	prepared.consumers.Close()
	return prepared.preparedSecretGeneration.Close(ctx)
}

func (broker *consumerPreparationBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.scopes = append(broker.scopes, scope)
	resolved, ok := broker.resolved[raw]
	if !ok {
		return "", fmt.Errorf("unmapped test credential")
	}
	return resolved, nil
}

func newConsumerAttemptFactory(
	t *testing.T,
	broker *consumerPreparationBroker,
) (*consumerAttemptFactory, *Compiler) {
	t.Helper()
	compiler := newTestCompiler(t)
	materializer := testutil.NewSecretMaterializer(broker, compiler.schemas.catalog)
	preparer, err := newConsumerBindingPreparer(compiler.schemas.catalog)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := newGenerationFactory(compiler, materializer)
	if err != nil {
		t.Fatal(err)
	}
	return &consumerAttemptFactory{registration: factory, consumers: preparer}, compiler
}

func TestConsumerBindingPreparerMaterializesExactFinalConsumerOccurrences(t *testing.T) {
	broker := &consumerPreparationBroker{resolved: map[string]string{
		"$ENV://BASIC_USER":     "resolved-user",
		"$ENV://BASIC_PASSWORD": "resolved-password",
	}}
	factory, _ := newConsumerAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 51, []generation.Resource{
		resourceValue(
			"consumers",
			"consumer-1",
			`{"username":"consumer-1","group_id":"group-1","plugins":{"basic-auth":{"username":"$ENV://BASIC_USER","password":"$ENV://BASIC_PASSWORD"}}}`,
		),
		resourceValue("consumer_groups", "group-1", `{"id":"group-1","plugins":{}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)

	prepared, err := factory.prepareGenerationSecrets(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumer, ok := prepared.consumers.ConsumerByPluginKey("basic-auth", "resolved-user")
	if !ok || consumer.Username != "consumer-1" || consumer.GroupID != "group-1" {
		t.Fatalf("resolved consumer = %#v/%v", consumer, ok)
	}
	config, ok := consumer.Plugins["basic-auth"].(map[string]any)
	if !ok || config["username"] != "resolved-user" || config["password"] != "resolved-password" {
		t.Fatalf("resolved basic-auth config = %#v", consumer.Plugins["basic-auth"])
	}
	if _, ok := prepared.consumers.ConsumerGroupByID("group-1"); !ok {
		t.Fatal("resolved consumer group is missing")
	}
	if consumer.ConfigDigest == ([32]byte{}) {
		t.Fatal("consumer raw publication digest is zero")
	}
	if len(broker.scopes) != 2 {
		t.Fatalf("materialization scopes = %#v, want two declared fields", broker.scopes)
	}
	fields := make(map[string]bool, len(broker.scopes))
	for _, scope := range broker.scopes {
		if scope.Generation != 51 || scope.Generation != prepared.preparation.Generation() ||
			scope.Domain != generation.DomainHTTP || scope.Plugin != "basic-auth" ||
			scope.Resource != (generation.ResourceKey{Kind: "consumers", ID: "consumer-1"}) ||
			scope.Source != capability.SecretConsumerConfig {
			t.Fatalf("materialization scope = %#v", scope)
		}
		fields[scope.Field] = true
	}
	if !fields["username"] || !fields["password"] {
		t.Fatalf("materialized fields = %v", fields)
	}

	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := prepared.consumers.ConsumerByPluginKey("basic-auth", "resolved-user"); ok {
		t.Fatal("closed consumer bindings still expose credentials")
	}
	raw, ok := desired.Lookup(generation.ResourceKey{Kind: "consumers", ID: "consumer-1"})
	if !ok || !strings.Contains(string(raw), "$ENV://BASIC_USER") {
		t.Fatalf("preparation mutated input snapshot: %s", raw)
	}
}

func TestConsumerBindingPreparerReturnsEmptyBindingsForStreamOnlyAndConsumerTombstone(t *testing.T) {
	tests := map[string]struct {
		desired generation.Snapshot
		domain  generation.Domain
	}{
		"stream only": {
			desired: mustGenerationSnapshot(t, 52, []generation.Resource{
				resourceValue("stream_routes", "stream-1", `{"id":"stream-1"}`),
			}, nil),
			domain: generation.DomainStream,
		},
		"consumer tombstone": {
			desired: mustGenerationSnapshot(t, 53, nil, []generation.Tombstone{{
				Key: generation.ResourceKey{Kind: "consumers", ID: "deleted-consumer"}, Revision: 52,
			}}),
			domain: generation.DomainHTTP,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			broker := &consumerPreparationBroker{}
			factory, _ := newConsumerAttemptFactory(t, broker)
			prepared, err := factory.prepareGenerationSecrets(
				context.Background(), ticketForSnapshot(test.desired, test.domain), test.desired, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := prepared.consumers.ConsumerByID("deleted-consumer"); ok {
				t.Fatal("non-HTTP or tombstoned consumer was indexed")
			}
			if len(broker.scopes) != 0 {
				t.Fatalf("unexpected materialization scopes = %#v", broker.scopes)
			}
			if err := prepared.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConsumerBindingPreparerSkipsMissingOptionalDeclaredFields(t *testing.T) {
	broker := &consumerPreparationBroker{resolved: map[string]string{
		"$ENV://JWT_KEY": "resolved-jwt-key",
	}}
	factory, _ := newConsumerAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 54, []generation.Resource{
		resourceValue(
			"consumers",
			"jwt-consumer",
			`{"username":"jwt-consumer","plugins":{"jwt-auth":{"key":"$ENV://JWT_KEY","exp":60}}}`,
		),
	}, nil)
	prepared, err := factory.prepareGenerationSecrets(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := prepared.consumers.ConsumerByPluginKey("jwt-auth", "resolved-jwt-key"); !ok {
		t.Fatal("resolved JWT consumer is missing")
	}
	if len(broker.scopes) != 1 || broker.scopes[0].Field != "key" {
		t.Fatalf("optional field materialization scopes = %#v, want key only", broker.scopes)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConsumerBindingPreparerIndexesNonCredentialConsumerPlugin(t *testing.T) {
	broker := &consumerPreparationBroker{}
	factory, _ := newConsumerAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 64, []generation.Resource{
		resourceValue(
			"consumers",
			"rate-limited-consumer",
			`{"username":"rate-limited-consumer","plugins":{"limit-count":{"count":4,"time_window":60}}}`,
		),
	}, nil)
	prepared, err := factory.prepareGenerationSecrets(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer, ok := prepared.consumers.ConsumerByID("rate-limited-consumer")
	if !ok {
		t.Fatal("consumer with a non-credential plugin was not indexed")
	}
	config, ok := consumer.Plugins["limit-count"].(map[string]any)
	if !ok || fmt.Sprint(config["count"]) != "4" || fmt.Sprint(config["time_window"]) != "60" {
		t.Fatalf("prepared limit-count config = %#v", consumer.Plugins["limit-count"])
	}
	if len(broker.scopes) != 0 {
		t.Fatalf("non-credential plugin materialized credential scopes: %#v", broker.scopes)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConsumerBindingPreparerPreservesEmptyResolvedLookupCompatibility(t *testing.T) {
	broker := &consumerPreparationBroker{resolved: map[string]string{
		"$ENV://EMPTY_USER": "",
		"$ENV://PASSWORD":   "resolved-password",
	}}
	factory, _ := newConsumerAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 55, []generation.Resource{
		resourceValue(
			"consumers",
			"empty-user-consumer",
			`{"username":"empty-user-consumer","plugins":{"basic-auth":{"username":"$ENV://EMPTY_USER","password":"$ENV://PASSWORD"}}}`,
		),
	}, nil)
	prepared, err := factory.prepareGenerationSecrets(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer, ok := prepared.consumers.ConsumerByPluginKey("basic-auth", "")
	if !ok || consumer.Username != "empty-user-consumer" {
		t.Fatalf("empty resolved lookup = (%+v, %v)", consumer, ok)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConsumerBindingPreparerRejectsDuplicateResolvedLookupWithoutCredentialLeak(t *testing.T) {
	broker := &consumerPreparationBroker{resolved: map[string]string{
		"$ENV://USER_ONE": "duplicate-resolved-user",
		"$ENV://PASS_ONE": "resolved-password-one",
		"$ENV://USER_TWO": "duplicate-resolved-user",
		"$ENV://PASS_TWO": "resolved-password-two",
	}}
	factory, _ := newConsumerAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 56, []generation.Resource{
		resourceValue(
			"consumers",
			"consumer-one",
			`{"username":"consumer-one","plugins":{"basic-auth":{"username":"$ENV://USER_ONE","password":"$ENV://PASS_ONE"}}}`,
		),
		resourceValue(
			"consumers",
			"consumer-two",
			`{"username":"consumer-two","plugins":{"basic-auth":{"username":"$ENV://USER_TWO","password":"$ENV://PASS_TWO"}}}`,
		),
	}, nil)
	_, err := factory.prepareGenerationSecrets(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err == nil {
		t.Fatal("duplicate resolved consumer lookup unexpectedly succeeded")
	}
	for _, sensitive := range []string{
		"USER_ONE", "PASS_ONE", "USER_TWO", "PASS_TWO",
		"duplicate-resolved-user", "resolved-password-one", "resolved-password-two",
	} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("duplicate error leaked %q: %v", sensitive, err)
		}
	}
}

func TestConsumerBindingPreparerFailedCandidatePreservesLastGoodGeneration(t *testing.T) {
	const (
		oldUserRef       = "$ENV://LAST_GOOD_USER"
		oldPasswordRef   = "$ENV://LAST_GOOD_PASSWORD"
		newUserRef       = "$ENV://NEXT_USER"
		newPasswordRef   = "$ENV://NEXT_PASSWORD"
		oldResolvedUser  = "last-good-user"
		newResolvedUser  = "next-user"
		consumerResource = "rotating-consumer"
	)
	broker := &consumerPreparationBroker{resolved: map[string]string{
		oldUserRef:     oldResolvedUser,
		oldPasswordRef: "last-good-password",
	}}
	factory, _ := newConsumerAttemptFactory(t, broker)
	consumerSnapshot := func(revision uint64, usernameRef, passwordRef string) generation.Snapshot {
		t.Helper()
		return mustGenerationSnapshot(t, revision, []generation.Resource{
			resourceValue(
				"consumers",
				consumerResource,
				fmt.Sprintf(
					`{"username":%q,"plugins":{"basic-auth":{"username":%q,"password":%q}}}`,
					consumerResource,
					usernameRef,
					passwordRef,
				),
			),
		}, nil)
	}

	lastGoodSnapshot := consumerSnapshot(61, oldUserRef, oldPasswordRef)
	lastGood, err := factory.prepareGenerationSecrets(
		context.Background(),
		ticketForSnapshot(lastGoodSnapshot, generation.DomainHTTP),
		lastGoodSnapshot,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertLastGood := func() {
		t.Helper()
		consumer, ok := lastGood.consumers.ConsumerByPluginKey("basic-auth", oldResolvedUser)
		if !ok || consumer.Username != consumerResource {
			t.Fatalf("last-good lookup = %#v/%v", consumer, ok)
		}
	}
	assertLastGood()

	failedSnapshot := consumerSnapshot(62, newUserRef, newPasswordRef)
	previous := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, lastGoodSnapshot),
	}
	if failed, prepareErr := factory.prepareGenerationSecrets(
		context.Background(),
		ticketForSnapshot(failedSnapshot, generation.DomainHTTP),
		failedSnapshot,
		previous,
	); prepareErr == nil {
		if failed != nil {
			_ = failed.Close(context.Background())
		}
		t.Fatal("candidate with unavailable consumer credentials unexpectedly prepared")
	}
	assertLastGood()
	if _, ok := lastGood.consumers.ConsumerByPluginKey("basic-auth", newResolvedUser); ok {
		t.Fatal("failed candidate credential leaked into last-good generation")
	}

	broker.resolved[newUserRef] = newResolvedUser
	broker.resolved[newPasswordRef] = "next-password"
	recoveredSnapshot := consumerSnapshot(63, newUserRef, newPasswordRef)
	recovered, err := factory.prepareGenerationSecrets(
		context.Background(),
		ticketForSnapshot(recoveredSnapshot, generation.DomainHTTP),
		recoveredSnapshot,
		previous,
	)
	if err != nil {
		t.Fatal(err)
	}
	recoveredConsumer, ok := recovered.consumers.ConsumerByPluginKey("basic-auth", newResolvedUser)
	if !ok || recoveredConsumer.Username != consumerResource {
		t.Fatalf("recovered lookup = %#v/%v", recoveredConsumer, ok)
	}
	assertLastGood()
	if _, ok := recovered.consumers.ConsumerByPluginKey("basic-auth", oldResolvedUser); ok {
		t.Fatal("recovered generation retained last-good credential")
	}

	if err := lastGood.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := recovered.consumers.ConsumerByPluginKey("basic-auth", newResolvedUser); !ok {
		t.Fatal("closing last-good generation invalidated recovered generation")
	}
	if err := recovered.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConsumerBindingPreparerKeepsOverlappingGenerationsIndependent(t *testing.T) {
	broker := &consumerPreparationBroker{resolved: map[string]string{
		"$ENV://USER_N":        "resolved-user-n",
		"$ENV://PASSWORD_N":    "resolved-password-n",
		"$ENV://USER_N_PLUS_1": "resolved-user-n-plus-1",
		"$ENV://PASS_N_PLUS_1": "resolved-password-n-plus-1",
	}}
	factory, _ := newConsumerAttemptFactory(t, broker)
	prepare := func(revision uint64, id, usernameRef, passwordRef string) *preparedConsumerAttempt {
		t.Helper()
		desired := mustGenerationSnapshot(t, revision, []generation.Resource{
			resourceValue(
				"consumers", id,
				fmt.Sprintf(
					`{"username":%q,"plugins":{"basic-auth":{"username":%q,"password":%q}}}`,
					id, usernameRef, passwordRef,
				),
			),
		}, nil)
		prepared, err := factory.prepareGenerationSecrets(
			context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		return prepared
	}

	generationN := prepare(57, "consumer-n", "$ENV://USER_N", "$ENV://PASSWORD_N")
	generationN1 := prepare(
		58, "consumer-n-plus-1", "$ENV://USER_N_PLUS_1", "$ENV://PASS_N_PLUS_1",
	)
	if _, ok := generationN.consumers.ConsumerByPluginKey("basic-auth", "resolved-user-n"); !ok {
		t.Fatal("generation N lookup is missing")
	}
	if _, ok := generationN1.consumers.ConsumerByPluginKey("basic-auth", "resolved-user-n-plus-1"); !ok {
		t.Fatal("generation N+1 lookup is missing")
	}

	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 1000 {
				generationN.consumers.ConsumerByPluginKey("basic-auth", "resolved-user-n")
			}
		})
	}
	if err := generationN.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	if _, ok := generationN.consumers.ConsumerByPluginKey("basic-auth", "resolved-user-n"); ok {
		t.Fatal("closed generation N remains readable")
	}
	if _, ok := generationN1.consumers.ConsumerByPluginKey("basic-auth", "resolved-user-n-plus-1"); !ok {
		t.Fatal("closing generation N invalidated generation N+1")
	}
	if err := generationN1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConsumerBindingPreparerRejectsForeignOccurrenceBeforeMaterialization(t *testing.T) {
	compiler := newTestCompiler(t)
	broker := &consumerPreparationBroker{resolved: map[string]string{
		"$ENV://BASIC_USER":     "resolved-user",
		"$ENV://BASIC_PASSWORD": "resolved-password",
	}}
	materializer := testutil.NewSecretMaterializer(broker, compiler.schemas.catalog)
	preparer, err := newConsumerBindingPreparer(compiler.schemas.catalog)
	if err != nil {
		t.Fatal(err)
	}
	desired := mustGenerationSnapshot(t, 59, []generation.Resource{
		resourceValue(
			"consumers",
			"consumer-1",
			`{"username":"consumer-1","plugins":{"basic-auth":{"username":"$ENV://BASIC_USER","password":"$ENV://BASIC_PASSWORD"}}}`,
		),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	set, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	specs, err := finalFactoryOccurrences(context.Background(), ticket, set, compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	specs = append(specs, factoryOccurrenceSpec{
		domain:   generation.DomainHTTP,
		resource: generation.ResourceKey{Kind: "consumers", ID: "foreign-consumer"},
		source:   capability.SecretConsumerConfig,
		factory:  "basic-auth",
	})
	materialization, err := materializer.PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	attempt, err := newPreparationGeneration(desired.Revision(), set.Domains, secrets, specs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.PrepareConsumers(context.Background(), attempt); err == nil {
		t.Fatal("foreign consumer occurrence unexpectedly succeeded")
	}
	if len(broker.scopes) != 0 {
		t.Fatalf("foreign occurrence reached materialization: %#v", broker.scopes)
	}
	if err := materialization.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConsumerPreparerHasNoMetadataDependency(t *testing.T) {
	preparer, err := newConsumerBindingPreparer(newTestCompiler(t).schemas.catalog)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = preparer.PrepareConsumers(context.Background(), PreparationGeneration{})
}

var (
	_ testutil.SecretResolver = (*consumerPreparationBroker)(nil)
	_ ConsumerPreparer        = (*consumerBindingPreparer)(nil)
)
