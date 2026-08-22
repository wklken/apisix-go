package ai_aliyun_content_moderation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config          Config
	client          *http.Client
	now             func() time.Time
	nonce           func() string
	failMode        ai_common.SafetyFailMode
	accessKeyID     *store.ResolvedSecret
	accessKeySecret *store.ResolvedSecret

	streamNow func() time.Time
}

const (
	priority = 1029
	name     = "ai-aliyun-content-moderation"
)

const schema = `
{
  "type": "object",
  "properties": {
    "stream_check_mode": {
      "type": "string",
      "enum": ["realtime", "final_packet"],
      "default": "final_packet"
    },
    "stream_check_cache_size": {
      "type": "integer",
      "minimum": 1,
      "default": 128
    },
    "stream_check_interval": {
      "type": "number",
      "minimum": 0.1,
      "default": 3
    },
    "endpoint": {
      "type": "string",
      "minLength": 1
    },
    "region_id": {
      "type": "string",
      "minLength": 1
    },
    "access_key_id": {
      "type": "string",
      "minLength": 1
    },
    "access_key_secret": {
      "type": "string",
      "minLength": 1
    },
    "check_request": {
      "type": "boolean",
      "default": true
    },
    "check_response": {
      "type": "boolean",
      "default": false
    },
    "request_check_service": {
      "type": "string",
      "minLength": 1,
      "default": "llm_query_moderation"
    },
    "request_check_length_limit": {
      "type": "number",
      "default": 2000
    },
    "response_check_service": {
      "type": "string",
      "minLength": 1,
      "default": "llm_response_moderation"
    },
    "response_check_length_limit": {
      "type": "number",
      "default": 5000
    },
    "risk_level_bar": {
      "type": "string",
      "enum": ["none", "low", "medium", "high", "max"],
      "default": "high"
    },
    "deny_code": {
      "type": "integer",
      "minimum": 100,
      "maximum": 599,
      "default": 200
    },
    "deny_message": {
      "type": "string"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 10000
    },
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 30
    },
    "keepalive": {
      "type": "boolean",
      "default": true
    },
    "keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 60000
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "fail_mode": {
      "type": "string",
      "enum": ["error", "warn", "skip"],
      "default": "error"
    }
  },
  "required": ["endpoint", "region_id", "access_key_id", "access_key_secret"]
}
`

type Config struct {
	StreamCheckMode          string  `json:"stream_check_mode,omitempty"`
	StreamCheckCacheSize     int     `json:"stream_check_cache_size,omitempty"`
	StreamCheckInterval      float64 `json:"stream_check_interval,omitempty"`
	Endpoint                 string  `json:"endpoint"`
	RegionID                 string  `json:"region_id"`
	AccessKeyID              string  `json:"access_key_id"`
	AccessKeySecret          string  `json:"access_key_secret"`
	CheckRequest             *bool   `json:"check_request,omitempty"`
	CheckResponse            bool    `json:"check_response,omitempty"`
	RequestCheckService      string  `json:"request_check_service,omitempty"`
	RequestCheckLengthLimit  int     `json:"request_check_length_limit,omitempty"`
	ResponseCheckService     string  `json:"response_check_service,omitempty"`
	ResponseCheckLengthLimit int     `json:"response_check_length_limit,omitempty"`
	RiskLevelBar             string  `json:"risk_level_bar,omitempty"`
	DenyCode                 int     `json:"deny_code,omitempty"`
	DenyMessage              string  `json:"deny_message,omitempty"`
	Timeout                  int     `json:"timeout,omitempty"`
	KeepalivePool            int     `json:"keepalive_pool,omitempty"`
	Keepalive                *bool   `json:"keepalive,omitempty"`
	KeepaliveTimeout         int     `json:"keepalive_timeout,omitempty"`
	SSLVerify                *bool   `json:"ssl_verify,omitempty"`
	FailMode                 string  `json:"fail_mode,omitempty"`
}

type serviceParameters struct {
	SessionID string `json:"sessionId"`
	Content   string `json:"content"`
}

type moderationSessionKey struct{}

type moderationRequestState struct {
	bodyTab  map[string]any
	protocol ai_protocols.Protocol
}

type moderationRequestStateKey struct{}

type aliyunResponse struct {
	Data *struct {
		RiskLevel string `json:"RiskLevel"`
		Advice    []struct {
			Answer string `json:"Answer"`
		} `json:"Advice"`
	} `json:"Data"`
}

type moderationResult struct {
	Denied    bool
	Message   string
	RiskLevel string
}

type moderationError struct {
	Class ai_common.SafetyFailureClass
	Err   error
}

func (e *moderationError) Error() string {
	if e == nil || e.Err == nil {
		return "Aliyun moderation check failed"
	}
	return e.Err.Error()
}

func (e *moderationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *moderationError) Classify() string {
	if e == nil {
		return ""
	}
	return string(e.Class)
}

