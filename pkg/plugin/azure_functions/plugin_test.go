package azure_functions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
	pluginruntime "github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	broker := &azureScopedBroker{values: map[string]string{}}
	if cfg.Authorization != nil && cfg.Authorization.APIKey != "" {
		broker.values[cfg.Authorization.APIKey] = cfg.Authorization.APIKey
	}
	capabilityValue, registration, scope := registerAzureScopedRouteConfigAt(t, broker, 1, cfg)
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

func TestMetadataSchemaAcceptsAzureAuthorization(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, tt := range []struct {
		name     string
		metadata map[string]any
		wantErr  bool
	}{
		{
			name: "authorization",
			metadata: map[string]any{
				"master_apikey":   "key-a",
				"master_clientid": "client-a",
			},
		},
		{name: "empty"},
		{
			name: "additive",
			metadata: map[string]any{
				"master_apikey": "key-a",
				"extra":         "accepted",
			},
		},
		{
			name: "apikey wrong type",
			metadata: map[string]any{
				"master_apikey": 1,
			},
			wantErr: true,
		},
		{
			name: "clientid wrong type",
			metadata: map[string]any{
				"master_clientid": true,
			},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := util.Validate(tt.metadata, p.GetMetadataSchema())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPostInitDecodesMetadataWithRouteAuthorizationPresent(t *testing.T) {
	view, err := pluginruntime.NewMetadataView(map[string][]byte{
		name: []byte(`{"master_apikey":1}`),
	})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	p := &Plugin{config: Config{
		FunctionURI: "http://function.invalid",
		Authorization: &Authorization{
			APIKey:   "route-key",
			ClientID: "route-client",
		},
	}}
	p.SetDependencies(base.Dependencies{Metadata: view})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err = p.PostInit()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata with route authorization present")
	}
	if !strings.Contains(err.Error(), "azure-functions metadata decode failed") {
		t.Fatalf("PostInit() error = %v, want metadata decode failure", err)
	}
}

func TestProductionDoesNotUseGlobalMetadataFallback(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	productionPath := filepath.Join(filepath.Dir(testFile), "plugin.go")
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, productionPath, nil, 0)
	if err != nil {
		t.Fatalf("ParseFile(%q) error = %v", productionPath, err)
	}

	forbidden := map[string]struct{}{
		"LoadPluginMetadata":         {},
		"GetPluginMetadata":          {},
		"GetValidatedPluginMetadata": {},
		"GetPluginMetadataRaw":       {},
	}
	var calls []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector := selectorFromCall(call.Fun)
		if _, ok := forbidden[selector]; ok {
			calls = append(calls, fmt.Sprintf("%s at %s", selector, fileSet.Position(call.Pos())))
		}
		return true
	})
	if len(calls) != 0 {
		t.Fatalf("production metadata fallback calls = %v", calls)
	}
}

func selectorFromCall(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.SelectorExpr:
		return expression.Sel.Name
	case *ast.IndexExpr:
		return selectorFromCall(expression.X)
	case *ast.IndexListExpr:
		return selectorFromCall(expression.X)
	default:
		return ""
	}
}

func TestHandlerInvokesAzureFunctionAndRelaysResponse(t *testing.T) {
	var gotMethod, gotQuery, gotBody, gotKey, gotClientID, gotHost string
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		gotHost = r.Host
		gotKey = r.Header.Get("X-Functions-Key")
		gotClientID = r.Header.Get("X-Functions-Clientid")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read function request body: %v", err)
		}
		gotBody = string(body)

		w.Header().Set("X-Function-Result", "azure")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("hello from azure"))
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: function.URL + "/api/HttpTrigger",
		Authorization: &Authorization{
			APIKey:   "function-key",
			ClientID: "client-id",
		},
	})

	res := performRequest(p, http.MethodPost, "/azure?name=APISIX", "payload", nil)

	if res.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusAccepted)
	}
	if got := res.Body.String(); got != "hello from azure" {
		t.Fatalf("response body = %q, want function body", got)
	}
	if got := res.Header().Get("X-Function-Result"); got != "azure" {
		t.Fatalf("X-Function-Result = %q, want azure", got)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("function method = %q, want POST", gotMethod)
	}
	if gotQuery != "name=APISIX" {
		t.Fatalf("function query = %q, want name=APISIX", gotQuery)
	}
	if gotBody != "payload" {
		t.Fatalf("function body = %q, want payload", gotBody)
	}
	if gotKey != "function-key" {
		t.Fatalf("X-Functions-Key = %q, want function-key", gotKey)
	}
	if gotClientID != "client-id" {
		t.Fatalf("X-Functions-Clientid = %q, want client-id", gotClientID)
	}
	if !strings.Contains(gotHost, "127.0.0.1") {
		t.Fatalf("function Host = %q, want function host", gotHost)
	}
}

