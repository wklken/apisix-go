package log

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

const (
	snapshotValueBudget = 1 << 20
	snapshotBodyLimit   = 512 << 10
)

// LogSnapshot is the detached request/response view handed to log and
// finalizer callbacks.  It intentionally contains no live writer, request
// body reader, lifecycle, or plugin/config object.
type LogSnapshot struct {
	Request  RequestLogSnapshot
	Response ResponseLogSnapshot
	NodeID   string
	Outcome  apisixctx.ResponseOutcome
	Source   apisixctx.ResponseSource
	Started  time.Time
	Finished time.Time
}

type RequestLogSnapshot struct {
	ID            string
	Method        string
	URI           string
	URL           string
	Path          string
	Host          string
	RemoteAddr    string
	Scheme        string
	Proto         string
	ContentLength int64
	Header        http.Header
	Query         url.Values
	Body          []byte
	BodyTruncated bool
	APISIXVars    map[string]any
	RequestVars   map[string]any
	Consumer      SafeConsumerLogIdentity
}

type ResponseLogSnapshot struct {
	Header        http.Header
	Trailer       http.Header
	Body          []byte
	BodyTruncated bool
}

// ResponseSnapshot is the detached response capture input used while the
// plugin-facing base package builds a LogSnapshot. It mirrors the response
// capture contract without coupling this package to pkg/plugin/base.
type ResponseSnapshot struct {
	Header        http.Header
	Trailer       http.Header
	Body          []byte
	BodyTruncated bool
}

// SafeConsumerLogIdentity is the only consumer data that may enter a log
// snapshot.  The complete consumer resource may contain credentials and
// plugin configuration, so it is never copied into APISIXVars.
type SafeConsumerLogIdentity struct {
	Username string
	GroupID  string
}

// RequestCorrelation is the bounded identity and upstream-attempt view shared
// by the final log snapshot and lifecycle-owned trace exporters.
type RequestCorrelation struct {
	RequestID      string
	NodeID         string
	UpstreamStatus string
	RetryCount     string
}

// CaptureRequestCorrelation reads only detached-safe scalar values. The
// request-id context is authoritative after the plugin runs; the APISIX value
// covers replacement requests, and the conventional header preserves early
// rejection correlation before route rewrite plugins execute.
func CaptureRequestCorrelation(r *http.Request) RequestCorrelation {
	if r == nil {
		return RequestCorrelation{}
	}
	correlation := RequestCorrelation{}
	correlation.RequestID, _ = r.Context().Value(apisixctx.RequestIDKey).(string)
	apisixVars := apisixctx.GetApisixVars(r)
	if correlation.RequestID == "" {
		correlation.RequestID, _ = apisixVars["$request_id"].(string)
	}
	if correlation.RequestID == "" {
		correlation.RequestID = r.Header.Get("X-Request-Id")
	}
	correlation.NodeID, _ = apisixVars["$node_id"].(string)
	requestVars := apisixctx.GetRequestVars(r)
	correlation.UpstreamStatus = scalarString(requestVars["$upstream_status"])
	correlation.RetryCount = scalarString(requestVars["$retry_count"])
	return correlation
}

func scalarString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

// CloneSafeValue returns a detached JSON-like value.  Unsupported values are
// omitted rather than stringified; this prevents credentials, plugin objects,
// channels, and pointers from crossing the logging boundary.
func CloneSafeValue(v any, remaining *int) (any, bool) {
	return cloneSafeValue(reflect.ValueOf(v), remaining, 0)
}

func cloneSafeValue(value reflect.Value, remaining *int, depth int) (any, bool) {
	if !value.IsValid() {
		return nil, true
	}
	if depth > 64 {
		return nil, false
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return nil, true
		}
		return cloneSafeValue(value.Elem(), remaining, depth+1)
	case reflect.Bool:
		if !consumeSnapshotBudget(remaining, 1) {
			return nil, false
		}
		return value.Interface(), true
	case reflect.String:
		text := value.String()
		if !consumeSnapshotBudget(remaining, len(text)) {
			return nil, false
		}
		return value.Interface(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if !consumeSnapshotBudget(remaining, 8) {
			return nil, false
		}
		return value.Interface(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if !consumeSnapshotBudget(remaining, 8) {
			return nil, false
		}
		return value.Interface(), true
	case reflect.Float32, reflect.Float64:
		if !consumeSnapshotBudget(remaining, 8) {
			return nil, false
		}
		return value.Interface(), true
	case reflect.Slice, reflect.Array:
		// []byte is mutable binary data rather than a JSON-like value.  Body
		// fields are copied through the explicit bounded body paths below.
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return nil, false
		}
		if !consumeSnapshotBudget(remaining, 1) {
			return nil, false
		}
		result := make([]any, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			item, ok := cloneSafeValue(value.Index(i), remaining, depth+1)
			if !ok {
				continue
			}
			result = append(result, item)
		}
		return result, true
	case reflect.Map:
		if value.IsNil() || value.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		if !consumeSnapshotBudget(remaining, 1) {
			return nil, false
		}
		result := make(map[string]any, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			if !consumeSnapshotBudget(remaining, len(key)) {
				break
			}
			item, ok := cloneSafeValue(iter.Value(), remaining, depth+1)
			if ok {
				result[key] = item
			}
		}
		return result, true
	default:
		return nil, false
	}
}