const (
	requestInvalidPayloadMessage   = "Request body is not valid JSON"
	requestUnknownProtocolMessage  = "Request format not recognized by ai-aliyun-content-moderation"
	requestEmptyContentMessage     = "No inspectable AI request content"
	requestReadFailureMessage      = "Unable to read request body"
	requestContentTypeMessage      = "Unsupported request content type"
	moderationUnavailableMessage   = "AI content moderation service unavailable"
	upstreamInvalidResponseMessage = "Upstream response is not valid AI JSON"
)

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if p.accessKeyID == nil || p.accessKeySecret == nil {
		if err := p.MaterializeSecrets(); err != nil {
			return errors.New("ai-aliyun-content-moderation credentials are unavailable")
		}
	}
	mode, err := ai_common.ParseSafetyFailMode(p.config.FailMode)
	if err != nil {
		return err
	}
	p.failMode = mode
	p.config.FailMode = string(mode)
	if p.config.StreamCheckMode == "" {
		p.config.StreamCheckMode = "final_packet"
	}
	if p.config.StreamCheckCacheSize == 0 {
		p.config.StreamCheckCacheSize = 128
	}
	if p.config.StreamCheckInterval == 0 {
		p.config.StreamCheckInterval = 3
	}
	if p.config.CheckRequest == nil {
		checkRequest := true
		p.config.CheckRequest = &checkRequest
	}
	if p.config.RequestCheckService == "" {
		p.config.RequestCheckService = "llm_query_moderation"
	}
	if p.config.RequestCheckLengthLimit == 0 {
		p.config.RequestCheckLengthLimit = 2000
	}
	if p.config.ResponseCheckService == "" {
		p.config.ResponseCheckService = "llm_response_moderation"
	}
	if p.config.ResponseCheckLengthLimit == 0 {
		p.config.ResponseCheckLengthLimit = 5000
	}
	if p.config.RiskLevelBar == "" {
		p.config.RiskLevelBar = "high"
	}
	if p.config.DenyCode == 0 {
		p.config.DenyCode = http.StatusOK
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 10000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 30
	}
	if p.config.Keepalive == nil {
		keepalive := true
		p.config.Keepalive = &keepalive
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}
	if p.failMode == ai_common.SafetyFailError && p.config.CheckResponse && p.config.StreamCheckMode == "realtime" {
		return errors.New(
			"ai-aliyun-content-moderation: fail_mode=error is incompatible with realtime response moderation",
		)
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.nonce == nil {
		p.nonce = randomNonce
	}
	if p.streamNow == nil {
		p.streamNow = time.Now
	}

	p.client = &http.Client{
		Timeout:   time.Duration(p.config.Timeout) * time.Millisecond,
		Transport: p.transport(),
	}
	return nil
}

func (p *Plugin) MaterializeSecrets() error {
	if p.accessKeyID != nil && p.accessKeySecret != nil {
		return nil
	}
	accessKeyID, err := store.MaterializeSecret(p.config.AccessKeyID)
	if err != nil {
		return err
	}
	accessKeySecret, err := store.MaterializeSecret(p.config.AccessKeySecret)
	if err != nil {
		accessKeyID.Destroy()
		return err
	}
	p.accessKeyID = accessKeyID
	p.accessKeySecret = accessKeySecret
	p.config.AccessKeyID = accessKeyID.Descriptor()
	p.config.AccessKeySecret = accessKeySecret.Descriptor()
	return nil
}

func (p *Plugin) Stop() {
	p.accessKeyID.Destroy()
	p.accessKeySecret.Destroy()
}

