package store

import (
	"fmt"

	"github.com/wklken/apisix-go/pkg/util"
)

type keyAuth struct {
	Key string `json:"key"`
}

type basicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

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
	Key string `json:"key"`
}

type wolfRBAC struct {
	AppID string `json:"appid"`
}

func (s *Store) consumerKVAdd(id []byte, value []byte) error {
	consumer, err := ParseConsumer(value)
	if err != nil {
		return err
	}
	s.consumerMu.Lock()
	defer s.consumerMu.Unlock()
	key := util.BytesToString(id)

	// clear old keys
	if keys, ok := s.consumerToKeys[key]; ok {
		for _, k := range keys {
			delete(s.consumerKV, k)
		}
	}
	s.consumerToKeys[key] = []string{}

	// add self
	s.consumerKV[key] = id

	// add plugin unique keys

	// if "key-auth" in consumer.Plugins
	keyAuthPlugin, ok := consumer.Plugins["key-auth"]
	if ok {
		var ka keyAuth
		err = util.Parse(keyAuthPlugin, &ka)
		if err != nil {
			return err
		}
		k := fmt.Sprintf("key-auth:%s", ka.Key)
		s.consumerKV[k] = id

		// add to consumerToKeys
		s.consumerToKeys[key] = append(s.consumerToKeys[key], k)
	}

	// if "basic-auth" in consumer.Plugins
	basicAuthPlugin, ok := consumer.Plugins["basic-auth"]
	if ok {
		var ba basicAuth
		err = util.Parse(basicAuthPlugin, &ba)
		if err != nil {
			return err
		}
		k := fmt.Sprintf("basic-auth:%s", ba.Username)
		s.consumerKV[k] = id

		// add to consumerToKeys
		s.consumerToKeys[key] = append(s.consumerToKeys[key], k)
	}

	// if "jwt-auth" in consumer.Plugins
	jwtAuthPlugin, ok := consumer.Plugins["jwt-auth"]
	if ok {
		var ja jwtAuth
		err = util.Parse(jwtAuthPlugin, &ja)
		if err != nil {
			return err
		}
		k := fmt.Sprintf("jwt-auth:%s", ja.Key)
		s.consumerKV[k] = id

		// add to consumerToKeys
		s.consumerToKeys[key] = append(s.consumerToKeys[key], k)
	}

	// if "hmac-auth" in consumer.Plugins
	hmacAuthPlugin, ok := consumer.Plugins["hmac-auth"]
	if ok {
		var ha hmacAuth
		err = util.Parse(hmacAuthPlugin, &ha)
		if err != nil {
			return err
		}
		k := fmt.Sprintf("hmac-auth:%s", ha.KeyID)
		s.consumerKV[k] = id

		// add to consumerToKeys
		s.consumerToKeys[key] = append(s.consumerToKeys[key], k)
	}

	ldapAuthPlugin, ok := consumer.Plugins["ldap-auth"]
	if ok {
		var la ldapAuth
		err = util.Parse(ldapAuthPlugin, &la)
		if err != nil {
			return err
		}
		k := fmt.Sprintf("ldap-auth:%s", la.UserDN)
		s.consumerKV[k] = id

		s.consumerToKeys[key] = append(s.consumerToKeys[key], k)
	}

	jweDecryptPlugin, ok := consumer.Plugins["jwe-decrypt"]
	if ok {
		var jd jweDecrypt
		err = util.Parse(jweDecryptPlugin, &jd)
		if err != nil {
			return err
		}
		k := fmt.Sprintf("jwe-decrypt:%s", jd.Key)
		s.consumerKV[k] = id

		s.consumerToKeys[key] = append(s.consumerToKeys[key], k)
	}

	wolfRBACPlugin, ok := consumer.Plugins["wolf-rbac"]
	if ok {
		var wr wolfRBAC
		err = util.Parse(wolfRBACPlugin, &wr)
		if err != nil {
			return err
		}
		k := fmt.Sprintf("wolf-rbac:%s", wr.AppID)
		s.consumerKV[k] = id

		s.consumerToKeys[key] = append(s.consumerToKeys[key], k)
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