func TestPreparedGenerationsRetainMetadataAuthorization(t *testing.T) {
	pN := newTestPluginWithMetadata(t, []byte(
		`{"master_apikey":"n-key","master_clientid":"n-client"}`,
	))
	pNPlusOne := newTestPluginWithMetadata(t, []byte(
		`{"master_apikey":"n1-key","master_clientid":"n1-client"}`,
	))

	requestN := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	pN.processRequest(requestN, function_upstream.Config{})
	if got := requestN.Header.Get("X-Functions-Key"); got != "n-key" {
		t.Fatalf("generation N key = %q, want n-key", got)
	}
	if got := requestN.Header.Get("X-Functions-Clientid"); got != "n-client" {
		t.Fatalf("generation N client id = %q, want n-client", got)
	}

	requestNPlusOne := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	pNPlusOne.processRequest(requestNPlusOne, function_upstream.Config{})
	if got := requestNPlusOne.Header.Get("X-Functions-Key"); got != "n1-key" {
		t.Fatalf("generation N+1 key = %q, want n1-key", got)
	}
	if got := requestNPlusOne.Header.Get("X-Functions-Clientid"); got != "n1-client" {
		t.Fatalf("generation N+1 client id = %q, want n1-client", got)
	}
}

func TestScopedRouteAuthorizationOverridesMetadataAuthorization(t *testing.T) {
	const raw = "$ENV://AZURE_ROUTE_PRIORITY_KEY"
	routeConfig := Config{
		FunctionURI: "http://function.invalid",
		Authorization: &Authorization{
			APIKey:   raw,
			ClientID: "route-client",
		},
	}
	broker := &azureScopedBroker{values: map[string]string{raw: "scoped-route-key"}}
	capabilityValue, registration, scope := registerAzureScopedRouteConfigAt(
		t, broker, 1, routeConfig,
	)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	metadata, err := pluginruntime.NewMetadataView(map[string][]byte{
		name: []byte(`{"master_apikey":"metadata-key","master_clientid":"metadata-client"}`),
	})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	p := &Plugin{config: routeConfig}
	p.SetDependencies(base.Dependencies{Metadata: metadata})
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	request := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	p.processRequest(request, function_upstream.Config{})
	if got := request.Header.Get("X-Functions-Key"); got != "scoped-route-key" {
		t.Fatalf("route API key = %q, want scoped-route-key", got)
	}
	if got := request.Header.Get("X-Functions-Clientid"); got != "route-client" {
		t.Fatalf("route client id = %q, want route-client", got)
	}
}

