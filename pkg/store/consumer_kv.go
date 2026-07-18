package store

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type keyAuth struct {
	Key string `json:"key"`
}

type basicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

const basicAuthConsumerSchema = `
{
  "type": "object",
  "title": "work with consumer object",
  "required": ["username", "password"],
  "properties": {
    "username": {"type": "string"},
    "password": {"type": "string"}
  }
}`

type jwtAuth struct {
	Key string `json:"key"`
}

type hmacAuth struct {
	KeyID string `json:"key_id"`
}

type ldapAuth struct {
	UserDN string `json:"user_dn"`
}

type jweDecrypt struct {
	Key             any  `json:"key"`
	Secret          any  `json:"secret"`
	IsBase64Encoded bool `json:"is_base64_encoded"`
}

type wolfRBAC struct {
	AppID string `json:"appid"`
}

type consumerSnapshot struct {
	id               []byte
	consumer         resource.Consumer
	pluginKeys       []string
	referencePlugins []string
}

func (s *Store) prepareConsumerSnapshot(id []byte, value []byte) (consumerSnapshot, error) {
	consumer, err := ParseConsumer(value)
	if err != nil {
		return consumerSnapshot{}, err
	}
	if basicAuthPlugin, ok := consumer.Plugins["basic-auth"]; ok {
		if err := util.Validate(basicAuthPlugin, basicAuthConsumerSchema); err != nil {
			return consumerSnapshot{}, fmt.Errorf("basic-auth consumer configuration: %w", err)
		}
	}
	jweDecryptPlugin, hasJWEDecrypt := consumer.Plugins["jwe-decrypt"]
	var jweDecryptConfig jweDecrypt
	if hasJWEDecrypt {
		if err := util.Parse(jweDecryptPlugin, &jweDecryptConfig); err != nil {
			return consumerSnapshot{}, err
		}
		if err := validateJWEDecryptConsumerConfig(jweDecryptConfig); err != nil {
			return consumerSnapshot{}, err
		}
	}
	pluginKeys := make([]string, 0, len(consumer.Plugins))
	referencePlugins := make([]string, 0, len(consumer.Plugins))
	keyAuthPlugin, ok := consumer.Plugins["key-auth"]
	if ok {
		var ka keyAuth
		if err := util.Parse(keyAuthPlugin, &ka); err != nil {
			return consumerSnapshot{}, err
		}
		pluginKeys, referencePlugins = addConsumerLookupKey(
			pluginKeys, referencePlugins, "key-auth", ka.Key,
		)
	}
	basicAuthPlugin, ok := consumer.Plugins["basic-auth"]
	if ok {
		var ba basicAuth
		if err := util.Parse(basicAuthPlugin, &ba); err != nil {
			return consumerSnapshot{}, err
		}
		pluginKeys, referencePlugins = addConsumerLookupKey(
			pluginKeys, referencePlugins, "basic-auth", ba.Username,
		)
	}
	jwtAuthPlugin, ok := consumer.Plugins["jwt-auth"]
	if ok {
		var ja jwtAuth
		if err := util.Parse(jwtAuthPlugin, &ja); err != nil {
			return consumerSnapshot{}, err
		}
		pluginKeys, referencePlugins = addConsumerLookupKey(
			pluginKeys, referencePlugins, "jwt-auth", ja.Key,
		)
	}
	hmacAuthPlugin, ok := consumer.Plugins["hmac-auth"]
	if ok {
		var ha hmacAuth
		if err := util.Parse(hmacAuthPlugin, &ha); err != nil {
			return consumerSnapshot{}, err
		}
		pluginKeys, referencePlugins = addConsumerLookupKey(
			pluginKeys, referencePlugins, "hmac-auth", ha.KeyID,
		)
	}
	ldapAuthPlugin, ok := consumer.Plugins["ldap-auth"]
	if ok {
		var la ldapAuth
		if err := util.Parse(ldapAuthPlugin, &la); err != nil {
			return consumerSnapshot{}, err
		}
		pluginKeys, referencePlugins = addConsumerLookupKey(
			pluginKeys, referencePlugins, "ldap-auth", la.UserDN,
		)
	}
	if hasJWEDecrypt {
		pluginKeys, referencePlugins = addConsumerLookupKey(
			pluginKeys, referencePlugins, "jwe-decrypt", jweDecryptConfig.Key.(string),
		)
	}
	wolfRBACPlugin, ok := consumer.Plugins["wolf-rbac"]
	if ok {
		var wr wolfRBAC
		if err := util.Parse(wolfRBACPlugin, &wr); err != nil {
			return consumerSnapshot{}, err
		}
		pluginKeys, referencePlugins = addConsumerLookupKey(
			pluginKeys, referencePlugins, "wolf-rbac", wr.AppID,
		)
	}

	return consumerSnapshot{
		id:               append([]byte(nil), id...),
		consumer:         consumer,
		pluginKeys:       pluginKeys,
		referencePlugins: referencePlugins,
	}, nil
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
	switch pluginName {
	case "key-auth":
		var parsed keyAuth
		if err := util.Parse(config, &parsed); err != nil {
			return "", err
		}
		return parsed.Key, nil
	case "basic-auth":
		var parsed basicAuth
		if err := util.Parse(config, &parsed); err != nil {
			return "", err
		}
		return parsed.Username, nil
	case "jwt-auth":
		var parsed jwtAuth
		if err := util.Parse(config, &parsed); err != nil {
			return "", err
		}
		return parsed.Key, nil
	case "hmac-auth":
		var parsed hmacAuth
		if err := util.Parse(config, &parsed); err != nil {
			return "", err
		}
		return parsed.KeyID, nil
	case "ldap-auth":
		var parsed ldapAuth
		if err := util.Parse(config, &parsed); err != nil {
			return "", err
		}
		return parsed.UserDN, nil
	case "jwe-decrypt":
		var parsed jweDecrypt
		if err := util.Parse(config, &parsed); err != nil {
			return "", err
		}
		key, ok := parsed.Key.(string)
		if !ok {
			return "", fmt.Errorf("jwe-decrypt consumer key must be a string")
		}
		return key, nil
	case "wolf-rbac":
		var parsed wolfRBAC
		if err := util.Parse(config, &parsed); err != nil {
			return "", err
		}
		return parsed.AppID, nil
	default:
		return "", fmt.Errorf("consumer lookup is unsupported for plugin %q", pluginName)
	}
}

