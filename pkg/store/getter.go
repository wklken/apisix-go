package store

import (
	"encoding/json"
	"fmt"

	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

var ErrNotFound = fmt.Errorf("not found")

// FIXME: add a cache layer here, if the source data changed, del the cache at the same time

func GetPluginMetadata(id string, v any) error {
	config := s.GetFromBucket("plugin_metadata", []byte(id))

	// FIXME: add cache here? or unmarshal at sync? should not do unmarshal here

	err := json.Unmarshal(config, v)
	return err
}

func GetUpstream(id string) (resource.Upstream, error) {
	config := s.GetFromBucket("upstreams", util.StringToBytes(id))
	if config == nil {
		return resource.Upstream{}, ErrNotFound
	}

	return ParseUpstream(config)
}

func GetService(id string) (resource.Service, error) {
	config := s.GetFromBucket("services", util.StringToBytes(id))
	if config == nil {
		return resource.Service{}, ErrNotFound
	}

	return ParseService(config)
}

func GetConsumer(id string) (resource.Consumer, error) {
	config := s.GetFromBucket("consumers", util.StringToBytes(id))
	if config == nil {
		return resource.Consumer{}, ErrNotFound
	}

	return ParseConsumer(config)
}

func ListRoutes() ([]resource.Route, error) {
	var routes []resource.Route
	data := s.GetBucketData("routes")
	for _, d := range data {
		r, err := ParseRoute(d)
		if err != nil {
			continue
			// FIXME: do skip, process
			// FIXME: append d and error
			// return nil, err
		}
		routes = append(routes, r)
	}
	return routes, nil
}

func ParseRoute(config []byte) (resource.Route, error) {
	var r resource.Route
	err := json.Unmarshal(config, &r)
	if err != nil {
		return r, err
	}
	return r, nil
}

func ParseService(config []byte) (resource.Service, error) {
	var s resource.Service
	err := json.Unmarshal(config, &s)
	if err != nil {
		return s, err
	}
	return s, nil
}

func ParseUpstream(config []byte) (resource.Upstream, error) {
	var u resource.Upstream
	err := json.Unmarshal(config, &u)
	if err != nil {
		return u, err
	}
	return u, nil
}

func ParseConsumer(config []byte) (resource.Consumer, error) {
	var c resource.Consumer
	err := json.Unmarshal(config, &c)
	if err != nil {
		return c, err
	}
	return c, nil
}

func GetConsumerByPluginKey(pluginName string, key string) (resource.Consumer, error) {
	id, err := s.GetConsumerNameByPluginKey(pluginName, key)
	if err != nil {
		return resource.Consumer{}, err
	}

	return GetConsumer(util.BytesToString(id))
}
