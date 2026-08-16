package batch_requests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
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
	defaultMaxResponseSize  = 4 * 1024 * 1024
	defaultMaxPipelineItems = 20
	defaultMaxConcurrency   = 8
	defaultTimeout          = 30 * time.Second
)

const schema = `{"type":"object"}`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "max_body_size": {
      "type": "integer",
      "exclusiveMinimum": 0,
      "default": 1048576
    },
    "max_pipeline_items": {
      "type": "integer",
      "exclusiveMinimum": 0,
      "default": 20
    },
    "max_concurrency": {
      "type": "integer",
      "exclusiveMinimum": 0,
      "default": 8
    },
    "max_response_body_size": {
      "type": "integer",
      "exclusiveMinimum": 0,
      "default": 4194304
    }
  }
}
`

var compiledBatchLimitsSchema = mustCompileBatchLimitsSchema()

func mustCompileBatchLimitsSchema() *util.CompiledSchema {
	compiled, err := util.CompileSchema(metadataSchema)
	if err != nil {
		panic("compile batch-requests metadata schema: " + err.Error())
	}
	return compiled
}

type Config struct{}

type Limits struct {
	MaxBodySize         int64 `json:"max_body_size,omitempty"`
	MaxResponseBodySize int64 `json:"max_response_body_size,omitempty"`
	MaxPipelineItems    int   `json:"max_pipeline_items,omitempty"`
	MaxConcurrency      int   `json:"max_concurrency,omitempty"`
}

type Request struct {
	Query    map[string]string `json:"query,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Timeout  *int              `json:"timeout,omitempty"`
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
	p.MetadataSchema = metadataSchema
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
	return newMetadataHandler(dispatcher, loadLimits)
}

func NewHandlerWithLimits(dispatcher http.Handler, limits Limits) http.Handler {
	limits = applyLimitDefaults(limits)
	batchDispatcher := newBatchDispatcher(dispatcher, limits.MaxConcurrency)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveBatchRequest(batchDispatcher, limits, w, r)
	})
}

func newMetadataHandler(dispatcher http.Handler, loader func() (Limits, error)) http.Handler {
	// Seed the Store-owned last-good snapshot before the public endpoint serves
	// its first request. Later router generations repeat this validation against
	// the same active Store.
	initial, err := loader()
	if err != nil {
		initial = Limits{}
	}
	initial = applyLimitDefaults(initial)
	batchDispatcher := newBatchDispatcher(dispatcher, initial.MaxConcurrency)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limits, err := loader()
		if err != nil {
			if writeErr := util.WriteJSON(w, http.StatusBadRequest, ErrorResponse{
				ErrorMessage: fmt.Sprintf("invalid configuration: %s", err),
			}); writeErr != nil {
				logger.Debugf("failed to write batch-requests error response: %s", writeErr)
			}
			return
		}
		limits = applyLimitDefaults(limits)
		batchDispatcher.setLimit(limits.MaxConcurrency)
		serveBatchRequest(batchDispatcher, limits, w, r)
	})
}

func serveBatchRequest(dispatcher *batchDispatcher, limits Limits, w http.ResponseWriter, r *http.Request) {
	responses, errStatus, err := handleBatchRequest(dispatcher, w, r, limits)
	if err != nil {
		if writeErr := util.WriteJSON(w, errStatus, ErrorResponse{ErrorMessage: err.Error()}); writeErr != nil {
			logger.Debugf("failed to write batch-requests error response: %s", writeErr)
		}
		return
	}
	if writeErr := util.WriteJSON(w, http.StatusOK, responses); writeErr != nil {
		logger.Debugf("failed to write batch-requests response: %s", writeErr)
	}
}

