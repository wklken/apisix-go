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

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
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
	defaultMaxTimeout       = 30000
	hardMaxTimeout          = 60000
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
    },
    "max_timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 30000
    }
  }
}
`

type Config struct{}

type Limits struct {
	MaxBodySize         int64 `json:"max_body_size,omitempty"`
	MaxResponseBodySize int64 `json:"max_response_body_size,omitempty"`
	MaxPipelineItems    int   `json:"max_pipeline_items,omitempty"`
	MaxConcurrency      int   `json:"max_concurrency,omitempty"`
	MaxTimeout          int   `json:"max_timeout,omitempty"`
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
	Status  int            `json:"status"`
	Reason  string         `json:"reason"`
	Headers map[string]any `json:"headers,omitempty"`
	Body    string         `json:"body,omitempty"`
}

// DispatchLease keeps a route generation alive while a batch subrequest is
// executing through the server request boundary.
type DispatchLease struct {
	Handler http.Handler
	Release func()
}

// DispatchLeaseFactory acquires a dispatch lease for one batch worker. The
// factory must increment the generation reference before returning and its
// lease release must be idempotent.
type DispatchLeaseFactory func() (DispatchLease, bool)

type dispatchLeaseFactoryContextKey struct{}

// batchSubrequestContextKey marks requests dispatched by a batch handler. It
// is deliberately private so callers cannot opt into nested batch execution.
type batchSubrequestContextKey struct{}

func isBatchSubrequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	_, ok := r.Context().Value(batchSubrequestContextKey{}).(struct{})
	return ok
}

// WithDispatchLeaseFactory attaches a generation-aware subrequest factory to
// the request context without exposing the context key to other packages.
func WithDispatchLeaseFactory(r *http.Request, factory DispatchLeaseFactory) *http.Request {
	if r == nil || factory == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), dispatchLeaseFactoryContextKey{}, factory))
}

// DispatchLeaseFactoryFromRequest returns the generation-aware subrequest
// factory attached by the server boundary, if any.
func DispatchLeaseFactoryFromRequest(r *http.Request) DispatchLeaseFactory {
	if r == nil {
		return nil
	}
	factory, _ := r.Context().Value(dispatchLeaseFactoryContextKey{}).(DispatchLeaseFactory)
	return factory
}

type ErrorResponse struct {
	ErrorMessage string `json:"error_msg"`
}

var errMissingPipeline = errors.New(`bad request body: object matches none of the required: ["pipeline"]`)

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
	return NewHandlerWithLimits(dispatcher, Limits{})
}

func NewHandlerFromMetadata(dispatcher http.Handler, view runtime.MetadataView) (http.Handler, error) {
	var limits Limits
	if _, err := view.Decode(name, &limits); err != nil {
		return nil, fmt.Errorf("batch-requests metadata decode failed: %w", err)
	}
	return NewHandlerWithLimits(dispatcher, limits), nil
}

func NewHandlerWithLimits(dispatcher http.Handler, limits Limits) http.Handler {
	limits = applyLimitDefaults(limits)
	batchDispatcher := newBatchDispatcher(dispatcher, limits.MaxConcurrency)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveBatchRequest(batchDispatcher, limits, w, r)
	})
}

func serveBatchRequest(dispatcher *batchDispatcher, limits Limits, w http.ResponseWriter, r *http.Request) {
	tasks := runtime.NewRequestTaskGroup(r.Context(), "request/batch-requests")
	defer func() {
		if waitErr := tasks.Wait(); waitErr != nil {
			logger.Debugf("failed to join batch-requests workers: %s", waitErr)
		}
	}()
	responses, errStatus, err := handleBatchRequest(dispatcher, w, r, limits, tasks)
	if err != nil {
		if errors.Is(err, errMissingPipeline) {
			body, marshalErr := json.Marshal(ErrorResponse{ErrorMessage: err.Error()})
			if marshalErr != nil {
				logger.Debugf("failed to marshal batch-requests missing-pipeline response: %s", marshalErr)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(errStatus)
			if _, writeErr := w.Write(append(body, '\n')); writeErr != nil {
				logger.Debugf("failed to write batch-requests missing-pipeline response: %s", writeErr)
			}
			return
		}
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
	tasks *runtime.RequestTaskGroup,
) ([]PipelineResponse, int, error) {
	if isBatchSubrequest(r) {
		return nil, http.StatusBadRequest, fmt.Errorf("nested batch requests are not allowed")
	}
	limits = applyLimitDefaults(limits)
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
		return nil, http.StatusBadRequest, errMissingPipeline
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err)
	}
	if err := validateRequest(req, limits); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err)
	}

	timeoutMilliseconds := limits.MaxTimeout
	if req.Timeout != nil {
		timeoutMilliseconds = *req.Timeout
	}
	// applyLimitDefaults caps MaxTimeout at hardMaxTimeout, so this conversion
	// cannot overflow time.Duration.
	timeout := time.Duration(timeoutMilliseconds) * time.Millisecond

	responses := make([]PipelineResponse, 0, len(req.Pipeline))
	for _, item := range req.Pipeline {
		result, timedOut, err := dispatcher.dispatch(
			r, req, item, timeout, limits.MaxResponseBodySize, tasks,
		)
		if err != nil {
			if waitErr := tasks.Wait(); waitErr != nil {
				return nil, http.StatusInternalServerError, errors.Join(err, waitErr)
			}
			return nil, http.StatusInternalServerError, err
		}
		if !timedOut && !result.completed {
			if waitErr := tasks.Wait(); waitErr != nil {
				return nil, http.StatusInternalServerError, waitErr
			}
		}
		responses = append(responses, result.response)
		if timedOut {
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
	limits = applyLimitDefaults(limits)
	if req.Timeout != nil {
		if *req.Timeout < 1 {
			return fmt.Errorf("timeout must be at least 1 millisecond")
		}
		if *req.Timeout > limits.MaxTimeout {
			return fmt.Errorf("timeout must not exceed %d milliseconds", limits.MaxTimeout)
		}
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
		if !strings.HasPrefix(item.Path, "/") {
			return fmt.Errorf("pipeline[%d].path must start with /", i)
		}
		target, err := url.ParseRequestURI(item.Path)
		if err != nil || strings.HasPrefix(item.Path, "//") || target.IsAbs() || target.Host != "" ||
			target.RawQuery != "" {
			if err == nil {
				err = fmt.Errorf("target must be an origin-form request URI")
			}
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
	if limits.MaxTimeout <= 0 {
		limits.MaxTimeout = defaultMaxTimeout
	}
	if limits.MaxTimeout > hardMaxTimeout {
		limits.MaxTimeout = hardMaxTimeout
	}
	return limits
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

// pipelineResult carries one subrequest response and its completion state out
// of its worker task.
type pipelineResult struct {
	response  PipelineResponse
	completed bool
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
	tasks *runtime.RequestTaskGroup,
) (pipelineResult, bool, error) {
	return dispatchPipelineRequestBounded(d, outer, batch, item, timeout, maxResponseBodySize, tasks)
}

func dispatchPipelineRequestBounded(
	dispatcher *batchDispatcher,
	outer *http.Request,
	batch Request,
	item PipelineRequest,
	timeout time.Duration,
	maxResponseBodySize int64,
	tasks *runtime.RequestTaskGroup,
) (pipelineResult, bool, error) {
	// The subrequest context derives from the incoming request so canceling
	// the parent cancels every subrequest.
	var ctx context.Context = contextWithoutValues{Context: outer.Context()}
	ctx = context.WithValue(ctx, batchSubrequestContextKey{}, struct{}{})
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
		return pipelineResult{
			response:  PipelineResponse{Status: http.StatusBadRequest, Reason: http.StatusText(http.StatusBadRequest)},
			completed: true,
		}, false, nil
	}
	if !dispatcher.acquire(ctx) {
		return pipelineResult{response: timeoutResponse(), completed: true}, true, nil
	}
	leaseFactory := DispatchLeaseFactoryFromRequest(outer)
	handler := dispatcher.handler
	var releaseLease func()
	if leaseFactory != nil {
		lease, ok := leaseFactory()
		if !ok || lease.Handler == nil {
			if lease.Release != nil {
				lease.Release()
			}
			dispatcher.release()
			return pipelineResult{response: unavailableResponse(), completed: true}, false, nil
		}
		handler = lease.Handler
		var releaseOnce sync.Once
		releaseLease = func() {
			releaseOnce.Do(func() {
				if lease.Release != nil {
					lease.Release()
				}
			})
		}
	}
	trustedOuterHeaders := apisixctx.TrustedRequestHeaders(outer)
	req.RemoteAddr = outer.RemoteAddr
	req.Host = outer.Host
	req.Header = mergeHeaders(trustedOuterHeaders, batch.Headers, item.Headers, outer.RemoteAddr)
	req = apisixctx.WithRequestHeaderProvenance(
		req,
		trustedOuterHeaders,
		pipelineHeaderKeys(batch.Headers, item.Headers),
	)
	req.Header.Del("X-Consumer-Username")
	req.Header.Del("Host")
	req.Header.Del("X-Forwarded-Host")
	for _, value := range outer.Header.Values("X-Forwarded-Host") {
		req.Header.Add("X-Forwarded-Host", value)
	}
	if leaseFactory != nil {
		req = WithDispatchLeaseFactory(req, leaseFactory)
	}

	recorder := newBoundedResponseRecorder(maxResponseBodySize)
	done := make(chan pipelineResult, 1)
	if err := tasks.Go(func(context.Context) error {
		result := pipelineResult{}
		defer func() {
			done <- result
		}()
		if releaseLease != nil {
			defer releaseLease()
		}
		defer dispatcher.release()
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					if recovered == http.ErrAbortHandler {
						result.response = abortResponse()
						result.completed = true
						return
					}
					panic(recovered)
				}
			}()
			handler.ServeHTTP(recorder, req)
			result.response = recorder.pipelineResponse()
			result.completed = true
		}()
		return nil
	}); err != nil {
		if cancel != nil {
			cancel()
		}
		if releaseLease != nil {
			releaseLease()
		}
		dispatcher.release()
		if waitErr := tasks.Wait(); waitErr != nil {
			return pipelineResult{}, false, errors.Join(err, waitErr)
		}
		return pipelineResult{}, false, err
	}

	select {
	case <-ctx.Done():
		// Return immediately once the timeout expires; the buffered done
		// channel lets a late worker publish without blocking even when the
		// handler ignores the canceled context.
		return pipelineResult{response: timeoutResponse(), completed: true}, true, nil
	case result := <-done:
		if !result.completed {
			return result, false, nil
		}
		if ctx.Err() != nil {
			return pipelineResult{response: timeoutResponse(), completed: true}, true, nil
		}
		return result, false, nil
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
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	status := r.status
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

func abortResponse() PipelineResponse {
	return PipelineResponse{
		Status: http.StatusBadGateway,
		Reason: http.StatusText(http.StatusBadGateway),
	}
}

func unavailableResponse() PipelineResponse {
	return PipelineResponse{
		Status: http.StatusBadGateway,
		Reason: "route unavailable",
	}
}

func flattenHeaders(header http.Header) map[string]any {
	out := make(map[string]any, len(header))
	for key, values := range header {
		switch len(values) {
		case 0:
			continue
		case 1:
			out[key] = values[0]
		default:
			out[key] = append([]string(nil), values...)
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
		if pipelineHeaderOverrideAllowed(key) {
			headers.Set(key, value)
		}
	}
	for key, value := range item {
		if pipelineHeaderOverrideAllowed(key) {
			headers.Set(key, value)
		}
	}
	if remoteIP := base.RemoteIP(remoteAddr); remoteIP != "" {
		headers.Set("X-Real-IP", remoteIP)
	}
	return headers
}

func pipelineHeaderOverrideAllowed(key string) bool {
	key = http.CanonicalHeaderKey(key)
	if strings.HasPrefix(key, "X-Forwarded-") {
		return false
	}
	switch key {
	case "Authorization", "Connection", "Cookie", "Forwarded", "Keep-Alive", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "X-Consumer-Username", "X-Real-Ip":
		return false
	default:
		return true
	}
}

func pipelineHeaderKeys(common, item map[string]string) []string {
	keys := make([]string, 0, len(common)+len(item))
	for key := range common {
		keys = append(keys, key)
	}
	for key := range item {
		keys = append(keys, key)
	}
	return keys
}
