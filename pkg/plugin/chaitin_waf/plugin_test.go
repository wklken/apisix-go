package chaitin_waf

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerPassesAllowedRequestAndRestoresBody(t *testing.T) {
	var wafPath string
	var wafBody string
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wafPath = r.URL.RequestURI()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read waf body: %v", err)
		}
		wafBody = string(body)
		json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:                 "block",
		AppendWAFRespHeader:  boolPtr(true),
		AppendWAFDebugHeader: boolPtr(true),
		Nodes:                []Node{nodeFromURL(t, waf.URL)},
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders?debug=1", strings.NewReader("a=1"))
	req.RemoteAddr = "198.51.100.2:12345"
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read restored body: %v", err)
		}
		if string(body) != "a=1" {
			t.Fatalf("restored body = %q, want original", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if wafPath != "/orders?debug=1" {
		t.Fatalf("waf path = %q, want original request URI", wafPath)
	}
	if wafBody != "a=1" {
		t.Fatalf("waf body = %q, want original body", wafBody)
	}
	if rr.Header().Get(HeaderChaitinWAF) != "yes" || rr.Header().Get(HeaderChaitinWAFAction) != "pass" {
		t.Fatalf("waf headers = %q/%q", rr.Header().Get(HeaderChaitinWAF), rr.Header().Get(HeaderChaitinWAFAction))
	}
	if rr.Header().Get(HeaderChaitinWAFServer) == "" {
		t.Fatal("debug server header is empty")
	}
}

func TestHandlerBlocksRejectedRequest(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wafDecision{Status: http.StatusForbidden, EventID: "evt-1"})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:                 "block",
		AppendWAFRespHeader:  boolPtr(true),
		AppendWAFDebugHeader: boolPtr(false),
		Nodes:                []Node{nodeFromURL(t, waf.URL)},
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader("a=1 and 1=1"))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for blocked request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"event_id": "evt-1"`) {
		t.Fatalf("body = %q, want event id", rr.Body.String())
	}
	if rr.Header().Get(HeaderChaitinWAFAction) != "reject" {
		t.Fatalf("action = %q, want reject", rr.Header().Get(HeaderChaitinWAFAction))
	}
	if rr.Header().Get(HeaderChaitinWAFServer) != "" {
		t.Fatalf("debug server header = %q, want hidden", rr.Header().Get(HeaderChaitinWAFServer))
	}
}

func TestHandlerMonitorModeDoesNotBlockRejectedRequest(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wafDecision{Status: http.StatusForbidden, EventID: "evt-2"})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:                "monitor",
		AppendWAFRespHeader: boolPtr(true),
		Nodes:               []Node{nodeFromURL(t, waf.URL)},
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader("a=1 and 1=1"))
	rr := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called in monitor mode")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	if rr.Header().Get(HeaderChaitinWAFAction) != "reject" || rr.Header().Get(HeaderChaitinWAFStatus) != "403" {
		t.Fatalf(
			"waf headers action/status = %q/%q",
			rr.Header().Get(HeaderChaitinWAFAction),
			rr.Header().Get(HeaderChaitinWAFStatus),
		)
	}
}

func TestHandlerOffAndNoMatchSkipWAF(t *testing.T) {
	wafCalls := 0
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wafCalls++
	}))
	t.Cleanup(waf.Close)

	offPlugin := newTestPlugin(t, Config{
		Mode:                "off",
		AppendWAFRespHeader: boolPtr(true),
		Nodes:               []Node{nodeFromURL(t, waf.URL)},
	})
	rr := httptest.NewRecorder()
	offPlugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://example.com/orders", nil))

	if wafCalls != 0 {
		t.Fatal("waf server was called in off mode")
	}
	if rr.Header().Get(HeaderChaitinWAF) != "off" {
		t.Fatalf("waf header = %q, want off", rr.Header().Get(HeaderChaitinWAF))
	}

	noMatchPlugin := newTestPlugin(t, Config{
		Mode:                "block",
		AppendWAFRespHeader: boolPtr(true),
		Nodes:               []Node{nodeFromURL(t, waf.URL)},
		Match: []MatchRule{
			{Vars: [][]any{{"method", "==", "POST"}}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	rr = httptest.NewRecorder()
	noMatchPlugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if wafCalls != 0 {
		t.Fatal("waf server was called for non-matching request")
	}
	if rr.Header().Get(HeaderChaitinWAF) != "no" {
		t.Fatalf("waf header = %q, want no", rr.Header().Get(HeaderChaitinWAF))
	}
}

func TestPostInitDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.Mode != "monitor" {
		t.Fatalf("mode = %q, want monitor", p.config.Mode)
	}
	if !*p.config.AppendWAFRespHeader {
		t.Fatal("append_waf_resp_header = false, want true")
	}
	if *p.config.AppendWAFDebugHeader {
		t.Fatal("append_waf_debug_header = true, want false")
	}
	if p.config.Config.RealClientIP == nil || !*p.config.Config.RealClientIP {
		t.Fatal("real_client_ip = false, want true")
	}

	p = newTestPlugin(t, Config{Config: WAFConfig{RealClientIP: boolPtr(false)}})
	if p.config.Config.RealClientIP == nil || *p.config.Config.RealClientIP {
		t.Fatal("real_client_ip = true, want explicit false")
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func nodeFromURL(t *testing.T, rawURL string) Node {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse waf url: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse waf port: %v", err)
	}
	return Node{Host: parsed.Hostname(), Port: port}
}
