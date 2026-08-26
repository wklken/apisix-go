package wolf_rbac

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	projectjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client         *http.Client
	insecureClient *http.Client
	registry       *public_api.Registry
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

var errWolfConsumerNotFound = errors.New("wolf-rbac consumer not found")

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
	if p.config.SSLVerify == nil {
		p.config.SSLVerify = new(true)
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: 10 * time.Second}
	}
	if p.insecureClient == nil {
		p.insecureClient = insecureWolfClient(p.client)
	}
	if strings.HasPrefix(strings.ToLower(p.config.Server), "http://") {
		logger.Warn("Using wolf-rbac server with no TLS is a security risk")
	}
	if p.registry == nil {
		p.registry = public_api.NewRegistry()
	}
	if err := p.registry.ClaimOwner(name, p.publicAPIConfigIdentity()); err != nil {
		return err
	}
	p.registry.Register(http.MethodPost, WolfLoginURI, http.HandlerFunc(p.handleLogin))
	p.registry.Register(http.MethodPut, WolfChangePasswordURI, http.HandlerFunc(p.handleChangePassword))
	p.registry.Register(http.MethodGet, WolfUserInfoURI, http.HandlerFunc(p.handleUserInfo))
	return nil
}

func (p *Plugin) publicAPIConfigIdentity() string {
	// Public endpoints use only these route-level fallbacks; appid and header
	// prefix affect the protected request path, not the public handlers.
	return fmt.Sprintf(
		"server=%q ssl_verify=%t",
		p.config.Server,
		p.config.SSLVerify != nil && *p.config.SSLVerify,
	)
}

// SetPublicAPIRegistry injects the registry owned by the route generation
// before PostInit registers Wolf's public endpoints.
func (p *Plugin) SetPublicAPIRegistry(registry *public_api.Registry) {
	p.registry = registry
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return base.AdaptRequestPhase(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx.AttachConsumerFromAuthenticationState(r)
		next.ServeHTTP(w, r)
	}))
}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	prefix := p.config.HeaderPrefix
	if prefix == "" {
		prefix = "X-"
	}
	clearUserHeaders(r, prefix)
	clearResponseHeaders(w, prefix)

	rawToken := fetchRBACToken(r)
	if rawToken == "" {
		_ = util.WriteJSONMessage(w, http.StatusUnauthorized, "Missing rbac token in request")
		return base.StopRequest(r)
	}

	token, err := parseRBACToken(rawToken)
	if err != nil {
		_ = util.WriteJSONMessage(w, http.StatusUnauthorized, "invalid rbac token: parse failed")
		return base.StopRequest(r)
	}

	consumer, cfg, err := p.consumerByAppID(token.AppID)
	if err != nil {
		logger.Errorf("consumer [%s] not found", token.AppID)
		_ = util.WriteJSONMessage(w, http.StatusUnauthorized, "Invalid appid in rbac token")
		return base.StopRequest(r)
	}
	clearUserHeaders(r, cfg.headerPrefix())
	clearResponseHeaders(w, cfg.headerPrefix())

	status, reason, userInfo, err := p.checkPermission(r, cfg, token)
	if err != nil {
		_ = util.WriteJSONMessage(w, http.StatusInternalServerError, err.Error())
		return base.StopRequest(r)
	}
	if status != http.StatusOK {
		if reason == "" {
			reason = http.StatusText(status)
		}
		logger.Errorf("wolf-rbac permission denied, status:%d, reason:%s", status, reason)
		_ = util.WriteJSONMessage(w, status, reason)
		return base.StopRequest(r)
	}
	if err := p.setUserHeaders(w, r, cfg.headerPrefix(), userInfo); err != nil {
		_ = util.WriteJSONMessage(w, http.StatusInternalServerError, err.Error())
		return base.StopRequest(r)
	}

	return base.ContinueRequest(ctx.WithAuthenticationState(r, ctx.NewAuthenticationState(name, consumer)))
}

