package chaitin_waf

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/store"
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
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:                 "block",
		AppendWAFRespHeader:  new(true),
		AppendWAFDebugHeader: new(true),
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

func TestHandlerSendsBoundedBodyToWAFAndPreservesFullBodyForUpstream(t *testing.T) {
	var wafBody []byte
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		wafBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read WAF body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:   "block",
		Nodes:  []Node{nodeFromURL(t, waf.URL)},
		Config: WAFConfig{ReqBodySize: 1},
	})
	fullBody := bytes.Repeat([]byte("a"), 2*1024)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", bytes.NewReader(fullBody))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if !bytes.Equal(got, fullBody) {
			t.Fatalf("upstream body length = %d, want %d", len(got), len(fullBody))
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if len(wafBody) != 1024 {
		t.Fatalf("WAF body length = %d, want 1024", len(wafBody))
	}
}

func TestHandlerSendsNoBodyToWAFForNegativeInspectionLimit(t *testing.T) {
	var wafBody []byte
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		wafBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read WAF body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:   "block",
		Nodes:  []Node{nodeFromURL(t, waf.URL)},
		Config: WAFConfig{ReqBodySize: -1},
	})
	fullBody := []byte("complete request body")
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", bytes.NewReader(fullBody))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if !bytes.Equal(got, fullBody) {
			t.Fatalf("upstream body = %q, want %q", got, fullBody)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if len(wafBody) != 0 {
		t.Fatalf("WAF body length = %d, want 0", len(wafBody))
	}
}

func TestHandlerBlocksRejectedRequest(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusForbidden, EventID: "evt-1"})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:                 "block",
		AppendWAFRespHeader:  new(true),
		AppendWAFDebugHeader: new(false),
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

func TestHandlerBlocksRejectedRequestWithoutEventID(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusForbidden})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:                "block",
		AppendWAFRespHeader: new(true),
		Nodes:               []Node{nodeFromURL(t, waf.URL)},
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called for blocked request")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if rr.Header().Get(HeaderChaitinWAFAction) != "reject" {
		t.Fatalf("action = %q, want reject", rr.Header().Get(HeaderChaitinWAFAction))
	}
}

func TestHandlerRejectsNonTerminalWAFStatus(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wafDecision{Status: 600, EventID: "evt-bad"})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:                "block",
		AppendWAFRespHeader: new(true),
		Nodes:               []Node{nodeFromURL(t, waf.URL)},
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called for invalid WAF status")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestHandlerMonitorModeDoesNotBlockRejectedRequest(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusForbidden, EventID: "evt-2"})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:                "monitor",
		AppendWAFRespHeader: new(true),
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
		AppendWAFRespHeader: new(true),
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
		Mode:                "monitor",
		AppendWAFRespHeader: new(true),
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

func TestHandlerSupportsNestedMatchExpression(t *testing.T) {
	wafCalls := 0
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wafCalls++
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:  "monitor",
		Nodes: []Node{nodeFromURL(t, waf.URL)},
		Match: []MatchRule{{Vars: []any{
			"AND",
			[]any{"method", "in", []any{"POST", "PUT"}},
			[]any{"http_x_env", "~*", "^prod$"},
			[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
		}}},
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", nil)
	req.RemoteAddr = "192.0.2.40:1234"
	req.Header.Set("X-Env", "PrOd")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if wafCalls != 1 {
		t.Fatalf("waf calls = %d, want 1 for matching expression", wafCalls)
	}
}

func TestPostInitRejectsInvalidMatchExpression(t *testing.T) {
	p := &Plugin{config: Config{Match: []MatchRule{{Vars: []any{
		[]any{"method", "bogus", "POST"},
	}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid match expression rejected")
	}
}

func TestValidateMetadataRejectsInvalidNodes(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{name: "empty nodes", metadata: map[string]any{"nodes": []any{}}},
		{name: "missing host", metadata: map[string]any{"nodes": []any{map[string]any{"port": 8000}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateMetadata(test.metadata); err == nil {
				t.Fatal("validateMetadata() error = nil, want invalid nodes rejected")
			}
		})
	}
}

func TestHandlerMovesPastFailedWAFNode(t *testing.T) {
	healthyCalls := 0
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyCalls++
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(healthy.Close)

	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("failed WAF node unexpectedly received a second request")
	}))
	failedURL := failed.URL
	failed.Close()

	p := newTestPlugin(t, Config{
		Mode:                "monitor",
		AppendWAFRespHeader: new(true),
		Nodes:               []Node{nodeFromURL(t, failedURL), nodeFromURL(t, healthy.URL)},
	})

	for i := range 2 {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader("a=1"))
		res := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(res, req)
		if res.Code != http.StatusNoContent {
			t.Fatalf("request %d status = %d, want 204", i+1, res.Code)
		}
	}

	if healthyCalls != 1 {
		t.Fatalf("healthy WAF calls = %d, want 1 after failed node is quarantined", healthyCalls)
	}
}

