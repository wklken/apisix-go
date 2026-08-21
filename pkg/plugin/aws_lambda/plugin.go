package aws_lambda

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
)

type Plugin struct {
	function_upstream.Plugin
	config Config
}

const (
	priority = -1899
	name     = "aws-lambda"
	algo     = "AWS4-HMAC-SHA256"
)

var now = time.Now

const schema = `
{
  "type": "object",
  "properties": {
    "function_uri": {
      "type": "string"
    },
    "authorization": {
      "type": "object",
      "properties": {
        "apikey": {
          "type": "string"
        },
        "iam": {
          "type": "object",
          "properties": {
            "accesskey": {
              "type": "string"
            },
            "secretkey": {
              "type": "string"
            },
            "aws_region": {
              "type": "string",
              "default": "us-east-1"
            },
            "service": {
              "type": "string",
              "default": "execute-api"
            }
          },
          "required": ["accesskey", "secretkey"]
        }
      }
    },
    "timeout": {
      "type": "integer",
      "minimum": 100,
      "default": 3000
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
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
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    }
  },
  "required": ["function_uri"]
}
`

type Config struct {
	FunctionURI      string         `json:"function_uri"`
	Authorization    *Authorization `json:"authorization,omitempty"`
	Timeout          int            `json:"timeout,omitempty"`
	SSLVerify        *bool          `json:"ssl_verify,omitempty"`
	Keepalive        *bool          `json:"keepalive,omitempty"`
	KeepaliveTimeout int            `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int            `json:"keepalive_pool,omitempty"`
}

type Authorization struct {
	APIKey string `json:"apikey,omitempty"`
	IAM    *IAM   `json:"iam,omitempty"`
}

type IAM struct {
	AccessKey string `json:"accesskey"`
	SecretKey string `json:"secretkey"`
	AWSRegion string `json:"aws_region,omitempty"`
	Service   string `json:"service,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.Processor = p.processRequest

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Authorization != nil && p.config.Authorization.IAM != nil {
		if p.config.Authorization.IAM.AWSRegion == "" {
			p.config.Authorization.IAM.AWSRegion = "us-east-1"
		}
		if p.config.Authorization.IAM.Service == "" {
			p.config.Authorization.IAM.Service = "execute-api"
		}
	}

	p.Plugin.Config = function_upstream.Config{
		FunctionURI:      p.config.FunctionURI,
		Timeout:          p.config.Timeout,
		SSLVerify:        p.config.SSLVerify,
		Keepalive:        p.config.Keepalive,
		KeepaliveTimeout: p.config.KeepaliveTimeout,
		KeepalivePool:    p.config.KeepalivePool,
	}
	return p.Plugin.PostInit()
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) processRequest(r *http.Request, _ function_upstream.Config) {
	if p.config.Authorization == nil {
		return
	}
	if p.config.Authorization.APIKey != "" {
		deleteHeader(r.Header, "X-Api-Key")
		r.Header.Set("X-Api-Key", p.config.Authorization.APIKey)
		return
	}
	if p.config.Authorization.IAM == nil {
		return
	}

	removeClientIAMHeaders(r.Header)
	p.signIAMRequest(r, p.config.Authorization.IAM)
}

func removeClientIAMHeaders(headers http.Header) {
	for name := range headers {
		if isIAMCredentialHeader(name) {
			deleteHeader(headers, name)
		}
	}
}

func isIAMCredentialHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "x-amz-credential", "x-amz-date", "x-amz-signature",
		"x-amz-signedheaders", "x-amz-content-sha256", "x-amz-security-token":
		return true
	default:
		return false
	}
}

func deleteHeader(headers http.Header, name string) {
	for existing := range headers {
		if strings.EqualFold(existing, name) {
			delete(headers, existing)
		}
	}
}

func (p *Plugin) signIAMRequest(r *http.Request, iam *IAM) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	_ = ai_auth.SignAWSRequestWithOptions(r, body, ai_auth.AWSConfig{
		AccessKeyID:     iam.AccessKey,
		SecretAccessKey: iam.SecretKey,
	}, ai_auth.SignAWSRequestOptions{
		Region:                   iam.AWSRegion,
		Service:                  iam.Service,
		DeriveHeadersFromRequest: true,
		CanonicalURI:             ai_auth.CanonicalURICleaned,
		CanonicalQuery:           ai_auth.CanonicalQuerySortedParts,
		RewriteQuery:             true,
	}, now())
}
