package pluginintegration

import (
	"bufio"
	"bytes"
	cgzip "compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apisixcmd "github.com/wklken/apisix-go/cmd"
	"go.yaml.in/yaml/v3"
)

const helperProcessEnv = "APISIX_GO_INTEGRATION_HELPER"

func TestPluginIntegration(t *testing.T) {
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("discover plugin manifests: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no plugin manifests found")
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		manifest, err := loadManifest(path, data)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		pluginName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		for _, spec := range manifest.Cases {
			t.Run(pluginName+"/"+spec.Name, func(t *testing.T) {
				if len(spec.Variants) == 0 {
					runCase(t, spec)
					return
				}
				for i := range spec.Variants {
					variant := &spec.Variants[i]
					t.Run(variant.Name, func(t *testing.T) {
						runCase(t, *variant.caseSpec())
					})
				}
			})
		}
	}
}

func TestRenderRuntimeConfigForcesStandaloneIsolation(t *testing.T) {
	rendered, err := renderRuntimeConfig(19080, map[string]any{
		"apisix": map[string]any{
			"node_listen": 9443,
		},
		"deployment": map[string]any{
			"role": "traditional",
		},
		"plugin_attr": map[string]any{
			"redirect": map[string]any{"https_port": 10443},
		},
	})
	if err != nil {
		t.Fatalf("renderRuntimeConfig() error = %v", err)
	}

	var config map[string]any
	if err := yaml.Unmarshal(rendered, &config); err != nil {
		t.Fatalf("unmarshal runtime config: %v", err)
	}
	apisix := config["apisix"].(map[string]any)
	listen := apisix["node_listen"].([]any)
	address := listen[0].(map[string]any)
	if address["ip"] != "127.0.0.1" || address["port"] != 19080 {
		t.Fatalf("node_listen = %#v, want loopback:19080", listen)
	}
	deployment := config["deployment"].(map[string]any)
	if got := deployment["role"]; got != "data_plane" {
		t.Fatalf("deployment.role = %v, want data_plane", got)
	}
	roleDataPlane := deployment["role_data_plane"].(map[string]any)
	if got := roleDataPlane["config_provider"]; got != "yaml" {
		t.Fatalf("config_provider = %v, want yaml", got)
	}
	pluginAttr := config["plugin_attr"].(map[string]any)
	redirect := pluginAttr["redirect"].(map[string]any)
	if got := redirect["https_port"]; got != 10443 {
		t.Fatalf("https_port = %v, want 10443", got)
	}
	plugins, ok := config["plugins"].([]any)
	if !ok {
		t.Fatalf("plugins = %#v, want [prometheus] for request metrics initialization", config["plugins"])
	}
	if len(plugins) != 1 || plugins[0] != "prometheus" {
		t.Fatalf("plugins = %#v, want [prometheus] for request metrics initialization", plugins)
	}
	prometheus, ok := pluginAttr["prometheus"].(map[string]any)
	if !ok {
		t.Fatalf("plugin_attr.prometheus = %#v, want map", pluginAttr["prometheus"])
	}
	if got := prometheus["enable_export_server"]; got != false {
		t.Fatalf("prometheus.enable_export_server = %v, want false", got)
	}
}

func TestRenderRuntimeConfigPreservesRequiredPlugins(t *testing.T) {
	rendered, err := renderRuntimeConfig(19080, map[string]any{
		"plugins": []any{"node-status"},
	})
	if err != nil {
		t.Fatalf("renderRuntimeConfig() error = %v", err)
	}

	var config map[string]any
	if err := yaml.Unmarshal(rendered, &config); err != nil {
		t.Fatalf("unmarshal runtime config: %v", err)
	}
	plugins := config["plugins"].([]any)
	if got, want := fmt.Sprint(plugins), "[node-status prometheus]"; got != want {
		t.Fatalf("plugins = %s, want %s", got, want)
	}
}

