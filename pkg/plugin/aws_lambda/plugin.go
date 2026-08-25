package aws_lambda

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
	"github.com/wklken/apisix-go/pkg/secret"
)

type Plugin struct {
	function_upstream.Plugin
	config Config

	credentialsMu sync.RWMutex
	apiKey        secret.Value
	apiKeySet     bool
	iamAccessKey  secret.Value
	iamSecretKey  secret.Value
	iamSet        bool
	retired       bool
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
	p.credentialsMu.RLock()
	defer p.credentialsMu.RUnlock()
	if p.retired || !p.credentialsPreparedLocked() {
		return secret.ErrCredentialUnavailable
	}
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

// MaterializeScopedSecrets resolves every configured Lambda credential for one
// immutable generation before installing any private or public state.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.credentialsMu.Lock()
	defer p.credentialsMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.credentialsInstalledLocked() || p.config.Authorization == nil {
		return nil
	}

	auth := p.config.Authorization
	if auth.IAM != nil && (auth.IAM.AccessKey == "" || auth.IAM.SecretKey == "") {
		return secret.ErrCredentialUnavailable
	}

	var apiKey, accessKey, secretKey secret.Value
	var apiDescriptor, accessDescriptor, secretDescriptor string
	var err error
	if auth.APIKey != "" {
		apiKey, apiDescriptor, err = materializeScopedAWSLambdaCredential(
			ctx, access, "authorization.apikey", auth.APIKey,
		)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
	}
	if auth.IAM != nil {
		accessKey, accessDescriptor, err = materializeScopedAWSLambdaCredential(
			ctx, access, "authorization.iam.accesskey", auth.IAM.AccessKey,
		)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		secretKey, secretDescriptor, err = materializeScopedAWSLambdaCredential(
			ctx, access, "authorization.iam.secretkey", auth.IAM.SecretKey,
		)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
	}

	if auth.APIKey != "" {
		p.apiKey = apiKey
		p.apiKeySet = true
		auth.APIKey = apiDescriptor
	}
	if auth.IAM != nil {
		p.iamAccessKey = accessKey
		p.iamSecretKey = secretKey
		p.iamSet = true
		auth.IAM.AccessKey = accessDescriptor
		auth.IAM.SecretKey = secretDescriptor
	}
	return nil
}

func materializeScopedAWSLambdaCredential(
	ctx context.Context,
	access base.ScopedSecretAccess,
	field string,
	raw string,
) (secret.Value, string, error) {
	value, err := access.Materialize(ctx, field, raw)
	if err != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	if err := value.Use(func(plaintext string) error {
		if strings.TrimSpace(plaintext) == "" {
			return secret.ErrCredentialUnavailable
		}
		return nil
	}); err != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	return value, descriptor.String(), nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
func (p *Plugin) MaterializeSecrets() error {
	p.credentialsMu.Lock()
	defer p.credentialsMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.credentialsInstalledLocked() || p.config.Authorization == nil {
		return nil
	}

	auth := p.config.Authorization
	if auth.IAM != nil && (auth.IAM.AccessKey == "" || auth.IAM.SecretKey == "") {
		return secret.ErrCredentialUnavailable
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) credentialsInstalledLocked() bool {
	return p.apiKeySet || p.iamSet
}

func (p *Plugin) credentialsPreparedLocked() bool {
	if p.config.Authorization == nil {
		return true
	}
	if p.config.Authorization.APIKey != "" && !p.apiKeySet {
		return false
	}
	if p.config.Authorization.IAM != nil && !p.iamSet {
		return false
	}
	return true
}

func (p *Plugin) processRequest(r *http.Request, _ function_upstream.Config) {
	p.credentialsMu.RLock()
	defer p.credentialsMu.RUnlock()
	if p.retired {
		deleteHeader(r.Header, "X-Api-Key")
		removeClientIAMHeaders(r.Header)
		return
	}
	if p.config.Authorization == nil {
		return
	}
	if p.config.Authorization.APIKey != "" {
		deleteHeader(r.Header, "X-Api-Key")
		_ = p.useAPIKeyLocked(func(apiKey string) error {
			r.Header.Set("X-Api-Key", apiKey)
			return nil
		})
		return
	}
	if p.config.Authorization.IAM == nil {
		return
	}

	removeClientIAMHeaders(r.Header)
	_ = p.useIAMLocked(func(accessKey, secretKey string) error {
		_ = p.signIAMRequest(r, p.config.Authorization.IAM, accessKey, secretKey)
		return nil
	})
}

func (p *Plugin) useAPIKeyLocked(use func(string) error) error {
	if p.apiKeySet {
		return p.apiKey.Use(use)
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) useIAMLocked(use func(string, string) error) error {
	if p.iamSet {
		return p.iamAccessKey.Use(func(accessKey string) error {
			return p.iamSecretKey.Use(func(secretKey string) error {
				return use(accessKey, secretKey)
			})
		})
	}
	return secret.ErrCredentialUnavailable
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

func (p *Plugin) signIAMRequest(r *http.Request, iam *IAM, accessKey, secretKey string) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return ai_auth.SignAWSRequestWithOptions(r, body, ai_auth.AWSConfig{
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}, ai_auth.SignAWSRequestOptions{
		Region:                   iam.AWSRegion,
		Service:                  iam.Service,
		DeriveHeadersFromRequest: true,
		CanonicalURI:             ai_auth.CanonicalURICleaned,
		CanonicalQuery:           ai_auth.CanonicalQuerySortedParts,
		RewriteQuery:             true,
	}, now())
}

// Stop releases the embedded neutral upstream before destroying transitional
// owners and dropping immutable-generation values.
func (p *Plugin) Stop() {
	p.Plugin.Stop()
	p.credentialsMu.Lock()
	defer p.credentialsMu.Unlock()
	if p.retired {
		return
	}
	p.retired = true
	p.apiKey = secret.Value{}
	p.apiKeySet = false
	p.iamAccessKey = secret.Value{}
	p.iamSecretKey = secret.Value{}
	p.iamSet = false
}
