package redirect

import (
	"fmt"
	"net/http"
	"strings"

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

	pluginAttr, ok := config.GlobalConfig.PluginAttr["redirect"]
	if !ok {
		p.config.httpsPort = nil
	} else {
		defaultHttpsPort := 443
		httpsPort, ok := pluginAttr["https_port"].(int)
		if ok {
			p.config.httpsPort = &httpsPort
		} else {
			// FIXME: read and return random  https port from apisix.ssl.listen
			p.config.httpsPort = &defaultHttpsPort
		}
	}

	return nil
}

func (p *Plugin) Config() interface{} {
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
		if p.config.HttpToHttps != nil && *p.config.HttpToHttps && r.Proto == "http" {
			retPort := p.config.httpsPort
			host := r.Host
			path := r.URL.Path

			var url string
			if retPort == nil || *retPort == 443 || *retPort < 0 || *retPort > 65535 {
				url = "https://" + host + path
			} else {
				// if port in host, replace it
				newHost := strings.Split(host, ":")[0]

				url = fmt.Sprintf("https://%s:%d%s", newHost, *retPort, path)
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
		// FIXME: not support regex_uri

		if p.config.Uri != "" {
			// FIXME:  not support encode_uri

			// FIXME: add cache here?
			url := fmt.Sprintf("%s://%s%s", v.GetNginxVar(r, "$scheme"), r.Host, p.config.Uri)
			if p.config.AppendQueryString != nil && *p.config.AppendQueryString {
				if strings.Contains(url, "?") {
					url = url + "&" + r.URL.RawQuery
				} else {
					url = url + "?" + r.URL.RawQuery
				}
			}
			http.Redirect(w, r, url, p.config.RetCode)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
