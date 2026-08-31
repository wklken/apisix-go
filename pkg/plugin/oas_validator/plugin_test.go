package oas_validator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	return newTestPluginWithMetadata(t, cfg, nil)
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, metadata map[string]any) *Plugin {
	t.Helper()
	return newTestPluginWithOwner(t, cfg, metadata, "plugin/test/oas/helper")
}

type oasTestTaskState struct {
	tasks    *runtime.TaskRegistry
	failures <-chan runtime.TaskFailure
	stopOnce sync.Once
}

var oasTestTaskStates sync.Map

func newTestPluginWithOwner(t *testing.T, cfg Config, metadata map[string]any, prefix string) *Plugin {
	t.Helper()
	failures := make(chan runtime.TaskFailure, 4)
	tasks := runtime.NewTaskRegistry(context.Background(), func(failure runtime.TaskFailure) {
		failures <- failure
	})
	owner, err := runtime.NewTaskOwner(tasks, prefix, runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Metadata: mustMetadataView(t, metadata), Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newOASScopedSecretHarness(t, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	registerOASTestTaskState(t, p, tasks, failures)

	return p
}

func registerOASTestTaskState(
	t *testing.T,
	p *Plugin,
	tasks *runtime.TaskRegistry,
	failures <-chan runtime.TaskFailure,
) {
	t.Helper()
	state := &oasTestTaskState{tasks: tasks, failures: failures}
	oasTestTaskStates.Store(p, state)
	t.Cleanup(func() { stopOASTestPlugin(t, p) })
}

func stopOASTestPlugin(t *testing.T, p *Plugin) {
	t.Helper()
	value, ok := oasTestTaskStates.Load(p)
	if !ok {
		p.Stop()
		return
	}
	state := value.(*oasTestTaskState)
	state.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if residuals, err := state.tasks.Stop(ctx); err != nil || len(residuals) != 0 {
			t.Fatalf("TaskRegistry.Stop() = (%v, %v)", residuals, err)
		}
		p.Stop()
		select {
		case failure := <-state.failures:
			t.Fatalf("unexpected task failure = %#v", failure)
		default:
		}
	})
}

func oasTestTasks(t *testing.T, p *Plugin) *runtime.TaskRegistry {
	t.Helper()
	value, ok := oasTestTaskStates.Load(p)
	if !ok {
		t.Fatal("plugin has no test task registry")
	}
	return value.(*oasTestTaskState).tasks
}

func mustMetadataView(t *testing.T, metadata map[string]any) runtime.MetadataView {
	t.Helper()
	if len(metadata) == 0 {
		return runtime.MetadataView{}
	}
	document, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	view, err := runtime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestOASSpecRefreshUsesGenerationTaskOwner(t *testing.T) {
	refreshEntered := make(chan struct{})
	refreshCanceled := make(chan struct{})
	var fetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fetches.Add(1) == 2 {
			close(refreshEntered)
			<-r.Context().Done()
			close(refreshCanceled)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	t.Cleanup(server.Close)

	p := newTestPluginWithOwner(
		t,
		Config{SpecURL: server.URL},
		map[string]any{"spec_url_ttl": 1},
		"plugin/test/oas/attempt-1",
	)
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }
	if _, err := p.validator(); err != nil {
		t.Fatalf("initial validator() error = %v", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := p.validator(); err != nil {
		t.Fatalf("due validator() error = %v", err)
	}
	<-refreshEntered

	want := []string{"plugin/test/oas/attempt-1/spec-refresh"}
	if got := oasTestTasks(t, p).Active(); !slices.Equal(got, want) {
		t.Fatalf("active owners = %v, want %v", got, want)
	}
	stopOASTestPlugin(t, p)
	<-refreshCanceled
	if got := oasTestTasks(t, p).Active(); len(got) != 0 {
		t.Fatalf("active owners after stop = %v, want none", got)
	}
}

func TestOASRefreshAdmissionFailureLeavesCurrentValidator(t *testing.T) {
	var fetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	t.Cleanup(server.Close)

	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	if residuals, err := tasks.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("TaskRegistry.Stop() = (%v, %v)", residuals, err)
	}
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/oas/rejected", runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{config: Config{SpecURL: server.URL}}
	p.SetDependencies(base.Dependencies{Metadata: runtime.MetadataView{}, Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, closeAttempt := newOASScopedSecretHarness(t, nil)
	t.Cleanup(closeAttempt)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }
	want, err := p.validator()
	if err != nil {
		t.Fatalf("initial validator() error = %v", err)
	}
	now = now.Add(p.specURLTTL() + time.Second)
	p.wakeSpecRefresh()
	p.wakeSpecRefresh()
	if got := p.compiled.Load(); got != want {
		t.Fatal("task admission failure replaced last-good validator")
	}
	if !errors.Is(p.refreshAdmissionErr, runtime.ErrTaskRegistryStopped) {
		t.Fatalf("refresh admission error = %v, want %v", p.refreshAdmissionErr, runtime.ErrTaskRegistryStopped)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches after rejected refresh admission = %d, want initial fetch only", got)
	}
}

func TestHandlerValidatesInlineOpenAPISpec(t *testing.T) {
	p := newTestPlugin(t, Config{
		Spec:                testSpec(),
		VerboseErrors:       true,
		RejectionStatusCode: http.StatusUnprocessableEntity,
	})

	req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"age":3}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Trace", "trace-id")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response code = %d, want 422", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to validate request") ||
		!strings.Contains(rr.Body.String(), "name") {
		t.Fatalf("response body = %q, want verbose validation error mentioning name", rr.Body.String())
	}
}

func TestHandlerPassesAndRestoresValidRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: testSpec()})

	req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Trace", "trace-id")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read restored body: %v", err)
		}
		if string(body) != `{"name":"doggie"}` {
			t.Fatalf("restored body = %q, want original", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerMatchesOpenAPIServerURLPrefix(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "servers": [{"url": "/api/v3"}],
  "paths": {
    "/pets": {
      "get": {"responses": {"204": {"description": "no content"}}}
    }
  }
}`})

	req := httptest.NewRequest(http.MethodGet, "/api/v3/pets", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerPrefersLiteralPathOverPathParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/api/v31/pet/{petId}": {
      "get": {
        "parameters": [{"name": "petId", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "OK"}}
      }
    },
    "/api/v31/pet/findByStatus": {
      "get": {
        "responses": {"200": {"description": "OK"}}
      }
    }
  }
}`})

	req := httptest.NewRequest(http.MethodGet, "/api/v31/pet/findByStatus", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v31/pet/findByStatus" {
			t.Fatalf("path = %q, want literal path preserved", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMetadataSchemaRejectsNonpositiveSpecURLTTL(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: testSpec()})
	metadataSchema := p.GetMetadataSchema()
	if err := util.Validate(map[string]any{"spec_url_ttl": 1}, metadataSchema); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	if err := util.Validate(map[string]any{"spec_url_ttl": 0}, metadataSchema); err == nil {
		t.Fatal("zero spec_url_ttl accepted")
	}
}

