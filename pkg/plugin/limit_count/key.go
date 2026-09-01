package limit_count

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"strconv"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// BuildLocalKey returns the APISIX 3.17 limit-count counter identity. A
// grouped configuration deliberately does not depend on its parent or
// configuration version; all other configurations use the parent resource
// key, the CRC32 of the effective plugin document, and the resolved request
// key. Workflow actions are isolated by including _vid in the versioned
// document and as the final key component.
func BuildLocalKey(
	pluginContext base.APISIXPluginContext,
	config map[string]any,
	resolvedKey string,
) (string, error) {
	if pluginContext.WorkflowVID > 0 {
		return BuildLocalKeyWithVID(pluginContext, config, resolvedKey, pluginContext.WorkflowVID)
	}
	return BuildLocalKeyWithVID(pluginContext, config, resolvedKey, nil)
}

// BuildLocalKeyWithVID is BuildLocalKey with an optional APISIX workflow
// identity. APISIX uses numeric workflow indexes and string identities for
// transformed ai-rate-limiting configurations, so the value is deliberately
// accepted as any scalar and rendered with its native string form.
func BuildLocalKeyWithVID(
	pluginContext base.APISIXPluginContext,
	config map[string]any,
	resolvedKey string,
	vid any,
) (string, error) {
	document := normalizeLimitCountDocument(config)
	if group, ok := document["group"].(string); ok && group != "" {
		return group + ":" + resolvedKey, nil
	}

	parent, err := pluginContext.ParentResourceKey()
	if err != nil {
		return "", err
	}
	if vid == nil {
		vid = document["_vid"]
	}
	hasVID, err := validLimitCountVID(vid)
	if err != nil {
		return "", err
	}
	if hasVID {
		document["_vid"] = vid
	}

	canonical, err := marshalLimitCountDocument(document)
	if err != nil {
		return "", fmt.Errorf("marshal APISIX limit-count config: %w", err)
	}
	version := strconv.FormatUint(uint64(crc32.ChecksumIEEE(canonical)), 10)
	key := parent + ":" + version + ":" + resolvedKey
	if hasVID {
		key += ":" + fmt.Sprint(vid)
	}
	return key, nil
}

func validLimitCountVID(vid any) (bool, error) {
	if vid == nil {
		return false, nil
	}
	switch vid.(type) {
	case string, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return fmt.Sprint(vid) != "", nil
	default:
		return false, fmt.Errorf("APISIX limit-count workflow ID must be a scalar")
	}
}

func (p *Plugin) hasAPISIXPluginContext() bool {
	return p.apisixContext.SourceResourceKey != "" ||
		p.apisixContext.SourceID != "" || p.apisixContext.SourceKind != ""
}

func (p *Plugin) effectiveLimitCountDocument() (map[string]any, error) {
	var document map[string]any
	if p.apisixContext.SourceConfig != nil {
		document, _ = cloneLimitCountValue(p.apisixContext.SourceConfig).(map[string]any)
	} else {
		encoded, err := json.Marshal(p.config)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &document); err != nil {
			return nil, err
		}
	}
	if document == nil {
		document = make(map[string]any)
	}

	p.credentialMu.Lock()
	scoped := p.scopedSet
	p.credentialMu.Unlock()
	if !scoped {
		return document, nil
	}
	if err := p.withLimitCountKey(func(value string) error {
		document["key"] = value
		return nil
	}); err != nil {
		return nil, err
	}
	if err := p.withLimitCountRedisHost(func(value string) error {
		if value != "" {
			document["redis_host"] = value
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := p.withLimitCountRedisNodes(func(values []string) error {
		if len(values) > 0 {
			document["redis_cluster_nodes"] = append([]string(nil), values...)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return document, nil
}

func normalizeLimitCountDocument(config map[string]any) map[string]any {
	document, _ := cloneLimitCountValue(config).(map[string]any)
	if document == nil {
		document = make(map[string]any)
	}
	if _, ok := document["key"]; !ok {
		document["key"] = "remote_addr"
	}
	if _, ok := document["key_type"]; !ok {
		document["key_type"] = "var"
	}
	if _, ok := document["rejected_code"]; !ok {
		document["rejected_code"] = 503
	}
	if _, ok := document["policy"]; !ok {
		document["policy"] = "local"
	}
	if _, ok := document["allow_degradation"]; !ok {
		document["allow_degradation"] = false
	}
	if _, ok := document["show_limit_quota_header"]; !ok {
		document["show_limit_quota_header"] = true
	}
	if _, ok := document["_meta"]; !ok {
		document["_meta"] = []any{}
	}
	if policy, _ := document["policy"].(string); policy == "redis" {
		setLimitCountDefault(document, "redis_port", 6379)
		setLimitCountDefault(document, "redis_database", 0)
		setLimitCountDefault(document, "redis_timeout", 1000)
		setLimitCountDefault(document, "redis_ssl", false)
		setLimitCountDefault(document, "redis_ssl_verify", false)
		setLimitCountDefault(document, "redis_keepalive_timeout", 10000)
		setLimitCountDefault(document, "redis_keepalive_pool", 100)
	} else if policy == "redis-cluster" {
		setLimitCountDefault(document, "redis_timeout", 1000)
		setLimitCountDefault(document, "redis_cluster_ssl", false)
		setLimitCountDefault(document, "redis_cluster_ssl_verify", false)
		setLimitCountDefault(document, "redis_keepalive_timeout", 10000)
		setLimitCountDefault(document, "redis_keepalive_pool", 100)
	}
	return document
}

func setLimitCountDefault(document map[string]any, key string, value any) {
	if _, ok := document[key]; !ok {
		document[key] = value
	}
}

func marshalLimitCountDocument(document map[string]any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalizeLimitCountValue(document)); err != nil {
		return nil, err
	}
	encoded := buffer.Bytes()
	if len(encoded) > 0 && encoded[len(encoded)-1] == '\n' {
		encoded = encoded[:len(encoded)-1]
	}
	return encoded, nil
}

func normalizeLimitCountValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			return []any{}
		}
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if key == "_version" {
				continue
			}
			result[key] = normalizeLimitCountValue(child)
		}
		if len(result) == 0 {
			return []any{}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = normalizeLimitCountValue(child)
		}
		return result
	default:
		return value
	}
}

func cloneLimitCountValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = cloneLimitCountValue(child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = cloneLimitCountValue(child)
		}
		return result
	default:
		return value
	}
}
