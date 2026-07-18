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

func (s *Store) consumerKVAdd(id []byte, value []byte) error {
	consumer, err := ParseConsumer(value)
	if err != nil {
		return err
	}
	if err := s.resolveConsumerSecrets(&consumer); err != nil {
		return err
	}
	if basicAuthPlugin, ok := consumer.Plugins["basic-auth"]; ok {
		if err := util.Validate(basicAuthPlugin, basicAuthConsumerSchema); err != nil {
			return fmt.Errorf("basic-auth consumer configuration: %w", err)
		}
	}
	jweDecryptPlugin, hasJWEDecrypt := consumer.Plugins["jwe-decrypt"]
	var jweDecryptConfig jweDecrypt
	if hasJWEDecrypt {
		if err := util.Parse(jweDecryptPlugin, &jweDecryptConfig); err != nil {
			return err
		}
		if err := validateJWEDecryptConsumerConfig(jweDecryptConfig); err != nil {
			return err
		}
	}
	pluginKeys := make([]string, 0, len(consumer.Plugins))
	keyAuthPlugin, ok := consumer.Plugins["key-auth"]
	if ok {
		var ka keyAuth
		if err := util.Parse(keyAuthPlugin, &ka); err != nil {
			return err
		}
		if !strings.HasPrefix(ka.Key, "$") {
			pluginKeys = append(pluginKeys, fmt.Sprintf("key-auth:%s", ka.Key))
		}
	}
	basicAuthPlugin, ok := consumer.Plugins["basic-auth"]
	if ok {
		var ba basicAuth
		if err := util.Parse(basicAuthPlugin, &ba); err != nil {
			return err
		}
		pluginKeys = append(pluginKeys, fmt.Sprintf("basic-auth:%s", ba.Username))
	}
	jwtAuthPlugin, ok := consumer.Plugins["jwt-auth"]
	if ok {
		var ja jwtAuth
		if err := util.Parse(jwtAuthPlugin, &ja); err != nil {
			return err
		}
		pluginKeys = append(pluginKeys, fmt.Sprintf("jwt-auth:%s", ja.Key))
	}
	hmacAuthPlugin, ok := consumer.Plugins["hmac-auth"]
	if ok {
		var ha hmacAuth
		if err := util.Parse(hmacAuthPlugin, &ha); err != nil {
			return err
		}
		pluginKeys = append(pluginKeys, fmt.Sprintf("hmac-auth:%s", ha.KeyID))
	}
	ldapAuthPlugin, ok := consumer.Plugins["ldap-auth"]
	if ok {
		var la ldapAuth
		if err := util.Parse(ldapAuthPlugin, &la); err != nil {
			return err
		}
		pluginKeys = append(pluginKeys, fmt.Sprintf("ldap-auth:%s", la.UserDN))
	}
	if hasJWEDecrypt {
		pluginKeys = append(pluginKeys, fmt.Sprintf("jwe-decrypt:%s", jweDecryptConfig.Key.(string)))
	}
	wolfRBACPlugin, ok := consumer.Plugins["wolf-rbac"]
	if ok {
		var wr wolfRBAC
		if err := util.Parse(wolfRBACPlugin, &wr); err != nil {
			return err
		}
		pluginKeys = append(pluginKeys, fmt.Sprintf("wolf-rbac:%s", wr.AppID))
	}

	s.consumerMu.Lock()
	defer s.consumerMu.Unlock()
	key := util.BytesToString(id)
	if keys, ok := s.consumerToKeys[key]; ok {
		for _, oldKey := range keys {
			delete(s.consumerKV, oldKey)
		}
	}
	consumerID := append([]byte(nil), id...)
	s.consumerKV[key] = consumerID
	s.consumerToKeys[key] = pluginKeys
	for _, pluginKey := range pluginKeys {
		s.consumerKV[pluginKey] = consumerID
	}
	if s.consumerValues == nil {
		s.consumerValues = make(map[string]resource.Consumer)
	}
	s.consumerValues[key] = consumer

	return nil
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