func TestPreparedGenerationsRetainMetadataTTL(t *testing.T) {
	var firstFetches atomic.Int32
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstFetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer firstServer.Close()
	var secondFetches atomic.Int32
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondFetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer secondServer.Close()

	first := newTestPluginWithMetadata(t, Config{SpecURL: firstServer.URL}, map[string]any{"spec_url_ttl": 10})
	second := newTestPluginWithMetadata(t, Config{SpecURL: secondServer.URL}, map[string]any{"spec_url_ttl": 20})
	var nowSeconds atomic.Int64
	nowSeconds.Store(100)
	currentTime := func() time.Time { return time.Unix(nowSeconds.Load(), 0) }
	first.now = currentTime
	second.now = currentTime

	if _, err := first.validator(); err != nil {
		t.Fatalf("generation N validator() error = %v", err)
	}
	if _, err := second.validator(); err != nil {
		t.Fatalf("generation N+1 validator() error = %v", err)
	}
	nowSeconds.Store(115)
	if _, err := first.validator(); err != nil {
		t.Fatalf("generation N expired validator() error = %v", err)
	}
	if _, err := second.validator(); err != nil {
		t.Fatalf("generation N+1 cached validator() error = %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for firstFetches.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := firstFetches.Load(); got != 2 {
		t.Fatalf("generation N fetches = %d, want refresh after 10s TTL", got)
	}
	if got := secondFetches.Load(); got != 1 {
		t.Fatalf("generation N+1 fetches = %d, want cached at 15s with 20s TTL", got)
	}
	if got := oasTestTasks(t, second).Active(); len(got) != 0 {
		t.Fatal("generation N+1 unexpectedly started a refresh worker")
	}
}

func TestMetadataDecodeFailsBeforeInlineSpecValidation(t *testing.T) {
	p := &Plugin{config: Config{Spec: testSpec()}}
	p.SetDependencies(base.Dependencies{Metadata: mustMetadataView(t, map[string]any{
		"spec_url_ttl": "invalid",
	})})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newOASScopedSecretHarness(t, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if p.compiled.Load() != nil {
		t.Fatal("metadata decode failure published an OpenAPI validator")
	}
}

func TestPostInitRejectsInvalidInlineSpec(t *testing.T) {
	p := &Plugin{config: Config{Spec: "invalid json string"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newOASScopedSecretHarness(t, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	err := p.PostInit()
	if err == nil {
		t.Fatal("PostInit() accepted invalid inline OpenAPI spec")
	}
	if !strings.Contains(err.Error(), "failed to parse inline openapi spec") {
		t.Fatalf("PostInit() error = %q, want inline spec parsing context", err)
	}
}

func TestMaterializedInlineSpecCompilesWithoutExposingPlaintext(t *testing.T) {
	const environmentName = "APISIX_GO_OAS_INLINE_SPEC"
	const spec = `{"openapi":"3.0.0","info":{"title":"secret-spec","version":"1"},"paths":{}}`
	t.Setenv(environmentName, spec)
	p := &Plugin{config: Config{Spec: "$ENV://" + environmentName}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	rawSpec := "$ENV://" + environmentName
	secrets, scope, _, cleanup := newOASScopedSecretHarness(t, map[string]string{rawSpec: spec})
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if strings.Contains(p.config.Spec, "secret-spec") {
		t.Fatalf("materialized config exposed inline spec: %q", p.config.Spec)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if _, err := p.validator(); err != nil {
		t.Fatalf("validator() error = %v", err)
	}
	stopOASTestPlugin(t, p)
}

func TestHandlerValidatesRequestBodyWithLocalSchemaRef(t *testing.T) {
	p := newTestPlugin(t, Config{
		Spec:          testSpecWithComponentsRef(),
		VerboseErrors: true,
	})

	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"age":3}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "name") {
		t.Fatalf("response body = %q, want validation error mentioning name", rr.Body.String())
	}
}

func TestHandlerResolvesLocalParameterRef(t *testing.T) {
	spec := `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {"$ref": "#/components/parameters/Trace"}
        ],
        "responses": {"200": {"description": "OK"}}
      }
    }
  },
  "components": {
    "parameters": {
      "Trace": {
        "name": "X-Trace",
        "in": "header",
        "required": true,
        "schema": {"type": "string", "minLength": 1}
      }
    }
  }
}`
	p := newTestPlugin(t, Config{Spec: spec, VerboseErrors: true})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, test := range []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusBadRequest},
		{name: "present", header: "trace-id", wantStatus: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/pets", nil)
			if test.header != "" {
				req.Header.Set("X-Trace", test.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestHandlerResolvesLocalRequestBodyRef(t *testing.T) {
	spec := `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {"$ref": "#/components/requestBodies/Pet"}
      }
    }
  },
  "components": {
    "requestBodies": {
      "Pet": {
        "required": true,
        "content": {
          "application/json": {
            "schema": {
              "type": "object",
              "required": ["name"],
              "properties": {"name": {"type": "string"}}
            }
          }
        }
      }
    }
  }
}`
	p := newTestPlugin(t, Config{Spec: spec, VerboseErrors: true})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusBadRequest},
		{name: "present", body: `{"name":"doggie"}`, wantStatus: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestHandlerValidatesURLFormBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Spec: formSpec(),
	})
	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader("name=doggie&age=3"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid form body", rr.Code)
	}
}

func TestHandlerValidatesMultipartBody(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", "doggie"); err != nil {
		t.Fatalf("WriteField(name) error = %v", err)
	}
	if err := writer.WriteField("age", "3"); err != nil {
		t.Fatalf("WriteField(age) error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	p := newTestPlugin(t, Config{Spec: strings.Replace(formSpec(),
		"application/x-www-form-urlencoded", "multipart/form-data", 1)})
	req := httptest.NewRequest(http.MethodPost, "/pets", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid multipart body", rr.Code)
	}
}

func TestHandlerValidatesPlainTextBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: plainTextSpec()})
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader("hello apisix"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid text body", rr.Code)
	}
}

func TestHandlerValidatesXMLBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: xmlBodySpec()})
	req := httptest.NewRequest(
		http.MethodPost,
		"/users",
		strings.NewReader(
			`<user id="u-1"><name>alice</name><age>3</age><tags><tag>admin</tag><tag>viewer</tag></tags></user>`,
		),
	)
	req.Header.Set("Content-Type", "application/xml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid XML body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesYAMLBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: yamlBodySpec()})
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("name: alice\nage: 3\n"))
	req.Header.Set("Content-Type", "application/yaml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid YAML body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMalformedYAMLBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: yamlBodySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("name: [alice\nage: 3\n"))
	req.Header.Set("Content-Type", "text/yaml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for an invalid YAML body")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for invalid YAML body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMalformedXMLBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: xmlBodySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(`<user><name>alice</name>`))
	req.Header.Set("Content-Type", "application/xml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for an invalid XML body")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for invalid XML body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesStructuredJSONMediaTypeSuffix(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: jsonSuffixBodySpec()})
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"id":"evt-1"}`))
	req.Header.Set("Content-Type", "application/problem+json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for +json body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerMatchesStructuredJSONWildcardMediaType(t *testing.T) {
	spec := strings.Replace(jsonSuffixBodySpec(), `"application/json"`, `"application/*+json"`, 1)
	p := newTestPlugin(t, Config{Spec: spec})
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"id":"evt-1"}`))
	req.Header.Set("Content-Type", "application/problem+json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for wildcard +json body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerMatchesStructuredXMLAndYAMLWildcardMediaTypes(t *testing.T) {
	tests := []struct {
		name        string
		spec        string
		contentType string
		body        string
		path        string
	}{
		{
			name:        "xml",
			spec:        strings.Replace(xmlBodySpec(), `"application/xml"`, `"application/*+xml"`, 1),
			contentType: "application/problem+xml",
			body:        `<user id="u-1"><name>alice</name><age>3</age><tags><tag>admin</tag><tag>viewer</tag></tags></user>`,
			path:        "/users",
		},
		{
			name:        "yaml",
			spec:        strings.Replace(yamlBodySpec(), `"application/yaml"`, `"application/*+yaml"`, 1),
			contentType: "application/problem+yaml",
			body:        "name: alice\nage: 3\n",
			path:        "/users",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Spec: test.spec})
			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", test.contentType)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("response code = %d, want 204 for wildcard %s body: %s", rr.Code, test.name, rr.Body.String())
			}
		})
	}
}

