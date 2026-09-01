package limit_req

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"strconv"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func buildLimitReqKey(
	pluginContext base.APISIXPluginContext,
	config map[string]any,
	resolvedKey string,
) (string, error) {
	parent, err := pluginContext.ParentResourceKey()
	if err != nil {
		return "", err
	}
	document := normalizeLimitReqDocument(config)
	canonical, err := marshalLimitReqDocument(document)
	if err != nil {
		return "", fmt.Errorf("marshal APISIX limit-req config: %w", err)
	}
	version := strconv.FormatUint(uint64(crc32.ChecksumIEEE(canonical)), 10)
	return parent + ":" + version + ":" + resolvedKey, nil
}

func normalizeLimitReqDocument(config map[string]any) map[string]any {
	document := cloneLimitReqMap(config)
	setLimitReqDefault(document, "key_type", "var")
	setLimitReqDefault(document, "policy", "local")
	setLimitReqDefault(document, "rejected_code", 503)
	setLimitReqDefault(document, "nodelay", false)
	setLimitReqDefault(document, "allow_degradation", false)
	if _, ok := document["_meta"]; !ok {
		document["_meta"] = []any{}
	}
	switch document["policy"] {
	case "redis":
		setLimitReqDefault(document, "redis_port", 6379)
		setLimitReqDefault(document, "redis_database", 0)
		setLimitReqDefault(document, "redis_timeout", 1000)
		setLimitReqDefault(document, "redis_ssl", false)
		setLimitReqDefault(document, "redis_ssl_verify", false)
		setLimitReqDefault(document, "redis_keepalive_timeout", 10000)
		setLimitReqDefault(document, "redis_keepalive_pool", 100)
	case "redis-cluster":
		setLimitReqDefault(document, "redis_timeout", 1000)
		setLimitReqDefault(document, "redis_cluster_ssl", false)
		setLimitReqDefault(document, "redis_cluster_ssl_verify", false)
		setLimitReqDefault(document, "redis_keepalive_timeout", 10000)
		setLimitReqDefault(document, "redis_keepalive_pool", 100)
	}
	return document
}

func setLimitReqDefault(document map[string]any, key string, value any) {
	if _, ok := document[key]; !ok {
		document[key] = value
	}
}

func marshalLimitReqDocument(document map[string]any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalizeLimitReqValue(document)); err != nil {
		return nil, err
	}
	encoded := buffer.Bytes()
	if len(encoded) > 0 && encoded[len(encoded)-1] == '\n' {
		encoded = encoded[:len(encoded)-1]
	}
	return encoded, nil
}

func normalizeLimitReqValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			return []any{}
		}
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if key != "_version" {
				result[key] = normalizeLimitReqValue(child)
			}
		}
		if len(result) == 0 {
			return []any{}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = normalizeLimitReqValue(child)
		}
		return result
	default:
		return value
	}
}

func cloneLimitReqMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneLimitReqValue(value)
	}
	return result
}

func cloneLimitReqValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneLimitReqMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = cloneLimitReqValue(child)
		}
		return result
	default:
		return value
	}
}
