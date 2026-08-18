package csrf

import (
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config       Config
	randomReader io.Reader
}

const (
	// version  = "0.1"
	priority           = 2980
	name               = "csrf"
	defaultCSRFExpires = 7200
)

const schema = `
{
	"type": "object",
	"properties": {
	  "key": {
		"description": "use to generate csrf token",
		"type": "string"
	  },
	  "expires": {
		"description": "expires time(s) for csrf token",
		"type": "integer",
		"default": 7200
	  },
	  "name": {
		"description": "the csrf token name",
		"type": "string",
		"default": "apisix-csrf-token"
	  }
	},
	"required": ["key"]
}`

type Config struct {
	Key     string `json:"key"`
	Expires *int64 `json:"expires,omitempty"`
	Name    string `json:"name,omitempty"`

	safeMethods map[string]struct{}
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.randomReader == nil {
		p.randomReader = crand.Reader
	}

	keyring, enabled := data_encryption.Keyring()
	resolved, err := data_encryption.NewResolver(enabled, keyring).ResolveForContext(p.config.Key, "csrf.key")
	if err != nil {
		return fmt.Errorf("csrf key: %w", err)
	}
	p.config.Key = resolved

	p.config.safeMethods = map[string]struct{}{
		http.MethodGet:     {},
		http.MethodHead:    {},
		http.MethodOptions: {},
	}

	if p.config.Expires == nil {
		expires := int64(defaultCSRFExpires)
		p.config.Expires = &expires
	}
	if p.config.Name == "" {
		p.config.Name = "apisix-csrf-token"
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if _, ok := p.config.safeMethods[r.Method]; !ok {
			// check csrf token
			headerToken := r.Header.Get(p.config.Name)
			if headerToken == "" {
				// token not found
				writeCSRFError(w, "no csrf token in headers")
				return
			}
			// read token from cookie
			cookie, err := r.Cookie(p.config.Name)
			if err != nil {
				// 如果 Cookie 不存在
				writeCSRFError(w, "no csrf cookie")
				return
			}
			cookieToken := cookie.Value

			if headerToken != cookieToken {
				// token not match
				writeCSRFError(w, "csrf token mismatch")
				return
			}

			// check token expires
			ok := checkCSRFToken(cookieToken, p.config.Key, p.expires())
			if !ok {
				writeCSRFError(w, "Failed to verify the csrf token signature")
				return
			}
		}

		// add csrf token into cookie
		csrfToken, err := genCSRFToken(p.config.Key, p.randomReader)
		if err != nil {
			writeCSRFErrorStatus(w, http.StatusInternalServerError, "failed to generate csrf token")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     p.config.Name,
			Value:    csrfToken,
			Path:     "/",
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(time.Duration(p.expires()) * time.Second),
		})

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) expires() int64 {
	if p.config.Expires == nil {
		return defaultCSRFExpires
	}
	return *p.config.Expires
}

func writeCSRFError(w http.ResponseWriter, message string) {
	writeCSRFErrorStatus(w, http.StatusUnauthorized, message)
}

func writeCSRFErrorStatus(w http.ResponseWriter, status int, message string) {
	_ = util.WriteJSON(w, status, map[string]string{"error_msg": message})
}

type csrfToken struct {
	Random  float64 `json:"random"`
	Expires int64   `json:"expires"`
	Sign    string  `json:"sign"`
}

// checkCSRFToken 检查 CSRF Token 是否有效
func checkCSRFToken(token string, key string, expires int64) bool {
	// 解码 Base64 字符串
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		logger.Error("csrf token base64 decode error")
		return false
	}

	// 解码 JSON 字符串
	var csrfToken csrfToken
	err = json.Unmarshal(decoded, &csrfToken)
	if err != nil {
		logger.Errorf("decode token err: %s", err)
		return false
	}

	// 检查 Token 是否过期
	if expires > 0 && time.Now().Unix()-csrfToken.Expires > expires {
		logger.Error("token has expired")
		return false
	}

	// 生成签名
	sign := genSign(csrfToken.Random, csrfToken.Expires, key)

	// 检查签名是否匹配
	if !constantTimeEqual(sign, csrfToken.Sign) {
		logger.Error("Invalid signatures")
		return false
	}

	return true
}

func constantTimeEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

// genCSRFToken 生成一个 CSRF Token，并以 Base64 编码的 JSON 形式返回
func genCSRFToken(key string, reader io.Reader) (string, error) {
	random, err := secureRandomFloat64(reader)
	if err != nil {
		return "", fmt.Errorf("generate csrf token random value: %w", err)
	}
	timestamp := time.Now().Unix()

	sign := genSign(random, timestamp, key)

	token := csrfToken{
		Random:  random,
		Expires: timestamp,
		Sign:    sign,
	}

	// 将 Token 结构体编码为 JSON
	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return "", fmt.Errorf("encode csrf token: %w", err)
	}

	// 将 JSON 编码为 Base64 字符串
	cookie := base64.StdEncoding.EncodeToString(tokenJSON)
	return cookie, nil
}

func secureRandomFloat64(reader io.Reader) (float64, error) {
	if reader == nil {
		return 0, fmt.Errorf("csrf token entropy reader is nil")
	}

	var raw [8]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return 0, fmt.Errorf("read csrf token entropy: %w", err)
	}

	const randomBits = uint(53)
	random := binary.BigEndian.Uint64(raw[:]) >> (64 - randomBits)
	return float64(random) / float64(uint64(1)<<randomBits), nil
}

func genSign(random float64, expires int64, key string) string {
	// 构造待签名的字符串
	// FIXME: format float64 here?
	sign := fmt.Sprintf("{expires:%d,random:%v,key:%s}", expires, random, key)

	// 创建一个新的 SHA256 哈希对象
	hash := sha256.New()

	// 写入待签名字符串到哈希对象，注意这里输入的是字节数组
	hash.Write([]byte(sign))

	// 计算最终的哈希值并得到其字节表示
	digest := hash.Sum(nil)

	// 将字节序列转换为十六进制字符串表示
	return hex.EncodeToString(digest)
}
