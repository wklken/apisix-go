package google_cloud_logging

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
}

const (
	priority = 407
	name     = "google-cloud-logging"

	defaultTokenURI   = "https://oauth2.googleapis.com/token"
	defaultEntriesURI = "https://logging.googleapis.com/v2/entries:write"
	defaultLogID      = "apisix.apache.org%2Flogs"

	jwtBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

var defaultScopes = []string{
	"https://www.googleapis.com/auth/logging.read",
	"https://www.googleapis.com/auth/logging.write",
	"https://www.googleapis.com/auth/logging.admin",
	"https://www.googleapis.com/auth/cloud-platform",
}

const schema = `
{
  "type": "object",
  "properties": {
    "auth_config": {
      "type": "object",
      "properties": {
        "client_email": {
          "type": "string"
        },
        "private_key": {
          "type": "string"
        },
        "project_id": {
          "type": "string"
        },
        "token_uri": {
          "type": "string",
          "default": "https://oauth2.googleapis.com/token"
        },
        "scope": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "minItems": 1
        },
        "scopes": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "minItems": 1
        },
        "entries_uri": {
          "type": "string",
          "default": "https://logging.googleapis.com/v2/entries:write"
        }
      },
      "required": ["client_email", "private_key", "project_id", "token_uri"]
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "auth_file": {
      "type": "string"
    },
    "resource": {
      "type": "object",
      "properties": {
        "type": {
          "type": "string"
        },
        "labels": {
          "type": "object"
        }
      },
      "default": {
        "type": "global"
      },
      "required": ["type"]
    },
    "log_id": {
      "type": "string",
      "default": "apisix.apache.org%2Flogs"
    },
    "log_format": {
      "type": "object"
    }
  },
  "oneOf": [
    {"required": ["auth_config"]},
    {"required": ["auth_file"]}
  ]
}
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type AuthConfig struct {
	ClientEmail string   `json:"client_email"`
	PrivateKey  string   `json:"private_key"`
	ProjectID   string   `json:"project_id"`
	TokenURI    string   `json:"token_uri"`
	Scope       []string `json:"scope,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
	EntriesURI  string   `json:"entries_uri,omitempty"`
}

type MonitoredResource struct {
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels,omitempty"`
}

type Config struct {
	AuthConfig *AuthConfig       `json:"auth_config,omitempty"`
	AuthFile   string            `json:"auth_file,omitempty"`
	SSLVerify  *bool             `json:"ssl_verify,omitempty"`
	Resource   MonitoredResource `json:"resource,omitempty"`
	LogID      string            `json:"log_id,omitempty"`
	LogFormat  map[string]string `json:"log_format,omitempty"`
}

type googleLogEntry struct {
	JSONPayload map[string]any     `json:"jsonPayload"`
	Labels      map[string]string  `json:"labels"`
	Timestamp   string             `json:"timestamp"`
	Resource    MonitoredResource  `json:"resource"`
	LogName     string             `json:"logName"`
	InsertID    string             `json:"insertId,omitempty"`
	HTTPRequest *googleHTTPRequest `json:"httpRequest,omitempty"`
}

