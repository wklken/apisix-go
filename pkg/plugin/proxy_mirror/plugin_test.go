package proxy_mirror

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/util"
)

type mirrorRequest struct {
	Method string
	Path   string
	Query  string
	Host   string
	Header http.Header
	Body   string
}

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

func newMirrorServer(t *testing.T) (*httptest.Server, <-chan mirrorRequest) {
	t.Helper()

	seen := make(chan mirrorRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read mirror body: %v", err)
		}
		seen <- mirrorRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Host:   r.Host,
			Header: r.Header.Clone(),
			Body:   string(body),
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	return server, seen
}

func TestHandlerMirrorsRequestAndPreservesUpstreamBody(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{
		Host: mirror.URL,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/original?x=1", strings.NewReader("payload"))
	req.Header.Set("X-Test", "yes")
	req.Host = "original.example"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RunBeforeProxyHooks(r)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if got := string(body); got != "payload" {
			t.Fatalf("upstream body = %q, want payload", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}

	mirrored := waitForMirror(t, seen)
	if mirrored.Method != http.MethodPost {
		t.Fatalf("mirror method = %q, want POST", mirrored.Method)
	}
	if mirrored.Path != "/original" || mirrored.Query != "x=1" {
		t.Fatalf("mirror target = %s?%s, want /original?x=1", mirrored.Path, mirrored.Query)
	}
	if mirrored.Body != "payload" {
		t.Fatalf("mirror body = %q, want payload", mirrored.Body)
	}
	if got := mirrored.Header.Get("X-Test"); got != "yes" {
		t.Fatalf("mirror X-Test = %q, want yes", got)
	}
	if mirrored.Host != "original.example" {
		t.Fatalf("mirror Host = %q, want original.example", mirrored.Host)
	}
}

func TestHandlerMirrorsFinalizedRequestAfterLowerPriorityPlugins(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{Host: mirror.URL})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/original", strings.NewReader("before"))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/final"
		r.Body = io.NopCloser(strings.NewReader("after"))
		apisixctx.RunBeforeProxyHooks(r)
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	mirrored := waitForMirror(t, seen)
	if mirrored.Path != "/final" || mirrored.Body != "after" {
		t.Fatalf("mirrored finalized request = %s %q, want /final after", mirrored.Path, mirrored.Body)
	}
}

func TestPostInitConfiguresHTTP2Transport(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "https://mirror.example.com"})

	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", p.client.Transport)
	}
	if len(transport.TLSNextProto) == 0 {
		t.Fatal("client transport has no configured HTTP/2 protocol")
	}
}

func TestHandlerMirrorsUnaryGRPCOverHTTP2(t *testing.T) {
	seen := make(chan struct{}, 1)
	mirror := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("mirror protocol = HTTP/%d, want HTTP/2", r.ProtoMajor)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Errorf("mirror content type = %q, want application/grpc", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read mirrored gRPC body: %v", err)
		}
		if got := string(body); got != "\x00\x00\x00\x00\x03abc" {
			t.Errorf("mirrored gRPC body = %q, want unary frame", got)
		}
		seen <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	mirror.EnableHTTP2 = true
	mirror.StartTLS()
	t.Cleanup(mirror.Close)

	p := newTestPlugin(t, Config{Host: mirror.URL})
	p.client = mirror.Client()

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/greeter.SayHello",
		strings.NewReader("\x00\x00\x00\x00\x03abc"),
	)
	req.Header.Set("Content-Type", "application/grpc")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RunBeforeProxyHooks(r)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read main gRPC body: %v", err)
		}
		if got := string(body); got != "\x00\x00\x00\x00\x03abc" {
			t.Fatalf("main gRPC body = %q, want unary frame", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
	select {
	case <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mirrored gRPC request")
	}
}

func TestHandlerReplacesMirrorPath(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{
		Host: mirror.URL,
		Path: "/shadow",
	})

	performRequest(p, "http://example.com/original?x=1")

	mirrored := waitForMirror(t, seen)
	if mirrored.Path != "/shadow" || mirrored.Query != "x=1" {
		t.Fatalf("mirror target = %s?%s, want /shadow?x=1", mirrored.Path, mirrored.Query)
	}
}

func TestHandlerPrefixesMirrorPath(t *testing.T) {
	mirror, seen := newMirrorServer(t)
	defer mirror.Close()

	p := newTestPlugin(t, Config{
		Host:           mirror.URL,
		Path:           "/shadow",
		PathConcatMode: "prefix",
	})

	performRequest(p, "http://example.com/original?x=1")

	mirrored := waitForMirror(t, seen)
	if mirrored.Path != "/shadow/original" || mirrored.Query != "x=1" {
		t.Fatalf("mirror target = %s?%s, want /shadow/original?x=1", mirrored.Path, mirrored.Query)
	}
}

func TestSchemaRejectsHostWithPath(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{"host": "http://mirror.example.com/base"}, p.GetSchema())
	if err == nil {
		t.Fatal("schema accepted host with path, want rejection")
	}
}

func TestSchemaRejectsPathWithQueryDelimiter(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{
		"host": "http://mirror.example.com",
		"path": "/shadow?debug=true",
	}, p.GetSchema())
	if err == nil {
		t.Fatal("schema accepted path with query delimiter, want rejection")
	}
}

func TestSchemaAcceptsOfficialHTTPAndHTTPSHosts(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, host := range []string{
		"http://mirror.example.com",
		"https://mirror.example.com:9443",
		"http://127.0.0.1:9080",
		"http://[2001:db8::1]:9080",
		"grpc://mirror.example.com:9080",
		"grpcs://mirror.example.com:9443",
	} {
		t.Run(host, func(t *testing.T) {
			err := util.Validate(map[string]any{
				"host": host,
				"path": "/shadow",
			}, p.GetSchema())
			if err != nil {
				t.Fatalf("schema rejected %s: %v", host, err)
			}
		})
	}
}

func performRequest(p *Plugin, rawURL string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RunBeforeProxyHooks(r)
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	return rr
}

func waitForMirror(t *testing.T, seen <-chan mirrorRequest) mirrorRequest {
	t.Helper()

	select {
	case req := <-seen:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mirrored request")
		return mirrorRequest{}
	}
}
