package config

import (
	"fmt"

	apisixv1 "github.com/apache/apisix-ingress-controller/pkg/types/apisix/v1"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/store"
	"k8s.io/apimachinery/pkg/runtime"
)

// for standalone mode: https://apisix.apache.org/docs/apisix/deployment-modes/#standalone

// watch the config file change

func InitStandaloneFileWatcher(path string, events chan *store.Event) {
	v := viper.New()
	v.SetConfigFile(path)
	v.OnConfigChange(func(e fsnotify.Event) {
		fmt.Println("Config file changed:", e.Name)
		ReadAndReload(path, events)
	})
	v.WatchConfig()
}

func ReadAndReload(path string, events chan *store.Event) {
	// how to cmp the current config and the new config?
	// store the latest file content?

	// save the previous config => diff => send event

	// read the config file
	// v := viper.New()
	// v.SetConfigFile(path)
	// err := v.ReadInConfig()
	// if err != nil {
	// 	fmt.Println("read config file error", err)
	// 	return
	// }
	// v.AllKeys()
}

type ApisixConfigurationStandalone struct {
	Routes          []*Route          `json:"routes,omitempty" yaml:"routes,omitempty"`
	Services        []*Service        `json:"services,omitempty" yaml:"services,omitempty"`
	PluginMetadatas []*PluginMetadata `json:"plugin_metadata,omitempty" yaml:"plugin_metadata,omitempty"`
	SSLs            []*SSL            `json:"ssls,omitempty" yaml:"ssls,omitempty"`
	// StreamRoutes    []*StreamRoute    `json:"stream_routes,omitempty" yaml:"stream_routes,omitempty"`
}

type Service struct {
	apisixv1.Metadata `json:",inline" yaml:",inline"`

	Upstream        *Upstream        `json:"upstream,omitempty" yaml:"upstream,omitempty"`
	EnableWebsocket bool             `json:"enable_websocket,omitempty" yaml:"enable_websocket,omitempty"`
	Hosts           []string         `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	Plugins         apisixv1.Plugins `json:"plugins" yaml:"plugins"`

	CreateTime int64 `json:"create_time,omitempty" yaml:"create_time,omitempty"`
	UpdateTime int64 `json:"update_time,omitempty" yaml:"update_time,omitempty"`
}

type Upstream struct {
	Type          *string                       `json:"type,omitempty" yaml:"type,omitempty"`
	DiscoveryType *string                       `json:"discovery_type,omitempty" yaml:"discovery_type,omitempty"`
	ServiceName   *string                       `json:"service_name,omitempty" yaml:"service_name,omitempty"`
	HashOn        *string                       `json:"hash_on,omitempty" yaml:"hash_on,omitempty"`
	Key           *string                       `json:"key,omitempty" yaml:"key,omitempty"`
	Checks        *apisixv1.UpstreamHealthCheck `json:"checks,omitempty" yaml:"checks,omitempty"`
	Nodes         apisixv1.UpstreamNodes        `json:"nodes" yaml:"nodes"`
	Scheme        *string                       `json:"scheme,omitempty" yaml:"scheme,omitempty"`
	Retries       *int                          `json:"retries,omitempty" yaml:"retries,omitempty"`
	RetryTimeout  *int                          `json:"retry_timeout,omitempty" yaml:"retry_timeout,omitempty"`
	PassHost      *string                       `json:"pass_host,omitempty" yaml:"pass_host,omitempty"`
	UpstreamHost  *string                       `json:"upstream_host,omitempty" yaml:"upstream_host,omitempty"`
	Timeout       *apisixv1.UpstreamTimeout     `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// TLS           *apisixv1.                `json:"tls,omitempty" yaml:"tls,omitempty"`
}

type Route struct {
	apisixv1.Route `json:",inline" yaml:",inline"`
	Status         *int `json:"status,omitempty" yaml:"status,omitempty"`

	Upstream  *Upstream `json:"upstream,omitempty" yaml:"upstream,omitempty"`
	ServiceID string    `json:"service_id,omitempty" yaml:"service_id,omitempty"`

	CreateTime int64 `json:"create_time,omitempty" yaml:"create_time,omitempty"`
	UpdateTime int64 `json:"update_time,omitempty" yaml:"update_time,omitempty"`
}

type SSL struct {
	apisixv1.Ssl `json:",inline" yaml:",inline"`

	CreateTime int64 `json:"create_time,omitempty" yaml:"create_time,omitempty"`
	UpdateTime int64 `json:"update_time,omitempty" yaml:"update_time,omitempty"`
}

type PluginMetadata struct {
	runtime.RawExtension `json:",inline" yaml:",inline"`

	ID string `json:"id" yaml:"id"`
}

// MarshalYAML is serializing method for plugin_metadata
func (pm *PluginMetadata) MarshalYAML() (interface{}, error) {
	by, err := json.Marshal(pm.RawExtension)
	if err != nil {
		return nil, err
	}
	var resMap map[string]interface{}
	json.Unmarshal(by, &resMap)
	resMap["id"] = pm.ID
	return resMap, nil
}

// UnmarshalYAML is serializing method for plugin_metadata
func (pm *PluginMetadata) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var resMap map[string]interface{}
	err := unmarshal(&resMap)
	if err != nil {
		return err
	}
	var ok bool
	pm.ID, ok = resMap["id"].(string)
	if !ok {
		return fmt.Errorf("unmarshal yaml failed: ID field is not string")
	}
	delete(resMap, "id")
	pm.Raw, _ = json.Marshal(resMap)
	return nil
}
