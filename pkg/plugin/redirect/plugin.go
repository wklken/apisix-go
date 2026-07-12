package redirect

import (
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/cast"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 900
	name     = "redirect"
)

// "pattern": "(\$[0-9a-zA-Z_]+)|\$\{([0-9a-zA-Z_]+)\}|\$([0-9a-zA-Z_]+)|(\$|[^$\\]+)"
const schema = `
{
	"type": "object",
	"properties": {
	  "ret_code": {
		"type": "integer",
		"minimum": 200,
		"default": 302
	  },
	  "uri": {
		"type": "string",
		"minLength": 2
	  },
	  "regex_uri": {
		"description": "params for generating new uri that substitute from client uri, first param is regular expression, the second one is uri template",
		"type": "array",
		"maxItems": 2,
		"minItems": 2,
		"items": {
		  "description": "regex uri",
		  "type": "string"
		}
	  },
	  "http_to_https": {
		"type": "boolean"
	  },
	  "encode_uri": {
		"type": "boolean",
		"default": false
	  },
	  "append_query_string": {
		"type": "boolean",
		"default": false
	  }
	},
	"oneOf": [
	  { "required": ["uri"] },
	  { "required": ["regex_uri"] },
	  { "required": ["http_to_https"] }
	]
}`

type Config struct {
	RetCode           int      `json:"ret_code,omitempty"`
	Uri               string   `json:"uri,omitempty"`
	RegexUri          []string `json:"regex_uri,omitempty"`
	HttpToHttps       *bool    `json:"http_to_https,omitempty"`
	EncodeUri         *bool    `json:"encode_uri,omitempty"`
	AppendQueryString *bool    `json:"append_query_string,omitempty"`

	httpsPort *int
	regexURI  *regexp.Regexp
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.RetCode == 0 {
		p.config.RetCode = 302
	}

	if p.config.HttpToHttps == nil {
		defaultValue := false
		p.config.HttpToHttps = &defaultValue
	}

	if p.config.AppendQueryString == nil {
		defaultValue := false
		p.config.AppendQueryString = &defaultValue
	}
	if p.config.EncodeUri == nil {
		defaultValue := false
		p.config.EncodeUri = &defaultValue
	}

	if len(p.config.RegexUri) > 0 {
		pattern, err := regexp.Compile(p.config.RegexUri[0])
		if err != nil {
			return fmt.Errorf("invalid regex_uri pattern %q: %w", p.config.RegexUri[0], err)
		}
		p.config.regexURI = pattern
	}

	p.config.httpsPort = configuredHTTPSPort()
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// http_to_https、uri 和 regex_uri 只能配置其中一个属性。
		// http_to_https、和 append_query_string 只能配置其中一个属性。
		// 当开启 http_to_https 时，重定向 URL 中的端口将按如下顺序选取一个值（按优先级从高到低排列）
		// 从配置文件（conf/config.yaml）中读取 plugin_attr.redirect.https_port。
		// 如果 apisix.ssl 处于开启状态，读取 apisix.ssl.listen 并从中随机选一个 port。
		// 使用 443 作为默认 https port。
		if p.config.HttpToHttps != nil && *p.config.HttpToHttps && requestScheme(r) != "https" {
			retPort := p.config.httpsPort
			host := requestHostname(r)
			path := r.URL.RequestURI()

			var url string
			if retPort == nil || *retPort == 443 || *retPort <= 0 || *retPort > 65535 {
				url = "https://" + urlHostname(host) + path
			} else {
				url = fmt.Sprintf("https://%s%s", net.JoinHostPort(requestHostname(r), fmt.Sprint(*retPort)), path)
			}

			var retCode int
			if r.Method == "GET" || r.Method == "HEAD" {
				retCode = 301
			} else {
				// https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/308
				retCode = 308
			}

			http.Redirect(w, r, url, retCode)
			return
		}
		if p.config.regexURI != nil {
			redirectURI, matched := p.redirectRegexURI(r)
			if !matched {
				next.ServeHTTP(w, r)
				return
			}
			p.redirect(w, redirectURI)
			return
		}

		if p.config.Uri != "" {
			// FIXME: add cache here?
			url := fmt.Sprintf("%s://%s%s", v.GetNginxVar(r, "$scheme"), r.Host, p.config.Uri)
			if p.config.AppendQueryString != nil && *p.config.AppendQueryString {
				if strings.Contains(url, "?") {
					url = url + "&" + r.URL.RawQuery
				} else {
					url = url + "?" + r.URL.RawQuery
				}
			}
			p.redirect(w, url)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func configuredHTTPSPort() *int {
	if config.GlobalConfig == nil {
		return nil
	}
	if pluginAttr := config.GlobalConfig.PluginAttr[name]; pluginAttr != nil {
		if rawPort, ok := pluginAttr["https_port"]; ok {
			if port, err := cast.ToIntE(rawPort); err == nil {
				return &port
			}
		}
	}

	ssl := config.GlobalConfig.Apisix.Ssl
	if !ssl.Enable || len(ssl.Listen) == 0 {
		return nil
	}
	port := ssl.Listen[rand.IntN(len(ssl.Listen))].Port
	return &port
}

func requestScheme(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme, _, _ := strings.Cut(forwarded, ",")
		return strings.ToLower(strings.TrimSpace(scheme))
	}
	if r.TLS != nil {
		return "https"
	}
	if r.URL.Scheme != "" {
		return strings.ToLower(r.URL.Scheme)
	}
	return "http"
}

func requestHostname(r *http.Request) string {
	if hostname := r.URL.Hostname(); hostname != "" {
		return hostname
	}
	if hostname, _, err := net.SplitHostPort(r.Host); err == nil {
		return hostname
	}
	return strings.Trim(r.Host, "[]")
}

func urlHostname(host string) string {
	if strings.Contains(host, ":") {
		return "[" + strings.Trim(host, "[]") + "]"
	}
	return host
}

func (p *Plugin) redirectRegexURI(r *http.Request) (string, bool) {
	path := r.URL.Path
	if !p.config.regexURI.MatchString(path) {
		return "", false
	}
	return p.config.regexURI.ReplaceAllString(path, p.config.RegexUri[1]), true
}

func (p *Plugin) redirect(w http.ResponseWriter, location string) {
	if p.config.EncodeUri != nil && *p.config.EncodeUri {
		location = encodeRedirectURI(location)
	}
	w.Header().Set("Location", location)
	w.WriteHeader(p.config.RetCode)
}

func encodeRedirectURI(location string) string {
	path, rawQuery, hasQuery := strings.Cut(location, "?")
	encoded := encodePathPreservingSlash(path)
	if hasQuery {
		return encoded + "?" + rawQuery
	}
	return encoded
}

func encodePathPreservingSlash(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