func TestHandlerValidatesOctetStreamBodyAsString(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: octetStreamBodySpec()})
	req := httptest.NewRequest(http.MethodPost, "/blobs", strings.NewReader("binary-payload"))
	req.Header.Set("Content-Type", "application/octet-stream")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for octet-stream body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesCustomOpaqueMediaBodyAsString(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: customOpaqueBodySpec()})
	req := httptest.NewRequest(http.MethodPost, "/payload", strings.NewReader("csv-like,payload\n"))
	req.Header.Set("Content-Type", "text/csv")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for opaque custom media body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesSpaceDelimitedQueryArray(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: styledQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?tags=red%20blue", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid space-delimited query array", rr.Code)
	}
}

func TestHandlerRejectsDelimitedQueryObject(t *testing.T) {
	for _, test := range []struct {
		name  string
		style string
		query string
	}{
		{name: "space", style: "spaceDelimited", query: "filter=role+admin+age+3"},
		{name: "pipe", style: "pipeDelimited", query: "filter=role%7Cadmin%7Cage%7C3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Spec: delimitedQueryObjectSpec(test.style)})
			req := httptest.NewRequest(http.MethodGet, "/pets?"+test.query, nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf(
					"response code = %d, want 400 for %s-delimited query object: %s",
					rr.Code,
					test.style,
					rr.Body.String(),
				)
			}
		})
	}
}

