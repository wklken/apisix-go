package runtime

import (
	"errors"
	"reflect"
	"sort"
	"sync"

	"github.com/wklken/apisix-go/pkg/resource"
)

var (
	errConsumerIDRequired           = errors.New("consumer bindings: consumer id is required")
	errConsumerIDDuplicate          = errors.New("consumer bindings: duplicate consumer id")
	errConsumerGroupIDRequired      = errors.New("consumer bindings: consumer group id is required")
	errConsumerGroupIDDuplicate     = errors.New("consumer bindings: duplicate consumer group id")
	errCredentialPluginRequired     = errors.New("consumer bindings: credential plugin is required")
	errCredentialConsumerIDRequired = errors.New("consumer bindings: credential consumer id is required")
	errCredentialDuplicate          = errors.New("consumer bindings: duplicate credential")
	errCredentialConsumerUnknown    = errors.New("consumer bindings: credential references unknown consumer")
)

type ConsumerRecord struct {
	ID       string
	Consumer resource.Consumer
}

type ConsumerGroupRecord struct {
	ID    string
	Group resource.ConsumerGroup
}

type ConsumerCredentialBinding struct {
	Plugin     string
	Key        string
	ConsumerID string
}

type consumerCredentialKey struct {
	plugin string
	key    string
}

// ConsumerBindings is an immutable, generation-local consumer index.
// Lookups return defensive copies and are safe to call concurrently with Close.
type ConsumerBindings struct {
	mu          sync.RWMutex
	closed      bool
	consumers   map[string]resource.Consumer
	groups      map[string]resource.ConsumerGroup
	credentials map[consumerCredentialKey]string
}

func NewConsumerBindings(
	consumerRecords []ConsumerRecord,
	groupRecords []ConsumerGroupRecord,
	credentialRecords []ConsumerCredentialBinding,
) (*ConsumerBindings, error) {
	consumers, err := buildConsumerIndex(consumerRecords)
	if err != nil {
		return nil, err
	}
	groups, err := buildConsumerGroupIndex(groupRecords)
	if err != nil {
		return nil, err
	}
	credentials, err := buildConsumerCredentialIndex(credentialRecords, consumers)
	if err != nil {
		return nil, err
	}
	return &ConsumerBindings{
		consumers:   consumers,
		groups:      groups,
		credentials: credentials,
	}, nil
}

func (bindings *ConsumerBindings) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	if bindings == nil {
		return resource.Consumer{}, false
	}
	bindings.mu.RLock()
	defer bindings.mu.RUnlock()
	if bindings.closed {
		return resource.Consumer{}, false
	}
	consumerID, ok := bindings.credentials[consumerCredentialKey{plugin: plugin, key: key}]
	if !ok {
		return resource.Consumer{}, false
	}
	consumer, ok := bindings.consumers[consumerID]
	if !ok {
		return resource.Consumer{}, false
	}
	return cloneBoundConsumer(consumer), true
}

func (bindings *ConsumerBindings) ConsumerByID(id string) (resource.Consumer, bool) {
	if bindings == nil {
		return resource.Consumer{}, false
	}
	bindings.mu.RLock()
	defer bindings.mu.RUnlock()
	if bindings.closed {
		return resource.Consumer{}, false
	}
	consumer, ok := bindings.consumers[id]
	if !ok {
		return resource.Consumer{}, false
	}
	return cloneBoundConsumer(consumer), true
}

func (bindings *ConsumerBindings) ConsumerGroupByID(id string) (resource.ConsumerGroup, bool) {
	if bindings == nil {
		return resource.ConsumerGroup{}, false
	}
	bindings.mu.RLock()
	defer bindings.mu.RUnlock()
	if bindings.closed {
		return resource.ConsumerGroup{}, false
	}
	group, ok := bindings.groups[id]
	if !ok {
		return resource.ConsumerGroup{}, false
	}
	return cloneBoundConsumerGroup(group), true
}

// Close drops the maps controlled by the binding. It cannot erase Go string
// storage that may still be retained elsewhere in the process.
func (bindings *ConsumerBindings) Close() {
	if bindings == nil {
		return
	}
	bindings.mu.Lock()
	bindings.closed = true
	bindings.consumers = nil
	bindings.groups = nil
	bindings.credentials = nil
	bindings.mu.Unlock()
}