func TestHarnessRunsStandaloneRoute(t *testing.T) {
	body := "ok"
	caseSpec := Case{
		Name:   "smoke",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "smoke",
					"uri": "/smoke",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{UPSTREAM_ADDR}}": 1},
					},
				},
			},
		},
		Input: HTTPInput{Method: "GET", Path: "/smoke"},
		Upstream: &UpstreamSpec{
			Respond: HTTPResponse{Status: 200, Body: body},
		},
		Output: HTTPOutput{
			Status: 200,
			Body:   &Matcher{Equals: &body},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessRunsRequestSequence(t *testing.T) {
	firstBody := "first"
	rejectedMessage := "too many requests"
	rejectedBody := `{"error_msg":"too many requests"}`
	caseSpec := Case{
		Name:   "sequence",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "sequence",
					"uri": "/sequence",
					"plugins": map[string]any{
						"limit-count": map[string]any{
							"count":         1,
							"time_window":   60,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
							"rejected_msg":  rejectedMessage,
						},
					},
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.primary.ADDR}}": 1},
					},
				},
			},
		},
		Fixtures: []FixtureSpec{
			{
				Name: "primary",
				Kind: "http",
				Respond: []HTTPResponse{
					{Status: http.StatusOK, Body: firstBody},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name:   "allowed",
				Input:  HTTPInput{Method: http.MethodGet, Path: "/sequence"},
				Output: HTTPOutput{Status: http.StatusOK, Body: &Matcher{Equals: &firstBody}},
			},
			{
				Name:   "rejected",
				Input:  HTTPInput{Method: http.MethodGet, Path: "/sequence"},
				Output: HTTPOutput{Status: http.StatusTooManyRequests, Body: &Matcher{Equals: &rejectedBody}},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessFixtureEchoesRequestBody(t *testing.T) {
	body := "echo me"
	caseSpec := Case{
		Name:   "fixture-echo",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "fixture-echo",
					"uri": "/echo",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.primary.ADDR}}": 1},
					},
				},
			},
		},
		Fixtures: []FixtureSpec{
			{
				Name: "primary",
				Kind: "http",
				Expect: []HTTPAssertion{
					{Body: &Matcher{Equals: &body}},
				},
				Respond: []HTTPResponse{
					{Status: http.StatusOK, EchoRequestBody: true},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name:  "echo",
				Input: HTTPInput{Method: http.MethodPost, Path: "/echo", Body: body},
				Output: HTTPOutput{
					Status: http.StatusOK,
					Body:   &Matcher{Equals: &body},
				},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessExpandsIterationPlaceholders(t *testing.T) {
	bodyTemplate := "body-{{ITERATION}}"
	firstBody := "body-1"
	secondBody := "body-2"
	firstPath := "/echo?iteration=1"
	secondPath := "/echo?iteration=2"
	firstIteration := "1"
	secondIteration := "2"
	caseSpec := Case{
		Name:   "iteration-placeholders",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "iteration-placeholders",
					"uri": "/echo",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.primary.ADDR}}": 1},
					},
				},
			},
		},
		Fixtures: []FixtureSpec{
			{
				Name: "primary",
				Kind: "http",
				Expect: []HTTPAssertion{
					{
						Path: &Matcher{Equals: &firstPath},
						Headers: map[string]Matcher{
							"X-Iteration": {Equals: &firstIteration},
						},
						Body: &Matcher{Equals: &firstBody},
					},
					{
						Path: &Matcher{Equals: &secondPath},
						Headers: map[string]Matcher{
							"X-Iteration": {Equals: &secondIteration},
						},
						Body: &Matcher{Equals: &secondBody},
					},
				},
				Respond: []HTTPResponse{
					{Status: http.StatusOK, EchoRequestBody: true},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name:   "repeat",
				Repeat: 2,
				Input: HTTPInput{
					Method: http.MethodPost,
					Path:   "/echo?iteration={{ITERATION}}",
					Headers: map[string]string{
						"X-Iteration": "{{ITERATION}}",
					},
					Body: bodyTemplate,
				},
				Output: HTTPOutput{
					Status: http.StatusOK,
					Body:   &Matcher{Equals: &bodyTemplate},
				},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessDoesNotBlockUnassertedFixtureCaptures(t *testing.T) {
	body := "body"
	caseSpec := Case{
		Name:   "unasserted-fixture-captures",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "unasserted-fixture-captures",
					"uri": "/echo",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.primary.ADDR}}": 1},
					},
				},
			},
		},
		Fixtures: []FixtureSpec{
			{
				Name: "primary",
				Kind: "http",
				Respond: []HTTPResponse{
					{Status: http.StatusOK, EchoRequestBody: true},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name:   "repeat",
				Repeat: 3,
				Input:  HTTPInput{Method: http.MethodPost, Path: "/echo", Body: body},
				Output: HTTPOutput{Status: http.StatusOK, Body: &Matcher{Equals: &body}},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessRepeatsStepsAndChecksGeneratedHeaders(t *testing.T) {
	caseSpec := Case{
		Name:   "repeat-generated-headers",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "repeat-generated-headers",
					"uri": "/repeat",
					"plugins": map[string]any{
						"request-id": map[string]any{"algorithm": "uuidv7"},
						"mocking": map[string]any{
							"response_status":  http.StatusOK,
							"response_example": "ok",
						},
					},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name:   "generated-ids",
				Repeat: 20,
				Input:  HTTPInput{Path: "/repeat"},
				Output: HTTPOutput{
					Status:           http.StatusOK,
					UniqueHeaders:    []string{"X-Request-Id"},
					MonotonicHeaders: []string{"X-Request-Id"},
				},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessReusesResponseCookiesInLaterSteps(t *testing.T) {
	cookiePattern := `apisix-csrf-token=[^;]+`
	caseSpec := Case{
		Name:   "response-cookie-sequence",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "response-cookie-sequence",
					"uri": "/csrf",
					"plugins": map[string]any{
						"csrf": map[string]any{"key": "userkey", "expires": 3600},
					},
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.primary.ADDR}}": 1},
					},
				},
			},
		},
		Fixtures: []FixtureSpec{
			{
				Name: "primary",
				Kind: "http",
				Respond: []HTTPResponse{
					{Status: http.StatusOK, Body: "ok"},
					{Status: http.StatusOK, Body: "ok"},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name:  "issue-cookie",
				Input: HTTPInput{Path: "/csrf"},
				Output: HTTPOutput{
					Status: http.StatusOK,
					Headers: map[string]Matcher{
						"Set-Cookie": {Matches: &cookiePattern},
					},
				},
			},
			{
				Name: "reuse-cookie",
				Input: HTTPInput{
					Method:  http.MethodPost,
					Path:    "/csrf",
					Headers: map[string]string{"apisix-csrf-token": "{{COOKIE.apisix-csrf-token}}"},
				},
				Output: HTTPOutput{Status: http.StatusOK},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessCapturesResponseHeaderForLaterStep(t *testing.T) {
	statePattern := `state=([^&]+)`
	firstPath := "/authorize"
	secondPath := "/callback?state=dynamic-state"
	done := "done"
	caseSpec := Case{
		Name:   "response-header-capture",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "response-header-capture",
					"uri": "/*",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.primary.ADDR}}": 1},
					},
				},
			},
		},
		Fixtures: []FixtureSpec{
			{
				Name: "primary",
				Kind: "http",
				Expect: []HTTPAssertion{
					{Path: &Matcher{Equals: &firstPath}},
					{Path: &Matcher{Equals: &secondPath}},
				},
				Respond: []HTTPResponse{
					{Status: http.StatusOK, Headers: map[string]string{"X-State": "state=dynamic-state"}},
					{Status: http.StatusOK, Body: done},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name:  "capture",
				Input: HTTPInput{Path: "/authorize"},
				Output: HTTPOutput{
					Status: http.StatusOK,
					Captures: map[string]HeaderCapture{
						"state": {Header: "X-State", Matches: statePattern},
					},
				},
			},
			{
				Name:   "reuse",
				Input:  HTTPInput{Path: "/callback?state={{CAPTURE.state}}"},
				Output: HTTPOutput{Status: http.StatusOK, Body: &Matcher{Equals: &done}},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessSendsRepeatedRequestHeaders(t *testing.T) {
	body := "ok"
	caseSpec := Case{
		Name:   "repeated-request-headers",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "repeated-request-headers",
					"uri": "/headers",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{UPSTREAM_ADDR}}": 1},
					},
				},
			},
		},
		Input: HTTPInput{
			Path: "/headers",
			HeaderValues: map[string][]string{
				"X-Repeated": {"first", "second"},
			},
		},
		Upstream: &UpstreamSpec{
			Expect: HTTPAssertion{Headers: map[string]Matcher{
				"X-Repeated": {Values: []string{"first", "second"}},
			}},
			Respond: HTTPResponse{Status: http.StatusOK, Body: body},
		},
		Output: HTTPOutput{Status: http.StatusOK, Body: &Matcher{Equals: &body}},
	}

	runCase(t, caseSpec)
}

