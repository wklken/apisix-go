package base

import (
	"regexp"
	"strings"
)

// RequestVariablePattern matches $name and ${name}-less APISIX/NGINX variable
// references in interpolation templates.
var RequestVariablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

// ResolveRequestVariables replaces each $name variable reference in value with
// the lookup result for name. Lookup receives the name without the leading
// dollar sign.
func ResolveRequestVariables(value string, lookup func(name string) string) string {
	return RequestVariablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return lookup(strings.TrimPrefix(variable, "$"))
	})
}