func consumeSnapshotBudget(remaining *int, cost int) bool {
	if remaining == nil {
		return true
	}
	if cost < 0 || *remaining < cost {
		return false
	}
	*remaining -= cost
	return true
}

// GetFieldsFromSnapshot expands an access-log format without consulting a
// live request.  Unknown variables retain the existing empty-string behavior.
func GetFieldsFromSnapshot(snapshot LogSnapshot, logFormat map[string]string) map[string]any {
	fields := make(map[string]any, len(logFormat))
	for key, expression := range logFormat {
		if !strings.HasPrefix(expression, "$") {
			fields[key] = expression
			continue
		}
		fields[key] = snapshotField(snapshot, expression)
	}
	return fields
}

// ValueFromSnapshot resolves one plugin expression variable without consulting
// a live request. Metadata filters use this at log/finalizer time so they keep
// their request-time semantics after the request has been detached.
func ValueFromSnapshot(snapshot LogSnapshot, name string) any {
	if !strings.HasPrefix(name, "$") {
		name = "$" + name
	}
	return snapshotField(snapshot, name)
}

// CloneSnapshot returns a detached copy suitable for applying a per-callback
// body policy.  The clone deliberately re-copies maps and byte slices even
// though the source is already detached, so callbacks cannot affect one
// another through shared state.
func CloneSnapshot(snapshot LogSnapshot) LogSnapshot {
	clone := snapshot
	clone.Request.Header = cloneHeader(snapshot.Request.Header)
	clone.Request.Query = cloneQuery(snapshot.Request.Query)
	clone.Request.Body = append([]byte(nil), snapshot.Request.Body...)
	clone.Request.APISIXVars = cloneSafeMap(snapshot.Request.APISIXVars, nil)
	clone.Request.RequestVars = cloneSafeMap(snapshot.Request.RequestVars, nil)
	clone.Response.Header = cloneHeader(snapshot.Response.Header)
	clone.Response.Trailer = cloneHeader(snapshot.Response.Trailer)
	clone.Response.Body = append([]byte(nil), snapshot.Response.Body...)
	return clone
}

func snapshotField(snapshot LogSnapshot, key string) any {
	switch key {
	case "$time_iso8601":
		return snapshotTime(snapshot).Format(time.RFC3339)
	case "$time_local":
		return snapshotTime(snapshot).Format("02/Jan/2006:15:04:05 -0700")
	case "$request_method", "$method":
		return snapshot.Request.Method
	case "$request_line":
		return snapshot.Request.Method + " " + snapshot.Request.URI + " " + snapshot.Request.Proto
	case "$uri":
		return snapshotRequestPath(snapshot.Request)
	case "$request_uri":
		if snapshot.Request.URI != "" {
			return snapshot.Request.URI
		}
		return snapshot.Request.URL
	case "$host":
		return snapshotAddressHost(snapshot.Request.Host)
	case "$http_host":
		return snapshot.Request.Host
	case "$remote_addr":
		if value, ok := snapshot.Request.APISIXVars["$remote_addr"]; ok {
			return value
		}
		return snapshotAddressHost(snapshot.Request.RemoteAddr)
	case "$remote_port":
		if value, ok := snapshot.Request.APISIXVars["$remote_port"]; ok {
			return value
		}
		_, port, _ := net.SplitHostPort(snapshot.Request.RemoteAddr)
		return port
	case "$args", "$query_string":
		return snapshotRequestQuery(snapshot.Request)
	case "$scheme":
		return snapshot.Request.Scheme
	case "$server_protocol", "$proto":
		return snapshot.Request.Proto
	case "$status", "$status_code":
		return snapshot.Outcome.Status
	case "$request_length":
		if value, ok := snapshot.Request.RequestVars[key]; ok {
			return value
		}
		return max(snapshot.Request.ContentLength, 0)
	case "$bytes_sent":
		if value, ok := snapshot.Request.RequestVars[key]; ok {
			return value
		}
		return snapshot.Outcome.Bytes
	case "$request_body":
		return string(snapshot.Request.Body)
	case "$response_body":
		return string(snapshot.Response.Body)
	case "$consumer_name":
		return snapshot.Request.Consumer.Username
	case "$consumer_group_id":
		return snapshot.Request.Consumer.GroupID
	case "$response_source":
		return string(snapshot.Source)
	case "$content_length":
		return snapshot.Request.Header.Get("Content-Length")
	case "$content_type":
		return snapshot.Request.Header.Get("Content-Type")
	}
	if value, ok := snapshot.Request.APISIXVars[key]; ok {
		return value
	}
	if value, ok := snapshot.Request.RequestVars[key]; ok {
		return value
	}
	if suffix, ok := strings.CutPrefix(key, "$arg_"); ok {
		return snapshot.Request.Query.Get(suffix)
	}
	if suffix, ok := strings.CutPrefix(key, "$http_"); ok {
		name := http.CanonicalHeaderKey(strings.ReplaceAll(suffix, "_", "-"))
		return snapshot.Request.Header.Get(name)
	}
	return ""
}

