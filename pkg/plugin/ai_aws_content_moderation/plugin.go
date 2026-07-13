package ai_aws_content_moderation

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
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
}

const (
	priority = 1050
	name     = "ai-aws-content-moderation"
)

const schema = `
{
  "type": "object",
  "properties": {
    "comprehend": {
      "type": "object",
      "properties": {
        "access_key_id": {
          "type": "string"
        },
        "secret_access_key": {
          "type": "string"
        },
		"session_token": {
		  "type": "string"
		},
        "region": {
          "type": "string"
        },
        "endpoint": {
          "type": "string",
          "pattern": "^https?://"
        },
        "ssl_verify": {
          "type": "boolean",
          "default": true
        }
      },
      "required": ["access_key_id", "secret_access_key", "region"]
    },
    "moderation_categories": {
      "type": "object",
      "patternProperties": {
        "^(PROFANITY|HATE_SPEECH|INSULT|HARASSMENT_OR_ABUSE|SEXUAL|VIOLENCE_OR_THREAT)$": {
          "type": "number",
          "minimum": 0,
          "maximum": 1
        }
      },
      "additionalProperties": false
    },
    "moderation_threshold": {
      "type": "number",
      "minimum": 0,
      "maximum": 1,
      "default": 0.5
    }
  },
  "required": ["comprehend"]
}
`

type Config struct {
	Comprehend           Comprehend         `json:"comprehend"`
	ModerationCategories map[string]float64 `json:"moderation_categories,omitempty"`
	ModerationThreshold  *float64           `json:"moderation_threshold,omitempty"`
}

type Comprehend struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint,omitempty"`
	SSLVerify       *bool  `json:"ssl_verify,omitempty"`
}

type comprehendRequest struct {
	LanguageCode string        `json:"LanguageCode"`
	TextSegments []textSegment `json:"TextSegments"`
}

type textSegment struct {
	Text string `json:"Text"`
}

type comprehendResponse struct {
	ResultList []moderationResult `json:"ResultList"`
}

type moderationResult struct {
	Toxicity float64 `json:"Toxicity"`
	Labels   []label `json:"Labels"`
}

type label struct {
	Name  string  `json:"Name"`
	Score float64 `json:"Score"`
}

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
	if p.config.Comprehend.SSLVerify == nil {
		sslVerify := true
		p.config.Comprehend.SSLVerify = &sslVerify
	}
	if p.config.ModerationThreshold == nil {
		threshold := 0.5
		p.config.ModerationThreshold = &threshold
	}
	if p.now == nil {
		p.now = time.Now
	}
	p.client = &http.Client{
		Timeout:   30 * time.Second,
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

		if len(body) == 0 {
			writeJSONMessage(w, http.StatusBadRequest, "missing request body")
			return
		}

		code, message := p.checkContent(r, string(body))
		if code != 0 {
			writeJSONMessage(w, code, message)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) checkContent(r *http.Request, body string) (int, string) {
	result, err := p.detectToxicContent(r, body)
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	if len(result.ResultList) == 0 {
		return http.StatusInternalServerError, "failed to get moderation results from response"
	}

	for _, item := range result.ResultList {
		for _, label := range item.Labels {
			threshold, ok := p.config.ModerationCategories[label.Name]
			if ok && label.Score > threshold {
				return http.StatusBadRequest, "request body exceeds " + label.Name + " threshold"
			}
		}
		if item.Toxicity > *p.config.ModerationThreshold {
			return http.StatusBadRequest, "request body exceeds toxicity threshold"
		}
	}

	return 0, ""
}

func (p *Plugin) detectToxicContent(r *http.Request, body string) (comprehendResponse, error) {
	var result comprehendResponse
	payload, err := json.Marshal(comprehendRequest{
		LanguageCode: "en",
		TextSegments: []textSegment{{Text: body}},
	})
	if err != nil {
		return result, fmt.Errorf("failed to encode moderation request body: %w", err)
	}

	endpoint := p.endpoint()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return result, fmt.Errorf("failed to create moderation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Comprehend_20171127.DetectToxicContent")
	p.sign(req, payload)

	resp, err := p.client.Do(req)
	if err != nil {
		return result, fmt.Errorf("failed to send request to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return result, fmt.Errorf("failed to read moderation response body: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return result, fmt.Errorf(
			"failed to request aws comprehend service, status: %d, body: %s",
			resp.StatusCode,
			respBody,
		)
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return result, fmt.Errorf("failed to decode moderation response: %w", err)
	}
	return result, nil
}

func (p *Plugin) endpoint() string {
	if p.config.Comprehend.Endpoint != "" {
		return p.config.Comprehend.Endpoint
	}
	return "https://comprehend." + p.config.Comprehend.Region + ".amazonaws.com"
}

func (p *Plugin) sign(req *http.Request, payload []byte) {
	t := p.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if p.config.Comprehend.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", p.config.Comprehend.SessionToken)
	}

	payloadHash := hashHex(payload)
	canonicalHeaders, signedHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{date, p.config.Comprehend.Region, "comprehend", "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(
		hmacSHA256(
			signingKey(p.config.Comprehend.SecretAccessKey, date, p.config.Comprehend.Region),
			[]byte(stringToSign),
		),
	)
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential="+p.config.Comprehend.AccessKeyID+"/"+scope+
			", SignedHeaders="+signedHeaders+", Signature="+signature,
	)
}

func canonicalHeaders(req *http.Request) (string, string) {
	names := []string{"content-type", "host", "x-amz-date", "x-amz-target"}
	values := map[string]string{
		"content-type": req.Header.Get("Content-Type"),
		"host":         req.URL.Host,
		"x-amz-date":   req.Header.Get("X-Amz-Date"),
		"x-amz-target": req.Header.Get("X-Amz-Target"),
	}
	if token := req.Header.Get("X-Amz-Security-Token"); token != "" {
		names = append(names, "x-amz-security-token")
		values["x-amz-security-token"] = token
	}
	sort.Strings(names)

	var headers strings.Builder
	for _, name := range names {
		headers.WriteString(name)
		headers.WriteByte(':')
		headers.WriteString(strings.TrimSpace(values[name]))
		headers.WriteByte('\n')
	}
	return headers.String(), strings.Join(names, ";")
}

func canonicalURI(u *url.URL) string {
	if u.EscapedPath() == "" {
		return "/"
	}
	return u.EscapedPath()
}

func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return u.RawQuery
	}
	return values.Encode()
}

func signingKey(secret string, date string, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("comprehend"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key []byte, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.config.Comprehend.SSLVerify != nil && !*p.config.Comprehend.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return transport
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
