package openfunction

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
	"github.com/wklken/apisix-go/pkg/secret"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	raw := ""
	if cfg.Authorization != nil {
		raw = cfg.Authorization.ServiceToken
	}
	broker := &openFunctionScopedBroker{values: map[string]string{raw: raw}}
	capabilityValue, registration, scope := registerOpenFunctionScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerInvokesOpenFunctionWithBasicAuthorization(t *testing.T) {
	var gotAuthorization string
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("X-Function-Result", "openfunction")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello from openfunction"))
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: function.URL + "/default/function-sample/test",
		Authorization: &Authorization{
			ServiceToken: "test:test",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/hello", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := http.StatusInternalServerError
		http.Error(w, http.StatusText(t), t)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusCreated)
	}
	if got := rr.Body.String(); got != "hello from openfunction" {
		t.Fatalf("response body = %q, want function body", got)
	}
	if got := rr.Header().Get("X-Function-Result"); got != "openfunction" {
		t.Fatalf("X-Function-Result = %q, want openfunction", got)
	}
	if gotAuthorization != "Basic dGVzdDp0ZXN0" {
		t.Fatalf("Authorization = %q, want Basic dGVzdDp0ZXN0", gotAuthorization)
	}
}

func TestRunRequestPhasePublishesUpstreamSource(t *testing.T) {
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer function.Close()
	p := newTestPlugin(t, Config{FunctionURI: function.URL})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	response := httptest.NewRecorder()

	result := p.RunRequestPhase(response, request)
	if result.Decision != 1 || result.Source != apisixctx.ResponseSourceUpstream {
		t.Fatalf("result = %+v, want upstream stop", result)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceUpstream {
		t.Fatalf("source = %q, want upstream", lifecycle.ResponseSource())
	}
}

func TestMaterializeScopedSecretsOwnsOpenFunctionServiceToken(t *testing.T) {
	const raw = "$ENV://OPENFUNCTION_SERVICE_TOKEN"
	broker := &openFunctionScopedBroker{values: map[string]string{raw: "service:test"}}
	capabilityValue, registration, scope := registerOpenFunctionScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{ServiceToken: raw},
	}}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if got := broker.scopedCalls(); len(got) != 1 ||
		got[0].Field != "authorization.service_token" ||
		got[0].Plugin != name || got[0].Resource.ID != "openfunction-route" {
		t.Fatalf("scoped calls = %#v, want one exact declaration", got)
	}
	if p.config.Authorization.ServiceToken == raw ||
		strings.Contains(p.config.Authorization.ServiceToken, "service:test") ||
		!strings.HasPrefix(p.config.Authorization.ServiceToken, "plugin_config#sha256:") {
		t.Fatalf("public service token = %q, want redacted descriptor", p.config.Authorization.ServiceToken)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
	p.processRequest(req, function_upstream.Config{})
	if got := req.Header.Get("Authorization"); got != "Basic c2VydmljZTp0ZXN0" {
		t.Fatalf("Authorization = %q, want resolved service token", got)
	}
}

func TestMaterializeScopedSecretsSkipsAbsentAndEmptyServiceToken(t *testing.T) {
	tests := []struct {
		name                 string
		config               Config
		includeAuthorization bool
	}{
		{
			name:   "absent",
			config: Config{FunctionURI: "http://function.invalid"},
		},
		{
			name: "empty",
			config: Config{
				FunctionURI:   "http://function.invalid",
				Authorization: &Authorization{},
			},
			includeAuthorization: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker := &openFunctionScopedBroker{}
			capabilityValue, registration, scope := registerOpenFunctionScopedRouteConfigAt(
				t, broker, 1, "", tt.includeAuthorization,
			)
			t.Cleanup(func() {
				if err := registration.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})

			p := &Plugin{config: tt.config}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			if got := broker.scopedCalls(); len(got) != 0 {
				t.Fatalf("scoped calls = %#v, want zero", got)
			}
		})
	}
}