func fetchRBACToken(r *http.Request) string {
	ctx.RegisterSensitiveQueryName(r, "rbac_token")
	rbacHeaderToken := ctx.RestoreTrustedRequestHeader(r, "X-Rbac-Token")
	if token := r.URL.Query().Get("rbac_token"); token != "" {
		return token
	}
	if token := r.Header.Get("Authorization"); token != "" {
		return token
	}
	if rbacHeaderToken != "" {
		return rbacHeaderToken
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
	consumer, ok := p.consumerRecordByAppID(appID)
	if !ok {
		return resource.Consumer{}, consumerConfig{}, errWolfConsumerNotFound
	}

	raw, ok := consumer.Plugins[name]
	if !ok {
		return resource.Consumer{}, consumerConfig{}, errWolfConsumerNotFound
	}
	var cfg consumerConfig
	if err := util.Parse(raw, &cfg); err != nil {
		return resource.Consumer{}, consumerConfig{}, err
	}
	cfg.applyDefaults(p.config)
	return consumer, cfg, nil
}

func (p *Plugin) consumerRecordByAppID(appID string) (resource.Consumer, bool) {
	lookup := p.ConsumerLookup()
	if lookup == nil {
		return resource.Consumer{}, false
	}
	return lookup.ConsumerByPluginKey(name, appID)
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
	values.Set("clientIP", remoteClientIP(r))

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
	if err := projectjson.NewDecoder(resp.Body).Decode(&body); err != nil {
		if resp.StatusCode == http.StatusOK {
			return http.StatusBadGateway, "check permission failed! parse response json failed!", nil, nil
		}
		return resp.StatusCode, "check permission failed! parse response json failed!", nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, body.Reason, body.Data.UserInfo, nil
	}
	if !body.OK {
		reason := strings.TrimSpace(body.Reason)
		if reason == "" {
			reason = "permission denied"
		}
		return http.StatusForbidden, reason, nil, nil
	}
	if err := validateUserInfo(body.Data.UserInfo); err != nil {
		return http.StatusForbidden, err.Error(), nil, nil
	}
	return resp.StatusCode, body.Reason, body.Data.UserInfo, nil
}

func remoteClientIP(r *http.Request) string {
	return ctx.EffectiveRemoteIP(r)
}

func (p *Plugin) clientForConfig(cfg consumerConfig) *http.Client {
	if cfg.SSLVerify == nil || *cfg.SSLVerify {
		return p.client
	}
	// The insecure client is immutable and shared: build it once instead of
	// cloning the transport on every request.
	return p.insecureClient
}

// insecureWolfClient returns a shared client whose transport skips TLS
// certificate verification, mirroring the previous per-request clone but built
// once so requests never allocate a transport.
func insecureWolfClient(base *http.Client) *http.Client {
	client := *base
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

func (p *Plugin) setUserHeaders(w http.ResponseWriter, r *http.Request, prefix string, userInfo map[string]any) error {
	if len(userInfo) == 0 {
		return fmt.Errorf("wolf-rbac userinfo is missing")
	}

	userID, err := identityFieldString(userInfo["id"], "id")
	if err != nil {
		return err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("wolf-rbac userinfo field %q is missing", "id")
	}
	username, err := identityFieldString(userInfo["username"], "username")
	if err != nil {
		return err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("wolf-rbac userinfo field %q is missing", "username")
	}
	nickname := username
	if userInfo["nickname"] != nil {
		nickname, err = identityFieldString(userInfo["nickname"], "nickname")
		if err != nil {
			return err
		}
		nickname = strings.TrimSpace(nickname)
		if nickname == "" {
			nickname = username
		}
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
	return nil
}

func validateUserInfo(userInfo map[string]any) error {
	if len(userInfo) == 0 {
		return fmt.Errorf("wolf-rbac userinfo is missing")
	}

	userID, err := identityFieldString(userInfo["id"], "id")
	if err != nil {
		return err
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("wolf-rbac userinfo field %q is missing", "id")
	}
	username, err := identityFieldString(userInfo["username"], "username")
	if err != nil {
		return err
	}
	if strings.TrimSpace(username) == "" {
		return fmt.Errorf("wolf-rbac userinfo field %q is missing", "username")
	}
	if nickname, exists := userInfo["nickname"]; exists && nickname != nil {
		if _, err := identityFieldString(nickname, "nickname"); err != nil {
			return err
		}
	}
	return nil
}

func clearUserHeaders(r *http.Request, prefix string) {
	for _, suffix := range []string{"UserId", "Username", "Nickname"} {
		r.Header.Del(prefix + suffix)
	}
}

func clearResponseHeaders(w http.ResponseWriter, prefix string) {
	for _, suffix := range []string{"UserId", "Username", "Nickname"} {
		w.Header().Del(prefix + suffix)
	}
}

func identityFieldString(value any, name string) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case nil:
		return "", nil
	case int, int64, uint64, float64:
		return fmt.Sprint(v), nil
	default:
		return "", fmt.Errorf("wolf-rbac userinfo field %q has unsupported type %T", name, value)
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

func (p *Plugin) handleLogin(w http.ResponseWriter, r *http.Request) {
	args, err := requestArguments(r)
	if err != nil {
		_ = util.WriteJSONMessage(w, http.StatusBadRequest, "invalid request")
		return
	}
	appid, _ := args["appid"].(string)
	if appid == "" {
		_ = util.WriteJSONMessage(w, http.StatusBadRequest, "appid is missing")
		return
	}
	_, cfg, err := p.consumerByAppID(appid)
	if err != nil {
		_ = util.WriteJSONMessage(w, http.StatusBadRequest, "appid not found")
		return
	}
	response, err := p.requestWolf(r, cfg, http.MethodPost, "/wolf/rbac/login.rest", "", args)
	if !writeWolfPublicFailure(w, response, err) {
		return
	}
	_ = util.WriteJSON(w, http.StatusOK, map[string]any{
		"rbac_token": "V1#" + appid + "#" + response.Data.Token,
		"user_info":  response.Data.UserInfo,
	})
}

func (p *Plugin) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	args, err := requestArguments(r)
	if err != nil {
		_ = util.WriteJSONMessage(w, http.StatusBadRequest, "invalid request")
		return
	}
	_, cfg, token, ok := p.publicAPIToken(w, r)
	if !ok {
		return
	}
	response, err := p.requestWolf(r, cfg, http.MethodPost, "/wolf/rbac/change_pwd", token.WolfToken, args)
	if !writeWolfPublicFailure(w, response, err) {
		return
	}
	_ = util.WriteJSON(w, http.StatusOK, map[string]any{"message": "success to change password"})
}

func (p *Plugin) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	_, cfg, token, ok := p.publicAPIToken(w, r)
	if !ok {
		return
	}
	response, err := p.requestWolf(r, cfg, http.MethodGet, "/wolf/rbac/user_info", token.WolfToken, map[string]any{})
	if !writeWolfPublicFailure(w, response, err) {
		return
	}
	_ = util.WriteJSON(w, http.StatusOK, map[string]any{"user_info": response.Data.UserInfo})
}

func (p *Plugin) publicAPIToken(
	w http.ResponseWriter,
	r *http.Request,
) (resource.Consumer, consumerConfig, rbacToken, bool) {
	rawToken := fetchRBACToken(r)
	if rawToken == "" {
		_ = util.WriteJSONMessage(w, http.StatusUnauthorized, "Missing rbac token in request")
		return resource.Consumer{}, consumerConfig{}, rbacToken{}, false
	}
	token, err := parseRBACToken(rawToken)
	if err != nil {
		_ = util.WriteJSONMessage(w, http.StatusUnauthorized, "invalid rbac token: parse failed")
		return resource.Consumer{}, consumerConfig{}, rbacToken{}, false
	}
	consumer, cfg, err := p.consumerByAppID(token.AppID)
	if err != nil {
		_ = util.WriteJSONMessage(w, http.StatusBadRequest, "appid not found")
		return resource.Consumer{}, consumerConfig{}, rbacToken{}, false
	}
	return consumer, cfg, token, true
}

type wolfPublicResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	Data   struct {
		Token    string         `json:"token"`
		UserInfo map[string]any `json:"userInfo"`
	} `json:"data"`
}

