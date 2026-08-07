package base

import "github.com/wklken/apisix-go/pkg/json"

// EncodeLogBatch encodes either a single entry or an entry array according to
// the logger batch boundary. When originKey is set and every entry contains a
// raw origin string, those strings are encoded instead of their envelopes.
func EncodeLogBatch(entries []map[string]any, batchMaxSize int, originKey string) ([]byte, error) {
	if originKey != "" {
		if originEntries, ok := OriginLogEntries(entries, originKey); ok {
			if batchMaxSize == 1 && len(originEntries) == 1 {
				return []byte(originEntries[0]), nil
			}
			return json.Marshal(originEntries)
		}
	}
	if batchMaxSize == 1 && len(entries) == 1 {
		return json.Marshal(entries[0])
	}
	return json.Marshal(entries)
}

// OriginLogEntries unwraps raw origin entries only when every batch entry
// contains a string under originKey.
func OriginLogEntries(entries []map[string]any, originKey string) ([]string, bool) {
	if len(entries) == 0 {
		return nil, false
	}
	originEntries := make([]string, 0, len(entries))
	for _, entry := range entries {
		raw, ok := entry[originKey].(string)
		if !ok {
			return nil, false
		}
		originEntries = append(originEntries, raw)
	}
	return originEntries, true
}
