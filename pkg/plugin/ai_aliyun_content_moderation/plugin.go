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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client
	now    func() time.Time
	nonce  func() string

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
      "type": "number",
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
}

type serviceParameters struct {
	SessionID string `json:"sessionId"`
	Content   string `json:"content"`
}

type moderationSessionKey struct{}

type aliyunResponse struct {
	Data *struct {
		RiskLevel string `json:"RiskLevel"`
		Advice    []struct {
			Answer string `json:"Answer"`
		} `json:"Advice"`
	} `json:"Data"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
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

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		checkRequest := p.config.CheckRequest == nil || *p.config.CheckRequest
		if !checkRequest && !p.config.CheckResponse {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), moderationSessionKey{}, p.nonce()))

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := r.Body.Close(); err != nil {
			writeJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}

		if checkRequest {
			if err := validateJSONContentType(r); err != nil {
				writeJSONMessage(w, http.StatusBadRequest, err.Error())
				return
			}
		}

		bodyTab, protocol, content, err := extractRequestContent(r.URL.Path, body)
		if err != nil && checkRequest {
			writeJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		if checkRequest {
			code, message, _ := p.moderateContent(
				r,
				content,
				p.config.RequestCheckLengthLimit,
				p.config.RequestCheckService,
			)
			if code != 0 {
				writeProtocolDeny(w, code, protocol, bodyTab, message)
				return
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
	pending      string
	lastModerate time.Time
	blocked      bool
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

	w.content.WriteString(w.extractContent(body))
	finalPacket := isFinalSSEPacket(body)
	now := w.plugin.streamNow()
	cacheFull := len([]rune(w.content.String())) >= w.plugin.config.StreamCheckCacheSize
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
	if w.pending != "" {
		w.content.WriteString(extractSSEText(w.protocol, []byte(w.pending+"\n")))
		w.pending = ""
	}
	if !w.blocked && w.status < http.StatusBadRequest && w.content.Len() > 0 {
		w.moderate()
	}
	w.Flush()
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
	code, message, _ := w.plugin.moderateContent(
		w.request,
		content,
		w.plugin.config.ResponseCheckLengthLimit,
		w.plugin.config.ResponseCheckService,
	)
	if code == 0 {
		return false
	}

	model, _ := w.requestBody["model"].(string)
	encoded, _, err := ai_protocols.BuildDenyWireResponse(w.protocol, model, message, true)
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
		writeCapturedResponse(w, response, response.body.Bytes())
		return
	}
	content := ai_protocols.ExtractResponseText(protocol, body)
	code, message, _ := p.moderateContent(
		r,
		content,
		p.config.ResponseCheckLengthLimit,
		p.config.ResponseCheckService,
	)
	if code == 0 {
		writeCapturedResponse(w, response, response.body.Bytes())
		return
	}

	copyResponseHeaders(w.Header(), response.header)
	w.Header().Del("Content-Length")
	writeProtocolDeny(w, code, protocol, requestBody, message)
}

func (p *Plugin) writeModeratedStream(
	w http.ResponseWriter,
	r *http.Request,
	response *capturedResponse,
	protocol ai_protocols.Protocol,
	requestBody map[string]any,
) {
	content := extractSSEText(protocol, response.body.Bytes())
	_, _, riskLevel := p.moderateContent(
		r,
		content,
		p.config.ResponseCheckLengthLimit,
		p.config.ResponseCheckService,
	)
	body := response.body.Bytes()
	if p.config.StreamCheckMode == "final_packet" && riskLevel != "" {
		body = addRiskLevelToFinalSSEPacket(body, riskLevel)
	}
	writeCapturedResponse(w, response, body)
}

func extractSSEText(protocol ai_protocols.Protocol, body []byte) string {
	parts := make([]string, 0)
	for _, line := range strings.Split(string(body), "\n") {
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == line || data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if content := extractSSEEventText(protocol, event); content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "")
}

func extractSSEEventText(protocol ai_protocols.Protocol, event map[string]any) string {
	switch protocol {
	case ai_protocols.OpenAIResponses:
		text, _ := event["delta"].(string)
		return text
	case ai_protocols.AnthropicMessages:
		delta, _ := event["delta"].(map[string]any)
		text, _ := delta["text"].(string)
		return text
	default:
		choices, _ := event["choices"].([]any)
		if len(choices) == 0 {
			return ""
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		text, _ := delta["content"].(string)
		return text
	}
}

func addRiskLevelToFinalSSEPacket(body []byte, riskLevel string) []byte {
	lines := strings.Split(string(body), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
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
) (int, string, string) {
	if strings.TrimSpace(content) == "" {
		return 0, "", ""
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
		end := start + lengthLimit
		if end > len(runes) {
			end = len(runes)
		}
		hit, message, riskLevel, err := p.checkSingleContent(r, sessionID, string(runes[start:end]), serviceName)
		if err != nil {
			return 0, "", ""
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
			return p.config.DenyCode, message, riskLevel
		}
	}

	return 0, "", lastRiskLevel
}

func (p *Plugin) checkSingleContent(
	r *http.Request,
	sessionID string,
	content string,
	serviceName string,
) (bool, string, string, error) {
	paramsBody, err := p.buildFormBody(sessionID, content, serviceName)
	if err != nil {
		return false, "", "", err
	}

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		p.config.Endpoint,
		strings.NewReader(paramsBody),
	)
	if err != nil {
		return false, "", "", fmt.Errorf("failed to create Aliyun moderation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return false, "", "", fmt.Errorf("failed to request: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", "", fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, "", "", fmt.Errorf(
			"failed to request aliyun text moderation service, status: %d, body: %s",
			resp.StatusCode,
			rawBody,
		)
	}

	var response aliyunResponse
	if err := json.Unmarshal(rawBody, &response); err != nil {
		return false, "", "", fmt.Errorf("failed to decode response: %w", err)
	}
	if response.Data == nil || response.Data.RiskLevel == "" {
		return false, "", "", fmt.Errorf("failed to get risk level: %s", rawBody)
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
	serviceParameters, err := json.Marshal(serviceParameters{SessionID: sessionID, Content: content})
	if err != nil {
		return "", fmt.Errorf("failed to encode service parameters: %w", err)
	}

	params := map[string]string{
		"AccessKeyId":       p.config.AccessKeyID,
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
	params["Signature"] = aliyunSignature(params, p.config.AccessKeySecret+"&")

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
		return nil, ai_protocols.Protocol{}, "", fmt.Errorf("missing request body")
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, ai_protocols.Protocol{}, "", fmt.Errorf("could not parse JSON request body: %w", err)
	}
	protocol, err := ai_protocols.Detect(requestPath, data)
	if err != nil {
		return nil, ai_protocols.Protocol{}, "", err
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
		writeJSONMessage(w, http.StatusInternalServerError, err.Error())
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

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