func TestHandlerRejectsMalformedDelimitedQueryObject(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: delimitedQueryObjectSpec("space"), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter=role+admin+age", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for malformed delimited query object")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "failed to parse openapi spec") {
		t.Fatalf("response body = %q, want spec parse failure", rr.Body.String())
	}
}

func TestHandlerRejectsRepeatedDelimitedQueryArrayValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		style string
		query string
	}{
		{name: "space", style: "spaceDelimited", query: "tags=red+blue&tags=green+yellow"},
		{name: "pipe", style: "pipeDelimited", query: "tags=red%7Cblue&tags=green%7Cyellow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Spec: repeatedDelimitedQuerySpec(test.style)})
			req := httptest.NewRequest(http.MethodGet, "/pets?"+test.query, nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf(
					"response code = %d, want 400 for repeated %s-delimited values: %s",
					rr.Code,
					test.style,
					rr.Body.String(),
				)
			}
		})
	}
}

func TestHandlerValidatesDeepObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: deepObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter%5Bname%5D=doggie&filter%5Bage%5D=3", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid deepObject query parameter: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsRepeatedDeepObjectScalarProperty(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: deepObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(
		http.MethodGet,
		"/pets?filter%5Bname%5D=doggie&filter%5Bname%5D=cat&filter%5Bage%5D=3",
		nil,
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for a repeated deepObject scalar property")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "filter") {
		t.Fatalf("response body = %q, want deepObject error mentioning filter", rr.Body.String())
	}
}

func TestHandlerValidatesJSONContentQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: jsonContentQueryParameterSpec(), VerboseErrors: true})
	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{
			name:       "valid object",
			query:      `coordinates=%7B%22lat%22%3A1.5%2C%22long%22%3A2.5%7D`,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "missing required property",
			query:      `coordinates=%7B%22lat%22%3A1.5%7D`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed JSON",
			query:      `coordinates=not-json`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "repeated parameter",
			query:      `coordinates=%7B%22lat%22%3A1.5%2C%22long%22%3A2.5%7D&coordinates=%7B%22lat%22%3A1.5%2C%22long%22%3A2.5%7D`,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/pets?"+test.query, nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestHandlerRejectsNonJSONContentQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: textContentQueryParameterSpec(), VerboseErrors: true})
	for _, value := range []string{"3", "not-an-integer"} {
		req := httptest.NewRequest(http.MethodGet, "/pets?limit="+value, nil)
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("next handler was called for a non-JSON content parameter")
		})).ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("response code = %d, want 400 for %q: %s", rr.Code, value, rr.Body.String())
		}
	}
}

func TestHandlerRejectsUnsupportedContentQueryMediaType(t *testing.T) {
	spec := strings.Replace(textContentQueryParameterSpec(), "text/plain", "application/xml", 1)
	p := newTestPlugin(t, Config{Spec: spec, VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?limit=3", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for unsupported parameter content media type")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "limit") {
		t.Fatalf("response body = %q, want parameter name in error", rr.Body.String())
	}
}

func TestHandlerValidatesExplodedFormObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: formObjectQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?role=admin&first=Alex", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for exploded form object", rr.Code)
	}
}

func TestHandlerRejectsUnprefixedExplodedFormObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: freeFormExplodedFormObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?role=admin&tenant=blue", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for unprefixed exploded form object properties")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for unprefixed exploded form object: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesNonExplodedFormObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: nonExplodedFormObjectQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter=name,Alex,age,3", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for non-exploded form object: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMalformedNonExplodedFormObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: nonExplodedFormObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter=name,Alex,age", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for malformed non-exploded form object")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "filter") {
		t.Fatalf("response body = %q, want parameter name", rr.Body.String())
	}
}