func snapshotTime(snapshot LogSnapshot) time.Time {
	if !snapshot.Finished.IsZero() {
		return snapshot.Finished
	}
	return snapshot.Started
}

func snapshotRequestPath(request RequestLogSnapshot) string {
	if parsed, err := url.ParseRequestURI(request.URI); err == nil {
		return parsed.Path
	}
	return request.URI
}

func snapshotRequestQuery(request RequestLogSnapshot) string {
	if parsed, err := url.ParseRequestURI(request.URI); err == nil {
		return parsed.RawQuery
	}
	return request.Query.Encode()
}

func snapshotAddressHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		return host
	}
	return address
}

// BuildSnapshot copies all request and response data into one detached value.
// Request bodies are bounded to the same hard ceiling as logger policies and
// are restored before returning to the request pipeline.
func BuildSnapshot(
	r *http.Request,
	response ResponseSnapshot,
	outcome apisixctx.ResponseOutcome,
	source apisixctx.ResponseSource,
	started time.Time,
	finished time.Time,
) LogSnapshot {
	var requestBody []byte
	var requestBodyTruncated bool
	if r != nil {
		requestBody, requestBodyTruncated = captureRequestBody(r)
	}
	return BuildSnapshotFromOwnedInputs(
		r,
		ResponseSnapshot{
			Header:        cloneHeader(response.Header),
			Trailer:       cloneHeader(response.Trailer),
			Body:          append([]byte(nil), response.Body...),
			BodyTruncated: response.BodyTruncated,
		},
		requestBody,
		requestBodyTruncated,
		outcome,
		source,
		started,
		finished,
	)
}

