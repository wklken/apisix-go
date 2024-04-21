package csrf

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 2980
	name     = "csrf"
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
	Expires int64  `json:"expires,omitempty"`
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
	p.config.safeMethods = map[string]struct{}{
		http.MethodGet:     {},
		http.MethodHead:    {},
		http.MethodOptions: {},
	}

	if p.config.Expires == 0 {
		p.config.Expires = 7200
	}
	if p.config.Name == "" {
		p.config.Name = "apisix-csrf-token"
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if _, ok := p.config.safeMethods[r.Method]; !ok {
			// check csrf token
			headerToken := r.Header.Get(p.config.Name)
			if headerToken == "" {
				// token not found
				http.Error(w, "no csrf token in headers", http.StatusUnauthorized)
				return
			}
			// read token from cookie
			cookie, err := r.Cookie(p.config.Name)
			if err != nil {
				// 如果 Cookie 不存在
				http.Error(w, "no csrf cookie", http.StatusUnauthorized)
				return
			}
			cookieToken := cookie.Value

			if headerToken != cookieToken {
				// token not match
				http.Error(w, "csrf token mismatch", http.StatusUnauthorized)
				return
			}

			// check token expires
			ok := checkCSRFToken(cookieToken, p.config.Key, p.config.Expires)
			if !ok {
				http.Error(w, "Failed to verify the csrf token signature", http.StatusUnauthorized)
				return
			}
		}

		// add csrf token into cookie
		csrfToken := genCSRFToken(p.config.Key)
		http.SetCookie(w, &http.Cookie{
			Name:     p.config.Name,
			Value:    csrfToken,
			Path:     "/",
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(time.Duration(p.config.Expires) * time.Second),
		})

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
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
	if time.Now().Unix()-csrfToken.Expires > expires {
		logger.Error("token has expired")
		return false
	}

	// 生成签名
	sign := genSign(csrfToken.Random, csrfToken.Expires, key)

	// 检查签名是否匹配
	if sign != csrfToken.Sign {
		logger.Error("Invalid signatures")
		return false
	}

	return true
}

// genCSRFToken 生成一个 CSRF Token，并以 Base64 编码的 JSON 形式返回
func genCSRFToken(key string) string {
	// rand.Seed(time.Now().UnixNano())
	random := rand.Float64()
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
		fmt.Println("Error encoding token to JSON:", err)
		return ""
	}

	// 将 JSON 编码为 Base64 字符串
	cookie := base64.StdEncoding.EncodeToString(tokenJSON)
	return cookie
}

func genSign(random float64, expires int64, key string) string {
	// 构造待签名的字符串
	// FIXME: format float64 here?
	sign := fmt.Sprintf("{expires:%d,random:%s,key:%s}", expires, random, key)

	// 创建一个新的 SHA256 哈希对象
	hash := sha256.New()

	// 写入待签名字符串到哈希对象，注意这里输入的是字节数组
	hash.Write([]byte(sign))

	// 计算最终的哈希值并得到其字节表示
	digest := hash.Sum(nil)

	// 将字节序列转换为十六进制字符串表示
	return hex.EncodeToString(digest)
}