func handleBatchRequest(
	dispatcher *batchDispatcher,
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

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		// Do not echo the request body back to the client through the error.
		return nil, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err)
	}
	pipelinePresent, err := validateDecodedRequestTypes(decoded)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err)
	}
	if !pipelinePresent {
		return nil, http.StatusBadRequest, fmt.Errorf("bad request body: pipeline is required")
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err)
	}
	if err := validateRequest(req, limits); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err)
	}

	timeout := defaultTimeout
	if req.Timeout != nil {
		timeout = time.Duration(*req.Timeout) * time.Millisecond
	}

	responses := make([]PipelineResponse, 0, len(req.Pipeline))
	for _, item := range req.Pipeline {
		response, timedOut := dispatcher.dispatch(r, req, item, timeout, limits.MaxResponseBodySize)
		responses = append(responses, response)
		if timedOut && r.Context().Err() != nil {
			break
		}
	}
	return responses, http.StatusOK, nil
}

func validateDecodedRequestTypes(decoded any) (bool, error) {
	request, ok := decoded.(map[string]any)
	if !ok {
		return false, nil
	}
	if timeout, exists := request["timeout"]; exists {
		number, ok := timeout.(float64)
		if !ok || number != math.Trunc(number) {
			return false, fmt.Errorf(`property "timeout" validation failed: expected integer`)
		}
	}
	rawPipeline, present := request["pipeline"]
	if !present {
		return false, nil
	}
	pipeline, ok := rawPipeline.([]any)
	if !ok {
		return true, nil
	}
	for i, rawItem := range pipeline {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		for key := range item {
			switch key {
			case "version", "method", "path", "query", "headers", "body", "ssl_verify":
			default:
				return true, fmt.Errorf(
					`property "pipeline" validation failed: item %d unknown field %q`,
					i+1,
					key,
				)
			}
		}
		if sslVerify, exists := item["ssl_verify"]; exists {
			if _, ok := sslVerify.(bool); !ok {
				return true, fmt.Errorf(
					`property "pipeline" validation failed: item %d property "ssl_verify" expected boolean`,
					i+1,
				)
			}
		}
	}
	return true, nil
}

