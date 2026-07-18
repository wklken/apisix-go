package config

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	apisixv1 "github.com/apache/apisix-ingress-controller/pkg/types/apisix/v1"
	"github.com/fsnotify/fsnotify"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/runtime"
)

// for standalone mode: https://apisix.apache.org/docs/apisix/deployment-modes/#standalone

const (
	standaloneProviderYAML = "yaml"
	standaloneProviderJSON = "json"
)

var standaloneBuckets = []string{
	"routes",
	"upstreams",
	"services",
	"plugin_metadata",
	"ssls",
	"stream_routes",
	"consumers",
	"consumer_groups",
	"global_rules",
	"plugin_configs",
	"protos",
}

type standaloneSnapshot map[string]map[string][]byte

// StandaloneFileWatcher loads the APISIX file-driven configuration and emits
// store events for added, updated, and removed resources.
type StandaloneFileWatcher struct {
	path     string
	provider string
	events   chan *store.Event

	mu      sync.Mutex
	current standaloneSnapshot
}

func StandaloneConfigFile(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case standaloneProviderJSON:
		return "conf/apisix.json"
	case standaloneProviderYAML:
		return "conf/apisix.yaml"
	default:
		return ""
	}
}

func NewStandaloneFileWatcher(path, provider string, events chan *store.Event) *StandaloneFileWatcher {
	return &StandaloneFileWatcher{
		path:     path,
		provider: strings.ToLower(strings.TrimSpace(provider)),
		events:   events,
		current:  make(standaloneSnapshot),
	}
}

func (w *StandaloneFileWatcher) Reload() error {
	next, err := readStandaloneSnapshot(w.path, w.provider)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, bucket := range standaloneBuckets {
		previous := w.current[bucket]
		updated := next[bucket]

		for _, id := range sortedSnapshotIDs(previous) {
			if _, ok := updated[id]; !ok {
				w.emit(store.EventTypeDelete, bucket, id, nil)
			}
		}
		for _, id := range sortedSnapshotIDs(updated) {
			if previousValue, ok := previous[id]; ok && bytes.Equal(previousValue, updated[id]) {
				continue
			}
			w.emit(store.EventTypePut, bucket, id, updated[id])
		}
	}
	w.current = next
	return nil
}

func (w *StandaloneFileWatcher) Watch() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Printf("watch standalone config %q failed: %s\n", w.path, err)
		return
	}
	if err := watcher.Add(filepath.Dir(w.path)); err != nil {
		_ = watcher.Close()
		fmt.Printf("watch standalone config %q failed: %s\n", w.path, err)
		return
	}

	configuredBase := filepath.Base(w.path)
	go func() {
		defer func() {
			_ = watcher.Close()
		}()
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Base(event.Name) != configuredBase ||
					!event.Has(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) {
					continue
				}
				if err := w.Reload(); err != nil {
					fmt.Printf("reload standalone config %q failed: %s\n", w.path, err)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Printf("watch standalone config %q failed: %s\n", w.path, err)
			}
		}
	}()
}

func (w *StandaloneFileWatcher) emit(eventType store.EventType, bucket, id string, value []byte) {
	event := store.NewEvent()
	event.Type = eventType
	event.Key = []byte("/apisix/" + bucket + "/" + id)
	event.Value = append([]byte(nil), value...)
	w.events <- event
}

func InitStandaloneFileWatcher(path string, events chan *store.Event) {
	watcher := NewStandaloneFileWatcher(path, standaloneProviderFromPath(path), events)
	if err := watcher.Reload(); err != nil {
		fmt.Printf("load standalone config %q failed: %s\n", path, err)
		return
	}
	watcher.Watch()
}

func ReadAndReload(path string, events chan *store.Event) {
	watcher := NewStandaloneFileWatcher(path, standaloneProviderFromPath(path), events)
	if err := watcher.Reload(); err != nil {
		fmt.Printf("load standalone config %q failed: %s\n", path, err)
	}
}

