package compiler

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestConsumerCredentialsPreserveParentAndSecretAuthority(t *testing.T) {
	broker := &consumerPreparationBroker{resolved: map[string]string{"$ENV://SECOND_CREDENTIAL": "second-key"}}
	factory, _ := newConsumerAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 51, []generation.Resource{
		resourceValue(
			"consumers",
			"rose",
			`{"username":"rose","labels":{"custom_id":"team-a"},"plugins":{"limit-count":{"count":1,"time_window":60}}}`,
		),
		resourceValue(
			"consumers",
			"rose/credentials/first",
			`{"id":"rose/credentials/first","plugins":{"key-auth":{"key":"first-key"}}}`,
		),
		resourceValue(
			"consumers",
			"rose/credentials/second",
			`{"id":"second","plugins":{"key-auth":{"key":"$ENV://SECOND_CREDENTIAL"}}}`,
		),
	}, nil)
	prepared, err := factory.prepareGenerationSecrets(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	lookup := newConsumerLookupView(prepared.consumers, prepared.preparation, factory.consumers.catalog)
	for _, pair := range []struct{ key, id string }{{"first-key", "first"}, {"second-key", "second"}} {
		used, err := base.UseConsumerCredential(
			context.Background(),
			lookup,
			"key-auth",
			pair.key,
			func(c resource.Consumer, config resource.PluginConfig) error {
				if c.ID != "rose" || c.Username != "rose" || c.CredentialID != pair.id ||
					c.Labels["custom_id"] != "team-a" {
					t.Fatalf("credential parent=%+v", c)
				}
				if c.Plugins["limit-count"] == nil || c.Plugins["key-auth"] != nil {
					t.Fatalf("credential changed parent plugins: %v", c.Plugins)
				}
				if config.(map[string]any)["key"] != pair.key {
					t.Fatalf("credential config=%v", config)
				}
				return nil
			},
		)
		if err != nil || !used {
			t.Fatalf("key=%s lookup=%v err=%v", pair.key, used, err)
		}
	}
	if len(broker.scopes) == 0 {
		t.Fatal("secret credential was not materialized")
	}
	for _, scope := range broker.scopes {
		if scope.Resource.ID != "rose/credentials/second" {
			t.Fatalf("secret authority=%v", scope.Resource)
		}
	}
	if _, ok := prepared.consumers.ConsumerByID("rose/credentials/first"); ok {
		t.Fatal("credential is exposed as a standalone consumer")
	}
	parent, ok := prepared.consumers.ConsumerByID("rose")
	if !ok || parent.CredentialID != "" {
		t.Fatalf("parent lookup=%+v %v", parent, ok)
	}
}

func TestConsumerCredentialRemovalPreservesParentAndOldGeneration(t *testing.T) {
	factory, _ := newConsumerAttemptFactory(t, &consumerPreparationBroker{})
	parent := resourceValue("consumers", "rose", `{"username":"rose"}`)
	desired := mustGenerationSnapshot(
		t,
		51,
		[]generation.Resource{
			parent,
			resourceValue("consumers", "rose/credentials/first", `{"plugins":{"key-auth":{"key":"first-key"}}}`),
		},
		nil,
	)
	previous, err := factory.prepareGenerationSecrets(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := previous.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	nextSnapshot := mustGenerationSnapshot(t, 52, []generation.Resource{parent}, nil)
	next, err := factory.prepareGenerationSecrets(
		context.Background(),
		ticketForSnapshot(nextSnapshot, generation.DomainHTTP),
		nextSnapshot,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := next.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if _, ok := next.consumers.ConsumerByID("rose"); !ok {
		t.Fatal("removing credential removed parent")
	}
	if _, ok := next.consumers.ConsumerByPluginKey("key-auth", "first-key"); ok {
		t.Fatal("deleted credential survived new generation")
	}
	if _, ok := previous.consumers.ConsumerByPluginKey("key-auth", "first-key"); !ok {
		t.Fatal("new generation mutated previous credential")
	}
}