// RunRequestPhase performs request-side moderation and publishes the parsed
// protocol/body for the explicit bounded and streaming response owners. It
// never invokes the downstream handler.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	checkRequest := p.config.CheckRequest == nil || *p.config.CheckRequest
	if !checkRequest && !p.config.CheckResponse {
		return base.ContinueRequest(r)
	}
	r = r.WithContext(context.WithValue(r.Context(), moderationSessionKey{}, p.nonce()))
	body, err := base.ReadRequestBody(r)
	if err != nil {
		if p.handleRequestFailure(w, r, ai_common.SafetyInvalidPayload, requestReadFailureMessage) {
			return base.ContinueRequest(r)
		}
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	if checkRequest {
		if err := validateJSONContentType(r); err != nil {
			if p.handleRequestFailure(w, r, ai_common.SafetyInvalidPayload, requestContentTypeMessage) {
				return base.ContinueRequest(r)
			}
			return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
		}
	}
	bodyTab, protocol, content, err := extractRequestContent(r.URL.Path, body)
	if err != nil && checkRequest {
		class := requestFailureClass(err)
		message := requestInvalidPayloadMessage
		if class == ai_common.SafetyUnknownProtocol {
			message = requestUnknownProtocolMessage
		}
		if p.handleRequestFailure(w, r, class, message) {
			return base.ContinueRequest(r)
		}
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	if err != nil {
		return base.ContinueRequest(r)
	}
	if checkRequest {
		if strings.TrimSpace(content) == "" {
			if !p.handleRequestFailure(w, r, ai_common.SafetyEmptyContent, requestEmptyContentMessage) {
				return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
			}
		} else {
			result, moderationErr := p.moderateContent(
				r, content, p.config.RequestCheckLengthLimit, p.config.RequestCheckService,
			)
			if moderationErr != nil {
				if !p.handleRequestFailure(w, r, moderationFailureClass(moderationErr), moderationUnavailableMessage) {
					return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
				}
			} else if result.Denied {
				recordOutcome(
					ai_common.SafetyPhaseRequest,
					ai_common.SafetyOutcomeDeny,
					string(ai_common.SafetyReasonRiskThreshold),
				)
				writeProtocolDeny(w, p.config.DenyCode, protocol, bodyTab, result.Message)
				return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
			} else {
				recordOutcome(
					ai_common.SafetyPhaseRequest,
					ai_common.SafetyOutcomeAllow,
					string(ai_common.SafetyReasonClean),
				)
			}
		}
	}
	state := &moderationRequestState{bodyTab: bodyTab, protocol: protocol}
	r = r.WithContext(context.WithValue(r.Context(), moderationRequestStateKey{}, state))
	return base.ContinueRequest(r)
}

func moderationStateFromRequest(r *http.Request) *moderationRequestState {
	if r == nil {
		return nil
	}
	state, _ := r.Context().Value(moderationRequestStateKey{}).(*moderationRequestState)
	return state
}

// SelectResponseMode chooses realtime streaming moderation only when request
// preparation identified a streaming protocol and the configured policy owns
// incremental checks. All other responses use the bounded canonical state.
func (p *Plugin) SelectResponseMode(r *http.Request) base.RequestResponseMode {
	state := moderationStateFromRequest(r)
	if p.config.CheckResponse && p.config.StreamCheckMode == "realtime" && state != nil &&
		ai_protocols.IsStreaming(state.protocol, state.bodyTab) {
		return base.RequestResponseModeStreaming
	}
	return base.RequestResponseModeBounded
}

// RunBufferedBodyFilter reuses the existing protocol-aware moderation logic
// against the canonical Plan 15 response state, keeping all writes local until
// the response executor commits the final representation.
func (p *Plugin) RunBufferedBodyFilter(r *http.Request, state *base.ResponseState) error {
	if state == nil || !p.config.CheckResponse {
		return nil
	}
	requestState := moderationStateFromRequest(r)
	if requestState == nil || ai_protocols.IsStreaming(requestState.protocol, requestState.bodyTab) {
		return nil
	}
	source := newCapturedResponse()
	source.status = state.Status
	source.header = state.Header.Clone()
	source.body.Write(state.Body)
	source.wroteHeader = true
	destination := newCapturedResponse()
	p.writeModeratedResponse(destination, r, source, requestState.protocol, requestState.bodyTab)
	state.Status = destination.status
	state.Header = destination.header
	state.Body = append(state.Body[:0], destination.body.Bytes()...)
	return nil
}

// WrapStreamingResponse owns realtime moderation state for one request. The
// writer's finalizer closes the UTF-8/SSE tail exactly once; final-packet mode
// remains a bounded response concern and therefore passes through here.
func (p *Plugin) WrapStreamingResponse(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, error) {
	if !p.config.CheckResponse || p.config.StreamCheckMode != "realtime" {
		return w, nil
	}
	state := moderationStateFromRequest(r)
	if state == nil || !ai_protocols.IsStreaming(state.protocol, state.bodyTab) {
		return w, nil
	}
	return newRealtimeResponseWriter(w, r, p, state.protocol, state.bodyTab), nil
}

// DescribeResponseMode includes bounded and realtime streaming moderation;
// request/config state selects the concrete path per request.
func (*Config) DescribeResponseMode() (base.ResponseModeDescriptor, error) {
	return base.ResponseModeDescriptor{Modes: base.ResponseModeBounded | base.ResponseModeStreaming}, nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		checkRequest := p.config.CheckRequest == nil || *p.config.CheckRequest
		if !checkRequest && !p.config.CheckResponse {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), moderationSessionKey{}, p.nonce()))

		body, err := base.ReadRequestBody(r)
		if err != nil {
			if p.handleRequestFailure(w, r, ai_common.SafetyInvalidPayload, requestReadFailureMessage) {
				next.ServeHTTP(w, r)
			}
			return
		}

		if checkRequest {
			if err := validateJSONContentType(r); err != nil {
				if p.handleRequestFailure(w, r, ai_common.SafetyInvalidPayload, requestContentTypeMessage) {
					next.ServeHTTP(w, r)
				}
				return
			}
		}

		bodyTab, protocol, content, err := extractRequestContent(r.URL.Path, body)
		if err != nil && checkRequest {
			class := requestFailureClass(err)
			message := requestInvalidPayloadMessage
			if class == ai_common.SafetyUnknownProtocol {
				message = requestUnknownProtocolMessage
			}
			if p.handleRequestFailure(w, r, class, message) {
				next.ServeHTTP(w, r)
			}
			return
		}
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		if checkRequest {
			if strings.TrimSpace(content) == "" {
				if !p.handleRequestFailure(w, r, ai_common.SafetyEmptyContent, requestEmptyContentMessage) {
					return
				}
			} else {
				result, err := p.moderateContent(
					r,
					content,
					p.config.RequestCheckLengthLimit,
					p.config.RequestCheckService,
				)
				if err != nil {
					if !p.handleRequestFailure(
						w,
						r,
						moderationFailureClass(err),
						moderationUnavailableMessage,
					) {
						return
					}
				} else if result.Denied {
					recordOutcome(
						ai_common.SafetyPhaseRequest,
						ai_common.SafetyOutcomeDeny,
						string(ai_common.SafetyReasonRiskThreshold),
					)
					writeProtocolDeny(w, p.config.DenyCode, protocol, bodyTab, result.Message)
					return
				} else {
					recordOutcome(
						ai_common.SafetyPhaseRequest,
						ai_common.SafetyOutcomeAllow,
						string(ai_common.SafetyReasonClean),
					)
				}
			}
		}

		if !p.config.CheckResponse {
			next.ServeHTTP(w, r)
			return
		}
		if ai_protocols.IsStreaming(protocol, bodyTab) && p.config.StreamCheckMode == "realtime" {
			streamWriter := newRealtimeResponseWriter(w, r, p, protocol, bodyTab)
			next.ServeHTTP(streamWriter, r)
			streamWriter.Close()
			return
		}

		response := newCapturedResponse()
		next.ServeHTTP(response, r)
		p.writeModeratedResponse(w, r, response, protocol, bodyTab)
	}
	return http.HandlerFunc(fn)
}

