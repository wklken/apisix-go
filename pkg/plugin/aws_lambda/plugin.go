package aws_lambda

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

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
	if r.Header.Get("X-Api-Key") == "" &&
		p.config.Authorization != nil &&
		p.config.Authorization.APIKey != "" {
		r.Header.Set("X-Api-Key", p.config.Authorization.APIKey)
		return
	}

	if r.Header.Get("Authorization") != "" ||
		p.config.Authorization == nil ||
		p.config.Authorization.IAM == nil {
		return
	}

	p.signIAMRequest(r, p.config.Authorization.IAM)
}

func (p *Plugin) signIAMRequest(r *http.Request, iam *IAM) {
	t := now().UTC()
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")
	r.Header.Set("X-Amz-Date", amzDate)

	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))

	signedHeaders, canonicalHeaders := canonicalHeaders(r)
	canonicalRequest := strings.Join([]string{
		strings.ToUpper(r.Method),
		canonicalURI(r.URL.Path),
		canonicalQueryString(r.URL.RawQuery),
		canonicalHeaders,
		signedHeaders,
		sha256Hex(body),
	}, "\n")

	credentialScope := dateStamp + "/" + iam.AWSRegion + "/" + iam.Service + "/aws4_request"
	stringToSign := strings.Join([]string{
		algo,
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signature := hmacHex(signingKey(iam.SecretKey, dateStamp, iam.AWSRegion, iam.Service), stringToSign)
	r.Header.Set("Authorization", algo+
		" Credential="+iam.AccessKey+"/"+credentialScope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

func canonicalURI(value string) string {
	if value == "" {
		return "/"
	}
	cleaned := path.Clean(value)
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

func canonicalQueryString(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	for i, part := range parts {
		key, value, found := strings.Cut(part, "=")
		key = unescapeQueryPart(key)
		if found {
			value = unescapeQueryPart(value)
			parts[i] = key + "=" + value
			continue
		}
		parts[i] = key + "="
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func unescapeQueryPart(value string) string {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

func canonicalHeaders(r *http.Request) (string, string) {
	values := make(map[string]string, len(r.Header)+1)
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	values["host"] = normalizeHeaderValue(host)
	for key, headerValues := range r.Header {
		key = strings.ToLower(key)
		if key == "connection" || key == "host" {
			continue
		}
		normalized := make([]string, 0, len(headerValues))
		for _, value := range headerValues {
			normalized = append(normalized, normalizeHeaderValue(value))
		}
		values[key] = strings.Join(normalized, ",")
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+":"+values[key]+"\n")
	}
	return strings.Join(keys, ";"), strings.Join(lines, "")
}

func normalizeHeaderValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacHex(key []byte, msg string) string {
	return hex.EncodeToString(hmacSHA256(key, msg))
}

func hmacSHA256(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}