func newTestPluginWithMetadata(t *testing.T, document []byte) *Plugin {
	t.Helper()

	view, err := pluginruntime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	p := &Plugin{config: Config{FunctionURI: "http://function.invalid"}}
	p.SetDependencies(base.Dependencies{Metadata: view})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func TestRunRequestPhasePublishesUpstreamSource(t *testing.T) {
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer function.Close()
	p := newTestPlugin(t, Config{FunctionURI: function.URL})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
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

func TestHandlerDoesNotOverwriteClientAzureAuthorization(t *testing.T) {
	var gotKey, gotClientID string
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Functions-Key")
		gotClientID = r.Header.Get("X-Functions-Clientid")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: function.URL,
		Authorization: &Authorization{
			APIKey:   "configured-key",
			ClientID: "configured-client",
		},
	})

	res := performRequest(p, http.MethodGet, "/azure", "", map[string]string{
		"X-Functions-Key":      "client-key",
		"X-Functions-Clientid": "client-client",
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if gotKey != "client-key" {
		t.Fatalf("X-Functions-Key = %q, want client-key", gotKey)
	}
	if gotClientID != "client-client" {
		t.Fatalf("X-Functions-Clientid = %q, want client-client", gotClientID)
	}
}

func TestProcessRequestPreservesPresentAzureAuthorizationHeaders(t *testing.T) {
	routeConfig := Config{
		FunctionURI: "http://function.invalid",
		Authorization: &Authorization{
			APIKey:   "route-key",
			ClientID: "route-client",
		},
	}
	broker := &azureScopedBroker{values: map[string]string{"route-key": "resolved-route-key"}}
	capabilityValue, registration, scope := registerAzureScopedRouteConfigAt(t, broker, 1, routeConfig)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := &Plugin{config: routeConfig}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	p.metadata = Metadata{MasterAPIKey: "metadata-key", MasterClientID: "metadata-client"}

	tests := []struct {
		name                string
		field               string
		value               string
		expectedKey         string
		expectedClientID    string
		expectedKeySet      bool
		expectedClientIDSet bool
	}{
		{
			name:           "key empty",
			field:          "X-Functions-Key",
			value:          "",
			expectedKey:    "",
			expectedKeySet: true,
		},
		{
			name:           "key nonempty",
			field:          "X-Functions-Key",
			value:          "client-key",
			expectedKey:    "client-key",
			expectedKeySet: true,
		},
		{
			name:                "clientid empty",
			field:               "X-Functions-Clientid",
			value:               "",
			expectedClientID:    "",
			expectedClientIDSet: true,
		},
		{
			name:                "clientid nonempty",
			field:               "X-Functions-Clientid",
			value:               "client-client",
			expectedClientID:    "client-client",
			expectedClientIDSet: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
			request.Header[http.CanonicalHeaderKey(tt.field)] = []string{tt.value}
			p.processRequest(request, function_upstream.Config{})
			if got := request.Header.Get("X-Functions-Key"); got != tt.expectedKey {
				t.Fatalf("X-Functions-Key = %q, want %q", got, tt.expectedKey)
			}
			if got := request.Header.Get("X-Functions-Clientid"); got != tt.expectedClientID {
				t.Fatalf("X-Functions-Clientid = %q, want %q", got, tt.expectedClientID)
			}
			if _, got := request.Header["X-Functions-Key"]; got != tt.expectedKeySet {
				t.Fatalf("X-Functions-Key presence = %v, want %v", got, tt.expectedKeySet)
			}
			if _, got := request.Header["X-Functions-Clientid"]; got != tt.expectedClientIDSet {
				t.Fatalf("X-Functions-Clientid presence = %v, want %v", got, tt.expectedClientIDSet)
			}
		})
	}
}

func TestAzureRouteFixtureOmitsAbsentAuthorization(t *testing.T) {
	broker := &azureScopedBroker{}
	_, registration, _ := registerAzureScopedRoute(t, broker, "")
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	publication := broker.candidatePublication()
	if len(publication.Domains) != 1 {
		t.Fatalf("candidate domains = %#v, want one HTTP domain", publication.Domains)
	}
	raw, ok := publication.Domains[generation.DomainHTTP].Snapshot.Lookup(
		generation.ResourceKey{Kind: "routes", ID: "azure-route"},
	)
	if !ok {
		t.Fatal("azure route is missing from candidate publication")
	}
	if bytes.Contains(raw, []byte(`"authorization"`)) {
		t.Fatalf("absent route publication retained authorization: %s", raw)
	}
}

func TestMaterializeScopedSecretsSkipsPresentEmptyAzureAPIKey(t *testing.T) {
	config := Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{APIKey: ""},
	}
	broker := &azureScopedBroker{}
	capabilityValue, registration, scope := registerAzureScopedRouteConfigAt(t, broker, 1, config)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := &Plugin{config: config}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	if got := broker.scopedCalls(); len(got) != 0 {
		t.Fatalf("present-empty route scoped calls = %#v, want zero", got)
	}
	publication := broker.candidatePublication()
	raw, ok := publication.Domains[generation.DomainHTTP].Snapshot.Lookup(
		generation.ResourceKey{Kind: "routes", ID: "azure-route"},
	)
	if !ok || !bytes.Contains(raw, []byte(`"authorization"`)) ||
		!bytes.Contains(raw, []byte(`"apikey":""`)) {
		t.Fatalf("present-empty route publication = %s, want explicit empty authorization.apikey", raw)
	}
}