func TestMaterializeScopedSecretsSupportsLiteralAndManagedReferences(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "literal", raw: "literal-service-token", resolved: "literal-service-token"},
		{name: "environment", raw: "$ENV://OPENFUNCTION_TOKEN", resolved: "environment-token"},
		{name: "managed", raw: "$secret://vault/openfunction/token", resolved: "managed-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker := &openFunctionScopedBroker{values: map[string]string{tt.raw: tt.resolved}}
			capabilityValue, registration, scope := registerOpenFunctionScopedRoute(t, broker, tt.raw)
			t.Cleanup(func() {
				if err := registration.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})

			p := &Plugin{config: Config{
				FunctionURI:   "http://function.invalid",
				Authorization: &Authorization{ServiceToken: tt.raw},
			}}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			calls := broker.scopedCalls()
			if len(calls) != 1 || calls[0].Field != "authorization.service_token" ||
				calls[0].Source != capability.SecretPluginConfig {
				t.Fatalf("scoped calls = %#v, want exact service token field", calls)
			}
			if strings.Contains(p.config.Authorization.ServiceToken, tt.resolved) ||
				strings.Contains(p.config.Authorization.ServiceToken, tt.raw) ||
				!strings.HasPrefix(p.config.Authorization.ServiceToken, "plugin_config#sha256:") {
				t.Fatalf("public service token = %q, want descriptor only", p.config.Authorization.ServiceToken)
			}

			req := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
			p.processRequest(req, function_upstream.Config{})
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte(tt.resolved))
			if got := req.Header.Get("Authorization"); got != want {
				t.Fatalf("Authorization = %q, want %q", got, want)
			}
		})
	}
}

func TestMaterializeScopedSecretsRedactsResolutionFailure(t *testing.T) {
	const raw = "$ENV://OPENFUNCTION_FAILURE"
	broker := &openFunctionScopedBroker{
		values:  map[string]string{raw: "private-service-token"},
		failRaw: raw,
	}
	capabilityValue, registration, scope := registerOpenFunctionScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{ServiceToken: raw},
	}}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil")
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "private-service-token") ||
		strings.Contains(err.Error(), "OPENFUNCTION_FAILURE") {
		t.Fatalf("materialization error leaked secret details: %v", err)
	}
	if got := broker.scopedCalls(); len(got) != 1 {
		t.Fatalf("scoped calls = %#v, want one failed exact call", got)
	}
	if p.config.Authorization.ServiceToken != raw {
		t.Fatalf("failed materialization changed public token = %q", p.config.Authorization.ServiceToken)
	}
}

func TestOpenFunctionGenerationsDoNotShareAuthorizationOrRetirement(t *testing.T) {
	const (
		rawN  = "$ENV://OPENFUNCTION_TOKEN_N"
		rawN1 = "$ENV://OPENFUNCTION_TOKEN_N1"
	)
	broker := &openFunctionScopedBroker{values: map[string]string{
		rawN:  "service-n",
		rawN1: "service-n1",
	}}
	capabilityN, registrationN, scopeN := registerOpenFunctionScopedRouteAt(t, broker, 11, rawN)
	capabilityN1, registrationN1, scopeN1 := registerOpenFunctionScopedRouteAt(t, broker, 12, rawN1)
	t.Cleanup(func() {
		if err := registrationN.Close(context.Background()); err != nil {
			t.Error(err)
		}
		if err := registrationN1.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	pN := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{ServiceToken: rawN},
	}}
	pN1 := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{ServiceToken: rawN1},
	}}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scopeN, capabilityN, pN); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scopeN1, capabilityN1, pN1); err != nil {
		t.Fatal(err)
	}

	requestN := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
	pN.processRequest(requestN, function_upstream.Config{})
	requestN1 := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
	pN1.processRequest(requestN1, function_upstream.Config{})
	if got := requestN.Header.Get("Authorization"); got != "Basic c2VydmljZS1u" {
		t.Fatalf("generation N Authorization = %q, want generation N value", got)
	}
	if got := requestN1.Header.Get("Authorization"); got != "Basic c2VydmljZS1uMQ==" {
		t.Fatalf("generation N+1 Authorization = %q, want generation N+1 value", got)
	}

	pN.Stop()
	pN.Stop()
	retired := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
	pN.processRequest(retired, function_upstream.Config{})
	if got := retired.Header.Get("Authorization"); got != "" {
		t.Fatalf("retired generation Authorization = %q, want empty", got)
	}
	retained := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
	pN1.processRequest(retained, function_upstream.Config{})
	if got := retained.Header.Get("Authorization"); got != "Basic c2VydmljZS1uMQ==" {
		t.Fatalf("retained generation Authorization = %q, want generation N+1 value", got)
	}
}