func TestHandlerAllowsRepeatedNonExplodedFormObjectField(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: nonExplodedFormObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter=name,Alex,name,Bob,age,3", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf(
			"response code = %d, want 204 for repeated non-exploded form object field: %s",
			rr.Code,
			rr.Body.String(),
		)
	}
}

func TestHandlerValidatesMatrixLabelAndSimplePathParameters(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: styledPathSpec()})
	req := httptest.NewRequest(
		http.MethodGet,
		"/pets/;id=3/.red.blue/role=admin,first=Alex",
		nil,
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for matrix/label/simple path parameters: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsParameterStylesUnsupportedForLocation(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		style       string
		requestPath string
		query       string
		header      string
	}{
		{
			name:        "query matrix",
			in:          "query",
			style:       "matrix",
			requestPath: "/pets",
			query:       "?id=3",
		},
		{
			name:        "path form",
			in:          "path",
			style:       "form",
			requestPath: "/pets/3",
		},
		{
			name:        "header form",
			in:          "header",
			style:       "form",
			requestPath: "/pets",
			header:      "3",
		},
		{
			name:        "cookie simple",
			in:          "cookie",
			style:       "simple",
			requestPath: "/pets",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Spec:          invalidParameterStyleSpec(test.in, test.style),
				VerboseErrors: true,
			})
			req := httptest.NewRequest(http.MethodGet, test.requestPath+test.query, nil)
			if test.header != "" {
				req.Header.Set("X-ID", test.header)
			}
			if test.in == "cookie" {
				req.Header.Set("Cookie", "id=3")
			}
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next handler was called for an unsupported parameter style")
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("response code = %d, want 500: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "failed to parse openapi spec") {
				t.Fatalf("response body = %q, want spec parse failure", rr.Body.String())
			}
		})
	}
}

func TestHandlerValidatesExplodedSimpleHeaderObject(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: simpleHeaderObjectSpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets", nil)
	req.Header.Set("X-Filter", "role=admin,first=Alex")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for simple header object: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerCookieParameterStyles(t *testing.T) {
	tests := []struct {
		name       string
		spec       string
		cookie     string
		wantStatus int
	}{
		{
			name:       "exploded object",
			spec:       explodedCookieObjectSpec(),
			cookie:     "role=admin; first=Alex",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "non-exploded object",
			spec:       nonExplodedCookieObjectSpec(),
			cookie:     "filter=name,Alex,age,3",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "repeated array",
			spec:       repeatedCookieArraySpec(),
			cookie:     "tags=red; tags=blue",
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Spec: test.spec})
			req := httptest.NewRequest(http.MethodGet, "/pets", nil)
			req.Header.Set("Cookie", test.cookie)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != test.wantStatus {
				t.Fatalf(
					"response code = %d, want %d for %s cookie: %s",
					rr.Code,
					test.wantStatus,
					test.name,
					rr.Body.String(),
				)
			}
		})
	}
}

func TestHandlerPreservesCommaInExplodedFormArrayQuery(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: explodedFormArrayQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?tags=a%2Cb", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf(
			"response code = %d, want 204 for one exploded array value containing a comma: %s",
			rr.Code,
			rr.Body.String(),
		)
	}
}

func TestHandlerUsesDefaultExplodeForFormArrayQuery(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: defaultExplodedFormArrayQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?tags=a%2Cb", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for default exploded form array: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsRepeatedNonExplodedFormArrayQuery(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: repeatedNonExplodedFormArrayQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?tags=red&tags=blue", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for a repeated non-exploded form array")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for repeated non-exploded form array: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsRepeatedDeepObjectArrayValues(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: deepObjectArrayQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter%5Btags%5D=red&filter%5Btags%5D=blue", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for repeated deepObject array values: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsInvalidCookieParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: explodedCookieObjectSpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets", nil)
	req.Header.Set("Cookie", "role=admin")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for an invalid cookie object")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for an invalid cookie object: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerCanSkipValidationAndAllowMismatch(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "skip query header body",
			cfg: Config{
				Spec:                        testSpec(),
				SkipRequestBodyValidation:   true,
				SkipRequestHeaderValidation: true,
				SkipQueryParamValidation:    true,
			},
		},
		{
			name: "log only mismatch",
			cfg: Config{
				Spec:             testSpec(),
				RejectIfNotMatch: new(false),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, tt.cfg)
			req := httptest.NewRequest(http.MethodPost, "/pets/123", strings.NewReader(`{"age":3}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusAccepted {
				t.Fatalf("response code = %d, want 202", rr.Code)
			}
		})
	}
}

func TestHandlerFetchesSpecURLWithConfiguredHeaders(t *testing.T) {
	var sawAuth bool
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got == "Bearer spec-token" {
			sawAuth = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{
		SpecURL: specServer.URL,
		SpecURLRequestHeaders: map[string]string{
			"Authorization": "Bearer spec-token",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Trace", "trace-id")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want 201", rr.Code)
	}
	if !sawAuth {
		t.Fatal("spec_url request missing configured Authorization header")
	}
}

func TestHandlerRefreshesSpecURLAfterMetadataTTL(t *testing.T) {
	var fetches atomic.Int32
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{SpecURL: specServer.URL})
	p.metadata.SpecURLTTL = 10
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	serve := func() {
		req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Trace", "trace-id")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("response code = %d, want 204", recorder.Code)
		}
	}

	serve()
	serve()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches after cached request = %d, want 1", got)
	}

	now = now.Add(11 * time.Second)
	serve()
	deadline := time.Now().Add(2 * time.Second)
	for fetches.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches after TTL expiry = %d, want 2", got)
	}
	stopOASTestPlugin(t, p)
}

func TestHandlerResolvesExternalSchemaRef(t *testing.T) {
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/schemas/pet.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "required": ["name"],
        "properties": {
          "name": {"type": "string"}
        }
      }
    }
  }
}`))
	}))
	defer externalServer.Close()

	spec := strings.Replace(
		testSpecWithComponentsRef(),
		"#/components/schemas/Pet",
		externalServer.URL+"/schemas/pet.json#/components/schemas/Pet",
		1,
	)
	p := newTestPlugin(t, Config{
		Spec:          spec,
		VerboseErrors: true,
	})

	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"age":3}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "name") {
		t.Fatalf("response body = %q, want external schema validation error mentioning name", rr.Body.String())
	}
}