type requestContentError struct {
	class ai_common.SafetyFailureClass
	err   error
}

func (e *requestContentError) Error() string {
	if e == nil || e.err == nil {
		return "invalid AI request content"
	}
	return e.err.Error()
}

func (e *requestContentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func requestFailureClass(err error) ai_common.SafetyFailureClass {
	var contentErr *requestContentError
	if errors.As(err, &contentErr) && contentErr.class != "" {
		return contentErr.class
	}
	return ai_common.SafetyInvalidPayload
}

func moderationFailureClass(err error) ai_common.SafetyFailureClass {
	var moderationErr *moderationError
	if errors.As(err, &moderationErr) && moderationErr.Class != "" {
		return moderationErr.Class
	}
	return ai_common.SafetyBackendUnavailable
}

func (p *Plugin) handleRequestFailure(
	w http.ResponseWriter,
	r *http.Request,
	class ai_common.SafetyFailureClass,
	message string,
) bool {
	decision := ai_common.DecideSafetyFailure(p.failMode, class)
	recordOutcome(ai_common.SafetyPhaseRequest, decision.Outcome, string(class))
	if decision.Action == ai_common.SafetyContinue {
		ai_common.LogSafetyDegradation(r, name, p.failMode, ai_common.SafetyPhaseRequest, class)
		return true
	}
	base.WriteJSONMessage(w, decision.Status, message)
	return false
}

func recordOutcome(phase ai_common.SafetyPhase, outcome ai_common.SafetyOutcome, reason string) {
	metrics.RecordAISafetyOutcome(name, string(phase), string(outcome), reason)
}

func validateJSONContentType(r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return fmt.Errorf("unsupported content-type: %s, only application/json is supported", contentType)
	}
	return nil
}

type capturedResponse struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
}

func newCapturedResponse() *capturedResponse {
	return &capturedResponse{header: make(http.Header), status: http.StatusOK}
}

func (w *capturedResponse) Header() http.Header {
	return w.header
}

func (w *capturedResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *capturedResponse) Write(body []byte) (int, error) {
	w.wroteHeader = true
	return w.body.Write(body)
}

func (w *capturedResponse) Flush() {}

type realtimeResponseWriter struct {
	http.ResponseWriter
	request     *http.Request
	plugin      *Plugin
	protocol    ai_protocols.Protocol
	requestBody map[string]any

	status       int
	content      strings.Builder
	contentRunes int
	pending      string
	lastModerate time.Time
	blocked      bool
	closeOnce    sync.Once
}

func newRealtimeResponseWriter(
	w http.ResponseWriter,
	r *http.Request,
	p *Plugin,
	protocol ai_protocols.Protocol,
	requestBody map[string]any,
) *realtimeResponseWriter {
	return &realtimeResponseWriter{
		ResponseWriter: w,
		request:        r,
		plugin:         p,
		protocol:       protocol,
		requestBody:    requestBody,
		status:         http.StatusOK,
		lastModerate:   p.streamNow(),
	}
}