func (s *Store) consumerKVAdd(id []byte, value []byte) error {
	snapshot, err := s.prepareConsumerSnapshot(id, value)
	if err != nil {
		return err
	}
	s.applyConsumerSnapshot(snapshot)
	return nil
}

func (s *Store) applyConsumerSnapshot(snapshot consumerSnapshot) {
	s.consumerMu.Lock()
	defer s.consumerMu.Unlock()
	key := util.BytesToString(snapshot.id)
	if keys, ok := s.consumerToKeys[key]; ok {
		for _, oldKey := range keys {
			delete(s.consumerKV, oldKey)
		}
	}
	for _, pluginName := range s.consumerToReferences[key] {
		delete(s.consumerReferenceKV[pluginName], key)
		if len(s.consumerReferenceKV[pluginName]) == 0 {
			delete(s.consumerReferenceKV, pluginName)
		}
	}
	consumerID := append([]byte(nil), snapshot.id...)
	s.consumerKV[key] = consumerID
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
}

func validateJWEDecryptConsumerConfig(config jweDecrypt) error {
	_, ok := config.Key.(string)
	if !ok {
		return fmt.Errorf("jwe-decrypt consumer key must be a string")
	}
	secret, ok := config.Secret.(string)
	if !ok {
		return fmt.Errorf("jwe-decrypt consumer secret must be a string")
	}
	if config.IsBase64Encoded {
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(secret, "="))
		if err != nil {
			decoded, err = base64.StdEncoding.DecodeString(secret)
			if err != nil {
				return fmt.Errorf("jwe-decrypt consumer secret base64 decode: %w", err)
			}
		}
		if len(decoded) != 32 {
			return fmt.Errorf("the secret length after base64 decode should be 32 chars")
		}
		return nil
	}
	if len(secret) != 32 {
		return fmt.Errorf("the secret length should be 32 chars")
	}
	return nil
}

func (s *Store) consumerKVDelete(id []byte) error {
	s.consumerMu.Lock()
	defer s.consumerMu.Unlock()
	key := util.BytesToString(id)

	// clear old keys
	if keys, ok := s.consumerToKeys[key]; ok {
		for _, k := range keys {
			delete(s.consumerKV, k)
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
	delete(s.consumerKV, key)
	delete(s.consumerValues, key)

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
