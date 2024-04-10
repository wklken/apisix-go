package store

import (
	"fmt"

	"github.com/wklken/apisix-go/pkg/util"
)

type keyAuth struct {
	Key string `json:"key"`
}

func (s *Store) consumerKVAdd(id []byte, value []byte) error {
	consumer, err := ParseConsumer(value)
	if err != nil {
		return err
	}
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
	return nil
}

func (s *Store) consumerKVDelete(id []byte) error {
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
	id, ok := s.consumerKV[k]
	if !ok {
		return []byte{}, ErrNotFound
	}
	return id, nil
}