func (w *realtimeResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *realtimeResponseWriter) Write(body []byte) (int, error) {
	if w.blocked {
		return len(body), nil
	}
	if w.status >= http.StatusBadRequest {
		return w.ResponseWriter.Write(body)
	}

	extracted := w.extractContent(body)
	w.content.WriteString(extracted)
	w.contentRunes += utf8.RuneCountInString(extracted)
	finalPacket := isFinalSSEPacket(body)
	now := w.plugin.streamNow()
	cacheFull := w.contentRunes >= w.plugin.config.StreamCheckCacheSize
	intervalElapsed := now.Sub(w.lastModerate) >=
		time.Duration(w.plugin.config.StreamCheckInterval*float64(time.Second))
	if w.content.Len() > 0 && (cacheFull || intervalElapsed || finalPacket) {
		if w.moderate() {
			return len(body), nil
		}
		w.lastModerate = now
	}

	return w.ResponseWriter.Write(body)
}

func (w *realtimeResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *realtimeResponseWriter) Close() {
	w.closeOnce.Do(func() {
		if w.pending != "" {
			flushed := extractSSEText(w.protocol, []byte(w.pending+"\n"))
			w.content.WriteString(flushed)
			w.contentRunes += utf8.RuneCountInString(flushed)
			w.pending = ""
		}
		if !w.blocked && w.status < http.StatusBadRequest && w.content.Len() > 0 {
			w.moderate()
		}
		w.Flush()
	})
}

func (w *realtimeResponseWriter) FinishStreamingResponse(error) error {
	if w != nil {
		w.Close()
	}
	return nil
}

func (w *realtimeResponseWriter) extractContent(body []byte) string {
	combined := w.pending + string(body)
	lastNewline := strings.LastIndexByte(combined, '\n')
	if lastNewline < 0 {
		w.pending = combined
		return ""
	}
	w.pending = combined[lastNewline+1:]
	return extractSSEText(w.protocol, []byte(combined[:lastNewline+1]))
}

func (w *realtimeResponseWriter) moderate() bool {
	content := w.content.String()
	w.content.Reset()
	w.contentRunes = 0
	result, err := w.plugin.moderateContent(
		w.request,
		content,
		w.plugin.config.ResponseCheckLengthLimit,
		w.plugin.config.ResponseCheckService,
	)
	if err != nil {
		class := moderationFailureClass(err)
		recordOutcome(ai_common.SafetyPhaseResponse, ai_common.SafetyOutcomeDegraded, string(class))
		ai_common.LogSafetyDegradation(w.request, name, w.plugin.failMode, ai_common.SafetyPhaseResponse, class)
		return false
	}
	if !result.Denied {
		recordOutcome(ai_common.SafetyPhaseResponse, ai_common.SafetyOutcomeAllow, string(ai_common.SafetyReasonClean))
		return false
	}
	recordOutcome(
		ai_common.SafetyPhaseResponse,
		ai_common.SafetyOutcomeDeny,
		string(ai_common.SafetyReasonRiskThreshold),
	)

	model, _ := w.requestBody["model"].(string)
	encoded, _, err := ai_protocols.BuildDenyWireResponse(w.protocol, model, result.Message, true)
	if err != nil {
		return false
	}
	_, _ = w.ResponseWriter.Write(encoded)
	w.blocked = true
	return true
}

func isFinalSSEPacket(body []byte) bool {
	text := string(body)
	return strings.Contains(text, "data: [DONE]") || strings.Contains(text, "response.completed") ||
		strings.Contains(text, "message_stop")
}

func (p *Plugin) writeModeratedResponse(
	w http.ResponseWriter,
	r *http.Request,
	response *capturedResponse,
	protocol ai_protocols.Protocol,
	requestBody map[string]any,
) {
	if response.status >= http.StatusBadRequest {
		writeCapturedResponse(w, response, response.body.Bytes())
		return
	}

	if ai_protocols.IsStreaming(protocol, requestBody) {
		p.writeModeratedStream(w, r, response, protocol, requestBody)
		return
	}

	var body map[string]any
	if err := json.Unmarshal(response.body.Bytes(), &body); err != nil {
		p.handleBufferedResponseFailure(
			w,
			r,
			response,
			ai_common.SafetyUpstreamInvalidResponse,
			upstreamInvalidResponseMessage,
		)
		return
	}
	if !validResponseEnvelope(protocol, body) {
		p.handleBufferedResponseFailure(
			w,
			r,
			response,
			ai_common.SafetyUpstreamInvalidResponse,
			upstreamInvalidResponseMessage,
		)
		return
	}
	content := ai_protocols.ExtractResponseText(protocol, body)
	if strings.TrimSpace(content) == "" {
		recordOutcome(ai_common.SafetyPhaseResponse, ai_common.SafetyOutcomeAllow, string(ai_common.SafetyReasonClean))
		writeCapturedResponse(w, response, response.body.Bytes())
		return
	}
	result, err := p.moderateContent(
		r,
		content,
		p.config.ResponseCheckLengthLimit,
		p.config.ResponseCheckService,
	)
	if err != nil {
		p.handleBufferedResponseFailure(
			w,
			r,
			response,
			moderationFailureClass(err),
			moderationUnavailableMessage,
		)
		return
	}
	if !result.Denied {
		recordOutcome(ai_common.SafetyPhaseResponse, ai_common.SafetyOutcomeAllow, string(ai_common.SafetyReasonClean))
		writeCapturedResponse(w, response, response.body.Bytes())
		return
	}
	recordOutcome(
		ai_common.SafetyPhaseResponse,
		ai_common.SafetyOutcomeDeny,
		string(ai_common.SafetyReasonRiskThreshold),
	)

	copyResponseHeaders(w.Header(), response.header)
	w.Header().Del("Content-Length")
	writeProtocolDeny(w, p.config.DenyCode, protocol, requestBody, result.Message)
}