func TestHandlerResolvesRelativeExternalSchemaRefFromSpecURL(t *testing.T) {
	spec := strings.Replace(
		testSpecWithComponentsRef(),
		"#/components/schemas/Pet",
		"schemas/pet.json#/components/schemas/Pet",
		1,
	)
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/openapi.json":
			_, _ = w.Write([]byte(spec))
		case "/schemas/pet.json":
			_, _ = w.Write([]byte(`{
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "required": ["name"],
        "properties": {"name": {"type": "string"}}
      }
    }
  }
}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{
		SpecURL:       specServer.URL + "/openapi.json",
		VerboseErrors: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"age":3}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "name") {
		t.Fatalf("response body = %q, want relative external schema validation error mentioning name", rr.Body.String())
	}
}

func TestHandlerLazilyRejectsExternalSchemaRefCycle(t *testing.T) {
	var fetches atomic.Int32
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/a.json":
			_, _ = w.Write([]byte(`{"$ref":"b.json"}`))
		case "/b.json":
			_, _ = w.Write([]byte(`{"$ref":"a.json"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer specServer.Close()

	spec := `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {"$ref": "` + specServer.URL + `/a.json"}
            }
          }
        }
      }
    }
  }
}`
	p := &Plugin{config: Config{Spec: spec}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newOASScopedSecretHarness(t, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want lazy external ref resolution", err)
	}
	if fetches.Load() != 0 {
		t.Fatalf("external ref fetches after PostInit() = %d, want 0", fetches.Load())
	}

	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"name":"doggie"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for cyclic external ref")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to parse openapi spec") {
		t.Fatalf("response body = %q, want spec parse failure", rr.Body.String())
	}
	if fetches.Load() == 0 {
		t.Fatal("external refs were not resolved lazily during the request")
	}
}

func TestHandlerLazilyRejectsMissingExternalSchemaRef(t *testing.T) {
	var fetches atomic.Int32
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		http.NotFound(w, r)
	}))
	defer externalServer.Close()

	spec := strings.Replace(
		testSpecWithComponentsRef(),
		"#/components/schemas/Pet",
		externalServer.URL+"/missing.json#/components/schemas/Pet",
		1,
	)
	p := &Plugin{config: Config{Spec: spec}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newOASScopedSecretHarness(t, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want lazy external ref resolution", err)
	}
	if fetches.Load() != 0 {
		t.Fatalf("external ref fetches after PostInit() = %d, want 0", fetches.Load())
	}

	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"name":"doggie"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for missing external ref")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to parse openapi spec") {
		t.Fatalf("response body = %q, want spec parse failure", rr.Body.String())
	}
	if fetches.Load() == 0 {
		t.Fatal("external ref was not resolved lazily during the request")
	}
}

func testSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets/{id}": {
      "post": {
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "trace", "in": "header", "required": true, "schema": {"type": "string"}},
          {"name": "verbose", "in": "query", "required": true, "schema": {"type": "string"}}
        ],
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "required": ["name"],
                "properties": {
                  "name": {"type": "string"},
                  "age": {"type": "integer"}
                }
              }
            }
          }
        },
        "responses": {"200": {"description": "OK"}}
      }
    }
  }
}`
}

func testSpecWithComponentsRef() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {"$ref": "#/components/schemas/Pet"}
            }
          }
        },
        "responses": {"200": {"description": "OK"}}
      }
    }
  },
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "required": ["name"],
        "properties": {
          "name": {"type": "string"},
          "age": {"type": "integer"}
        }
      }
    }
  }
}`
}

func formSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "required": true,
          "content": {
            "application/x-www-form-urlencoded": {
              "schema": {
                "type": "object",
                "required": ["name"],
                "properties": {
                  "name": {"type": "string"},
                  "age": {"type": "integer"}
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func styledQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "query",
            "style": "spaceDelimited",
            "schema": {"type": "array", "items": {"type": "string"}}
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func repeatedDelimitedQuerySpec(style string) string {
	return fmt.Sprintf(`{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "query",
            "required": true,
            "style": %q,
            "schema": {
              "type": "array",
              "minItems": 4,
              "maxItems": 4,
              "items": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`, style)
}

func delimitedQueryObjectSpec(style string) string {
	return fmt.Sprintf(`{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": %q,
            "schema": {
              "type": "object",
              "required": ["role", "age"],
              "properties": {
                "role": {"type": "string"},
                "age": {"type": "integer"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`, style)
}

func deepObjectQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "style": "deepObject",
            "schema": {
              "type": "object",
              "required": ["name", "age"],
              "properties": {
                "name": {"type": "string"},
                "age": {"type": "integer"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func jsonContentQueryParameterSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "coordinates",
            "in": "query",
            "required": true,
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "required": ["lat", "long"],
                  "properties": {
                    "lat": {"type": "number"},
                    "long": {"type": "number"}
                  }
                }
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func textContentQueryParameterSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "limit",
            "in": "query",
            "required": true,
            "content": {
              "text/plain": {
                "schema": {"type": "integer"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func formObjectQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": true,
            "schema": {
              "type": "object",
              "required": ["role", "first"],
              "properties": {
                "role": {"type": "string"},
                "first": {"type": "string"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func freeFormExplodedFormObjectQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": true,
            "schema": {
              "type": "object",
              "minProperties": 2,
              "additionalProperties": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func nonExplodedFormObjectQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": false,
            "schema": {
              "type": "object",
              "required": ["name", "age"],
              "properties": {
                "name": {"type": "string"},
                "age": {"type": "integer"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func styledPathSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets/{id}/{tags}/{filter}": {
      "get": {
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "style": "matrix",
            "schema": {"type": "integer"}
          },
          {
            "name": "tags",
            "in": "path",
            "required": true,
            "style": "label",
            "explode": true,
            "schema": {"type": "array", "items": {"type": "string"}, "minItems": 2}
          },
          {
            "name": "filter",
            "in": "path",
            "required": true,
            "style": "simple",
            "explode": true,
            "schema": {
              "type": "object",
              "required": ["role", "first"],
              "properties": {
                "role": {"type": "string"},
                "first": {"type": "string"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func invalidParameterStyleSpec(location, style string) string {
	path := "/pets"
	if location == "path" {
		path = "/pets/{id}"
	}
	return fmt.Sprintf(`{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    %q: {
      "get": {
        "parameters": [
          {
            "name": %q,
            "in": %q,
            "required": true,
            "style": %q,
            "schema": {"type": "integer"}
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`, path, map[string]string{
		"query":  "id",
		"path":   "id",
		"header": "X-ID",
		"cookie": "id",
	}[location], location, style)
}

func simpleHeaderObjectSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "X-Filter",
            "in": "header",
            "required": true,
            "style": "simple",
            "explode": true,
            "schema": {
              "type": "object",
              "required": ["role", "first"],
              "properties": {
                "role": {"type": "string"},
                "first": {"type": "string"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func explodedCookieObjectSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "cookie",
            "required": true,
            "style": "form",
            "explode": true,
            "schema": {
              "type": "object",
              "required": ["role", "first"],
              "properties": {
                "role": {"type": "string"},
                "first": {"type": "string"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func nonExplodedCookieObjectSpec() string {
	return strings.Replace(nonExplodedFormObjectQuerySpec(), `"in": "query"`, `"in": "cookie"`, 1)
}

func repeatedCookieArraySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "cookie",
            "required": true,
            "schema": {
              "type": "array",
              "minItems": 2,
              "maxItems": 2,
              "items": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func explodedFormArrayQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": true,
            "schema": {
              "type": "array",
              "minItems": 1,
              "maxItems": 1,
              "items": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func defaultExplodedFormArrayQuerySpec() string {
	return strings.Replace(explodedFormArrayQuerySpec(), "            \"explode\": true,\n", "", 1)
}

func repeatedNonExplodedFormArrayQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": false,
            "schema": {
              "type": "array",
              "minItems": 2,
              "maxItems": 2,
              "items": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func deepObjectArrayQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": "deepObject",
            "schema": {
              "type": "object",
              "required": ["tags"],
              "properties": {
                "tags": {
                  "type": "array",
                  "minItems": 2,
                  "items": {"type": "string"}
                }
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func jsonSuffixBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Event API", "version": "1.0.0"},
  "paths": {
    "/events": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "required": ["id"],
                "properties": {"id": {"type": "string"}}
              }
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func octetStreamBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Blob API", "version": "1.0.0"},
  "paths": {
    "/blobs": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "required": true,
          "content": {
            "application/octet-stream": {
              "schema": {"type": "string", "minLength": 1}
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func customOpaqueBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Opaque API", "version": "1.0.0"},
  "paths": {
    "/payload": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "required": true,
          "content": {
            "text/csv": {
              "schema": {"type": "string", "minLength": 1}
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func plainTextSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Message API", "version": "1.0.0"},
  "paths": {
    "/messages": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "required": true,
          "content": {
            "text/plain": {"schema": {"type": "string", "minLength": 1}}
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func yamlBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "User API", "version": "1.0.0"},
  "paths": {
    "/users": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "required": true,
          "content": {
            "application/yaml": {
              "schema": {
                "type": "object",
                "required": ["name", "age"],
                "properties": {
                  "name": {"type": "string", "minLength": 1},
                  "age": {"type": "integer", "minimum": 1}
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func xmlBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "User API", "version": "1.0.0"},
  "paths": {
    "/users": {
      "post": {
        "responses": {"200": {"description": "OK"}},
        "requestBody": {
          "required": true,
          "content": {
            "application/xml": {
              "schema": {
                "type": "object",
                "required": ["id", "name", "age", "tags"],
                "properties": {
                  "id": {"type": "string", "minLength": 1, "xml": {"attribute": true}},
                  "name": {"type": "string", "minLength": 1},
                  "age": {"type": "integer", "minimum": 1},
                  "tags": {
                    "type": "array",
                    "minItems": 2,
                    "items": {"type": "string"},
                    "xml": {"wrapped": true}
                  }
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func TestValidationSkipMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(*Config)
		wantStatus int
	}{
		{"none", func(*Config) {}, http.StatusBadRequest},
		{"body", func(c *Config) { c.SkipRequestBodyValidation = true }, http.StatusBadRequest},
		{"all", func(c *Config) {
			c.VerboseErrors = true
			c.SkipRequestBodyValidation = true
			c.SkipRequestHeaderValidation = true
			c.SkipQueryParamValidation = true
			c.SkipPathParamsValidation = true
		}, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Spec: skipMatrixSpec()}
			tc.configure(&cfg)
			p := newTestPlugin(t, cfg)

			req := httptest.NewRequest(http.MethodPost, "/pets/123?age=not-an-integer", nil)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Trace", "")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("response code = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

func skipMatrixSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets/{id}": {
      "post": {
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}},
          {"name": "X-Trace", "in": "header", "required": true, "schema": {"type": "string", "minLength": 1}},
          {"name": "age", "in": "query", "required": true, "schema": {"type": "integer"}}
        ],
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}}}
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func TestHandlerValidatesConcurrentlyDuringBlockingSpecRefresh(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fetches.Add(1) == 2 {
			close(blocked)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{SpecURL: specServer.URL})
	p.metadata.SpecURLTTL = 10
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }

	serve := func() int {
		req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Trace", "trace-id")
		recorder := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		})).ServeHTTP(recorder, req)
		return recorder.Code
	}

	// Prime the first validator synchronously.
	if code := serve(); code != http.StatusCreated {
		t.Fatalf("prime response code = %d, want 201", code)
	}

	// A due request wakes a blocked remote refresh; concurrent requests must
	// keep validating with the published validator without waiting.
	now = now.Add(11 * time.Second)
	started := make(chan struct{})
	go func() {
		close(started)
		serve()
	}()
	<-started
	<-blocked

	var wg sync.WaitGroup
	results := make(chan int, 8)
	for range 8 {
		wg.Go(func() {
			results <- serve()
		})
	}
	wg.Wait()
	close(results)
	for code := range results {
		if code != http.StatusCreated {
			t.Fatalf("concurrent response code = %d, want 201 during blocked refresh", code)
		}
	}
	close(release)
	stopOASTestPlugin(t, p)
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want exactly one refresh", got)
	}
}

func TestHandlerFailedSpecRefreshPreservesPriorValidator(t *testing.T) {
	var fetches atomic.Int32
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fetches.Add(1) == 2 {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{SpecURL: specServer.URL})
	p.metadata.SpecURLTTL = 10
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }

	serve := func() int {
		req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Trace", "trace-id")
		recorder := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		})).ServeHTTP(recorder, req)
		return recorder.Code
	}

	if code := serve(); code != http.StatusCreated {
		t.Fatalf("prime response code = %d, want 201", code)
	}
	now = now.Add(11 * time.Second)
	serve()
	deadline := time.Now().Add(2 * time.Second)
	for fetches.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if code := serve(); code != http.StatusCreated {
		t.Fatalf("response code after failed refresh = %d, want prior validator serving 201", code)
	}
	stopOASTestPlugin(t, p)
}

func TestHandlerSpecRefreshIsBoundToPluginLifecycle(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fetches.Add(1) == 2 {
			close(blocked)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{SpecURL: specServer.URL})
	p.metadata.SpecURLTTL = 10
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }

	serve := func() int {
		req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Trace", "trace-id")
		recorder := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		})).ServeHTTP(recorder, req)
		return recorder.Code
	}

	if code := serve(); code != http.StatusCreated {
		t.Fatalf("prime response code = %d, want 201", code)
	}

	// A due request triggers the refresh and completes with the stale
	// validator; the request's own context is cancelled right after, which
	// must not cancel the shared refresh (it is bound to the plugin
	// lifecycle, so generation task quiescence is what joins it).
	now = now.Add(11 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Trace", "trace-id")
		recorder := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		})).ServeHTTP(recorder, req.WithContext(ctx))
		done <- recorder.Code
	}()
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("due request response code = %d, want stale validator 201", code)
	}
	cancel()
	<-blocked

	stopped := make(chan struct{})
	go func() {
		stopOASTestPlugin(t, p)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Stop did not cancel and join the spec refresh")
	}
	close(release)
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want the refresh to complete despite request cancellation", got)
	}
}