type googleHTTPRequest struct {
	RequestMethod string `json:"requestMethod,omitempty"`
	RequestURL    string `json:"requestUrl,omitempty"`
	Status        int    `json:"status,omitempty"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	p.FireChan = make(chan map[string]any, 1000)
	p.AsyncBlock = true
	p.SendFunc = p.Send

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Resource.Type == "" {
		p.config.Resource.Type = "global"
	}
	if p.config.LogID == "" {
		p.config.LogID = defaultLogID
	}
	p.applyAuthDefaults(p.config.AuthConfig)

	configUID := shared.NewConfigUID()
	if p.config.AuthConfig != nil {
		configUID.Add(p.config.AuthConfig.ClientEmail)
		configUID.Add(p.config.AuthConfig.ProjectID)
		configUID.Add(p.config.AuthConfig.TokenURI)
		configUID.Add(p.config.AuthConfig.EntriesURI)
	}
	configUID.Add(p.config.AuthFile)
	configUID.Add(p.sslVerify())

	client := resty.New()
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.sslVerify()})
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = loadMetadataLogFormat()
	}

	p.Consume()
	return nil
}

func (p *Plugin) Send(log map[string]any) {
	auth, err := p.authConfig()
	if err != nil {
		logger.Errorf("failed to load google-cloud-logging auth config: %s", err)
		return
	}

	accessToken, tokenType, err := p.generateAccessToken(auth)
	if err != nil {
		logger.Errorf("failed to get google-cloud-logging oauth token: %s", err)
		return
	}
	if tokenType == "" {
		tokenType = "Bearer"
	}

	body := map[string]any{
		"entries":        []googleLogEntry{p.buildEntry(log)},
		"partialSuccess": false,
	}
	resp, err := p.client.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", tokenType+" "+accessToken).
		SetBody(body).
		Post(auth.EntriesURI)
	if err != nil {
		logger.Errorf("failed to write log to Google Cloud Logging endpoint %s: %s", auth.EntriesURI, err)
		return
	}
	if resp.StatusCode() != http.StatusOK {
		logger.Errorf("Google Cloud Logging endpoint returned status code [%d], body [%s]", resp.StatusCode(), resp.String())
	}
}

func (p *Plugin) authConfig() (*AuthConfig, error) {
	if p.config.AuthConfig != nil {
		auth := *p.config.AuthConfig
		p.applyAuthDefaults(&auth)
		return &auth, nil
	}
	if p.config.AuthFile == "" {
		return nil, errors.New("auth_config or auth_file is required")
	}

	data, err := os.ReadFile(p.config.AuthFile)
	if err != nil {
		return nil, err
	}
	var auth AuthConfig
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, err
	}
	p.applyAuthDefaults(&auth)
	return &auth, nil
}

func (p *Plugin) applyAuthDefaults(auth *AuthConfig) {
	if auth == nil {
		return
	}
	if auth.TokenURI == "" {
		auth.TokenURI = defaultTokenURI
	}
	if auth.EntriesURI == "" {
		auth.EntriesURI = defaultEntriesURI
	}
	if len(auth.Scope) == 0 && len(auth.Scopes) == 0 {
		auth.Scope = append([]string(nil), defaultScopes...)
	}
}

func (p *Plugin) buildJWTAssertion(now time.Time) (string, error) {
	auth, err := p.authConfig()
	if err != nil {
		return "", err
	}

	header := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	}
	claims := map[string]any{
		"iss":   auth.ClientEmail,
		"sub":   auth.ClientEmail,
		"aud":   auth.TokenURI,
		"scope": strings.Join(auth.scopes(), " "),
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}

	encodedHeader, err := encodeJWTPart(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := encodeJWTPart(claims)
	if err != nil {
		return "", err
	}
	signingInput := encodedHeader + "." + encodedClaims

	privateKey, err := parsePrivateKey(auth.PrivateKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (a *AuthConfig) scopes() []string {
	if len(a.Scopes) > 0 {
		return a.Scopes
	}
	return a.Scope
}

func encodeJWTPart(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func parsePrivateKey(privateKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privateKey))
	if block == nil {
		return nil, errors.New("invalid private key PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private key is not RSA")
		}
		return rsaKey, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func (p *Plugin) generateAccessToken(auth *AuthConfig) (string, string, error) {
	assertion, err := p.buildJWTAssertion(time.Now())
	if err != nil {
		return "", "", err
	}

	resp, err := p.client.R().
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetBody(url.Values{
			"grant_type": {jwtBearerGrantType},
			"assertion":  {assertion},
		}.Encode()).
		Post(auth.TokenURI)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode() != http.StatusOK {
		return "", "", errors.New(resp.String())
	}

	var token tokenResponse
	if err := json.Unmarshal(resp.Body(), &token); err != nil {
		return "", "", err
	}
	if token.AccessToken == "" {
		return "", "", errors.New("access_token is empty")
	}
	return token.AccessToken, token.TokenType, nil
}

func (p *Plugin) buildEntry(log map[string]any) googleLogEntry {
	auth, err := p.authConfig()
	projectID := ""
	if err == nil {
		projectID = auth.ProjectID
	}

	return googleLogEntry{
		JSONPayload: log,
		Labels: map[string]string{
			"source": "apache-apisix-google-cloud-logging",
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Resource:  p.config.Resource,
		LogName:   "projects/" + projectID + "/logs/" + p.config.LogID,
	}
}

func (p *Plugin) sslVerify() bool {
	return p.config.SSLVerify == nil || *p.config.SSLVerify
}

func loadMetadataLogFormat() (format map[string]string) {
	defer func() {
		if recover() != nil {
			format = nil
		}
	}()

	var metadata pluginMetadata
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return nil
	}
	return metadata.LogFormat
}