// BuildSnapshotFromOwnedInputs builds a snapshot from detached response data
// and a captured request body. The response fields and requestBody ownership
// are transferred to the returned snapshot; callers must not mutate or reuse
// them afterwards. The request itself is read only for scalar metadata and
// mutable request headers, query values, and context variables are cloned
// exactly once. In particular, request.Body is never read.
func BuildSnapshotFromOwnedInputs(
	r *http.Request,
	response ResponseSnapshot,
	requestBody []byte,
	requestBodyTruncated bool,
	outcome apisixctx.ResponseOutcome,
	source apisixctx.ResponseSource,
	started time.Time,
	finished time.Time,
) LogSnapshot {
	snapshot := LogSnapshot{
		Outcome:  outcome,
		Source:   source,
		Started:  started,
		Finished: finished,
		Response: ResponseLogSnapshot(response),
	}
	if r == nil {
		return snapshot
	}
	remoteIP := apisixctx.EffectiveRemoteIP(r)
	remotePort := apisixctx.GetString(r.Context(), string(apisixctx.RemotePortKey))
	if remotePort == "" {
		_, remotePort, _ = net.SplitHostPort(r.RemoteAddr)
	}
	remoteAddr := remoteIP
	if remoteIP != "" && remotePort != "" {
		remoteAddr = net.JoinHostPort(remoteIP, remotePort)
	}
	request := RequestLogSnapshot{
		Method:        r.Method,
		URI:           r.URL.RequestURI(),
		URL:           r.URL.String(),
		Path:          r.URL.Path,
		Host:          r.Host,
		RemoteAddr:    remoteAddr,
		Scheme:        requestScheme(r),
		Proto:         r.Proto,
		ContentLength: r.ContentLength,
		Header:        cloneHeader(r.Header),
		Query:         cloneQuery(r.URL.Query()),
		Body:          requestBody,
		BodyTruncated: requestBodyTruncated,
	}
	sensitiveQueryNames := apisixctx.SensitiveQueryNames(r)
	request.URI = RedactURI(request.URI, sensitiveQueryNames)
	request.URL = RedactURI(request.URL, sensitiveQueryNames)
	request.Query = RedactQuery(request.Query, sensitiveQueryNames)
	remaining := snapshotValueBudget
	request.APISIXVars = cloneSafeMap(apisixctx.GetApisixVars(r), &remaining)
	if request.APISIXVars == nil {
		request.APISIXVars = make(map[string]any)
	}
	if remoteIP != "" {
		request.APISIXVars["$remote_addr"] = remoteIP
	}
	if remotePort != "" {
		request.APISIXVars["$remote_port"] = remotePort
	}
	request.RequestVars = cloneSafeMap(apisixctx.GetRequestVars(r), &remaining)
	if len(sensitiveQueryNames) > 0 {
		redactedQueryString := RedactRawQuery(r.URL.RawQuery, sensitiveQueryNames)
		redactedRequestURI := RedactURI(r.URL.RequestURI(), sensitiveQueryNames)
		for _, values := range []map[string]any{request.APISIXVars, request.RequestVars} {
			if values == nil {
				continue
			}
			if _, ok := values["$args"]; ok {
				values["$args"] = redactedQueryString
			}
			if _, ok := values["$query_string"]; ok {
				values["$query_string"] = redactedQueryString
			}
			if _, ok := values["$request_uri"]; ok {
				values["$request_uri"] = redactedRequestURI
			}
			if upstreamURI, ok := values["$upstream_uri"].(string); ok {
				values["$upstream_uri"] = RedactURI(upstreamURI, sensitiveQueryNames)
			}
			for key := range values {
				if queryName, ok := strings.CutPrefix(key, "$arg_"); ok {
					if _, sensitive := sensitiveQueryNames[queryName]; sensitive {
						values[key] = sensitiveQueryPlaceholder
					}
				}
			}
		}
	}
	correlation := CaptureRequestCorrelation(r)
	request.ID = correlation.RequestID
	snapshot.NodeID = correlation.NodeID
	request.Consumer = safeConsumerIdentity(request.APISIXVars)
	snapshot.Request = request
	return snapshot
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	return header.Clone()
}

func cloneQuery(query url.Values) url.Values {
	if query == nil {
		return nil
	}
	cloned := make(url.Values, len(query))
	for key, values := range query {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func cloneSafeMap(source map[string]any, remaining *int) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		if key == "$consumer" {
			continue
		}
		cloned, ok := CloneSafeValue(value, remaining)
		if ok {
			result[key] = cloned
		}
	}
	return result
}

func safeConsumerIdentity(values map[string]any) SafeConsumerLogIdentity {
	identity := SafeConsumerLogIdentity{}
	if value, ok := values["$consumer_name"].(string); ok {
		identity.Username = value
	}
	if value, ok := values["$consumer_group_id"].(string); ok {
		identity.GroupID = value
	}
	return identity
}

func requestScheme(r *http.Request) string {
	if value := r.Header.Get("X-Forwarded-Proto"); value != "" {
		return value
	}
	if r.URL.Scheme != "" {
		return r.URL.Scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func captureRequestBody(r *http.Request) ([]byte, bool) {
	if value, ok := apisixctx.GetRequestVar(r, apisixctx.RequestBodyKey).([]byte); ok {
		body := append([]byte(nil), value...)
		if len(body) > snapshotBodyLimit {
			return body[:snapshotBodyLimit], true
		}
		return body, false
	}
	if r.Body == nil || r.Body == http.NoBody {
		return nil, false
	}
	original := r.Body
	prefix, err := io.ReadAll(io.LimitReader(original, snapshotBodyLimit+1))
	r.Body = &snapshotReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), original), Closer: original}
	if err != nil && len(prefix) == 0 {
		return nil, false
	}
	truncated := len(prefix) > snapshotBodyLimit
	if truncated {
		prefix = prefix[:snapshotBodyLimit]
	}
	return append([]byte(nil), prefix...), truncated
}

type snapshotReadCloser struct {
	io.Reader
	io.Closer
}