func TestMaterializeScopedSecretsOwnsAzureRouteAPIKey(t *testing.T) {
	const raw = "$ENV://AZURE_ROUTE_KEY"
	broker := &azureScopedBroker{values: map[string]string{raw: "route-key"}}
	capabilityValue, registration, scope := registerAzureScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{APIKey: raw},
	}}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if got := broker.scopedCalls(); len(got) != 1 || got[0].Field != "authorization.apikey" ||
		got[0].Plugin != name || got[0].Resource.ID != "azure-route" {
		t.Fatalf("scoped calls = %#v, want one exact route declaration", got)
	}
	if p.config.Authorization.APIKey != raw {
		if strings.Contains(p.config.Authorization.APIKey, "route-key") ||
			!strings.HasPrefix(p.config.Authorization.APIKey, "plugin_config#sha256:") {
			t.Fatalf("public API key = %q, want redacted descriptor", p.config.Authorization.APIKey)
		}
	} else {
		t.Fatalf("public API key retained raw reference: %q", p.config.Authorization.APIKey)
	}

	request := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	p.processRequest(request, function_upstream.Config{})
	if got := request.Header.Get("X-Functions-Key"); got != "route-key" {
		t.Fatalf("route header = %q, want resolved route key", got)
	}

	absentBroker := &azureScopedBroker{}
	absentCapability, absentRegistration, absentScope := registerAzureScopedRoute(t, absentBroker, "")
	t.Cleanup(func() {
		if err := absentRegistration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	absentPlugin := &Plugin{config: Config{FunctionURI: "http://function.invalid"}}
	absentPlugin.metadata = Metadata{MasterAPIKey: "metadata-key", MasterClientID: "metadata-client"}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), absentScope, absentCapability, absentPlugin,
	); err != nil {
		t.Fatalf("absent route MaterializeScopedPluginSecrets() error = %v", err)
	}
	if got := absentBroker.scopedCalls(); len(got) != 0 {
		t.Fatalf("absent route scoped calls = %#v, want zero", got)
	}
	absentRequest := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	absentPlugin.processRequest(absentRequest, function_upstream.Config{})
	if got := absentRequest.Header.Get("X-Functions-Key"); got != "metadata-key" {
		t.Fatalf("metadata fallback key = %q, want metadata-key", got)
	}
	if got := absentRequest.Header.Get("X-Functions-Clientid"); got != "metadata-client" {
		t.Fatalf("metadata fallback client id = %q, want metadata-client", got)
	}

	clientRequest := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	clientRequest.Header.Set("X-Functions-Key", "client-key")
	p.processRequest(clientRequest, function_upstream.Config{})
	if got := clientRequest.Header.Get("X-Functions-Key"); got != "client-key" {
		t.Fatalf("client header = %q, want client-key", got)
	}
}

func TestAzureRouteMaterializationFailsBeforePostInit(t *testing.T) {
	const raw = "$ENV://AZURE_ROUTE_FAILURE"
	broker := &azureScopedBroker{failRaw: raw}
	capabilityValue, registration, scope := registerAzureScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{APIKey: raw},
	}}
	err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil")
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "AZURE_ROUTE_FAILURE") {
		t.Fatalf("materialization error leaked route secret: %v", err)
	}
	if p.Client != nil {
		t.Fatal("PostInit ran after route materialization failure")
	}
	if p.config.Authorization.APIKey != raw {
		t.Fatalf("failed materialization changed public API key = %q", p.config.Authorization.APIKey)
	}
}

