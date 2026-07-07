package ai_aliyun_content_moderation

import (
	"bytes"
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

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client
	now    func() time.Time
	nonce  func() string
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

	p.client = &http.Client{
		Timeout:   time.Duration(p.config.Timeout) * time.Millisecond,
		Transport: p.transport(),
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
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

		if p.config.CheckRequest != nil && !*p.config.CheckRequest {
			next.ServeHTTP(w, r)
			return
		}
		if err := validateJSONContentType(r); err != nil {
			writeJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}

		content, err := extractRequestContent(body)
		if err != nil {
			writeJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}
		code, message := p.moderateContent(r, content, p.config.RequestCheckLengthLimit, p.config.RequestCheckService)
		if code != 0 {
			writeJSONMessage(w, code, message)
			return
		}

		next.ServeHTTP(w, r)
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

func (p *Plugin) moderateContent(r *http.Request, content string, lengthLimit int, serviceName string) (int, string) {
	if strings.TrimSpace(content) == "" {
		return 0, ""
	}
	if lengthLimit <= 0 {
		lengthLimit = len(content)
	}

	sessionID := p.nonce()
	for start := 0; start < len(content); start += lengthLimit {
		end := start + lengthLimit
		if end > len(content) {
			end = len(content)
		}
		hit, message, err := p.checkSingleContent(r, sessionID, content[start:end], serviceName)
		if err != nil {
			return 0, ""
		}
		if hit {
			if p.config.DenyMessage != "" {
				message = p.config.DenyMessage
			}
			if message == "" {
				message = "Your request violate our content policy."
			}
			return p.config.DenyCode, message
		}
	}

	return 0, ""
}

func (p *Plugin) checkSingleContent(
	r *http.Request,
	sessionID string,
	content string,
	serviceName string,
) (bool, string, error) {
	paramsBody, err := p.buildFormBody(sessionID, content, serviceName)
	if err != nil {
		return false, "", err
	}

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		p.config.Endpoint,
		strings.NewReader(paramsBody),
	)
	if err != nil {
		return false, "", fmt.Errorf("failed to create Aliyun moderation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("failed to request: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf(
			"failed to request aliyun text moderation service, status: %d, body: %s",
			resp.StatusCode,
			rawBody,
		)
	}

	var response aliyunResponse
	if err := json.Unmarshal(rawBody, &response); err != nil {
		return false, "", fmt.Errorf("failed to decode response: %w", err)
	}
	if response.Data == nil || response.Data.RiskLevel == "" {
		return false, "", fmt.Errorf("failed to get risk level: %s", rawBody)
	}
	if riskLevelToInt(response.Data.RiskLevel) < riskLevelToInt(p.config.RiskLevelBar) {
		return false, "", nil
	}

	if len(response.Data.Advice) > 0 {
		return true, response.Data.Advice[0].Answer, nil
	}
	return true, "", nil
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

func extractRequestContent(body []byte) (string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return "", fmt.Errorf("missing request body")
	}

	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("could not parse JSON request body: %w", err)
	}

	contents := make([]string, 0)
	collectAIContent(data, &contents)
	if len(contents) == 0 {
		return string(body), nil
	}
	return strings.Join(contents, " "), nil
}

func collectAIContent(value any, contents *[]string) {
	switch v := value.(type) {
	case map[string]any:
		if messages, ok := v["messages"].([]any); ok {
			for _, message := range messages {
				messageMap, ok := message.(map[string]any)
				if !ok {
					continue
				}
				appendContent(messageMap["content"], contents)
			}
		}
		appendContent(v["instructions"], contents)
		appendContent(v["input"], contents)
		appendContent(v["prompt"], contents)
	}
}

func appendContent(value any, contents *[]string) {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			*contents = append(*contents, v)
		}
	case []any:
		for _, item := range v {
			appendContent(item, contents)
		}
	case map[string]any:
		appendContent(v["text"], contents)
	}
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
