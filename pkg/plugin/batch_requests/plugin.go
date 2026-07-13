package batch_requests

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 4010
	name     = "batch-requests"

	DefaultURI              = "/apisix/batch-requests"
	defaultMaxBodySize      = 1024 * 1024
	defaultMaxPipelineItems = 1000
	defaultTimeout          = 30 * time.Second
)

const schema = `{"type":"object"}`

type Config struct{}

type Limits struct {
	MaxBodySize      int64 `json:"max_body_size,omitempty"`
	MaxPipelineItems int   `json:"max_pipeline_items,omitempty"`
}

type Request struct {
	Query    map[string]string `json:"query,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Timeout  int               `json:"timeout,omitempty"`
	Pipeline []PipelineRequest `json:"pipeline"`
}

type PipelineRequest struct {
	Version   float64           `json:"version,omitempty"`
	Method    string            `json:"method,omitempty"`
	Path      string            `json:"path"`
	Query     map[string]string `json:"query,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
	SSLVerify bool              `json:"ssl_verify,omitempty"`
}

type PipelineResponse struct {
	Status  int               `json:"status"`
	Reason  string            `json:"reason"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type ErrorResponse struct {
	ErrorMessage string `json:"error_msg"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func NewHandler(dispatcher http.Handler) http.Handler {
	return NewHandlerWithLimits(dispatcher, loadLimits())
}

func NewHandlerWithLimits(dispatcher http.Handler, limits Limits) http.Handler {
	limits = applyLimitDefaults(limits)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses, errStatus, err := handleBatchRequest(dispatcher, w, r, limits)
		if err != nil {
			writeJSON(w, errStatus, ErrorResponse{ErrorMessage: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, responses)
	})
}

func handleBatchRequest(
	dispatcher http.Handler,
	w http.ResponseWriter,
	r *http.Request,
	limits Limits,
) ([]PipelineResponse, int, error) {
	body, err := readLimitedBody(w, r, limits.MaxBodySize)
	if err != nil {
		return nil, http.StatusRequestEntityTooLarge, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("no request body, you should give at least one pipeline setting")
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid request body: %s, err: %w", body, err)
	}
	if err := validateRequest(req, limits); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err)
	}

	timeout := defaultTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Millisecond
	}

	responses := make([]PipelineResponse, 0, len(req.Pipeline))
	for _, item := range req.Pipeline {
		responses = append(responses, dispatchPipelineRequest(dispatcher, r, req, item, timeout))
	}
	return responses, http.StatusOK, nil
}

func readLimitedBody(w http.ResponseWriter, r *http.Request, maxSize int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSize))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func validateRequest(req Request, limits Limits) error {
	if len(req.Pipeline) == 0 {
		return fmt.Errorf("pipeline must contain at least one request")
	}
	if len(req.Pipeline) > limits.MaxPipelineItems {
		return fmt.Errorf("too many pipeline requests, %d exceeds the maximum of %d",
			len(req.Pipeline), limits.MaxPipelineItems)
	}
	for i, item := range req.Pipeline {
		if item.Path == "" {
			return fmt.Errorf("pipeline[%d].path is required", i)
		}
		if item.Method != "" && !validMethod(item.Method) {
			return fmt.Errorf("pipeline[%d].method is invalid", i)
		}
		if item.Version != 0 && item.Version != 1.0 && item.Version != 1.1 {
			return fmt.Errorf("pipeline[%d].version is invalid", i)
		}
	}
	return nil
}

func applyLimitDefaults(limits Limits) Limits {
	if limits.MaxBodySize <= 0 {
		limits.MaxBodySize = defaultMaxBodySize
	}
	if limits.MaxPipelineItems <= 0 {
		limits.MaxPipelineItems = defaultMaxPipelineItems
	}
	return limits
}

func loadLimits() Limits {
	var limits Limits
	if err := safeGetPluginMetadata(name, &limits); err != nil {
		return applyLimitDefaults(Limits{})
	}
	return applyLimitDefaults(limits)
}

func safeGetPluginMetadata(id string, v any) (err error) {
	defer func() {
		if recover() != nil {
			err = store.ErrNotFound
		}
	}()
	return store.GetPluginMetadata(id, v)
}

func validMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
		http.MethodHead, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return true
	default:
		return false
	}
}

func dispatchPipelineRequest(
	dispatcher http.Handler,
	outer *http.Request,
	batch Request,
	item PipelineRequest,
	timeout time.Duration,
) PipelineResponse {
	var ctx context.Context = contextWithoutValues{Context: outer.Context()}
	var cancel func()
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	method := item.Method
	if method == "" {
		method = http.MethodGet
	}
	target := item.Path
	query := mergeQuery(batch.Query, item.Query)
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req := httptest.NewRequest(method, target, strings.NewReader(item.Body)).WithContext(ctx)
	req.RemoteAddr = outer.RemoteAddr
	req.Host = outer.Host
	req.Header = mergeHeaders(outer.Header, batch.Headers, item.Headers, outer.RemoteAddr)
	if host := req.Header.Get("Host"); host != "" {
		req.Host = host
		req.Header.Del("Host")
	}

	recorder := httptest.NewRecorder()
	done := make(chan struct{}, 1)
	go func() {
		dispatcher.ServeHTTP(recorder, req)
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		return timeoutResponse()
	case <-done:
		if ctx.Err() != nil {
			return timeoutResponse()
		}
	}
	result := recorder.Result()
	defer func() { _ = result.Body.Close() }()

	body, err := io.ReadAll(result.Body)
	resp := PipelineResponse{
		Status:  result.StatusCode,
		Reason:  http.StatusText(result.StatusCode),
		Headers: flattenHeaders(result.Header),
	}
	if err == nil && len(body) > 0 {
		resp.Body = string(body)
	}
	return resp
}

type contextWithoutValues struct {
	context.Context
}

func (contextWithoutValues) Value(any) any {
	return nil
}

func timeoutResponse() PipelineResponse {
	return PipelineResponse{
		Status: http.StatusGatewayTimeout,
		Reason: "upstream timeout",
	}
}

func flattenHeaders(header http.Header) map[string]string {
	out := make(map[string]string, len(header))
	for key, values := range header {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

func mergeQuery(common map[string]string, item map[string]string) url.Values {
	values := url.Values{}
	for key, value := range common {
		values.Set(key, value)
	}
	for key, value := range item {
		values.Set(key, value)
	}
	return values
}

func mergeHeaders(outer http.Header, common map[string]string, item map[string]string, remoteAddr string) http.Header {
	headers := http.Header{}
	for key, value := range outer {
		if strings.HasPrefix(strings.ToLower(key), "content-") {
			continue
		}
		headers[key] = append([]string(nil), value...)
	}
	for key, value := range common {
		headers.Set(key, value)
	}
	for key, value := range item {
		headers.Set(key, value)
	}
	if remoteIP := base.RemoteIP(remoteAddr); remoteIP != "" {
		headers.Set("X-Real-IP", remoteIP)
	}
	return headers
}

func writeJSON(w http.ResponseWriter, statusCode int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(value)
}