func TestHarnessGeneratesRepeatedChunkedBody(t *testing.T) {
	body := "AAAAAA"
	caseSpec := Case{
		Name:   "repeated-chunked-body",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "repeated-chunked-body",
					"uri": "/body",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{UPSTREAM_ADDR}}": 1},
					},
				},
			},
		},
		Input: HTTPInput{
			Method:     http.MethodPost,
			Path:       "/body",
			Chunked:    true,
			BodyRepeat: &RepeatedBody{Value: "A", Count: 6},
		},
		Upstream: &UpstreamSpec{
			Expect:  HTTPAssertion{Body: &Matcher{Equals: &body}},
			Respond: HTTPResponse{Status: http.StatusOK, Body: "ok"},
		},
		Output: HTTPOutput{Status: http.StatusOK},
	}

	runCase(t, caseSpec)
}

func TestHarnessRunsNamedFixtures(t *testing.T) {
	primaryBody := "primary"
	auditBody := "audit"
	caseSpec := Case{
		Name:   "named-fixtures",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "primary",
					"uri": "/primary",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.primary.ADDR}}": 1},
					},
				},
				map[string]any{
					"id":  "audit",
					"uri": "/audit",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.audit.ADDR}}": 1},
					},
				},
			},
		},
		Fixtures: []FixtureSpec{
			{
				Name:    "primary",
				Kind:    "http",
				Respond: []HTTPResponse{{Status: http.StatusOK, Body: primaryBody}},
				Expect: []HTTPAssertion{
					{Method: http.MethodGet},
				},
			},
			{
				Name:    "audit",
				Kind:    "http",
				Respond: []HTTPResponse{{Status: http.StatusCreated, Body: auditBody}},
				Expect: []HTTPAssertion{
					{Method: http.MethodPost},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name:   "primary",
				Input:  HTTPInput{Method: http.MethodGet, Path: "/primary"},
				Output: HTTPOutput{Status: http.StatusOK, Body: &Matcher{Equals: &primaryBody}},
			},
			{
				Name:   "audit",
				Input:  HTTPInput{Method: http.MethodPost, Path: "/audit"},
				Output: HTTPOutput{Status: http.StatusCreated, Body: &Matcher{Equals: &auditBody}},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestHarnessRunsChunkedFixture(t *testing.T) {
	body := "hello world"
	caseSpec := Case{
		Name:   "chunked",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "chunked",
					"uri": "/chunked",
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{UPSTREAM_ADDR}}": 1},
					},
				},
			},
		},
		Input: HTTPInput{Path: "/chunked"},
		Upstream: &UpstreamSpec{
			Respond: HTTPResponse{Status: http.StatusOK, Chunks: []string{"hello ", "world"}},
		},
		Output: HTTPOutput{Status: http.StatusOK, Body: &Matcher{Equals: &body}},
	}

	runCase(t, caseSpec)
}

func TestHarnessSupportsHTTP10AndGzipBody(t *testing.T) {
	body := "01234567890123456789"
	caseSpec := Case{
		Name:   "http-version-gzip",
		Source: CaseSource{Tests: []int{1}},
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "http-version-gzip",
					"uri": "/gzip",
					"plugins": map[string]any{
						"gzip": map[string]any{
							"types":        []any{"text/plain"},
							"min_length":   1,
							"http_version": 1.1,
						},
					},
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{"{{FIXTURE.primary.ADDR}}": 1},
					},
				},
			},
		},
		Fixtures: []FixtureSpec{
			{
				Name: "primary",
				Kind: "http",
				Respond: []HTTPResponse{
					{Status: http.StatusOK, Headers: map[string]string{"Content-Type": "text/plain"}, Body: body},
					{Status: http.StatusOK, Headers: map[string]string{"Content-Type": "text/plain"}, Body: body},
				},
			},
		},
		Steps: []CaseStep{
			{
				Name: "http-1.0-not-compressed",
				Input: HTTPInput{
					Method:  http.MethodGet,
					Version: "1.0",
					Path:    "/gzip",
					Headers: map[string]string{"Accept-Encoding": "gzip"},
				},
				Output: HTTPOutput{Status: http.StatusOK, Body: &Matcher{Equals: &body}},
			},
			{
				Name: "http-1.1-compressed",
				Input: HTTPInput{
					Method:  http.MethodGet,
					Version: "1.1",
					Path:    "/gzip",
					Headers: map[string]string{"Accept-Encoding": "gzip"},
				},
				Output: HTTPOutput{
					Status:   http.StatusOK,
					GzipBody: &Matcher{Equals: &body},
				},
			},
		},
	}

	runCase(t, caseSpec)
}

func TestReplaceFixturePlaceholders(t *testing.T) {
	data, err := replaceFixturePlaceholders(
		[]byte("address={{FIXTURE.primary.ADDR}} url={{FIXTURE.primary.URL}}"),
		map[string]string{
			"{{FIXTURE.primary.ADDR}}": "127.0.0.1:1980",
			"{{FIXTURE.primary.URL}}":  "http://127.0.0.1:1980",
		},
	)
	if err != nil {
		t.Fatalf("replaceFixturePlaceholders() error = %v", err)
	}
	if got, want := string(data), "address=127.0.0.1:1980 url=http://127.0.0.1:1980"; got != want {
		t.Fatalf("replacement = %q, want %q", got, want)
	}

	data, err = replaceFixturePlaceholders(
		[]byte("port: '{{FIXTURE.primary.PORT}}'"),
		map[string]string{"{{FIXTURE.primary.PORT}}": "1980"},
	)
	if err != nil {
		t.Fatalf("replace numeric fixture placeholder: %v", err)
	}
	if got, want := string(data), "port: 1980"; got != want {
		t.Fatalf("numeric replacement = %q, want %q", got, want)
	}

	_, err = replaceFixturePlaceholders([]byte("{{FIXTURE.missing.URL}}"), nil)
	if err == nil || !strings.Contains(err.Error(), "unknown fixture placeholder") {
		t.Fatalf("unknown replacement error = %v", err)
	}
}