func TestHandlerTimesOutWAFNodeInMonitorMode(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:  "monitor",
		Nodes: []Node{nodeFromURL(t, waf.URL)},
		Config: WAFConfig{
			ReadTimeout: 5,
		},
	})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 in monitor mode; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get(HeaderChaitinWAF) != "timeout" {
		t.Fatalf("waf header = %q, want timeout", rr.Header().Get(HeaderChaitinWAF))
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

	p = newTestPlugin(t, Config{Config: WAFConfig{RealClientIP: new(false)}})
	if p.config.Config.RealClientIP == nil || *p.config.Config.RealClientIP {
		t.Fatal("real_client_ip = true, want explicit false")
	}
}

func TestClientIPDoesNotTrustForwardingHeadersWithoutRealIPContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.40")
	req.Header.Set("X-Real-IP", "198.51.100.41")

	if got := clientIP(req, true); got != "10.0.0.2" {
		t.Fatalf("clientIP(real_client_ip=true) = %q, want socket peer", got)
	}
	if got := clientIP(req, false); got != "10.0.0.2" {
		t.Fatalf("clientIP(real_client_ip=false) = %q, want socket peer", got)
	}

	req = req.WithContext(context.WithValue(req.Context(), apisixctx.RemoteAddrKey, "203.0.113.9"))
	if got := clientIP(req, true); got != "203.0.113.9" {
		t.Fatalf("clientIP(trusted real-ip context) = %q, want 203.0.113.9", got)
	}
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

var (
	chaitinMetadataOnce   sync.Once
	chaitinMetadataStore  *store.Store
	chaitinMetadataEvents chan *store.Event
)

func putChaitinMetadata(t *testing.T, nodes []any) {
	t.Helper()
	chaitinMetadataOnce.Do(func() {
		chaitinMetadataEvents = make(chan *store.Event, 8)
		var err error
		chaitinMetadataStore, err = store.GetStore(
			t.TempDir()+"/chaitin-waf.db",
			chaitinMetadataEvents,
			data_encryption.NewService(false, nil),
		)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		chaitinMetadataStore.Start()
	})
	body, err := json.Marshal(map[string]any{"nodes": nodes})
	if err != nil {
		t.Fatalf("marshal chaitin metadata: %v", err)
	}
	chaitinMetadataEvents <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/plugin_metadata/chaitin-waf"),
		Value: body,
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := store.GetPluginMetadataRaw(name)
		if err == nil && bytes.Equal(raw, body) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for chaitin-waf plugin metadata")
}

func TestHandlerRebuildsMetadataOnlyWhenConfigurationChanges(t *testing.T) {
	firstWAF := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(firstWAF.Close)
	secondWAF := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(secondWAF.Close)

	putChaitinMetadata(t, []any{map[string]any{"host": "127.0.0.1", "port": firstWAFPort(t, firstWAF)}})
	p := newTestPlugin(t, Config{Mode: "block"})

	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/check", nil)
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(rr, req)
		return rr
	}

	if rr := request(); rr.Code != http.StatusNoContent {
		t.Fatalf("first request code = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if rr := request(); rr.Code != http.StatusNoContent {
		t.Fatalf("second request code = %d, want 204", rr.Code)
	}

	p.metaMu.Lock()
	builds := p.metaBuilds
	p.metaMu.Unlock()
	if builds != 1 {
		t.Fatalf("metadata builds after two stable requests = %d, want 1", builds)
	}

	putChaitinMetadata(t, []any{map[string]any{"host": "127.0.0.1", "port": secondWAFPort(t, secondWAF)}})
	if rr := request(); rr.Code != http.StatusNoContent {
		t.Fatalf("request after metadata change code = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	p.metaMu.Lock()
	builds = p.metaBuilds
	p.metaMu.Unlock()
	if builds != 2 {
		t.Fatalf("metadata builds after one change = %d, want 2", builds)
	}
}

func TestHandlerConcurrentRequestsShareStableMetadata(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(waf.Close)

	putChaitinMetadata(t, []any{map[string]any{"host": "127.0.0.1", "port": firstWAFPort(t, waf)}})
	p := newTestPlugin(t, Config{Mode: "block"})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "http://example.com/check", nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Errorf("concurrent request code = %d, want 204", rr.Code)
			}
		})
	}
	wg.Wait()

	p.metaMu.Lock()
	builds := p.metaBuilds
	p.metaMu.Unlock()
	if builds != 1 {
		t.Fatalf("metadata builds under concurrency = %d, want 1", builds)
	}
}

func firstWAFPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("split WAF address: %v", err)
	}
	parsed, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse WAF port: %v", err)
	}
	return parsed
}

func secondWAFPort(t *testing.T, server *httptest.Server) int {
	return firstWAFPort(t, server)
}