func TestAzureRouteKeyRotationDoesNotCrossGenerations(t *testing.T) {
	const (
		rawN  = "$ENV://AZURE_ROUTE_N"
		rawN1 = "$ENV://AZURE_ROUTE_N1"
	)
	broker := &azureScopedBroker{values: map[string]string{
		rawN:  "route-key-n",
		rawN1: "route-key-n1",
	}}
	capabilityN, registrationN, scopeN := registerAzureScopedRouteAt(t, broker, 11, rawN)
	capabilityN1, registrationN1, scopeN1 := registerAzureScopedRouteAt(t, broker, 12, rawN1)
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
		Authorization: &Authorization{APIKey: rawN},
	}}
	pN1 := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{APIKey: rawN1},
	}}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scopeN, capabilityN, pN); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scopeN1, capabilityN1, pN1); err != nil {
		t.Fatal(err)
	}

	requestN := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	pN.processRequest(requestN, function_upstream.Config{})
	requestN1 := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	pN1.processRequest(requestN1, function_upstream.Config{})
	if got := requestN.Header.Get("X-Functions-Key"); got != "route-key-n" {
		t.Fatalf("generation N route key = %q, want route-key-n", got)
	}
	if got := requestN1.Header.Get("X-Functions-Key"); got != "route-key-n1" {
		t.Fatalf("generation N+1 route key = %q, want route-key-n1", got)
	}

	pN.Stop()
	pN.Stop()
	retained := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	pN1.processRequest(retained, function_upstream.Config{})
	if got := retained.Header.Get("X-Functions-Key"); got != "route-key-n1" {
		t.Fatalf("generation N+1 after N retirement = %q, want route-key-n1", got)
	}
}

func TestAzureStopIsIdempotentAndDropsRouteValue(t *testing.T) {
	broker := &azureScopedBroker{values: map[string]string{"route-key": "resolved-route-key"}}
	capabilityValue, registration, scope := registerAzureScopedRoute(t, broker, "route-key")
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{APIKey: "route-key"},
	}}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	p.Stop()
	if p.routeAPIKeySet || p.routeAPIKey != (secret.Value{}) {
		t.Fatalf(
			"route secret state after Stop = set:%v value:%#v",
			p.routeAPIKeySet,
			p.routeAPIKey,
		)
	}
}

func TestMaterializeScopedSecretsIsSingleFlight(t *testing.T) {
	const raw = "$ENV://AZURE_SINGLEFLIGHT_ROUTE_KEY"
	broker := &azureScopedBroker{
		values:         map[string]string{raw: "resolved-route-key"},
		resolveStarted: make(chan struct{}),
		resolveRelease: make(chan struct{}),
	}
	capabilityValue, registration, scope := registerAzureScopedRoute(t, broker, raw)
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{APIKey: raw},
	}}

	const workers = 16
	errs := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			errs <- base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
		}()
	}
	select {
	case <-broker.resolveStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first resolver call")
	}
	close(broker.resolveRelease)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
		}
	}
	if got := broker.scopedCalls(); len(got) != 1 {
		t.Fatalf("concurrent scoped calls = %d, want one", len(got))
	}
}

func TestAzureProcessRequestAndStopAreSafeConcurrently(t *testing.T) {
	broker := &azureScopedBroker{values: map[string]string{"route-key": "resolved-route-key"}}
	capabilityValue, registration, scope := registerAzureScopedRoute(t, broker, "route-key")
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := &Plugin{config: Config{
		FunctionURI:   "http://function.invalid",
		Authorization: &Authorization{APIKey: "route-key"},
	}}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				request := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
				p.processRequest(request, function_upstream.Config{})
				startOnce.Do(func() { close(started) })
			}
		})
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for concurrent request")
	}
	p.Stop()
	close(stop)
	group.Wait()

	request := httptest.NewRequest(http.MethodGet, "http://example.com/azure", nil)
	p.processRequest(request, function_upstream.Config{})
	if _, ok := request.Header[http.CanonicalHeaderKey("X-Functions-Key")]; ok {
		t.Fatalf("post-Stop request retained route key: %#v", request.Header)
	}
}

