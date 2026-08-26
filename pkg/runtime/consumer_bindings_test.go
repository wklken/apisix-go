package runtime

import (
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestConsumerBindingsDefensivelyCopiesConsumersAndGroups(t *testing.T) {
	consumerConfig := map[string]any{
		"nested": map[string]any{
			"items": []any{"original", map[string]string{"key": "value"}},
		},
	}
	consumer := resource.Consumer{
		Username: "alice",
		Plugins:  map[string]resource.PluginConfig{"key-auth": consumerConfig},
		Labels:   map[string]any{"teams": []string{"platform"}},
	}
	groupConfig := map[string]any{"rules": []any{map[string]any{"limit": 1}}}
	group := resource.ConsumerGroup{
		Plugins: map[string]resource.PluginConfig{"limit-count": groupConfig},
	}

	bindings, err := NewConsumerBindings(
		[]ConsumerRecord{{ID: "consumer-1", Consumer: consumer}},
		[]ConsumerGroupRecord{{ID: "group-1", Group: group}},
		[]ConsumerCredentialBinding{{Plugin: "key-auth", Key: "credential-1", ConsumerID: "consumer-1"}},
	)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}

	consumer.Username = "mutated"
	consumerConfig["nested"].(map[string]any)["items"].([]any)[0] = "mutated"
	consumer.Labels["teams"].([]string)[0] = "mutated"
	groupConfig["rules"].([]any)[0].(map[string]any)["limit"] = 99

	gotConsumer, ok := bindings.ConsumerByID("consumer-1")
	if !ok {
		t.Fatal("ConsumerByID() did not find consumer")
	}
	assertConsumerUnchanged(t, gotConsumer)
	gotGroup, ok := bindings.ConsumerGroupByID("group-1")
	if !ok {
		t.Fatal("ConsumerGroupByID() did not find group")
	}
	assertConsumerGroupUnchanged(t, gotGroup)

	gotConsumer.Username = "output-mutated"
	gotConsumer.Plugins["key-auth"].(map[string]any)["nested"].(map[string]any)["items"].([]any)[0] = "output-mutated"
	gotConsumer.Labels["teams"].([]string)[0] = "output-mutated"
	gotGroup.Plugins["limit-count"].(map[string]any)["rules"].([]any)[0].(map[string]any)["limit"] = 101

	secondConsumer, ok := bindings.ConsumerByPluginKey("key-auth", "credential-1")
	if !ok {
		t.Fatal("ConsumerByPluginKey() did not find credential")
	}
	assertConsumerUnchanged(t, secondConsumer)
	secondGroup, ok := bindings.ConsumerGroupByID("group-1")
	if !ok {
		t.Fatal("ConsumerGroupByID() did not find group after output mutation")
	}
	assertConsumerGroupUnchanged(t, secondGroup)
}

func TestConsumerBindingsIndexesAnonymousConsumerAndCredential(t *testing.T) {
	bindings, err := NewConsumerBindings(
		[]ConsumerRecord{
			{ID: "anonymous", Consumer: resource.Consumer{Username: "anonymous"}},
			{ID: "consumer-1", Consumer: resource.Consumer{Username: "alice"}},
		},
		nil,
		[]ConsumerCredentialBinding{{Plugin: "basic-auth", Key: "alice", ConsumerID: "consumer-1"}},
	)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}

	if consumer, ok := bindings.ConsumerByID("anonymous"); !ok || consumer.Username != "anonymous" {
		t.Fatalf("ConsumerByID(anonymous) = (%+v, %v)", consumer, ok)
	}
	if consumer, ok := bindings.ConsumerByPluginKey("basic-auth", "alice"); !ok || consumer.Username != "alice" {
		t.Fatalf("ConsumerByPluginKey() = (%+v, %v)", consumer, ok)
	}
	if _, ok := bindings.ConsumerByPluginKey("basic-auth", "missing"); ok {
		t.Fatal("ConsumerByPluginKey() found a missing credential")
	}
}

func TestConsumerBindingsPreservesEmptyCredentialLookupKeyCompatibility(t *testing.T) {
	bindings, err := NewConsumerBindings(
		[]ConsumerRecord{{ID: "consumer-1", Consumer: resource.Consumer{Username: "consumer-1"}}},
		nil,
		[]ConsumerCredentialBinding{{Plugin: "basic-auth", Key: "", ConsumerID: "consumer-1"}},
	)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}
	consumer, ok := bindings.ConsumerByPluginKey("basic-auth", "")
	if !ok || consumer.Username != "consumer-1" {
		t.Fatalf("ConsumerByPluginKey(empty) = (%+v, %v)", consumer, ok)
	}
}