func buildConsumerIndex(records []ConsumerRecord) (map[string]resource.Consumer, error) {
	ordered := append([]ConsumerRecord(nil), records...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].ID < ordered[right].ID
	})
	consumers := make(map[string]resource.Consumer, len(ordered))
	for _, record := range ordered {
		if record.ID == "" {
			return nil, errConsumerIDRequired
		}
		if _, exists := consumers[record.ID]; exists {
			return nil, errConsumerIDDuplicate
		}
		consumers[record.ID] = cloneBoundConsumer(record.Consumer)
	}
	return consumers, nil
}

func buildConsumerGroupIndex(records []ConsumerGroupRecord) (map[string]resource.ConsumerGroup, error) {
	ordered := append([]ConsumerGroupRecord(nil), records...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].ID < ordered[right].ID
	})
	groups := make(map[string]resource.ConsumerGroup, len(ordered))
	for _, record := range ordered {
		if record.ID == "" {
			return nil, errConsumerGroupIDRequired
		}
		if _, exists := groups[record.ID]; exists {
			return nil, errConsumerGroupIDDuplicate
		}
		groups[record.ID] = cloneBoundConsumerGroup(record.Group)
	}
	return groups, nil
}

func buildConsumerCredentialIndex(
	records []ConsumerCredentialBinding,
	consumers map[string]resource.Consumer,
) (map[consumerCredentialKey]string, error) {
	ordered := append([]ConsumerCredentialBinding(nil), records...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Plugin != ordered[right].Plugin {
			return ordered[left].Plugin < ordered[right].Plugin
		}
		if ordered[left].Key != ordered[right].Key {
			return ordered[left].Key < ordered[right].Key
		}
		return ordered[left].ConsumerID < ordered[right].ConsumerID
	})
	credentials := make(map[consumerCredentialKey]string, len(ordered))
	for _, record := range ordered {
		if record.Plugin == "" {
			return nil, errCredentialPluginRequired
		}
		if record.ConsumerID == "" {
			return nil, errCredentialConsumerIDRequired
		}
		credential := consumerCredentialKey{plugin: record.Plugin, key: record.Key}
		if _, exists := credentials[credential]; exists {
			return nil, errCredentialDuplicate
		}
		if _, exists := consumers[record.ConsumerID]; !exists {
			return nil, errCredentialConsumerUnknown
		}
		credentials[credential] = record.ConsumerID
	}
	return credentials, nil
}

func cloneBoundConsumer(consumer resource.Consumer) resource.Consumer {
	consumer.Plugins = cloneBoundPluginConfigs(consumer.Plugins)
	consumer.Labels = cloneBoundStringAnyMap(consumer.Labels)
	consumer.AuthConf = cloneBoundValue(consumer.AuthConf)
	return consumer
}

func cloneBoundConsumerGroup(group resource.ConsumerGroup) resource.ConsumerGroup {
	group.Plugins = cloneBoundPluginConfigs(group.Plugins)
	return group
}

func cloneBoundPluginConfigs(configs map[string]resource.PluginConfig) map[string]resource.PluginConfig {
	if configs == nil {
		return nil
	}
	cloned := make(map[string]resource.PluginConfig, len(configs))
	for name, config := range configs {
		cloned[name] = cloneBoundValue(config)
	}
	return cloned
}

func cloneBoundStringAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = cloneBoundValue(value)
	}
	return cloned
}

func cloneBoundValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneBoundReflectValue(reflect.ValueOf(value)).Interface()
}

func cloneBoundReflectValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(cloneBoundReflectValue(value.Elem()))
		return cloned
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type().Elem())
		cloned.Elem().Set(cloneBoundReflectValue(value.Elem()))
		return cloned
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			cloned.SetMapIndex(iterator.Key(), cloneBoundReflectValue(iterator.Value()))
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := range value.Len() {
			cloned.Index(index).Set(cloneBoundReflectValue(value.Index(index)))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for index := range value.Len() {
			cloned.Index(index).Set(cloneBoundReflectValue(value.Index(index)))
		}
		return cloned
	case reflect.Struct:
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(value)
		for index := range value.NumField() {
			if value.Type().Field(index).PkgPath == "" {
				cloned.Field(index).Set(cloneBoundReflectValue(value.Field(index)))
			}
		}
		return cloned
	default:
		return value
	}
}