func writeWolfPublicFailure(w http.ResponseWriter, response wolfPublicResponse, err error) bool {
	if err != nil {
		logger.Errorf("request to wolf-server failed: %s", err)
		_ = util.WriteJSONMessage(w, http.StatusInternalServerError, "request to wolf-server failed!")
		return false
	}
	if !response.OK {
		logger.Errorf("request to wolf-server failed! reason: %s", response.Reason)
		_ = util.WriteJSONMessage(w, http.StatusOK, "request to wolf-server failed!")
		return false
	}
	return true
}

func (p *Plugin) requestWolf(
	r *http.Request,
	cfg consumerConfig,
	method string,
	path string,
	wolfToken string,
	body map[string]any,
) (wolfPublicResponse, error) {
	encoded, err := projectjson.Marshal(body)
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
	if err := projectjson.NewDecoder(resp.Body).Decode(&result); err != nil {
		return wolfPublicResponse{}, err
	}
	return result, nil
}

func requestArguments(r *http.Request) (map[string]any, error) {
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var args map[string]any
		if err := projectjson.NewDecoder(r.Body).Decode(&args); err != nil {
			return nil, err
		}
		return args, nil
	}
	body, err := base.ReadRequestBody(r)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	args := make(map[string]any, len(values))
	for key, values := range values {
		if len(values) > 0 {
			args[key] = values[0]
		}
	}
	return args, nil
}