func TestConsumerBindingsRejectsInvalidInputDeterministicallyAndRedacted(t *testing.T) {
	validConsumer := resource.Consumer{Username: "alice"}
	tests := []struct {
		name        string
		consumers   []ConsumerRecord
		groups      []ConsumerGroupRecord
		credentials []ConsumerCredentialBinding
		want        string
	}{
		{
			name:      "empty consumer id",
			consumers: []ConsumerRecord{{ID: "secret-consumer"}, {ID: ""}},
			want:      "consumer bindings: consumer id is required",
		},
		{
			name: "duplicate consumer id",
			consumers: []ConsumerRecord{
				{ID: "secret-consumer", Consumer: validConsumer},
				{ID: "secret-consumer", Consumer: validConsumer},
			},
			want: "consumer bindings: duplicate consumer id",
		},
		{
			name:   "empty group id",
			groups: []ConsumerGroupRecord{{ID: "secret-group"}, {ID: ""}},
			want:   "consumer bindings: consumer group id is required",
		},
		{
			name:   "duplicate group id",
			groups: []ConsumerGroupRecord{{ID: "secret-group"}, {ID: "secret-group"}},
			want:   "consumer bindings: duplicate consumer group id",
		},
		{
			name:        "empty plugin",
			consumers:   []ConsumerRecord{{ID: "consumer-1", Consumer: validConsumer}},
			credentials: []ConsumerCredentialBinding{{Plugin: "", Key: "secret-key", ConsumerID: "consumer-1"}},
			want:        "consumer bindings: credential plugin is required",
		},
		{
			name:        "empty credential consumer id",
			consumers:   []ConsumerRecord{{ID: "consumer-1", Consumer: validConsumer}},
			credentials: []ConsumerCredentialBinding{{Plugin: "key-auth", Key: "secret-key", ConsumerID: ""}},
			want:        "consumer bindings: credential consumer id is required",
		},
		{
			name:      "duplicate credential",
			consumers: []ConsumerRecord{{ID: "consumer-1", Consumer: validConsumer}},
			credentials: []ConsumerCredentialBinding{
				{Plugin: "key-auth", Key: "secret-key", ConsumerID: "consumer-1"},
				{Plugin: "key-auth", Key: "secret-key", ConsumerID: "consumer-1"},
			},
			want: "consumer bindings: duplicate credential",
		},
		{
			name:      "unknown consumer",
			consumers: []ConsumerRecord{{ID: "consumer-1", Consumer: validConsumer}},
			credentials: []ConsumerCredentialBinding{
				{Plugin: "key-auth", Key: "secret-key", ConsumerID: "secret-unknown"},
			},
			want: "consumer bindings: credential references unknown consumer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, firstErr := NewConsumerBindings(tt.consumers, tt.groups, tt.credentials)
			if firstErr == nil || firstErr.Error() != tt.want {
				t.Fatalf("NewConsumerBindings() error = %v, want %q", firstErr, tt.want)
			}
			_, secondErr := NewConsumerBindings(
				reverseConsumers(tt.consumers),
				reverseGroups(tt.groups),
				reverseCredentials(tt.credentials),
			)
			if secondErr == nil || secondErr.Error() != firstErr.Error() {
				t.Fatalf("permuted NewConsumerBindings() error = %v, want %v", secondErr, firstErr)
			}
			if strings.Contains(firstErr.Error(), "secret-") {
				t.Fatalf("NewConsumerBindings() leaked input in error: %v", firstErr)
			}
		})
	}
}

func TestConsumerBindingsCloseIsIdempotentAndConcurrentWithReads(t *testing.T) {
	bindings, err := NewConsumerBindings(
		[]ConsumerRecord{{ID: "consumer-1", Consumer: resource.Consumer{Username: "alice"}}},
		[]ConsumerGroupRecord{{ID: "group-1", Group: resource.ConsumerGroup{}}},
		[]ConsumerCredentialBinding{{Plugin: "key-auth", Key: "credential-1", ConsumerID: "consumer-1"}},
	)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}

	start := make(chan struct{})
	var readers sync.WaitGroup
	for range 16 {
		readers.Go(func() {
			<-start
			for range 1000 {
				bindings.ConsumerByID("consumer-1")
				bindings.ConsumerByPluginKey("key-auth", "credential-1")
				bindings.ConsumerGroupByID("group-1")
			}
		})
	}
	close(start)
	bindings.Close()
	bindings.Close()
	readers.Wait()

	if _, ok := bindings.ConsumerByID("consumer-1"); ok {
		t.Fatal("ConsumerByID() returned a consumer after Close")
	}
	if _, ok := bindings.ConsumerByPluginKey("key-auth", "credential-1"); ok {
		t.Fatal("ConsumerByPluginKey() returned a consumer after Close")
	}
	if _, ok := bindings.ConsumerGroupByID("group-1"); ok {
		t.Fatal("ConsumerGroupByID() returned a group after Close")
	}
}

func assertConsumerUnchanged(t *testing.T, consumer resource.Consumer) {
	t.Helper()
	if consumer.Username != "alice" {
		t.Fatalf("consumer username = %q, want alice", consumer.Username)
	}
	items := consumer.Plugins["key-auth"].(map[string]any)["nested"].(map[string]any)["items"].([]any)
	if items[0] != "original" || items[1].(map[string]string)["key"] != "value" {
		t.Fatalf("consumer plugin config = %#v, want original values", consumer.Plugins)
	}
	if consumer.Labels["teams"].([]string)[0] != "platform" {
		t.Fatalf("consumer labels = %#v, want original values", consumer.Labels)
	}
}

func assertConsumerGroupUnchanged(t *testing.T, group resource.ConsumerGroup) {
	t.Helper()
	limit := group.Plugins["limit-count"].(map[string]any)["rules"].([]any)[0].(map[string]any)["limit"]
	if limit != 1 {
		t.Fatalf("consumer group plugins = %#v, want original values", group.Plugins)
	}
}

func reverseConsumers(records []ConsumerRecord) []ConsumerRecord {
	result := append([]ConsumerRecord(nil), records...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func reverseGroups(records []ConsumerGroupRecord) []ConsumerGroupRecord {
	result := append([]ConsumerGroupRecord(nil), records...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func reverseCredentials(records []ConsumerCredentialBinding) []ConsumerCredentialBinding {
	result := append([]ConsumerCredentialBinding(nil), records...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}