func readLimitedBody(w http.ResponseWriter, r *http.Request, maxSize int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSize))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func validateRequest(req Request, limits Limits) error {
	if req.Timeout != nil && *req.Timeout < 1 {
		return fmt.Errorf("timeout must be at least 1 millisecond")
	}
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
		if _, err := http.NewRequest(http.MethodGet, item.Path, nil); err != nil {
			return fmt.Errorf("pipeline[%d].path is invalid: %w", i, err)
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
	if limits.MaxResponseBodySize <= 0 {
		limits.MaxResponseBodySize = defaultMaxResponseSize
	}
	if limits.MaxConcurrency <= 0 {
		limits.MaxConcurrency = defaultMaxConcurrency
	}
	return limits
}

func loadLimits() (Limits, error) {
	var limits Limits
	usedLastGood, err := store.GetValidatedPluginMetadata(
		name,
		func(metadata map[string]any) error {
			return compiledBatchLimitsSchema.Validate(metadata)
		},
		&limits,
	)
	if errors.Is(err, store.ErrNotFound) {
		return applyLimitDefaults(Limits{}), nil
	}
	if err != nil {
		logger.Errorf("validate plugin_metadata %s: %s", name, err)
		if !usedLastGood {
			return Limits{}, err
		}
	}
	return applyLimitDefaults(limits), nil
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

// pipelineResult carries one completed subrequest response out of its worker
// goroutine.
type pipelineResult struct {
	response PipelineResponse
}

type batchDispatcher struct {
	handler http.Handler

	mu      sync.Mutex
	active  int
	limit   int
	changed chan struct{}
}

func newBatchDispatcher(handler http.Handler, limit int) *batchDispatcher {
	return &batchDispatcher{
		handler: handler,
		limit:   limit,
		changed: make(chan struct{}),
	}
}

func (d *batchDispatcher) setLimit(limit int) {
	d.mu.Lock()
	if d.limit != limit {
		d.limit = limit
		close(d.changed)
		d.changed = make(chan struct{})
	}
	d.mu.Unlock()
}

func (d *batchDispatcher) acquire(ctx context.Context) bool {
	for {
		d.mu.Lock()
		if ctx.Err() != nil {
			d.mu.Unlock()
			return false
		}
		if d.active < d.limit {
			d.active++
			d.mu.Unlock()
			return true
		}
		changed := d.changed
		d.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

func (d *batchDispatcher) release() {
	d.mu.Lock()
	d.active--
	close(d.changed)
	d.changed = make(chan struct{})
	d.mu.Unlock()
}

func (d *batchDispatcher) dispatch(
	outer *http.Request,
	batch Request,
	item PipelineRequest,
	timeout time.Duration,
	maxResponseBodySize int64,
) (PipelineResponse, bool) {
	return dispatchPipelineRequestBounded(d, outer, batch, item, timeout, maxResponseBodySize)
}

func dispatchPipelineRequestBounded(
	dispatcher *batchDispatcher,
	outer *http.Request,
	batch Request,
	item PipelineRequest,
	timeout time.Duration,
	maxResponseBodySize int64,
) (PipelineResponse, bool) {
	// The subrequest context derives from the incoming request so canceling
	// the parent cancels every subrequest.
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

	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(item.Body))
	if err != nil {
		return PipelineResponse{Status: http.StatusBadRequest, Reason: http.StatusText(http.StatusBadRequest)}, false
	}
	if !dispatcher.acquire(ctx) {
		return timeoutResponse(), true
	}
	req.RemoteAddr = outer.RemoteAddr
	req.Host = outer.Host
	req.Header = mergeHeaders(outer.Header, batch.Headers, item.Headers, outer.RemoteAddr)
	req.Header.Del("X-Consumer-Username")
	if host := req.Header.Get("Host"); host != "" {
		req.Host = host
		req.Header.Del("Host")
	}

	recorder := newBoundedResponseRecorder(maxResponseBodySize)
	done := make(chan pipelineResult, 1)
	go func() {
		defer dispatcher.release()
		dispatcher.handler.ServeHTTP(recorder, req)
		resp := recorder.pipelineResponse()
		done <- pipelineResult{response: resp}
	}()

	select {
	case <-ctx.Done():
		// Return immediately once the timeout expires; the buffered done
		// channel lets a late worker publish without blocking even when the
		// handler ignores the canceled context.
		return timeoutResponse(), true
	case result := <-done:
		if ctx.Err() != nil {
			return timeoutResponse(), true
		}
		return result.response, false
	}
}

type boundedResponseRecorder struct {
	header       http.Header
	resultHeader http.Header
	body         bytes.Buffer
	status       int
	written      int64
	maxBytes     int64
}

func newBoundedResponseRecorder(maxBytes int64) *boundedResponseRecorder {
	return &boundedResponseRecorder{header: make(http.Header), maxBytes: maxBytes}
}

func (r *boundedResponseRecorder) Header() http.Header { return r.header }

func (r *boundedResponseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.resultHeader = r.header.Clone()
	}
}

func (r *boundedResponseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	r.written += int64(len(body))
	remaining := r.maxBytes + 1 - int64(r.body.Len())
	if remaining > 0 {
		keep := min(int64(len(body)), remaining)
		_, _ = r.body.Write(body[:keep])
	}
	return len(body), nil
}

func (r *boundedResponseRecorder) Flush() {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
}

func (r *boundedResponseRecorder) pipelineResponse() PipelineResponse {
	if r.written > r.maxBytes {
		return PipelineResponse{Status: http.StatusBadGateway, Reason: http.StatusText(http.StatusBadGateway)}
	}
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	response := PipelineResponse{
		Status:  status,
		Reason:  http.StatusText(status),
		Headers: flattenHeaders(r.resultHeader),
	}
	if r.body.Len() > 0 {
		response.Body = r.body.String()
	}
	return response
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
