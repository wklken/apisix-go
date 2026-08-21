package log

import (
	"net/http"
	"net/url"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

const sensitiveQueryPlaceholder = "***"

// RedactQuery returns a detached query map with every value belonging to a
// registered sensitive key replaced by the fixed logging placeholder.
func RedactQuery(query url.Values, names map[string]struct{}) url.Values {
	if query == nil {
		return nil
	}
	redacted := make(url.Values, len(query))
	for key, values := range query {
		if _, sensitive := names[key]; sensitive {
			if len(values) == 0 {
				redacted[key] = []string{sensitiveQueryPlaceholder}
				continue
			}
			redacted[key] = make([]string, len(values))
			for index := range values {
				redacted[key][index] = sensitiveQueryPlaceholder
			}
			continue
		}
		redacted[key] = append([]string(nil), values...)
	}
	return redacted
}

// RedactCollapsedQuery returns a detached copy of a collapsed access-log
// query map. It accepts the map shape emitted by base.CollapseQueryValues.
func RedactCollapsedQuery(query map[string]any, names map[string]struct{}) map[string]any {
	if query == nil {
		return nil
	}
	redacted := make(map[string]any, len(query))
	for key, value := range query {
		if _, sensitive := names[key]; sensitive {
			switch values := value.(type) {
			case []string:
				masked := make([]string, len(values))
				for index := range masked {
					masked[index] = sensitiveQueryPlaceholder
				}
				if len(masked) == 0 {
					masked = []string{sensitiveQueryPlaceholder}
				}
				redacted[key] = masked
			case []any:
				masked := make([]any, len(values))
				for index := range masked {
					masked[index] = sensitiveQueryPlaceholder
				}
				if len(masked) == 0 {
					masked = []any{sensitiveQueryPlaceholder}
				}
				redacted[key] = masked
			default:
				redacted[key] = sensitiveQueryPlaceholder
			}
			continue
		}
		if values, ok := value.([]string); ok {
			redacted[key] = append([]string(nil), values...)
			continue
		}
		if values, ok := value.([]any); ok {
			redacted[key] = append([]any(nil), values...)
			continue
		}
		redacted[key] = value
	}
	return redacted
}

// RedactRawQuery preserves non-sensitive raw query fields and duplicate
// parameters while replacing only the values of registered sensitive names.
func RedactRawQuery(rawQuery string, names map[string]struct{}) string {
	if rawQuery == "" || len(names) == 0 {
		return rawQuery
	}
	parts := strings.Split(rawQuery, "&")
	for index, part := range parts {
		if part == "" {
			continue
		}
		key := part
		if before, _, found := strings.Cut(part, "="); found {
			key = before
		}
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			continue
		}
		if _, sensitive := names[decodedKey]; sensitive {
			parts[index] = key + "=" + sensitiveQueryPlaceholder
		}
	}
	return strings.Join(parts, "&")
}

// RedactURI returns a detached URI/URL string with registered query values
// redacted. Parse failures leave the original string untouched.
func RedactURI(rawURI string, names map[string]struct{}) string {
	if rawURI == "" || len(names) == 0 {
		return rawURI
	}
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return rawURI
	}
	parsed.RawQuery = RedactRawQuery(parsed.RawQuery, names)
	return parsed.String()
}

// RedactedRequestURI returns a detached request URI suitable for logging.
func RedactedRequestURI(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return RedactURI(r.URL.RequestURI(), apisixctx.SensitiveQueryNames(r))
}

// RedactedRequestURL returns a detached URL suitable for logging.
func RedactedRequestURL(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return RedactURI(r.URL.String(), apisixctx.SensitiveQueryNames(r))
}

// RedactedRequestQuery returns a detached parsed query suitable for logging.
func RedactedRequestQuery(r *http.Request) url.Values {
	if r == nil || r.URL == nil {
		return nil
	}
	return RedactQuery(r.URL.Query(), apisixctx.SensitiveQueryNames(r))
}

// RedactedRequestQueryString returns the raw query view suitable for logging.
func RedactedRequestQueryString(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return RedactRawQuery(r.URL.RawQuery, apisixctx.SensitiveQueryNames(r))
}

func redactedQueryField(r *http.Request, key string) (any, bool) {
	if r == nil || r.URL == nil {
		return "", true
	}
	names := apisixctx.SensitiveQueryNames(r)
	switch key {
	case "$args", "$query_string":
		return RedactRawQuery(r.URL.RawQuery, names), true
	case "$request_uri":
		return RedactURI(r.URL.RequestURI(), names), true
	case "$request_line":
		return r.Method + " " + RedactURI(r.URL.RequestURI(), names) + " " + r.Proto, true
	case "$upstream_uri":
		value := apisixctx.GetRequestVar(r, key)
		if value == nil {
			return "", true
		}
		if uri, ok := value.(string); ok {
			return RedactURI(uri, names), true
		}
		return value, true
	}
	if suffix, ok := strings.CutPrefix(key, "$arg_"); ok {
		values, present := r.URL.Query()[suffix]
		if !present || len(values) == 0 {
			return "", true
		}
		if _, sensitive := names[suffix]; sensitive {
			return sensitiveQueryPlaceholder, true
		}
		return values[0], true
	}
	return nil, false
}