func TestOpenFunctionMaterializeSecretsFailsClosed(t *testing.T) {
	const raw = "$ENV://OPENFUNCTION_LEGACY_TOKEN"
	t.Setenv("OPENFUNCTION_LEGACY_TOKEN", "legacy-service-token")
	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{ServiceToken: raw},
	}}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("MaterializeSecrets() error = %v, want credential unavailable", err)
	}
}

func TestOpenFunctionScopedProcessAndStopDoNotRaceCredentialUse(t *testing.T) {
	const raw = "$ENV://OPENFUNCTION_CONCURRENT_SCOPED"
	broker := &openFunctionScopedBroker{values: map[string]string{raw: "scoped-concurrent-token"}}
	capabilityValue, registration, scope := registerOpenFunctionScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{ServiceToken: raw},
	}}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}

	requestStarted := make(chan struct{})
	stopStarted := make(chan struct{})
	allowRequest := make(chan struct{})
	var stopEvent sync.Once
	p.testLifecycleHook = func(event string) {
		switch event {
		case lifecycleBeforeAuthorizationUse:
			close(requestStarted)
			<-allowRequest
		case lifecycleAfterUpstreamStop:
			stopEvent.Do(func() { close(stopStarted) })
		}
	}
	requestDone := make(chan *http.Request, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
		p.processRequest(req, function_upstream.Config{})
		requestDone <- req
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped credential use")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Stop to begin")
	}
	select {
	case <-stopDone:
		t.Fatal("Stop completed before the request finished using scoped credential")
	default:
	}
	close(allowRequest)
	var req *http.Request
	select {
	case req = <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped request")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped Stop")
	}
	if got := req.Header.Get("Authorization"); got != "Basic c2NvcGVkLWNvbmN1cnJlbnQtdG9rZW4=" {
		t.Fatalf("scoped Authorization = %q, want request credential", got)
	}
	p.Stop()
	retired := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
	p.processRequest(retired, function_upstream.Config{})
	if got := retired.Header.Get("Authorization"); got != "" {
		t.Fatalf("post-Stop scoped Authorization = %q, want empty", got)
	}
}

func TestOpenFunctionLegacyProcessAndStopDoNotRaceCredentialUse(t *testing.T) {
	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{ServiceToken: "legacy-concurrent-token"},
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	const raw = "legacy-concurrent-token"
	broker := &openFunctionScopedBroker{values: map[string]string{raw: raw}}
	capabilityValue, registration, scope := registerOpenFunctionScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}

	requestStarted := make(chan struct{})
	stopStarted := make(chan struct{})
	allowRequest := make(chan struct{})
	var stopEvent sync.Once
	p.testLifecycleHook = func(event string) {
		switch event {
		case lifecycleBeforeAuthorizationUse:
			close(requestStarted)
			<-allowRequest
		case lifecycleAfterUpstreamStop:
			stopEvent.Do(func() { close(stopStarted) })
		}
	}
	requestDone := make(chan *http.Request, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
		p.processRequest(req, function_upstream.Config{})
		requestDone <- req
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for legacy credential use")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Stop to begin")
	}
	select {
	case <-stopDone:
		t.Fatal("Stop completed before the request finished using legacy credential")
	default:
	}
	close(allowRequest)
	var req *http.Request
	select {
	case req = <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for legacy request")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for legacy Stop")
	}
	if got := req.Header.Get("Authorization"); got != "Basic bGVnYWN5LWNvbmN1cnJlbnQtdG9rZW4=" {
		t.Fatalf("legacy Authorization = %q, want request credential", got)
	}
	p.Stop()
	retired := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
	p.processRequest(retired, function_upstream.Config{})
	if got := retired.Header.Get("Authorization"); got != "" {
		t.Fatalf("post-Stop legacy Authorization = %q, want empty", got)
	}
}

