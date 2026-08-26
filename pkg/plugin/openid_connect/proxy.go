package openid_connect

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/wklken/apisix-go/pkg/httpclient"
)

func (p *Plugin) transport() http.RoundTripper {
	transport := httpclient.NewTransport()
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if p.httpProxy != nil || p.httpsProxy != nil {
		transport.Proxy = p.proxyForRequest
	}
	return transport
}

func (p *Plugin) configureProxy() error {
	if p.config.ProxyOpts == nil {
		return nil
	}

	var err error
	p.httpProxy, err = parseProxyURL(p.config.ProxyOpts.HTTPProxy, p.config.ProxyOpts.HTTPProxyAuthorization)
	if err != nil {
		return fmt.Errorf("invalid proxy_opts.http_proxy: %w", err)
	}
	p.httpsProxy, err = parseProxyURL(p.config.ProxyOpts.HTTPSProxy, p.config.ProxyOpts.HTTPSProxyAuthorization)
	if err != nil {
		return fmt.Errorf("invalid proxy_opts.https_proxy: %w", err)
	}
	for host := range strings.SplitSeq(p.config.ProxyOpts.NoProxy, ",") {
		if host = strings.TrimSpace(strings.ToLower(host)); host != "" {
			p.noProxy = append(p.noProxy, strings.TrimPrefix(host, "."))
		}
	}
	return nil
}

func parseProxyURL(rawURL, authorization string) (*url.URL, error) {
	if rawURL == "" {
		return nil, nil
	}
	proxyURL, err := url.Parse(rawURL)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		if err == nil {
			err = errors.New("proxy URL must include scheme and host")
		}
		return nil, err
	}
	if authorization == "" {
		return proxyURL, nil
	}
	parts := strings.SplitN(authorization, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Basic") {
		return nil, errors.New("proxy authorization must use Basic credentials")
	}
	credentials, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode proxy authorization: %w", err)
	}
	username, password, found := strings.Cut(string(credentials), ":")
	if !found {
		return nil, errors.New("proxy authorization must contain username and password")
	}
	proxyURL.User = url.UserPassword(username, password)
	return proxyURL, nil
}

func (p *Plugin) proxyForRequest(request *http.Request) (*url.URL, error) {
	host := strings.ToLower(request.URL.Hostname())
	for _, bypassHost := range p.noProxy {
		if bypassHost == "*" || host == bypassHost || strings.HasSuffix(host, "."+bypassHost) {
			return nil, nil
		}
	}
	if request.URL.Scheme == "https" {
		return p.httpsProxy, nil
	}
	return p.httpProxy, nil
}

func clearOutputHeaders(r *http.Request) {
	r.Header.Del("X-Access-Token")
	r.Header.Del("X-Userinfo")
	r.Header.Del("X-ID-Token")
	r.Header.Del("X-Refresh-Token")
}

func tokenActive(claims map[string]any) bool {
	active, ok := claims["active"].(bool)
	return ok && active
}

func locallyVerifiedTokenActive(claims map[string]any) bool {
	active, ok := claims["active"]
	if !ok {
		return true
	}
	switch value := active.(type) {
	case bool:
		return value
	case string:
		return value == "true"
	default:
		return false
	}
}

func requiredScopesPresent(required []string, claims map[string]any) bool {
	if len(required) == 0 {
		return true
	}

	available := map[string]struct{}{}
	if scope, ok := claims["scope"].(string); ok {
		for item := range strings.FieldsSeq(scope) {
			available[item] = struct{}{}
		}
	}
	for _, scope := range required {
		if _, ok := available[scope]; !ok {
			return false
		}
	}
	return true
}
