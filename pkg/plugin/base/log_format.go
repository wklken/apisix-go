package base

// TruncateLogFormat copies format and replaces nested objects at maxDepth
// with empty maps. The returned bool reports whether any non-empty object was
// truncated.
func TruncateLogFormat(format map[string]any, maxDepth int) (map[string]any, bool) {
	return truncateLogFormat(format, 0, maxDepth)
}

func truncateLogFormat(format map[string]any, depth, maxDepth int) (map[string]any, bool) {
	result := make(map[string]any, len(format))
	truncated := false
	for key, value := range format {
		nested, ok := value.(map[string]any)
		if !ok {
			result[key] = value
			continue
		}
		if depth+1 >= maxDepth {
			result[key] = map[string]any{}
			truncated = truncated || len(nested) > 0
			continue
		}
		resolved, childTruncated := truncateLogFormat(nested, depth+1, maxDepth)
		result[key] = resolved
		truncated = truncated || childTruncated
	}
	return result, truncated
}
