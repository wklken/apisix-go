package wolf_rbac

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	projectjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
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

	wolfRetryMax      = 3
	wolfRetryInterval = 100 * time.Millisecond

	WolfLoginURI          = "/apisix/plugin/wolf-rbac/login"
	WolfChangePasswordURI = "/apisix/plugin/wolf-rbac/change_pwd"
	WolfUserInfoURI       = "/apisix/plugin/wolf-rbac/user_info"
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
	public_api.Register(http.MethodPost, WolfLoginURI, http.HandlerFunc(p.handleLogin))
	public_api.Register(http.MethodPut, WolfChangePasswordURI, http.HandlerFunc(p.handleChangePassword))
	public_api.Register(http.MethodGet, WolfUserInfoURI, http.HandlerFunc(p.handleUserInfo))

	return nil
}

func (p *Plugin) Config() any {
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
		ctx.RunConsumerPlugins(w, r, next)
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
	values.Set("clientIP", base.RemoteIP(r.RemoteAddr))

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

	client := p.clientForConfig(cfg)
	var resp *http.Response
	for attempt := range wolfRetryMax {
		response, err := client.Do(req)
		if err != nil {
			return 0, "", nil, fmt.Errorf("request to wolf-server failed, err:%w", err)
		}
		if response.StatusCode < http.StatusInternalServerError {
			resp = response
			break
		}
		_ = response.Body.Close()
		if attempt+1 == wolfRetryMax {
			return http.StatusInternalServerError,
				fmt.Sprintf("request to wolf-server failed, status:%d", response.StatusCode), nil, nil
		}
		time.Sleep(wolfRetryInterval)
	}
	if resp == nil {
		return http.StatusInternalServerError, "request to wolf-server failed", nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	var body permissionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return resp.StatusCode, "check permission failed! parse response json failed!", nil, nil
	}
	return resp.StatusCode, body.Reason, body.Data.UserInfo, nil
}

func (p *Plugin) clientForConfig(cfg consumerConfig) *http.Client {
	if cfg.SSLVerify != nil && *cfg.SSLVerify {
		return p.client
	}

	client := *p.client
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		transport = http.DefaultTransport.(*http.Transport)
	}
	transport = transport.Clone()
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	} else {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec
	client.Transport = transport

	return &client
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

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = projectjson.NewEncoder(w).Encode(map[string]any{"message": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = projectjson.NewEncoder(w).Encode(value)
}

func (p *Plugin) handleLogin(w http.ResponseWriter, r *http.Request) {
	args, err := requestArguments(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	appid, _ := args["appid"].(string)
	if appid == "" {
		writeJSONError(w, http.StatusBadRequest, "appid is missing")
		return
	}
	_, cfg, err := p.consumerByAppID(appid)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "appid not found")
		return
	}
	response, err := p.requestWolf(r, cfg, http.MethodPost, "/wolf/rbac/login.rest", "", args)
	if err != nil || !response.OK {
		writeJSONError(w, http.StatusInternalServerError, "request to wolf-server failed!")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rbac_token": "V1#" + appid + "#" + response.Data.Token,
		"user_info":  response.Data.UserInfo,
	})
}

func (p *Plugin) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	args, err := requestArguments(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	_, cfg, token, ok := p.publicAPIToken(w, r)
	if !ok {
		return
	}
	response, err := p.requestWolf(r, cfg, http.MethodPost, "/wolf/rbac/change_pwd", token.WolfToken, args)
	if err != nil || !response.OK {
		writeJSONError(w, http.StatusInternalServerError, "request to wolf-server failed!")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "success to change password"})
}

func (p *Plugin) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	_, cfg, token, ok := p.publicAPIToken(w, r)
	if !ok {
		return
	}
	response, err := p.requestWolf(r, cfg, http.MethodGet, "/wolf/rbac/user_info", token.WolfToken, map[string]any{})
	if err != nil || !response.OK {
		writeJSONError(w, http.StatusInternalServerError, "request to wolf-server failed!")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user_info": response.Data.UserInfo})
}

func (p *Plugin) publicAPIToken(
	w http.ResponseWriter,
	r *http.Request,
) (resource.Consumer, consumerConfig, rbacToken, bool) {
	rawToken := fetchRBACToken(r)
	if rawToken == "" {
		writeJSONError(w, http.StatusUnauthorized, "Missing rbac token in request")
		return resource.Consumer{}, consumerConfig{}, rbacToken{}, false
	}
	token, err := parseRBACToken(rawToken)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid rbac token: parse failed")
		return resource.Consumer{}, consumerConfig{}, rbacToken{}, false
	}
	consumer, cfg, err := p.consumerByAppID(token.AppID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "appid not found")
		return resource.Consumer{}, consumerConfig{}, rbacToken{}, false
	}
	return consumer, cfg, token, true
}

type wolfPublicResponse struct {
	OK   bool `json:"ok"`
	Data struct {
		Token    string         `json:"token"`
		UserInfo map[string]any `json:"userInfo"`
	} `json:"data"`
}

func (p *Plugin) requestWolf(
	r *http.Request,
	cfg consumerConfig,
	method string,
	path string,
	wolfToken string,
	body map[string]any,
) (wolfPublicResponse, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return wolfPublicResponse{}, err
	}
	req, err := http.NewRequestWithContext(
		r.Context(), method, strings.TrimRight(cfg.server(), "/")+path, strings.NewReader(string(encoded)),
	)
	if err != nil {
		return wolfPublicResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if wolfToken != "" {
		req.Header.Set("X-Rbac-Token", wolfToken)
	}
	client := *p.clientForConfig(cfg)
	client.Timeout = 5 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return wolfPublicResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return wolfPublicResponse{}, fmt.Errorf("wolf server returned %d", resp.StatusCode)
	}
	var result wolfPublicResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return wolfPublicResponse{}, err
	}
	return result, nil
}

func requestArguments(r *http.Request) (map[string]any, error) {
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var args map[string]any
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			return nil, err
		}
		return args, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	args := make(map[string]any, len(r.PostForm))
	for key, values := range r.PostForm {
		if len(values) > 0 {
			args[key] = values[0]
		}
	}
	return args, nil
}