func (p *Plugin) writeModeratedStream(
	w http.ResponseWriter,
	r *http.Request,
	response *capturedResponse,
	protocol ai_protocols.Protocol,
	requestBody map[string]any,
) {
	content := extractSSEText(protocol, response.body.Bytes())
	if !validStreamingEnvelope(protocol, response.body.Bytes()) {
		p.handleBufferedResponseFailure(
			w,
			r,
			response,
			ai_common.SafetyUpstreamInvalidResponse,
			upstreamInvalidResponseMessage,
		)
		return
	}
	if strings.TrimSpace(content) == "" {
		recordOutcome(ai_common.SafetyPhaseResponse, ai_common.SafetyOutcomeAllow, string(ai_common.SafetyReasonClean))
		writeCapturedResponse(w, response, response.body.Bytes())
		return
	}
	result, err := p.moderateContent(
		r,
		content,
		p.config.ResponseCheckLengthLimit,
		p.config.ResponseCheckService,
	)
	if err != nil {
		p.handleBufferedResponseFailure(
			w,
			r,
			response,
			moderationFailureClass(err),
			moderationUnavailableMessage,
		)
		return
	}
	if result.Denied {
		recordOutcome(
			ai_common.SafetyPhaseResponse,
			ai_common.SafetyOutcomeDeny,
			string(ai_common.SafetyReasonRiskThreshold),
		)
		copyResponseHeaders(w.Header(), response.header)
		w.Header().Del("Content-Length")
		writeProtocolDeny(w, p.config.DenyCode, protocol, requestBody, result.Message)
		return
	}
	recordOutcome(ai_common.SafetyPhaseResponse, ai_common.SafetyOutcomeAllow, string(ai_common.SafetyReasonClean))
	body := response.body.Bytes()
	if p.config.StreamCheckMode == "final_packet" && result.RiskLevel != "" {
		body = addRiskLevelToFinalSSEPacket(body, result.RiskLevel)
	}
	writeCapturedResponse(w, response, body)
}

func (p *Plugin) handleBufferedResponseFailure(
	w http.ResponseWriter,
	r *http.Request,
	response *capturedResponse,
	class ai_common.SafetyFailureClass,
	message string,
) {
	decision := ai_common.DecideSafetyFailure(p.failMode, class)
	recordOutcome(ai_common.SafetyPhaseResponse, decision.Outcome, string(class))
	if decision.Action == ai_common.SafetyContinue {
		ai_common.LogSafetyDegradation(r, name, p.failMode, ai_common.SafetyPhaseResponse, class)
		writeCapturedResponse(w, response, response.body.Bytes())
		return
	}
	base.WriteJSONMessage(w, decision.Status, message)
}

func validResponseEnvelope(protocol ai_protocols.Protocol, body map[string]any) bool {
	if body == nil {
		return false
	}
	switch protocol {
	case ai_protocols.OpenAIChat:
		choices, ok := body["choices"].([]any)
		return ok && validChatChoices(choices)
	case ai_protocols.OpenAIResponses:
		output, ok := body["output"].([]any)
		return ok && validObjectArray(output)
	case ai_protocols.OpenAIEmbeddings:
		data, ok := body["data"].([]any)
		return ok && validObjectArray(data)
	case ai_protocols.AnthropicMessages:
		content, ok := body["content"].([]any)
		return ok && validObjectArray(content)
	case ai_protocols.BedrockConverse:
		output, ok := body["output"].(map[string]any)
		if !ok {
			return false
		}
		message, ok := output["message"].(map[string]any)
		if !ok {
			return false
		}
		content, ok := message["content"].([]any)
		return ok && validObjectArray(content)
	case ai_protocols.Passthrough:
		return true
	default:
		return false
	}
}

func validChatChoices(choices []any) bool {
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			return false
		}
		message, ok := choice["message"].(map[string]any)
		if !ok {
			return false
		}
		content, hasContent := message["content"]
		if hasContent && content != nil {
			switch value := content.(type) {
			case string:
			case []any:
				if !validObjectArray(value) {
					return false
				}
			default:
				return false
			}
		}
		if !hasContent || content == nil {
			if toolCalls, hasToolCalls := message["tool_calls"].([]any); hasToolCalls {
				if !validObjectArray(toolCalls) {
					return false
				}
				continue
			}
			if _, hasFunctionCall := message["function_call"].(map[string]any); hasFunctionCall {
				continue
			}
			if _, hasRefusal := message["refusal"].(string); !hasRefusal {
				return false
			}
		}
	}
	return true
}