func TestAPISIXProcess(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		return
	}
	os.Args = []string{"apisix", "-c", "conf/config.yaml"}
	apisixcmd.Execute()
}

type capturedRequest struct {
	method  string
	path    string
	host    string
	headers http.Header
	body    string
}

type fixtureServer struct {
	server   *httptest.Server
	requests chan capturedRequest
}

func startFixture(spec *UpstreamSpec) *fixtureServer {
	requests := make(chan capturedRequest, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := capturedRequest{
			method:  r.Method,
			path:    r.URL.RequestURI(),
			host:    r.Host,
			headers: r.Header.Clone(),
			body:    string(body),
		}
		select {
		case requests <- request:
		default:
		}

		writeFixtureResponse(w, spec.Respond, string(body))
	})

	var server *httptest.Server
	if spec.TLS {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	return &fixtureServer{server: server, requests: requests}
}

func startNamedFixture(spec FixtureSpec) *fixtureServer {
	requests := make(chan capturedRequest, len(spec.Respond)+len(spec.Expect)+1)
	var responseMu sync.Mutex
	nextResponse := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := capturedRequest{
			method:  r.Method,
			path:    r.URL.RequestURI(),
			host:    r.Host,
			headers: r.Header.Clone(),
			body:    string(body),
		}
		select {
		case requests <- request:
		default:
		}

		responseMu.Lock()
		responseIndex := nextResponse
		if nextResponse < len(spec.Respond)-1 {
			nextResponse++
		}
		responseMu.Unlock()
		response := spec.Respond[responseIndex]
		writeFixtureResponse(w, response, string(body))
	})

	var server *httptest.Server
	if spec.Kind == "https" {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	return &fixtureServer{server: server, requests: requests}
}

func writeFixtureResponse(w http.ResponseWriter, response HTTPResponse, requestBody string) {
	for name, value := range response.Headers {
		w.Header().Set(name, value)
	}
	status := response.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if response.EchoRequestBody {
		_, _ = io.WriteString(w, requestBody)
		return
	}
	if len(response.Chunks) == 0 {
		_, _ = io.WriteString(w, response.Body)
		return
	}
	flusher, _ := w.(http.Flusher)
	for _, chunk := range response.Chunks {
		_, _ = io.WriteString(w, chunk)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (f *fixtureServer) address() string {
	return strings.TrimPrefix(strings.TrimPrefix(f.server.URL, "http://"), "https://")
}

func (f *fixtureServer) host() string {
	host, _, err := net.SplitHostPort(f.address())
	if err != nil {
		return ""
	}
	return host
}

func (f *fixtureServer) port() string {
	_, port, err := net.SplitHostPort(f.address())
	if err != nil {
		return ""
	}
	return port
}

type apisixProcess struct {
	command *exec.Cmd
	done    chan error
	logFile *os.File
	logPath string
}

func startAPISIX(workDir string) (*apisixProcess, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate test executable: %w", err)
	}
	logPath := filepath.Join(workDir, "apisix.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create child log: %w", err)
	}
	command := exec.Command(executable, "-test.run=^TestAPISIXProcess$")
	command.Dir = workDir
	command.Env = append(os.Environ(), helperProcessEnv+"=1")
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start APISIX child: %w", err)
	}
	process := &apisixProcess{
		command: command,
		done:    make(chan error, 1),
		logFile: logFile,
		logPath: logPath,
	}
	go func() {
		process.done <- command.Wait()
		_ = logFile.Close()
	}()
	return process, nil
}

