package jwe_decrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 2509
	name     = "jwe-decrypt"
)

const schema = `
{
  "type": "object",
  "properties": {
    "header": {
      "type": "string",
      "default": "Authorization"
    },
    "forward_header": {
      "type": "string",
      "default": "Authorization"
    },
    "strict": {
      "type": "boolean",
      "default": true
    }
  },
  "required": ["header", "forward_header"]
}
`

type Config struct {
	Header        string `json:"header"`
	ForwardHeader string `json:"forward_header"`
	Strict        *bool  `json:"strict,omitempty"`
}

type consumerConfig struct {
	Key             string `json:"key"`
	Secret          string `json:"secret"`
	IsBase64Encoded bool   `json:"is_base64_encoded,omitempty"`
}

type jweToken struct {
	header     jweHeader
	iv         []byte
	ciphertext []byte
	tag        []byte
}

type jweHeader struct {
	Kid string `json:"kid"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Header == "" {
		p.config.Header = "Authorization"
	}
	if p.config.ForwardHeader == "" {
		p.config.ForwardHeader = "Authorization"
	}
	if p.config.Strict == nil {
		strict := true
		p.config.Strict = &strict
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		rawToken := p.fetchToken(r)
		if rawToken == "" {
			if *p.config.Strict {
				http.Error(w, util.BuildMessageResponse("missing JWE token in request"), http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		token, err := parseCompactJWE(rawToken)
		if err != nil {
			http.Error(w, util.BuildMessageResponse("JWE token invalid"), http.StatusBadRequest)
			return
		}
		if token.header.Kid == "" {
			http.Error(w, util.BuildMessageResponse("missing kid in JWE token"), http.StatusBadRequest)
			return
		}

		consumer, err := store.GetConsumerByPluginKey(name, token.header.Kid)
		if err != nil {
			http.Error(w, util.BuildMessageResponse("invalid kid in JWE token"), http.StatusBadRequest)
			return
		}

		plaintext, err := decryptJWE(token, consumer.Plugins[name])
		if err != nil {
			http.Error(w, util.BuildMessageResponse("failed to decrypt JWE token"), http.StatusBadRequest)
			return
		}

		r.Header.Set(p.config.ForwardHeader, string(plaintext))
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) fetchToken(r *http.Request) string {
	token := r.Header.Get(p.config.Header)
	if strings.HasPrefix(token, "Bearer ") || strings.HasPrefix(token, "bearer ") {
		return token[7:]
	}
	return token
}

func parseCompactJWE(raw string) (jweToken, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 5 {
		return jweToken{}, fmt.Errorf("compact JWE must have five parts")
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jweToken{}, err
	}
	var header jweHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return jweToken{}, err
	}

	iv, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jweToken{}, err
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return jweToken{}, err
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return jweToken{}, err
	}

	return jweToken{
		header:     header,
		iv:         iv,
		ciphertext: ciphertext,
		tag:        tag,
	}, nil
}

func decryptJWE(token jweToken, rawConfig any) ([]byte, error) {
	var cfg consumerConfig
	if err := util.Parse(rawConfig, &cfg); err != nil {
		return nil, err
	}

	secret := []byte(cfg.Secret)
	if cfg.IsBase64Encoded {
		decoded, err := base64.RawURLEncoding.DecodeString(cfg.Secret)
		if err != nil {
			return nil, err
		}
		secret = decoded
	}
	if len(secret) != 32 {
		return nil, errors.New("JWE consumer secret must be 32 bytes")
	}

	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	ciphertextAndTag := append(append([]byte{}, token.ciphertext...), token.tag...)
	return gcm.Open(nil, token.iv, ciphertextAndTag, nil)
}