func standaloneProviderFromPath(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

func readStandaloneSnapshot(path, provider string) (standaloneSnapshot, error) {
	if provider != standaloneProviderYAML && provider != standaloneProviderJSON {
		return nil, fmt.Errorf("unsupported standalone config provider %q", provider)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read standalone config %q: %w", path, err)
	}
	if provider == standaloneProviderYAML && !strings.HasSuffix(strings.TrimSpace(string(data)), "#END") {
		return nil, fmt.Errorf("standalone YAML config %q must end with #END", path)
	}

	var encoded []byte
	if provider == standaloneProviderYAML {
		var document any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("parse standalone YAML config %q: %w", path, err)
		}
		encoded, err = stdjson.Marshal(document)
		if err != nil {
			return nil, fmt.Errorf("normalize standalone config %q: %w", path, err)
		}
	} else {
		var document map[string]stdjson.RawMessage
		if err := stdjson.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("parse standalone JSON config %q: %w", path, err)
		}
		encoded = data
	}

	var sections map[string]stdjson.RawMessage
	if err := stdjson.Unmarshal(encoded, &sections); err != nil {
		return nil, fmt.Errorf("decode standalone resources %q: %w", path, err)
	}

	snapshot := make(standaloneSnapshot)
	for _, bucket := range standaloneBuckets {
		raw, ok := sections[bucket]
		if !ok {
			continue
		}
		var resources []stdjson.RawMessage
		if err := stdjson.Unmarshal(raw, &resources); err != nil {
			return nil, fmt.Errorf("decode standalone %s: %w", bucket, err)
		}
		for _, resource := range resources {
			id, value, err := normalizeStandaloneResource(bucket, resource)
			if err != nil {
				return nil, fmt.Errorf("decode standalone %s resource: %w", bucket, err)
			}
			if snapshot[bucket] == nil {
				snapshot[bucket] = make(map[string][]byte)
			}
			snapshot[bucket][id] = value
		}
	}
	return snapshot, nil
}

func normalizeStandaloneResource(bucket string, raw stdjson.RawMessage) (string, []byte, error) {
	var fields map[string]stdjson.RawMessage
	if err := stdjson.Unmarshal(raw, &fields); err != nil {
		return "", nil, err
	}

	keys := []string{"id"}
	if bucket == "consumers" {
		keys = []string{"username", "id"}
	}
	var idKey string
	var idRaw stdjson.RawMessage
	for _, key := range keys {
		if value, ok := fields[key]; ok {
			idKey = key
			idRaw = value
			break
		}
	}
	if idKey == "" {
		return "", nil, fmt.Errorf("missing id")
	}
	id, err := standaloneResourceID(idRaw)
	if err != nil {
		return "", nil, err
	}
	if idKey == "id" {
		fields[idKey], err = stdjson.Marshal(id)
		if err != nil {
			return "", nil, err
		}
	}
	value, err := stdjson.Marshal(fields)
	if err != nil {
		return "", nil, err
	}
	return id, value, nil
}

func standaloneResourceID(raw stdjson.RawMessage) (string, error) {
	decoder := stdjson.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch value := value.(type) {
	case string:
		if value == "" {
			return "", fmt.Errorf("id is empty")
		}
		return value, nil
	case stdjson.Number:
		return value.String(), nil
	default:
		return "", fmt.Errorf("id must be a string or number")
	}
}

func sortedSnapshotIDs(snapshot map[string][]byte) []string {
	ids := make([]string, 0, len(snapshot))
	for id := range snapshot {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

type ApisixConfigurationStandalone struct {
	Routes          []*Route                `json:"routes,omitempty" yaml:"routes,omitempty"`
	StreamRoutes    []*resource.StreamRoute `json:"stream_routes,omitempty" yaml:"stream_routes,omitempty"`
	Services        []*Service              `json:"services,omitempty" yaml:"services,omitempty"`
	PluginMetadatas []*PluginMetadata       `json:"plugin_metadata,omitempty" yaml:"plugin_metadata,omitempty"`
	SSLs            []*SSL                  `json:"ssls,omitempty" yaml:"ssls,omitempty"`
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
	TLS           *resource.UpstreamTLS         `json:"tls,omitempty" yaml:"tls,omitempty"`
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
func (pm *PluginMetadata) MarshalYAML() (any, error) {
	by, err := json.Marshal(pm.RawExtension)
	if err != nil {
		return nil, err
	}
	var resMap map[string]any
	if err := json.Unmarshal(by, &resMap); err != nil {
		return nil, err
	}
	resMap["id"] = pm.ID
	return resMap, nil
}

// UnmarshalYAML is serializing method for plugin_metadata
func (pm *PluginMetadata) UnmarshalYAML(unmarshal func(any) error) error {
	var resMap map[string]any
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