func validObjectArray(values []any) bool {
	for _, value := range values {
		if _, ok := value.(map[string]any); !ok {
			return false
		}
	}
	return true
}

func validStreamingEnvelope(protocol ai_protocols.Protocol, body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	validData := false
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") ||
			strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			return false
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		validData = true
		if data == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil || event == nil {
			return false
		}
		if !validStreamingEvent(protocol, event) {
			return false
		}
	}
	return validData
}

func validStreamingEvent(protocol ai_protocols.Protocol, event map[string]any) bool {
	switch protocol {
	case ai_protocols.OpenAIChat:
		choices, ok := event["choices"].([]any)
		if !ok {
			return false
		}
		for _, rawChoice := range choices {
			choice, ok := rawChoice.(map[string]any)
			if !ok {
				return false
			}
			if _, ok := choice["delta"].(map[string]any); !ok {
				return false
			}
		}
		return true
	case ai_protocols.OpenAIResponses, ai_protocols.AnthropicMessages:
		eventType, ok := event["type"].(string)
		return ok && eventType != ""
	default:
		return false
	}
}

func extractSSEText(protocol ai_protocols.Protocol, body []byte) string {
	parts := make([]string, 0)
	for line := range strings.SplitSeq(string(body), "\n") {
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == line || data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if content := ai_protocols.ExtractStreamEventText(protocol, event); content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "")
}

func addRiskLevelToFinalSSEPacket(body []byte, riskLevel string) []byte {
	lines := strings.Split(string(body), "\n")
	for i := range slices.Backward(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		event["risk_level"] = riskLevel
		encoded, err := json.Marshal(event)
		if err != nil {
			return body
		}
		lines[i] = "data: " + string(encoded)
		return []byte(strings.Join(lines, "\n"))
	}
	return body
}

