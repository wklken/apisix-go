package wolf_rbac

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	projectjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *http.Client
}

const (
	priority = 2555
	name     = "wolf-rbac"
)

const schema = `
{
  "type": "object",
  "properties": {
    "appid": {
      "type": "string",
      "default": "unset"
    },
    "server": {
      "type": "string",
      "default": "http://127.0.0.1:12180"
    },
    "header_prefix": {
      "type": "string",
      "default": "X-"
    },
    "ssl_verify": {
      "type": "boolean"
    }
  }
}
`

type Config struct {
	AppID        string `json:"appid,omitempty"`
	Server       string `json:"server,omitempty"`
	HeaderPrefix string `json:"header_prefix,omitempty"`
	SSLVerify    *bool  `json:"ssl_verify,omitempty"`
}

type consumerConfig struct {
	AppID        string `json:"appid,omitempty"`
	Server       string `json:"server,omitempty"`
	HeaderPrefix string `json:"header_prefix,omitempty"`
	SSLVerify    *bool  `json:"ssl_verify,omitempty"`
}

type rbacToken struct {
	AppID     string
	WolfToken string
}

type permissionResponse struct {
	OK     bool                   `json:"ok"`
	Reason string                 `json:"reason"`
	Data   permissionResponseData `json:"data"`
}

type permissionResponseData struct {
	UserInfo map[string]any `json:"userInfo"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.AppID == "" {
		p.config.AppID = "unset"
	}
	if p.config.Server == "" {
		p.config.Server = "http://127.0.0.1:12180"
	}
	if p.config.HeaderPrefix == "" {
		p.config.HeaderPrefix = "X-"
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: 10 * time.Second}
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawToken := fetchRBACToken(r)
		if rawToken == "" {
			writeJSONError(w, http.StatusUnauthorized, "Missing rbac token in request")
			return
		}

		token, err := parseRBACToken(rawToken)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid rbac token: parse failed")
			return
		}

		consumer, cfg, err := p.consumerByAppID(token.AppID)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "Invalid appid in rbac token")
			return
		}

		status, reason, userInfo, err := p.checkPermission(r, cfg, token)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p.setUserHeaders(w, r, cfg.headerPrefix(), userInfo)
		if status != http.StatusOK {
			if reason == "" {
				reason = http.StatusText(status)
			}
			writeJSONError(w, status, reason)
			return
		}

		ctx.AttachConsumer(r, consumer)
		next.ServeHTTP(w, r)
	})
}

func fetchRBACToken(r *http.Request) string {
	if token := r.URL.Query().Get("rbac_token"); token != "" {
		return token
	}
	if token := r.Header.Get("Authorization"); token != "" {
		return token
	}
	if token := r.Header.Get("X-Rbac-Token"); token != "" {
		return token
	}
	if cookie, err := r.Cookie("x-rbac-token"); err == nil {
		return cookie.Value
	}
	return ""
}

func parseRBACToken(raw string) (rbacToken, error) {
	parts := strings.SplitN(raw, "#", 3)
	if len(parts) != 3 || parts[0] != "V1" || parts[1] == "" || parts[2] == "" {
		return rbacToken{}, fmt.Errorf("invalid rbac token")
	}
	return rbacToken{AppID: parts[1], WolfToken: parts[2]}, nil
}

func (p *Plugin) consumerByAppID(appID string) (resource.Consumer, consumerConfig, error) {
	consumer, err := store.GetConsumerByPluginKey(name, appID)
	if err != nil {
		return resource.Consumer{}, consumerConfig{}, err
	}

	raw, ok := consumer.Plugins[name]
	if !ok {
		return resource.Consumer{}, consumerConfig{}, store.ErrNotFound
	}
	var cfg consumerConfig
	if err := util.Parse(raw, &cfg); err != nil {
		return resource.Consumer{}, consumerConfig{}, err
	}
	cfg.applyDefaults(p.config)
	return consumer, cfg, nil
}

func (p *Plugin) checkPermission(
	r *http.Request,
	cfg consumerConfig,
	token rbacToken,
) (int, string, map[string]any, error) {
	values := url.Values{}
	values.Set("appID", token.AppID)
	values.Set("resName", r.URL.Path)
	values.Set("action", r.Method)
	values.Set("clientIP", remoteIP(r))

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodGet,
		strings.TrimRight(cfg.server(), "/")+"/wolf/rbac/access_check?"+values.Encode(),
		nil,
	)
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("X-Rbac-Token", token.WolfToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, "", nil, fmt.Errorf("request to wolf-server failed, err:%w", err)
	}
	defer resp.Body.Close()

	var body permissionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return resp.StatusCode, "check permission failed! parse response json failed!", nil, nil
	}
	return resp.StatusCode, body.Reason, body.Data.UserInfo, nil
}

func (p *Plugin) setUserHeaders(w http.ResponseWriter, r *http.Request, prefix string, userInfo map[string]any) {
	if len(userInfo) == 0 {
		return
	}

	userID := fmt.Sprint(userInfo["id"])
	username := fmt.Sprint(userInfo["username"])
	nickname := username
	if userInfo["nickname"] != nil {
		nickname = fmt.Sprint(userInfo["nickname"])
	}
	escapedNickname := url.QueryEscape(nickname)

	headers := map[string]string{
		prefix + "UserId":   userID,
		prefix + "Username": username,
		prefix + "Nickname": escapedNickname,
	}
	for key, value := range headers {
		w.Header().Set(key, value)
		r.Header.Set(key, value)
	}
}

func (cfg *consumerConfig) applyDefaults(pluginCfg Config) {
	if cfg.AppID == "" {
		cfg.AppID = pluginCfg.AppID
	}
	if cfg.Server == "" {
		cfg.Server = pluginCfg.Server
	}
	if cfg.HeaderPrefix == "" {
		cfg.HeaderPrefix = pluginCfg.HeaderPrefix
	}
	if cfg.SSLVerify == nil {
		cfg.SSLVerify = pluginCfg.SSLVerify
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:12180"
	}
	if cfg.HeaderPrefix == "" {
		cfg.HeaderPrefix = "X-"
	}
}

func (cfg consumerConfig) server() string {
	if cfg.Server == "" {
		return "http://127.0.0.1:12180"
	}
	return cfg.Server
}

func (cfg consumerConfig) headerPrefix() string {
	if cfg.HeaderPrefix == "" {
		return "X-"
	}
	return cfg.HeaderPrefix
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = projectjson.NewEncoder(w).Encode(map[string]any{"message": message})
}
