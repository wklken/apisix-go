package server

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/version"
)

func TestDeleteURITailSlashRunsBeforeRouteMatchingAndPreservesRoot(t *testing.T) {
	cfg := &config.Config{Apisix: config.Apisix{DeleteURITailSlash: true}}

	var gotPaths []string
	handler := newConfiguredHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}), cfg)
	for _, path := range []string{"/orders/", "/"} {
		request := httptest.NewRequest(
			http.MethodGet,
			"http://gateway.test"+path+"?page=1",
			nil,
		)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}

	if len(gotPaths) != 2 || gotPaths[0] != "/orders" || gotPaths[1] != "/" {
		t.Fatalf("paths observed by route matcher = %#v, want [/orders /]", gotPaths)
	}
}

func TestConfiguredHTTPHandlerOverwritesOriginServerHeaderWithAPISIXToken(t *testing.T) {
	previousVersion := version.Version
	version.Version = "test-version"
	t.Cleanup(func() { version.Version = previousVersion })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "upstream-origin")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	localHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "local-origin")
		w.WriteHeader(http.StatusNoContent)
	})
	upstreamHandler := httputil.NewSingleHostReverseProxy(upstreamURL)

	for _, test := range []struct {
		name    string
		enabled bool
		want    string
		handler http.Handler
	}{
		{name: "local version enabled", enabled: true, want: "APISIX/test-version", handler: localHandler},
		{name: "local version hidden", enabled: false, want: "APISIX", handler: localHandler},
		{name: "upstream version enabled", enabled: true, want: "APISIX/test-version", handler: upstreamHandler},
		{name: "upstream version hidden", enabled: false, want: "APISIX", handler: upstreamHandler},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Apisix: config.Apisix{EnableServerTokens: test.enabled}}
			handler := newConfiguredHTTPHandler(test.handler, cfg)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))
			if got := response.Header().Values("Server"); len(got) != 1 || got[0] != test.want {
				t.Fatalf("Server headers = %q, want exactly [%q]", got, test.want)
			}
		})
	}
}

func TestConfiguredHTTPHandlerSetsSingleServerHeaderOnWebsocketUpgrade(t *testing.T) {
	previousVersion := version.Version
	version.Version = "test-version"
	t.Cleanup(func() { version.Version = previousVersion })

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseHeader := make(http.Header)
		responseHeader.Set("Server", "upstream-origin")
		connection, err := upgrader.Upgrade(w, r, responseHeader)
		if err != nil {
			t.Errorf("upstream websocket upgrade: %v", err)
			return
		}
		_ = connection.Close()
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	proxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Del("Server")
		return nil
	}
	handler := newConfiguredHTTPHandler(proxy, &config.Config{
		Apisix: config.Apisix{EnableServerTokens: true},
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	connection, response, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(gateway.URL, "http"),
		nil,
	)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	_ = connection.Close()
	if got := response.Header.Values("Server"); len(got) != 1 || got[0] != "APISIX/test-version" {
		t.Fatalf("Server headers = %q, want exactly [APISIX/test-version]", got)
	}
}

func TestConfiguredHTTPHandlerSetsServerHeaderBeforeStreamingFlush(t *testing.T) {
	previousVersion := version.Version
	version.Version = "test-version"
	t.Cleanup(func() { version.Version = previousVersion })

	handler := newConfiguredHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "local-origin")
		w.(http.Flusher).Flush()
	}), &config.Config{Apisix: config.Apisix{EnableServerTokens: true}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))

	if got := response.Header().Get("Server"); got != "APISIX/test-version" {
		t.Fatalf("Server header = %q, want APISIX/test-version", got)
	}
}