func TestMaterializeScopedSecretsFailureCanRetryWithoutRetainedState(t *testing.T) {
	const raw = "$ENV://OPENFUNCTION_RETRY"
	broker := &openFunctionScopedBroker{
		values:  map[string]string{raw: "retry-service-token"},
		failRaw: raw,
	}
	capabilityValue, registration, scope := registerOpenFunctionScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{ServiceToken: raw},
	}}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err == nil {
		t.Fatal("first MaterializeScopedPluginSecrets() error = nil")
	}
	if p.serviceToken != (secret.Value{}) || p.serviceTokenSet {
		t.Fatalf(
			"secret state after failed materialization = value:%#v set:%v",
			p.serviceToken,
			p.serviceTokenSet,
		)
	}
	if got := p.config.Authorization.ServiceToken; got != raw {
		t.Fatalf("public service token after failure = %q, want original reference", got)
	}
	broker.mu.Lock()
	broker.failRaw = ""
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("retry MaterializeScopedPluginSecrets() error = %v", err)
	}
	if !p.serviceTokenSet || p.serviceToken == (secret.Value{}) {
		t.Fatalf(
			"secret state after retry = value:%#v set:%v",
			p.serviceToken,
			p.serviceTokenSet,
		)
	}
	if got := p.config.Authorization.ServiceToken; got == raw || !strings.HasPrefix(got, "plugin_config#sha256:") {
		t.Fatalf("public service token after retry = %q, want descriptor", got)
	}
}

