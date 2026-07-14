package pluginintegration

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
				runCase(t, spec)
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

		for name, value := range spec.Respond.Headers {
			w.Header().Set(name, value)
		}
		status := spec.Respond.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, spec.Respond.Body)
	})

	var server *httptest.Server
	if spec.TLS {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	return &fixtureServer{server: server, requests: requests}
}

func (f *fixtureServer) address() string {
	return strings.TrimPrefix(strings.TrimPrefix(f.server.URL, "http://"), "https://")
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
	config["plugins"] = []any{"prometheus"}
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

func renderStandaloneConfig(config map[string]any, upstreamAddress string) ([]byte, error) {
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	data = bytes.ReplaceAll(data, []byte("{{UPSTREAM_ADDR}}"), []byte(upstreamAddress))
	data = append(data, []byte("#END\n")...)
	return data, nil
}

func runCase(t *testing.T, spec Case) {
	t.Helper()
	if strings.TrimSpace(spec.Skip) != "" {
		t.Skip(spec.Skip)
	}
	if err := spec.validate(); err != nil {
		t.Fatalf("validate case: %v", err)
	}

	var fixture *fixtureServer
	upstreamAddress := ""
	if spec.Upstream != nil {
		fixture = startFixture(spec.Upstream)
		defer fixture.server.Close()
		upstreamAddress = fixture.address()
	}

	port, err := reservePort()
	if err != nil {
		t.Fatalf("reserve APISIX port: %v", err)
	}
	workDir := t.TempDir()
	confDir := filepath.Join(workDir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("create conf directory: %v", err)
	}
	runtimeConfig, err := renderRuntimeConfig(port, spec.Runtime)
	if err != nil {
		t.Fatalf("render runtime config: %v", err)
	}
	standaloneConfig, err := renderStandaloneConfig(spec.Config, upstreamAddress)
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
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
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

	var requestErr error
	if spec.Input.Path != "" {
		method := spec.Input.Method
		if method == "" {
			method = http.MethodGet
		}
		request, err := http.NewRequest(method, "http://"+address+spec.Input.Path, strings.NewReader(spec.Input.Body))
		if err != nil {
			t.Fatalf("build client request: %v", err)
		}
		for name, value := range spec.Input.Headers {
			if strings.EqualFold(name, "Host") {
				request.Host = value
				continue
			}
			request.Header.Set(name, value)
		}
		transport := &http.Transport{
			DisableCompression: true,
		}
		defer transport.CloseIdleConnections()
		client := &http.Client{
			Timeout:   5 * time.Second,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		response, err := client.Do(request)
		requestErr = err
		if requestErr != nil {
			t.Errorf("client request: %v", requestErr)
		} else {
			responseBody, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				t.Errorf("read client response: %v", readErr)
			} else {
				assertOutput(t, spec.Output, response, string(responseBody))
			}
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

	if err := process.stop(); err != nil {
		t.Errorf("stop APISIX: %v", err)
	}
	stopped = true
	logs, err := process.logs()
	if err != nil {
		t.Errorf("read APISIX logs: %v", err)
	} else {
		if requestErr != nil {
			t.Logf(
				"child logs after request failure:\n%s\nruntime config:\n%s\nstandalone config:\n%s",
				logs,
				runtimeConfig,
				standaloneConfig,
			)
		}
		if spec.Output.Logs != nil {
			if err := spec.Output.Logs.match(logs, true); err != nil {
				t.Errorf("child logs: %v\n%s", err, logs)
			}
		}
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
		if err := matcher.match(value, present && len(values) > 0); err != nil {
			t.Errorf("%s header %s: %v", scope, name, err)
		}
	}
}
