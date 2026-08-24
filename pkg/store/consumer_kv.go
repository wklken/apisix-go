package store

import (
	"fmt"
	"strings"

	consumerregistry "github.com/wklken/apisix-go/pkg/consumer"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type consumerSnapshot struct {
	id               []byte
	consumer         resource.Consumer
	pluginKeys       []string
	referencePlugins []string
}

func (s *Store) prepareConsumerSnapshot(id []byte, value []byte) (consumerSnapshot, error) {
	consumer, err := s.ParseConsumer(value)
	if err != nil {
		return consumerSnapshot{}, err
	}
	for _, factory := range [...]string{
		"key-auth", "basic-auth", "jwt-auth", "hmac-auth", "ldap-auth", "wolf-rbac", "jwe-decrypt",
	} {
		if err := validateResolvedConsumerPlugin(consumer.Plugins, factory); err != nil {
			return consumerSnapshot{}, err
		}
	}
	pluginKeys := make([]string, 0, len(consumer.Plugins))
	referencePlugins := make([]string, 0, len(consumer.Plugins))
	for _, factory := range [...]string{
		"key-auth", "basic-auth", "jwt-auth", "hmac-auth", "ldap-auth", "jwe-decrypt", "wolf-rbac",
	} {
		pluginKeys, referencePlugins, err = addResolvedConsumerLookup(
			pluginKeys, referencePlugins, consumer.Plugins, factory,
		)
		if err != nil {
			return consumerSnapshot{}, err
		}
	}

	return consumerSnapshot{
		id:               append([]byte(nil), id...),
		consumer:         consumer,
		pluginKeys:       pluginKeys,
		referencePlugins: referencePlugins,
	}, nil
}

func validateResolvedConsumerPlugin(plugins map[string]resource.PluginConfig, factory string) error {
	config, ok := plugins[factory]
	if !ok {
		return nil
	}
	return consumerregistry.ValidateResolved(factory, config)
}

func addResolvedConsumerLookup(
	pluginKeys []string,
	referencePlugins []string,
	plugins map[string]resource.PluginConfig,
	factory string,
) ([]string, []string, error) {
	config, ok := plugins[factory]
	if !ok {
		return pluginKeys, referencePlugins, nil
	}
	key, err := consumerregistry.LookupKey(factory, config)
	if err != nil {
		return pluginKeys, referencePlugins, err
	}
	pluginKeys, referencePlugins = addConsumerLookupKey(pluginKeys, referencePlugins, factory, key)
	return pluginKeys, referencePlugins, nil
}

func addConsumerLookupKey(pluginKeys, referencePlugins []string, pluginName, key string) ([]string, []string) {
	if isConsumerSecretReference(key) {
		return pluginKeys, append(referencePlugins, pluginName)
	}
	return append(pluginKeys, fmt.Sprintf("%s:%s", pluginName, key)), referencePlugins
}

func isConsumerSecretReference(value string) bool {
	return (len(value) >= len(environmentSecretPrefix) &&
		strings.EqualFold(value[:len(environmentSecretPrefix)], environmentSecretPrefix)) ||
		strings.HasPrefix(value, managedSecretPrefix)
}

func consumerPluginLookupKey(pluginName string, config resource.PluginConfig) (string, error) {
	return consumerregistry.LookupKey(pluginName, config)
}

func (s *Store) consumerKVAdd(id []byte, value []byte) error {
	snapshot, err := s.prepareConsumerSnapshot(id, value)
	if err != nil {
		return err
	}
	return s.applyConsumerSnapshot(snapshot)
}

func (s *Store) applyConsumerSnapshot(snapshot consumerSnapshot) error {
	s.consumerMu.Lock()
	defer s.consumerMu.Unlock()
	if s.consumerKV == nil {
		s.consumerKV = make(map[string][]byte)
	}
	if s.consumerIDs == nil {
		s.consumerIDs = make(map[string][]byte)
	}
	key := util.BytesToString(snapshot.id)
	for _, pluginKey := range snapshot.pluginKeys {
		if owner := string(s.consumerKV[pluginKey]); owner != "" && owner != key {
			return duplicateConsumerLookupKeyError(pluginKey, owner)
		}
	}
	if keys, ok := s.consumerToKeys[key]; ok {
		for _, oldKey := range keys {
			if string(s.consumerKV[oldKey]) == key {
				delete(s.consumerKV, oldKey)
			}
		}
	}
	for _, pluginName := range s.consumerToReferences[key] {
		delete(s.consumerReferenceKV[pluginName], key)
		if len(s.consumerReferenceKV[pluginName]) == 0 {
			delete(s.consumerReferenceKV, pluginName)
		}
	}
	consumerID := append([]byte(nil), snapshot.id...)
	s.consumerIDs[key] = consumerID
	s.consumerToKeys[key] = snapshot.pluginKeys
	for _, pluginKey := range snapshot.pluginKeys {
		s.consumerKV[pluginKey] = consumerID
	}
	if s.consumerValues == nil {
		s.consumerValues = make(map[string]resource.Consumer)
	}
	if s.consumerReferenceKV == nil {
		s.consumerReferenceKV = make(map[string]map[string][]byte)
	}
	if s.consumerToReferences == nil {
		s.consumerToReferences = make(map[string][]string)
	}
	for _, pluginName := range snapshot.referencePlugins {
		if s.consumerReferenceKV[pluginName] == nil {
			s.consumerReferenceKV[pluginName] = make(map[string][]byte)
		}
		s.consumerReferenceKV[pluginName][key] = consumerID
	}
	s.consumerToReferences[key] = snapshot.referencePlugins
	s.consumerValues[key] = snapshot.consumer
	s.consumerGeneration.Add(1)
	return nil
}

func duplicateConsumerLookupKeyError(pluginKey, owner string) error {
	return fmt.Errorf("consumer lookup key %q is already owned by consumer %q", pluginKey, owner)
}

func (s *Store) consumerKVDelete(id []byte) error {
	s.consumerMu.Lock()
	defer s.consumerMu.Unlock()
	key := util.BytesToString(id)

	// clear old keys
	if keys, ok := s.consumerToKeys[key]; ok {
		for _, k := range keys {
			if string(s.consumerKV[k]) == key {
				delete(s.consumerKV, k)
			}
		}
		delete(s.consumerToKeys, key)
	}
	for _, pluginName := range s.consumerToReferences[key] {
		delete(s.consumerReferenceKV[pluginName], key)
		if len(s.consumerReferenceKV[pluginName]) == 0 {
			delete(s.consumerReferenceKV, pluginName)
		}
	}
	delete(s.consumerToReferences, key)

	// delete self
	delete(s.consumerIDs, key)
	delete(s.consumerValues, key)
	s.consumerGeneration.Add(1)

	return nil
}

func (s *Store) GetConsumerNameByPluginKey(pluginName string, key string) ([]byte, error) {
	k := fmt.Sprintf("%s:%s", pluginName, key)
	s.consumerMu.RLock()
	defer s.consumerMu.RUnlock()
	id, ok := s.consumerKV[k]
	if !ok {
		return []byte{}, ErrNotFound
	}
	return append([]byte(nil), id...), nil
}