func TestOpenFunctionDoesNotRetainPlaintextOrBasicCredentialFields(t *testing.T) {
	const token = "retention-user:retention-password"
	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(token))
	tests := []struct {
		name    string
		prepare func(*testing.T) *Plugin
	}{
		{
			name: "scoped",
			prepare: func(t *testing.T) *Plugin {
				const raw = "$ENV://OPENFUNCTION_RETENTION_TOKEN"
				broker := &openFunctionScopedBroker{values: map[string]string{raw: token}}
				capabilityValue, registration, scope := registerOpenFunctionScopedRoute(t, broker, raw)
				t.Cleanup(func() {
					if err := registration.Close(context.Background()); err != nil {
						t.Error(err)
					}
				})
				p := &Plugin{config: Config{
					FunctionURI:   "http://function.invalid",
					Authorization: &Authorization{ServiceToken: raw},
				}}
				if err := p.Init(); err != nil {
					t.Fatal(err)
				}
				if err := base.MaterializeScopedPluginSecrets(
					context.Background(), scope, capabilityValue, p,
				); err != nil {
					t.Fatal(err)
				}
				if err := p.PostInit(); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "legacy",
			prepare: func(t *testing.T) *Plugin {
				return newTestPlugin(t, Config{
					FunctionURI:   "http://function.invalid",
					Authorization: &Authorization{ServiceToken: token},
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := test.prepare(t)
			t.Cleanup(p.Stop)
			request := httptest.NewRequest(http.MethodGet, "http://example.com/openfunction", nil)
			p.processRequest(request, function_upstream.Config{})
			if got := request.Header.Get("Authorization"); got != wantHeader {
				t.Fatalf("Authorization = %q, want %q", got, wantHeader)
			}
			if valueGraphContainsString(reflect.ValueOf(p), wantHeader, make(map[uintptr]struct{}), 0) {
				t.Fatalf("plugin or shared upstream client retained encoded credential %q", wantHeader)
			}
		})
	}
}

func valueGraphContainsString(
	value reflect.Value,
	want string,
	visited map[uintptr]struct{},
	depth int,
) bool {
	if !value.IsValid() || depth > 20 {
		return false
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		return value.String() == want
	case reflect.Pointer:
		if value.IsNil() {
			return false
		}
		pointer := value.Pointer()
		if _, ok := visited[pointer]; ok {
			return false
		}
		visited[pointer] = struct{}{}
		return valueGraphContainsString(value.Elem(), want, visited, depth+1)
	case reflect.Struct:
		for _, fieldValue := range value.Fields() {
			if valueGraphContainsString(fieldValue, want, visited, depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 && value.Len() == len(want) {
			matched := true
			for index := range value.Len() {
				if byte(value.Index(index).Uint()) != want[index] {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
		for index := range value.Len() {
			if valueGraphContainsString(value.Index(index), want, visited, depth+1) {
				return true
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return false
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if valueGraphContainsString(iterator.Key(), want, visited, depth+1) ||
				valueGraphContainsString(iterator.Value(), want, visited, depth+1) {
				return true
			}
		}
	}
	return false
}

type openFunctionScopedBroker struct {
	mu      sync.Mutex
	values  map[string]string
	failRaw string
	calls   []secret.Scope
}

func (broker *openFunctionScopedBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (broker *openFunctionScopedBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *openFunctionScopedBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, scope)
	if raw == broker.failRaw {
		return "", fmt.Errorf("resolver failed for %s plaintext-service-token", raw)
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return "", fmt.Errorf("missing test credential")
}

func (broker *openFunctionScopedBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *openFunctionScopedBroker) scopedCalls() []secret.Scope {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]secret.Scope(nil), broker.calls...)
}

func registerOpenFunctionScopedRoute(
	t *testing.T,
	broker *openFunctionScopedBroker,
	raw string,
) (secret.GenerationCapability, secret.AttemptRegistration, secret.Scope) {
	t.Helper()
	return registerOpenFunctionScopedRouteAt(t, broker, 1, raw)
}

func registerOpenFunctionScopedRouteAt(
	t *testing.T,
	broker *openFunctionScopedBroker,
	revision uint64,
	raw string,
) (secret.GenerationCapability, secret.AttemptRegistration, secret.Scope) {
	t.Helper()
	return registerOpenFunctionScopedRouteConfigAt(t, broker, revision, raw, raw != "")
}

func registerOpenFunctionScopedRouteConfigAt(
	t *testing.T,
	broker *openFunctionScopedBroker,
	revision uint64,
	raw string,
	includeAuthorization bool,
) (secret.GenerationCapability, secret.AttemptRegistration, secret.Scope) {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	pluginConfig := map[string]any{"function_uri": "http://function.invalid"}
	if includeAuthorization {
		pluginConfig["authorization"] = map[string]any{"service_token": raw}
	}
	documentBytes, err := json.Marshal(map[string]any{
		"id":      "openfunction-route",
		"plugins": map[string]any{name: pluginConfig},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key:   generation.ResourceKey{Kind: "routes", ID: "openfunction-route"},
		Value: documentBytes,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := generation.ResourceKey{Kind: "routes", ID: "openfunction-route"}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain:   generation.DomainHTTP,
			Revision: snapshot.Revision(),
			Digest:   snapshot.Digest(),
			Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key:         key,
			Disposition: generation.DispositionPublished,
			Code:        "test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: snapshot.Revision(),
		DesiredDigest:   snapshot.Digest(),
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	registration, err := secret.NewScopedMaterializer(broker, catalog).RegisterCandidate(
		context.Background(), ticket, generation.PublicationSet{
			DesiredRevision: snapshot.Revision(),
			Domains: map[generation.Domain]generation.PublicationCandidate{
				generation.DomainHTTP: candidate,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, snapshot.Revision())
	if err != nil {
		t.Fatal(err)
	}
	return capabilityValue, registration, secret.Scope{
		Generation: snapshot.Revision(),
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
}
