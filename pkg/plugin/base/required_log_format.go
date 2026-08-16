package base

import (
	"fmt"
	"maps"
)

// RequireStringLogFormat selects a non-empty route log format, falling back to
// plugin metadata when the route does not provide one. The returned map is
// owned by the caller and is safe to update without mutating either input.
func RequireStringLogFormat(pluginName string, route, metadata map[string]string) (map[string]string, error) {
	if len(route) != 0 {
		return maps.Clone(route), nil
	}
	if len(metadata) != 0 {
		return maps.Clone(metadata), nil
	}
	return nil, fmt.Errorf("%s requires log_format in route config or plugin metadata", pluginName)
}
