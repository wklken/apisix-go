package ai_aws_content_moderation

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client
	now    func() time.Time
	awsCredentialState
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
    },
    "deny_code": {
      "type": "integer",
      "minimum": 200,
      "maximum": 599,
      "default": 200
    },
    "fail_mode": {
      "type": "string",
      "enum": ["skip", "warn", "error"],
      "default": "skip"
    }
  },
  "required": ["comprehend"]
}
`

type Config struct {
	Comprehend           Comprehend         `json:"comprehend"`
	ModerationCategories map[string]float64 `json:"moderation_categories,omitempty"`
	ModerationThreshold  *float64           `json:"moderation_threshold,omitempty"`
	DenyCode             *int               `json:"deny_code,omitempty"`
	FailMode             string             `json:"fail_mode,omitempty"`
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
	if p.config.FailMode == "" {
		p.config.FailMode = "skip"
	}
	if p.config.FailMode != "skip" && p.config.FailMode != "warn" && p.config.FailMode != "error" {
		return fmt.Errorf("invalid fail_mode: %s", p.config.FailMode)
	}
	if p.config.Comprehend.SSLVerify == nil {
		sslVerify := true
		p.config.Comprehend.SSLVerify = &sslVerify
	}
	if p.config.ModerationThreshold == nil {
		threshold := 0.5
		p.config.ModerationThreshold = &threshold
	}
	if p.config.DenyCode == nil {
		denyCode := http.StatusOK
		p.config.DenyCode = &denyCode
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
		if _, ok := ai_runtime.SelectedInstanceName(r); !ok {
			p.handleMissingAIInstance(w, r, next)
			return
		}

		body, err := base.ReadRequestBody(r)
		if err != nil {
			base.WriteJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(body) == 0 {
			base.WriteJSONMessage(w, http.StatusBadRequest, "missing request body")
			return
		}

		var requestBody map[string]any
		if err := json.Unmarshal(body, &requestBody); err != nil {
			base.WriteJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}
		protocol, err := ai_protocols.Detect(r.URL.Path, requestBody)
		if err != nil || protocol == ai_protocols.Passthrough {
			base.WriteJSONMessage(w, http.StatusBadRequest, "unsupported AI request protocol")
			return
		}
		content := strings.Join(ai_protocols.ExtractRequestContent(protocol, requestBody), " ")
		if content == "" {
			next.ServeHTTP(w, r)
			return
		}

		reason, err := p.checkContent(r, content)
		if err != nil {
			logger.Error(err.Error())
			writeText(w, http.StatusInternalServerError, err.Error())
			return
		}
		if reason != "" {
			p.writeDenyResponse(w, protocol, requestBody, reason)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) handleMissingAIInstance(w http.ResponseWriter, r *http.Request, next http.Handler) {
	const reason = "no ai instance picked"
	if p.config.FailMode == "error" {
		writeText(
			w,
			http.StatusInternalServerError,
			reason+", ai-aws-content-moderation plugin must be used with "+
				"ai-proxy or ai-proxy-multi plugin",
		)
		return
	}
	message := name + " skipped: " + reason
	if p.config.FailMode == "warn" {
		logger.Warn(message)
	} else {
		logger.Info(message)
	}
	next.ServeHTTP(w, r)
}

func (p *Plugin) writeDenyResponse(
	w http.ResponseWriter,
	protocol ai_protocols.Protocol,
	requestBody map[string]any,
	reason string,
) {
	model, _ := requestBody["model"].(string)
	body, contentType, err := ai_protocols.BuildDenyWireResponse(
		protocol,
		model,
		reason,
		ai_protocols.IsStreaming(protocol, requestBody),
	)
	if err != nil {
		logger.Error(err.Error())
		writeText(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(*p.config.DenyCode)
	_, _ = w.Write(body)
}

func writeText(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message)
}

func (p *Plugin) checkContent(r *http.Request, body string) (string, error) {
	result, err := p.detectToxicContent(r, body)
	if err != nil {
		return "", err
	}
	if len(result.ResultList) == 0 {
		return "", fmt.Errorf("failed to get moderation results from response")
	}

	for _, item := range result.ResultList {
		for _, label := range item.Labels {
			threshold, ok := p.config.ModerationCategories[label.Name]
			if ok && label.Score > threshold {
				return "request body exceeds " + label.Name + " threshold", nil
			}
		}
		if item.Toxicity > *p.config.ModerationThreshold {
			return "request body exceeds toxicity threshold", nil
		}
	}

	return "", nil
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
	type moderationResponse struct {
		statusCode int
		body       []byte
	}
	var response moderationResponse
	err = p.useAWSCredentials(func(accessKeyID, secretAccessKey, sessionToken string) error {
		req, requestErr := http.NewRequestWithContext(
			r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload),
		)
		if requestErr != nil {
			return fmt.Errorf("failed to create moderation request: %w", requestErr)
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
		req.Header.Set("X-Amz-Target", "Comprehend_20171127.DetectToxicContent")
		businessHeaders := req.Header.Clone()
		defer restoreAWSRequestHeaders(req, businessHeaders)
		if signErr := ai_auth.SignAWSRequestWithOptions(req, payload, ai_auth.AWSConfig{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
			SessionToken:    sessionToken,
		}, ai_auth.SignAWSRequestOptions{
			Region:                 p.config.Comprehend.Region,
			Service:                "comprehend",
			SetSecurityToken:       true,
			DisableURIPathEscaping: true,
			CanonicalHeaders:       []string{"content-type", "host", "x-amz-date", "x-amz-target"},
			HeaderValue:            strings.TrimSpace,
			CanonicalURI:           ai_auth.CanonicalURIPlain,
			CanonicalQuery:         ai_auth.CanonicalQueryRaw,
		}, p.now()); signErr != nil {
			return fmt.Errorf("failed to sign moderation request: %w", signErr)
		}

		resp, requestErr := p.client.Do(req)
		if requestErr != nil {
			return fmt.Errorf("failed to send request to %s: %w", endpoint, requestErr)
		}
		defer func() { _ = resp.Body.Close() }()
		response.statusCode = resp.StatusCode
		response.body, requestErr = io.ReadAll(resp.Body)
		if requestErr != nil {
			return fmt.Errorf("failed to read moderation response body: %w", requestErr)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	if response.statusCode < http.StatusOK || response.statusCode >= http.StatusMultipleChoices {
		return result, fmt.Errorf(
			"failed to request aws comprehend service, status: %d, body: %s",
			response.statusCode,
			response.body,
		)
	}
	if err := json.Unmarshal(response.body, &result); err != nil {
		return result, fmt.Errorf("failed to decode moderation response: %w", err)
	}
	return result, nil
}

func restoreAWSRequestHeaders(req *http.Request, headers http.Header) {
	for name, values := range req.Header {
		if _, preserve := headers[name]; preserve {
			continue
		}
		for index := range values {
			values[index] = ""
		}
		delete(req.Header, name)
	}
	maps.Copy(req.Header, headers)
}

func (p *Plugin) endpoint() string {
	if p.config.Comprehend.Endpoint != "" {
		return p.config.Comprehend.Endpoint
	}
	return "https://comprehend." + p.config.Comprehend.Region + ".amazonaws.com"
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	ai_common.ApplyTransportSSLVerify(transport, p.config.Comprehend.SSLVerify)
	return transport
}