func writeCapturedResponse(w http.ResponseWriter, response *capturedResponse, body []byte) {
	copyResponseHeaders(w.Header(), response.header)
	if len(body) != response.body.Len() {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(response.status)
	_, _ = w.Write(body)
}

func copyResponseHeaders(destination, source http.Header) {
	for field, values := range source {
		destination[field] = append([]string(nil), values...)
	}
}

func (p *Plugin) moderateContent(
	r *http.Request,
	content string,
	lengthLimit int,
	serviceName string,
) (moderationResult, error) {
	if strings.TrimSpace(content) == "" {
		return moderationResult{}, nil
	}
	runes := []rune(content)
	if lengthLimit <= 0 {
		lengthLimit = len(runes)
	}

	sessionID, _ := r.Context().Value(moderationSessionKey{}).(string)
	if sessionID == "" {
		sessionID = p.nonce()
	}
	lastRiskLevel := ""
	for start := 0; start < len(runes); start += lengthLimit {
		end := min(start+lengthLimit, len(runes))
		hit, message, riskLevel, err := p.checkSingleContent(r, sessionID, string(runes[start:end]), serviceName)
		if err != nil {
			return moderationResult{}, err
		}
		lastRiskLevel = riskLevel
		if riskLevel != "" && apisixctx.GetRequestVars(r) != nil {
			apisixctx.RegisterRequestVar(r, "$llm_content_risk_level", riskLevel)
		}
		if hit {
			if p.config.DenyMessage != "" {
				message = p.config.DenyMessage
			}
			if message == "" {
				message = "Your request violate our content policy."
			}
			return moderationResult{Denied: true, Message: message, RiskLevel: riskLevel}, nil
		}
	}

	return moderationResult{RiskLevel: lastRiskLevel}, nil
}

func (p *Plugin) checkSingleContent(
	r *http.Request,
	sessionID string,
	content string,
	serviceName string,
) (bool, string, string, error) {
	paramsBody, err := p.buildFormBody(sessionID, content, serviceName)
	if err != nil {
		return false, "", "", &moderationError{Class: ai_common.SafetyBackendUnavailable, Err: err}
	}

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		p.config.Endpoint,
		strings.NewReader(paramsBody),
	)
	if err != nil {
		return false, "", "", &moderationError{
			Class: ai_common.SafetyBackendUnavailable,
			Err:   errors.New("failed to create Aliyun moderation request"),
		}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return false, "", "", &moderationError{
			Class: ai_common.SafetyBackendUnavailable,
			Err:   errors.New("aliyun moderation transport unavailable"),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", "", &moderationError{
			Class: ai_common.SafetyBackendUnavailable,
			Err:   errors.New("failed to read Aliyun moderation response"),
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return false, "", "", &moderationError{
			Class: ai_common.SafetyBackendUnavailable,
			Err:   fmt.Errorf("aliyun moderation service returned status %d", resp.StatusCode),
		}
	}

	var response aliyunResponse
	if err := json.Unmarshal(rawBody, &response); err != nil {
		return false, "", "", &moderationError{
			Class: ai_common.SafetyBackendInvalidResponse,
			Err:   errors.New("malformed Aliyun moderation response"),
		}
	}
	if response.Data == nil || response.Data.RiskLevel == "" || riskLevelToInt(response.Data.RiskLevel) < 0 {
		return false, "", "", &moderationError{
			Class: ai_common.SafetyBackendInvalidResponse,
			Err:   errors.New("aliyun moderation response has no valid risk level"),
		}
	}
	if riskLevelToInt(response.Data.RiskLevel) < riskLevelToInt(p.config.RiskLevelBar) {
		return false, "", response.Data.RiskLevel, nil
	}

	if len(response.Data.Advice) > 0 {
		return true, response.Data.Advice[0].Answer, response.Data.RiskLevel, nil
	}
	return true, "", response.Data.RiskLevel, nil
}

func (p *Plugin) buildFormBody(sessionID string, content string, serviceName string) (string, error) {
	accessKeyID := p.accessKeyID.Bytes()
	accessKeySecret := p.accessKeySecret.Bytes()
	defer clear(accessKeyID)
	defer clear(accessKeySecret)
	if len(accessKeyID) == 0 || len(accessKeySecret) == 0 {
		return "", errors.New("aliyun moderation credentials are unavailable")
	}
	serviceParameters, err := json.Marshal(serviceParameters{SessionID: sessionID, Content: content})
	if err != nil {
		return "", fmt.Errorf("failed to encode service parameters: %w", err)
	}

	params := map[string]string{
		"AccessKeyId":       string(accessKeyID),
		"Action":            "TextModerationPlus",
		"Format":            "JSON",
		"RegionId":          p.config.RegionID,
		"Service":           serviceName,
		"ServiceParameters": string(serviceParameters),
		"SignatureMethod":   "HMAC-SHA1",
		"SignatureNonce":    p.nonce(),
		"SignatureVersion":  "1.0",
		"Timestamp":         p.now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":           "2022-03-02",
	}
	params["Signature"] = aliyunSignature(params, string(accessKeySecret)+"&")

	keys := sortedKeys(params)
	values := make(url.Values, len(params))
	for _, key := range keys {
		values.Set(key, params[key])
	}
	return values.Encode(), nil
}

func aliyunSignature(params map[string]string, secret string) string {
	keys := sortedKeys(params)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, aliyunEscape(key)+"="+aliyunEscape(params[key]))
	}
	canonical := strings.Join(pairs, "&")
	stringToSign := "POST&%2F&" + aliyunEscape(canonical)
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func aliyunEscape(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	escaped = strings.ReplaceAll(escaped, "*", "%2A")
	escaped = strings.ReplaceAll(escaped, "%7E", "~")
	return escaped
}

func extractRequestContent(
	requestPath string,
	body []byte,
) (map[string]any, ai_protocols.Protocol, string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, ai_protocols.Protocol{}, "", &requestContentError{
			class: ai_common.SafetyInvalidPayload,
			err:   errors.New("missing request body"),
		}
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, ai_protocols.Protocol{}, "", &requestContentError{
			class: ai_common.SafetyInvalidPayload,
			err:   errors.New("could not parse JSON request body"),
		}
	}
	protocol, err := ai_protocols.Detect(requestPath, data)
	if err != nil {
		return nil, ai_protocols.Protocol{}, "", &requestContentError{
			class: ai_common.SafetyUnknownProtocol,
			err:   errors.New("request protocol not recognized"),
		}
	}
	if protocol == ai_protocols.Passthrough {
		return nil, ai_protocols.Protocol{}, "", &requestContentError{
			class: ai_common.SafetyUnknownProtocol,
			err:   errors.New("request protocol passthrough is not inspectable"),
		}
	}
	return data, protocol, strings.Join(ai_protocols.ExtractRequestContent(protocol, data), " "), nil
}

func writeProtocolDeny(
	w http.ResponseWriter,
	status int,
	protocol ai_protocols.Protocol,
	body map[string]any,
	message string,
) {
	model, _ := body["model"].(string)
	encoded, contentType, err := ai_protocols.BuildDenyWireResponse(
		protocol,
		model,
		message,
		ai_protocols.IsStreaming(protocol, body),
	)
	if err != nil {
		base.WriteJSONMessage(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func riskLevelToInt(riskLevel string) int {
	switch riskLevel {
	case "max":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	case "none":
		return 0
	default:
		return -1
	}
}

func randomNonce() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = p.config.KeepalivePool
	transport.IdleConnTimeout = time.Duration(p.config.KeepaliveTimeout) * time.Millisecond
	if p.config.Keepalive != nil && !*p.config.Keepalive {
		transport.DisableKeepAlives = true
	}
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return transport
}