type azureScopedBroker struct {
	mu             sync.Mutex
	values         map[string]string
	failRaw        string
	calls          []secret.Scope
	candidateSets  []generation.PublicationSet
	resolveStarted chan struct{}
	resolveRelease chan struct{}
	resolveOnce    sync.Once
}

func (broker *azureScopedBroker) AuthorizeCandidate(
	_ context.Context,
	_ secret.AttemptID,
	_ generation.ApplyTicket,
	set generation.PublicationSet,
) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.candidateSets = append(broker.candidateSets, set)
	return nil
}

func (broker *azureScopedBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *azureScopedBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, scope)
	if raw == broker.failRaw {
		return "", fmt.Errorf("resolver failed for %s", raw)
	}
	if broker.resolveStarted != nil {
		broker.resolveOnce.Do(func() { close(broker.resolveStarted) })
		<-broker.resolveRelease
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return "", fmt.Errorf("missing test credential")
}

func (broker *azureScopedBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *azureScopedBroker) scopedCalls() []secret.Scope {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]secret.Scope(nil), broker.calls...)
}

func (broker *azureScopedBroker) candidatePublication() generation.PublicationSet {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.candidateSets) == 0 {
		return generation.PublicationSet{}
	}
	return broker.candidateSets[len(broker.candidateSets)-1]
}

func registerAzureScopedRoute(
	t *testing.T,
	broker *azureScopedBroker,
	raw string,
) (secret.GenerationCapability, secret.AttemptRegistration, secret.Scope) {
	return registerAzureScopedRouteAt(t, broker, 1, raw)
}

func registerAzureScopedRouteAt(
	t *testing.T,
	broker *azureScopedBroker,
	revision uint64,
	raw string,
) (secret.GenerationCapability, secret.AttemptRegistration, secret.Scope) {
	authorization := (*Authorization)(nil)
	if raw != "" {
		authorization = &Authorization{APIKey: raw}
	}
	return registerAzureScopedRouteConfigAt(t, broker, revision, Config{
		FunctionURI:   "http://function.invalid",
		Authorization: authorization,
	})
}

func registerAzureScopedRouteConfigAt(
	t *testing.T,
	broker *azureScopedBroker,
	revision uint64,
	config Config,
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
	pluginConfig := map[string]any{"function_uri": config.FunctionURI}
	if config.Authorization != nil {
		authorization := map[string]any{"apikey": config.Authorization.APIKey}
		if config.Authorization.ClientID != "" {
			authorization["clientid"] = config.Authorization.ClientID
		}
		pluginConfig["authorization"] = authorization
	}
	documentBytes, err := json.Marshal(map[string]any{
		"id":      "azure-route",
		"plugins": map[string]any{name: pluginConfig},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key:   generation.ResourceKey{Kind: "routes", ID: "azure-route"},
		Value: documentBytes,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := generation.ResourceKey{Kind: "routes", ID: "azure-route"}
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
	materializer := testutil.NewSecretMaterializer(broker, catalog)
	registration, err := materializer.RegisterCandidate(
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

func TestHandlerFallsBackToAzureMetadataAuthorization(t *testing.T) {
	var gotKey, gotClientID string
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Functions-Key")
		gotClientID = r.Header.Get("X-Functions-Clientid")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL})
	p.metadata = Metadata{
		MasterAPIKey:   "master-key",
		MasterClientID: "master-client",
	}
	res := performRequest(p, http.MethodGet, "/azure", "", nil)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if gotKey != "master-key" || gotClientID != "master-client" {
		t.Fatalf("metadata authorization = key:%q client:%q, want master values", gotKey, gotClientID)
	}
}

func performRequest(
	p *Plugin,
	method string,
	path string,
	body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
	for field, value := range headers {
		req.Header.Set(field, value)
	}

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := http.StatusInternalServerError
		http.Error(w, http.StatusText(t), t)
	})).ServeHTTP(rr, req)
	return rr
}
