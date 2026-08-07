package base

// ResolveLogFormat recursively resolves string leaves while preserving maps
// and non-string values.
func ResolveLogFormat(format map[string]any, resolve func(string) any) map[string]any {
	fields := make(map[string]any, len(format))
	for key, value := range format {
		switch typed := value.(type) {
		case map[string]any:
			fields[key] = ResolveLogFormat(typed, resolve)
		case string:
			fields[key] = resolve(typed)
		default:
			fields[key] = typed
		}
	}
	return fields
}

// ResolveStringLogFormat resolves every value in a flat string log format.
func ResolveStringLogFormat(format map[string]string, resolve func(string) any) map[string]any {
	fields := make(map[string]any, len(format))
	for key, value := range format {
		fields[key] = resolve(value)
	}
	return fields
}

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