func (p *apisixProcess) waitReady(address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			return fmt.Errorf("APISIX child exited before readiness: %w", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("APISIX child did not listen on %s within %s", address, timeout)
}

func (p *apisixProcess) stop() error {
	if p.command.Process == nil {
		return nil
	}
	if err := p.command.Process.Signal(os.Interrupt); err != nil {
		select {
		case waitErr := <-p.done:
			return waitErr
		default:
			return fmt.Errorf("signal APISIX child: %w", err)
		}
	}
	select {
	case err := <-p.done:
		return err
	case <-time.After(5 * time.Second):
		if err := p.command.Process.Kill(); err != nil {
			return fmt.Errorf("kill APISIX child after shutdown timeout: %w", err)
		}
		<-p.done
		return fmt.Errorf("APISIX child did not stop within 5s")
	}
}

func (p *apisixProcess) logs() (string, error) {
	data, err := os.ReadFile(p.logPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func reservePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func prepareFrontendTLS(
	runtimeConfig map[string]any,
	standaloneConfig map[string]any,
	sni string,
	port int,
	enableHTTP2 bool,
) (map[string]any, map[string]any, error) {
	runtimeConfig, err := cloneConfigMap(runtimeConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("clone runtime config: %w", err)
	}
	standaloneConfig, err = cloneConfigMap(standaloneConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("clone standalone config: %w", err)
	}
	certPEM, keyPEM, err := generateFrontendCertificate(sni)
	if err != nil {
		return nil, nil, err
	}

	apisix := ensureMap(runtimeConfig, "apisix")
	sslConfig := ensureMap(apisix, "ssl")
	sslConfig["enable"] = true
	sslConfig["listen"] = []any{map[string]any{
		"ip":           "127.0.0.1",
		"port":         port,
		"enable_http2": enableHTTP2,
	}}

	ssls, _ := standaloneConfig["ssls"].([]any)
	standaloneConfig["ssls"] = append(ssls, map[string]any{
		"id":     "integration-frontend-tls",
		"snis":   []any{sni},
		"cert":   certPEM,
		"key":    keyPEM,
		"status": 1,
	})
	return runtimeConfig, standaloneConfig, nil
}

func cloneConfigMap(config map[string]any) (map[string]any, error) {
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	var cloned map[string]any
	if err := yaml.Unmarshal(data, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func caseUsesHTTP2(spec Case) bool {
	if spec.Input.Version == "2" {
		return true
	}
	for _, step := range spec.Steps {
		if step.Input.Version == "2" {
			return true
		}
	}
	return false
}

func generateFrontendCertificate(sni string) (string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate frontend TLS key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: sni},
		DNSNames:     []string{sni},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", fmt.Errorf("create frontend TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("marshal frontend TLS key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return string(certPEM), string(keyPEM), nil
}

func renderRuntimeConfig(port int, overrides map[string]any) ([]byte, error) {
	config := map[string]any{
		"apisix": map[string]any{
			"enable_admin": false,
			"proxy_mode":   "http",
		},
		"deployment": map[string]any{},
	}
	mergeMap(config, overrides)
	apisix := ensureMap(config, "apisix")
	apisix["enable_admin"] = false
	apisix["node_listen"] = []any{
		map[string]any{"ip": "127.0.0.1", "port": port},
	}
	deployment := ensureMap(config, "deployment")
	deployment["role"] = "data_plane"
	roleDataPlane := ensureMap(deployment, "role_data_plane")
	roleDataPlane["config_provider"] = "yaml"
	plugins := make([]any, 0)
	switch configured := config["plugins"].(type) {
	case []any:
		plugins = append(plugins, configured...)
	case []string:
		for _, pluginName := range configured {
			plugins = append(plugins, pluginName)
		}
	}
	prometheusConfigured := false
	for _, pluginName := range plugins {
		if pluginName == "prometheus" {
			prometheusConfigured = true
			break
		}
	}
	if !prometheusConfigured {
		plugins = append(plugins, "prometheus")
	}
	config["plugins"] = plugins
	pluginAttr := ensureMap(config, "plugin_attr")
	prometheus := ensureMap(pluginAttr, "prometheus")
	prometheus["enable_export_server"] = false
	return yaml.Marshal(config)
}

func ensureMap(parent map[string]any, key string) map[string]any {
	if value, ok := parent[key].(map[string]any); ok {
		return value
	}
	value := make(map[string]any)
	parent[key] = value
	return value
}

func renderStandaloneConfig(config map[string]any, replacements map[string]string) ([]byte, error) {
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	data, err = replaceFixturePlaceholders(data, replacements)
	if err != nil {
		return nil, err
	}
	data = append(data, []byte("#END\n")...)
	return data, nil
}

func replaceFixturePlaceholders(data []byte, replacements map[string]string) ([]byte, error) {
	for placeholder, value := range replacements {
		if strings.HasSuffix(placeholder, ".PORT}}") {
			data = bytes.ReplaceAll(data, []byte("'"+placeholder+"'"), []byte(value))
			data = bytes.ReplaceAll(data, []byte(`"`+placeholder+`"`), []byte(value))
		}
		data = bytes.ReplaceAll(data, []byte(placeholder), []byte(value))
	}
	if bytes.Contains(data, []byte("{{FIXTURE.")) || bytes.Contains(data, []byte("{{UPSTREAM_")) ||
		bytes.Contains(data, []byte("{{APISIX_")) {
		return nil, fmt.Errorf("configuration contains an unknown fixture placeholder")
	}
	return data, nil
}

func runCase(t *testing.T, spec Case) {
	t.Helper()
	if err := spec.validate(); err != nil {
		t.Fatalf("validate case: %v", err)
	}

	replacements := make(map[string]string, len(spec.Fixtures)*2+1)
	var fixture *fixtureServer
	if spec.Upstream != nil {
		fixture = startFixture(spec.Upstream)
		defer fixture.server.Close()
		replacements["{{UPSTREAM_ADDR}}"] = fixture.address()
		replacements["{{UPSTREAM_HOST}}"] = fixture.host()
		replacements["{{UPSTREAM_PORT}}"] = fixture.port()
	}
	namedFixtures := make(map[string]*fixtureServer, len(spec.Fixtures))
	for _, fixtureSpec := range spec.Fixtures {
		namedFixture := startNamedFixture(fixtureSpec)
		defer namedFixture.server.Close()
		namedFixtures[fixtureSpec.Name] = namedFixture
		prefix := "{{FIXTURE." + fixtureSpec.Name
		replacements[prefix+".ADDR}}"] = namedFixture.address()
		replacements[prefix+".URL}}"] = namedFixture.server.URL
		replacements[prefix+".HOST}}"] = namedFixture.host()
		replacements[prefix+".PORT}}"] = namedFixture.port()
	}

	port, err := reservePort()
	if err != nil {
		t.Fatalf("reserve APISIX port: %v", err)
	}
	apisixAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	replacements["{{APISIX_URL}}"] = "http://" + apisixAddress
	runtimeOverrides := spec.Runtime
	standaloneResources := spec.Config
	tlsPort := 0
	enableHTTP2 := caseUsesHTTP2(spec)
	if spec.TLS != nil {
		tlsPort, err = reservePort()
		if err != nil {
			t.Fatalf("reserve APISIX TLS port: %v", err)
		}
		runtimeOverrides, standaloneResources, err = prepareFrontendTLS(
			spec.Runtime,
			spec.Config,
			spec.TLS.SNI,
			tlsPort,
			enableHTTP2,
		)
		if err != nil {
			t.Fatalf("prepare frontend TLS: %v", err)
		}
	}
	workDir := t.TempDir()
	confDir := filepath.Join(workDir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("create conf directory: %v", err)
	}
	runtimeConfig, err := renderRuntimeConfig(port, runtimeOverrides)
	if err != nil {
		t.Fatalf("render runtime config: %v", err)
	}
	runtimeConfig, err = replaceFixturePlaceholders(runtimeConfig, replacements)
	if err != nil {
		t.Fatalf("replace runtime fixture placeholders: %v", err)
	}
	standaloneConfig, err := renderStandaloneConfig(standaloneResources, replacements)
	if err != nil {
		t.Fatalf("render standalone config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "config.yaml"), runtimeConfig, 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "apisix.yaml"), standaloneConfig, 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	process, err := startAPISIX(workDir)
	if err != nil {
		t.Fatalf("start APISIX: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = process.stop()
		}
	}()
	address := apisixAddress
	if err := process.waitReady(address, 5*time.Second); err != nil {
		_ = process.stop()
		stopped = true
		logs, _ := process.logs()
		t.Fatalf(
			"wait for APISIX: %v\nchild logs:\n%s\nruntime config:\n%s\nstandalone config:\n%s",
			err,
			logs,
			runtimeConfig,
			standaloneConfig,
		)
	}
	tlsAddress := ""
	if tlsPort != 0 {
		tlsAddress = net.JoinHostPort("127.0.0.1", strconv.Itoa(tlsPort))
		if err := process.waitReady(tlsAddress, 5*time.Second); err != nil {
			_ = process.stop()
			stopped = true
			logs, _ := process.logs()
			t.Fatalf("wait for APISIX TLS: %v\nchild logs:\n%s", err, logs)
		}
	}

	transport := &http.Transport{DisableCompression: true, ForceAttemptHTTP2: enableHTTP2}
	if spec.TLS != nil {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // integration certificate is generated per case
			ServerName:         spec.TLS.SNI,
		}
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create client cookie jar: %v", err)
	}
	requestFailed := false
	logMatchers := make([]Matcher, 0, len(spec.Steps)+1)
	bodyLengths := make(map[string]int)
	headerHistory := make(map[string][]string)
	capturedCookies := make(map[string]string)
	capturedValues := make(map[string]string)
	if len(spec.Steps) > 0 {
		for _, step := range spec.Steps {
			t.Run(step.Name, func(t *testing.T) {
				repeat := step.Repeat
				if repeat == 0 {
					repeat = 1
				}
				for iteration := 1; iteration <= repeat; iteration++ {
					run := func(t *testing.T) {
						input := expandIterationInput(step.Input, iteration)
						output := expandIterationOutput(step.Output, iteration)
						if err := runHTTPInput(
							t, client, address, tlsAddress, input, output, bodyLengths, headerHistory,
							capturedCookies, capturedValues,
						); err != nil {
							requestFailed = true
						}
						if step.Wait > 0 {
							time.Sleep(step.Wait)
						}
					}
					if repeat == 1 {
						run(t)
					} else {
						t.Run(strconv.Itoa(iteration), run)
					}
				}
				if step.Output.Logs != nil {
					logMatchers = append(logMatchers, *step.Output.Logs)
				}
			})
		}
	} else if spec.Input.Path != "" {
		if err := runHTTPInput(
			t, client, address, tlsAddress, spec.Input, spec.Output, bodyLengths, headerHistory,
			capturedCookies, capturedValues,
		); err != nil {
			requestFailed = true
		}
	}

	if fixture != nil && fixtureAssertionsConfigured(spec.Upstream.Expect) {
		select {
		case received := <-fixture.requests:
			assertUpstreamRequest(t, spec.Upstream.Expect, received)
		case <-time.After(2 * time.Second):
			t.Error("fixture upstream did not receive a request")
		}
	}
	for _, fixtureSpec := range spec.Fixtures {
		namedFixture := namedFixtures[fixtureSpec.Name]
		for i, expected := range fixtureSpec.Expect {
			select {
			case received := <-namedFixture.requests:
				assertUpstreamRequest(t, expected, received)
			case <-time.After(2 * time.Second):
				t.Errorf("fixture %s did not receive expected request %d", fixtureSpec.Name, i+1)
			}
		}
		if len(fixtureSpec.Expect) > 0 {
			select {
			case extra := <-namedFixture.requests:
				t.Errorf(
					"fixture %s received unexpected extra request %s %s",
					fixtureSpec.Name,
					extra.method,
					extra.path,
				)
			default:
			}
		}
	}

	if err := process.stop(); err != nil {
		t.Errorf("stop APISIX: %v", err)
	}
	stopped = true
	logs, err := process.logs()
	if err != nil {
		t.Errorf("read APISIX logs: %v", err)
	} else {
		if requestFailed {
			t.Logf(
				"child logs after request failure:\n%s\nruntime config:\n%s\nstandalone config:\n%s",
				logs,
				runtimeConfig,
				standaloneConfig,
			)
		}
		if len(spec.Steps) == 0 && spec.Output.Logs != nil {
			logMatchers = append(logMatchers, *spec.Output.Logs)
		}
		for _, matcher := range logMatchers {
			if err := matcher.match(logs, true); err != nil {
				t.Errorf("child logs: %v\n%s", err, logs)
			}
		}
	}
}

func runHTTPInput(
	t *testing.T,
	client *http.Client,
	httpAddress string,
	tlsAddress string,
	input HTTPInput,
	output HTTPOutput,
	bodyLengths map[string]int,
	headerHistory map[string][]string,
	capturedCookies map[string]string,
	capturedValues map[string]string,
) error {
	t.Helper()
	var err error
	input, err = resolveCapturedInput(input, capturedValues)
	if err != nil {
		t.Errorf("resolve captured response value: %v", err)
		return err
	}
	method := input.Method
	if method == "" {
		method = http.MethodGet
	}
	scheme := input.Scheme
	address := httpAddress
	if scheme == "" {
		scheme = "http"
	}
	if scheme == "https" {
		address = tlsAddress
	}
	body := input.Body
	if input.BodyRepeat != nil {
		body = strings.Repeat(input.BodyRepeat.Value, input.BodyRepeat.Count)
	}
	request, err := http.NewRequest(method, scheme+"://"+address+input.Path, strings.NewReader(body))
	if err != nil {
		t.Errorf("build client request: %v", err)
		return err
	}
	for name, value := range input.Headers {
		value, err = replaceCookiePlaceholders(value, capturedCookies)
		if err != nil {
			t.Errorf("resolve input header %s: %v", name, err)
			return err
		}
		if strings.EqualFold(name, "Host") {
			request.Host = value
			continue
		}
		request.Header.Set(name, value)
	}
	for name, values := range input.HeaderValues {
		for _, value := range values {
			value, err = replaceCookiePlaceholders(value, capturedCookies)
			if err != nil {
				t.Errorf("resolve input header %s: %v", name, err)
				return err
			}
			request.Header.Add(name, value)
		}
	}
	if input.Chunked {
		request.ContentLength = -1
		request.TransferEncoding = []string{"chunked"}
	}
	if input.Version == "1.0" {
		return runRawHTTP10Input(
			t, client, address, request, output, bodyLengths, headerHistory, capturedCookies, capturedValues,
		)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("client request: %v", err)
		return err
	}
	if input.Version == "2" && response.ProtoMajor != 2 {
		err := fmt.Errorf("response protocol = %s, want HTTP/2", response.Proto)
		t.Error(err)
		_ = response.Body.Close()
		return err
	}
	responseBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Errorf("read client response: %v", err)
		return err
	}
	assertOutput(t, output, response, string(responseBody))
	assertBodyLength(t, output, len(responseBody), bodyLengths)
	assertGeneratedHeaders(t, output, response.Header, headerHistory)
	captureResponseCookies(response, capturedCookies)
	if err := captureResponseHeaders(output.Captures, response.Header, capturedValues); err != nil {
		t.Errorf("capture response header: %v", err)
		return err
	}
	return nil
}

func resolveCapturedInput(input HTTPInput, captured map[string]string) (HTTPInput, error) {
	var err error
	input.Path, err = replaceCapturePlaceholders(input.Path, captured)
	if err != nil {
		return input, err
	}
	input.Body, err = replaceCapturePlaceholders(input.Body, captured)
	if err != nil {
		return input, err
	}
	if input.Headers != nil {
		headers := make(map[string]string, len(input.Headers))
		for name, value := range input.Headers {
			value, err = replaceCapturePlaceholders(value, captured)
			if err != nil {
				return input, fmt.Errorf("header %s: %w", name, err)
			}
			headers[name] = value
		}
		input.Headers = headers
	}
	return input, nil
}

func replaceCapturePlaceholders(value string, captured map[string]string) (string, error) {
	const prefix = "{{CAPTURE."
	for {
		start := strings.Index(value, prefix)
		if start < 0 {
			return value, nil
		}
		endOffset := strings.Index(value[start:], "}}")
		if endOffset < 0 {
			return "", fmt.Errorf("unterminated capture placeholder")
		}
		end := start + endOffset + 2
		name := value[start+len(prefix) : start+endOffset]
		if name == "" {
			return "", fmt.Errorf("capture placeholder name is empty")
		}
		replacement, ok := captured[name]
		if !ok {
			return "", fmt.Errorf("response capture %q has not been recorded", name)
		}
		value = value[:start] + replacement + value[end:]
	}
}

func captureResponseHeaders(captures map[string]HeaderCapture, headers http.Header, captured map[string]string) error {
	for name, capture := range captures {
		value := headers.Get(capture.Header)
		match := regexp.MustCompile(capture.Matches).FindStringSubmatch(value)
		if len(match) != 2 {
			return fmt.Errorf("response header %s value %q does not match capture %q", capture.Header, value, name)
		}
		captured[name] = match[1]
	}
	return nil
}

func replaceCookiePlaceholders(value string, cookies map[string]string) (string, error) {
	const prefix = "{{COOKIE."
	for {
		start := strings.Index(value, prefix)
		if start < 0 {
			return value, nil
		}
		endOffset := strings.Index(value[start:], "}}")
		if endOffset < 0 {
			return "", fmt.Errorf("unterminated cookie placeholder")
		}
		end := start + endOffset + 2
		name := value[start+len(prefix) : start+endOffset]
		if name == "" {
			return "", fmt.Errorf("cookie placeholder name is empty")
		}
		replacement := cookies[name]
		if replacement == "" {
			return "", fmt.Errorf("cookie %q has not been captured", name)
		}
		value = value[:start] + replacement + value[end:]
	}
}

func captureResponseCookies(response *http.Response, captured map[string]string) {
	for _, cookie := range response.Cookies() {
		captured[cookie.Name] = cookie.Value
	}
}

func runRawHTTP10Input(
	t *testing.T,
	client *http.Client,
	address string,
	request *http.Request,
	output HTTPOutput,
	bodyLengths map[string]int,
	headerHistory map[string][]string,
	capturedCookies map[string]string,
	capturedValues map[string]string,
) error {
	t.Helper()
	var connection net.Conn
	var err error
	if request.URL.Scheme == "https" {
		transport, _ := client.Transport.(*http.Transport)
		connection, err = tls.Dial("tcp", address, transport.TLSClientConfig)
	} else {
		connection, err = net.DialTimeout("tcp", address, 5*time.Second)
	}
	if err != nil {
		t.Errorf("dial raw HTTP/1.0 request: %v", err)
		return err
	}
	defer func() { _ = connection.Close() }()

	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	writer := bufio.NewWriter(connection)
	_, _ = fmt.Fprintf(writer, "%s %s HTTP/1.0\r\nHost: %s\r\n", request.Method, request.URL.RequestURI(), host)
	for name, values := range request.Header {
		for _, value := range values {
			_, _ = fmt.Fprintf(writer, "%s: %s\r\n", name, value)
		}
	}
	if request.Body != nil && request.ContentLength != 0 {
		_, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n", request.ContentLength)
	}
	_, _ = writer.WriteString("Connection: close\r\n\r\n")
	if request.Body != nil {
		_, _ = io.Copy(writer, request.Body)
	}
	if err := writer.Flush(); err != nil {
		t.Errorf("write raw HTTP/1.0 request: %v", err)
		return err
	}

	response, err := http.ReadResponse(bufio.NewReader(connection), request)
	if err != nil {
		t.Errorf("read raw HTTP/1.0 response: %v", err)
		return err
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Errorf("read raw HTTP/1.0 body: %v", err)
		return err
	}
	assertOutput(t, output, response, string(body))
	assertBodyLength(t, output, len(body), bodyLengths)
	assertGeneratedHeaders(t, output, response.Header, headerHistory)
	captureResponseCookies(response, capturedCookies)
	if err := captureResponseHeaders(output.Captures, response.Header, capturedValues); err != nil {
		t.Errorf("capture response header: %v", err)
		return err
	}
	return nil
}

func assertGeneratedHeaders(
	t *testing.T,
	output HTTPOutput,
	headers http.Header,
	history map[string][]string,
) {
	t.Helper()
	for _, pair := range output.DifferentHeaders {
		first := headers.Get(pair[0])
		second := headers.Get(pair[1])
		if first == "" || second == "" {
			t.Errorf("response headers %s and %s must both be non-empty", pair[0], pair[1])
		} else if first == second {
			t.Errorf("response headers %s and %s both equal %q", pair[0], pair[1], first)
		}
	}
	unique := make(map[string]bool, len(output.UniqueHeaders))
	for _, name := range output.UniqueHeaders {
		unique[http.CanonicalHeaderKey(name)] = true
	}
	monotonic := make(map[string]bool, len(output.MonotonicHeaders))
	for _, name := range output.MonotonicHeaders {
		monotonic[http.CanonicalHeaderKey(name)] = true
	}
	for name := range unique {
		value := headers.Get(name)
		if value == "" {
			t.Errorf("response header %s is empty", name)
			continue
		}
		if slices.Contains(history[name], value) {
			t.Errorf("response header %s repeated value %q", name, value)
		}
	}
	for name := range monotonic {
		value := headers.Get(name)
		if value == "" {
			t.Errorf("response header %s is empty", name)
			continue
		}
		values := history[name]
		if len(values) > 0 && value <= values[len(values)-1] {
			t.Errorf("response header %s value %q is not greater than %q", name, value, values[len(values)-1])
		}
	}
	for name := range unique {
		history[name] = append(history[name], headers.Get(name))
	}
	for name := range monotonic {
		if !unique[name] {
			history[name] = append(history[name], headers.Get(name))
		}
	}
}

func assertBodyLength(t *testing.T, output HTTPOutput, length int, saved map[string]int) {
	t.Helper()
	if output.BodyLengthLessThan != "" {
		reference, ok := saved[output.BodyLengthLessThan]
		if !ok {
			t.Errorf("body length reference %q has not been saved", output.BodyLengthLessThan)
		} else if length >= reference {
			t.Errorf("response body length = %d, want less than %s (%d)", length, output.BodyLengthLessThan, reference)
		}
	}
	if output.SaveBodyLength != "" {
		if _, exists := saved[output.SaveBodyLength]; exists {
			t.Errorf("body length reference %q is already saved", output.SaveBodyLength)
			return
		}
		saved[output.SaveBodyLength] = length
	}
}

func fixtureAssertionsConfigured(assertion HTTPAssertion) bool {
	return assertion.Method != "" || assertion.Path != nil || assertion.Host != nil ||
		len(assertion.Headers) > 0 || assertion.Body != nil
}

func assertOutput(t *testing.T, expected HTTPOutput, response *http.Response, body string) {
	t.Helper()
	if response.StatusCode != expected.Status {
		t.Errorf("response status = %d, want %d", response.StatusCode, expected.Status)
	}
	assertHeaders(t, "response", expected.Headers, response.Header)
	if expected.Body != nil {
		if err := expected.Body.match(body, true); err != nil {
			t.Errorf("response body: %v", err)
		}
	}
	if expected.GzipBody != nil {
		reader, err := cgzip.NewReader(strings.NewReader(body))
		if err != nil {
			t.Errorf("create gzip response reader: %v", err)
			return
		}
		decoded, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Errorf("read gzip response body: %v", err)
			return
		}
		if err := expected.GzipBody.match(string(decoded), true); err != nil {
			t.Errorf("gzip response body: %v", err)
		}
	}
}

func expandIterationInput(input HTTPInput, iteration int) HTTPInput {
	replacement := strconv.Itoa(iteration)
	input.Path = replaceIteration(input.Path, replacement)
	input.Body = replaceIteration(input.Body, replacement)
	if input.Headers != nil {
		headers := make(map[string]string, len(input.Headers))
		for name, value := range input.Headers {
			headers[replaceIteration(name, replacement)] = replaceIteration(value, replacement)
		}
		input.Headers = headers
	}
	if input.HeaderValues != nil {
		headers := make(map[string][]string, len(input.HeaderValues))
		for name, values := range input.HeaderValues {
			expanded := make([]string, len(values))
			for i, value := range values {
				expanded[i] = replaceIteration(value, replacement)
			}
			headers[replaceIteration(name, replacement)] = expanded
		}
		input.HeaderValues = headers
	}
	if input.BodyRepeat != nil {
		repeated := *input.BodyRepeat
		repeated.Value = replaceIteration(repeated.Value, replacement)
		input.BodyRepeat = &repeated
	}
	return input
}

func expandIterationOutput(output HTTPOutput, iteration int) HTTPOutput {
	replacement := strconv.Itoa(iteration)
	output.Body = expandIterationMatcher(output.Body, replacement)
	output.GzipBody = expandIterationMatcher(output.GzipBody, replacement)
	output.Logs = expandIterationMatcher(output.Logs, replacement)
	if output.Headers != nil {
		headers := make(map[string]Matcher, len(output.Headers))
		for name, matcher := range output.Headers {
			headers[replaceIteration(name, replacement)] = *expandIterationMatcher(&matcher, replacement)
		}
		output.Headers = headers
	}
	output.SaveBodyLength = replaceIteration(output.SaveBodyLength, replacement)
	output.BodyLengthLessThan = replaceIteration(output.BodyLengthLessThan, replacement)
	return output
}

func expandIterationMatcher(matcher *Matcher, replacement string) *Matcher {
	if matcher == nil {
		return nil
	}
	expanded := *matcher
	if matcher.Equals != nil {
		value := replaceIteration(*matcher.Equals, replacement)
		expanded.Equals = &value
	}
	if matcher.Matches != nil {
		value := replaceIteration(*matcher.Matches, replacement)
		expanded.Matches = &value
	}
	if matcher.NotMatches != nil {
		value := replaceIteration(*matcher.NotMatches, replacement)
		expanded.NotMatches = &value
	}
	if matcher.Values != nil {
		expanded.Values = make([]string, len(matcher.Values))
		for i, value := range matcher.Values {
			expanded.Values[i] = replaceIteration(value, replacement)
		}
	}
	return &expanded
}

func replaceIteration(value string, replacement string) string {
	return strings.ReplaceAll(value, "{{ITERATION}}", replacement)
}

func assertUpstreamRequest(t *testing.T, expected HTTPAssertion, received capturedRequest) {
	t.Helper()
	if expected.Method != "" && received.method != expected.Method {
		t.Errorf("upstream method = %q, want %q", received.method, expected.Method)
	}
	if expected.Path != nil {
		if err := expected.Path.match(received.path, true); err != nil {
			t.Errorf("upstream path: %v", err)
		}
	}
	if expected.Host != nil {
		if err := expected.Host.match(received.host, true); err != nil {
			t.Errorf("upstream host: %v", err)
		}
	}
	assertHeaders(t, "upstream", expected.Headers, received.headers)
	if expected.Body != nil {
		if err := expected.Body.match(received.body, true); err != nil {
			t.Errorf("upstream body: %v", err)
		}
	}
}

func assertHeaders(t *testing.T, scope string, expected map[string]Matcher, actual http.Header) {
	t.Helper()
	for name, matcher := range expected {
		values, present := actual[http.CanonicalHeaderKey(name)]
		value := actual.Get(name)
		if !present {
			values = nil
		}
		if err := matcher.matchHeader(value, values); err != nil {
			t.Errorf("%s header %s: %v", scope, name, err)
		}
	}
}
