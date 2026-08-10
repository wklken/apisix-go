package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRequestStageRegistryExactMembership(t *testing.T) {
	want := map[string]RequestStageSpec{
		"request-context":     {Stage: RequestStageRewrite, AdaptLegacyHandler: false},
		"request-id":          {Stage: RequestStageRewrite, AdaptLegacyHandler: false},
		"real-ip":             {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"proxy-rewrite":       {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"proxy-control":       {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"proxy-mirror":        {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"traffic-label":       {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"traffic-split":       {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"ai-prompt-decorator": {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"ai-prompt-template":  {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"ai-rag":              {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"ai-request-rewrite":  {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"data-mask":           {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"degraphql":           {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"example-plugin":      {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"jwe-decrypt":         {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
		"limit-conn":          {Stage: RequestStageAccess, AdaptLegacyHandler: false},

		"basic-auth": {
			Stage:                 RequestStageAccess,
			AuthenticatesConsumer: true,
			ConsumerConfigOnly:    true,
		},
		"hmac-auth": {
			Stage:                 RequestStageAccess,
			AuthenticatesConsumer: true,
			ConsumerConfigOnly:    true,
		},
		"jwt-auth": {
			Stage:                 RequestStageAccess,
			AuthenticatesConsumer: true,
			ConsumerConfigOnly:    true,
		},
		"key-auth": {
			Stage:                 RequestStageAccess,
			AuthenticatesConsumer: true,
			ConsumerConfigOnly:    true,
		},
		"ldap-auth": {
			Stage:                 RequestStageAccess,
			AuthenticatesConsumer: true,
			ConsumerConfigOnly:    true,
		},
		"multi-auth": {
			Stage:                 RequestStageAccess,
			AuthenticatesConsumer: true,
			ConsumerConfigOnly:    true,
		},
		"wolf-rbac": {
			Stage:                 RequestStageAccess,
			AuthenticatesConsumer: true,
			ConsumerConfigOnly:    true,
		},

		"attach-consumer-label": {Stage: RequestStageConsumerRewrite, AdaptLegacyHandler: true},

		"acl":                       {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"ai-aws-content-moderation": {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"ai-prompt-guard":           {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"authz-casbin":              {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"authz-casdoor":             {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"authz-keycloak":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"cas-auth":                  {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"chaitin-waf":               {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"client-control":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"consumer-restriction":      {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"csrf":                      {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"dingtalk-auth":             {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"feishu-auth":               {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"forward-auth":              {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"graphql-limit-count":       {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"ip-restriction":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"limit-count":               {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"limit-req":                 {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"oas-validator":             {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"opa":                       {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"openid-connect":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"referer-restriction":       {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"request-validation":        {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"saml-auth":                 {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"ua-restriction":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"uri-blocker":               {Stage: RequestStageAccess, AdaptLegacyHandler: true},
		"workflow":                  {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	}
	for name, wantSpec := range want {
		got, ok := RequestStageFor(name)
		if !ok {
			t.Fatalf("RequestStageFor(%q) not registered", name)
		}
		if got != wantSpec {
			t.Fatalf("RequestStageFor(%q) = %#v, want %#v", name, got, wantSpec)
		}
	}
	for name := range requestStageRegistry {
		if _, ok := want[name]; !ok {
			t.Fatalf("unexpected request-stage registry key %q", name)
		}
	}
	if len(requestStageRegistry) != len(want) {
		t.Fatalf("request-stage registry size = %d, want %d", len(requestStageRegistry), len(want))
	}
}

func TestPlan14RegistrySubsetsAreExact(t *testing.T) {
	authentication := []string{
		"basic-auth", "hmac-auth", "jwt-auth", "key-auth", "ldap-auth", "multi-auth", "wolf-rbac",
	}
	legacyAccess := []string{
		"acl", "ai-aws-content-moderation", "ai-prompt-guard", "authz-casbin",
		"authz-casdoor", "authz-keycloak", "cas-auth", "chaitin-waf", "client-control",
		"consumer-restriction", "csrf", "dingtalk-auth", "feishu-auth", "forward-auth",
		"graphql-limit-count", "ip-restriction", "limit-count", "limit-req", "oas-validator",
		"opa", "openid-connect", "referer-restriction", "request-validation", "saml-auth",
		"ua-restriction", "uri-blocker", "workflow",
	}
	consumerRewrite := []string{"attach-consumer-label"}
	credentialOnly := []string{
		"basic-auth", "hmac-auth", "jwe-decrypt", "jwt-auth", "key-auth", "ldap-auth", "multi-auth", "wolf-rbac",
	}

	assertSet := func(name string, want []string, predicate func(RequestStageSpec) bool) {
		t.Helper()
		got := make([]string, 0)
		for key, spec := range requestStageRegistry {
			if predicate(spec) {
				got = append(got, key)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("%s size = %d, want %d (%v)", name, len(got), len(want), want)
		}
		for _, key := range want {
			if spec, ok := requestStageRegistry[key]; !ok || !predicate(spec) {
				t.Fatalf("%s missing exact key %q", name, key)
			}
		}
	}

	assertSet("authentication", authentication, func(spec RequestStageSpec) bool {
		return spec.Stage == RequestStageAccess && spec.AuthenticatesConsumer &&
			spec.ConsumerConfigOnly && !spec.AdaptLegacyHandler
	})
	assertSet("legacy access", legacyAccess, func(spec RequestStageSpec) bool {
		return spec.Stage == RequestStageAccess && !spec.AuthenticatesConsumer &&
			!spec.ConsumerConfigOnly && spec.AdaptLegacyHandler
	})
	assertSet("consumer rewrite", consumerRewrite, func(spec RequestStageSpec) bool {
		return spec.Stage == RequestStageConsumerRewrite && !spec.AuthenticatesConsumer &&
			!spec.ConsumerConfigOnly && spec.AdaptLegacyHandler
	})

	for _, key := range credentialOnly {
		if key == "jwe-decrypt" {
			spec, ok := requestStageRegistry[key]
			if !ok || spec.Stage != RequestStageRewrite || !spec.AdaptLegacyHandler {
				t.Fatalf("credential-only rewrite %q = %#v/%v", key, spec, ok)
			}
			continue
		}
		spec, ok := requestStageRegistry[key]
		if !ok || !spec.AuthenticatesConsumer || !spec.ConsumerConfigOnly {
			t.Fatalf("credential-only auth %q = %#v/%v", key, spec, ok)
		}
	}
}

func TestRequestStageEnumOrderIsFrozen(t *testing.T) {
	got := []RequestStage{
		RequestStageLegacy,
		RequestStageRewrite,
		RequestStageConsumerRewrite,
		RequestStageAccess,
		RequestStageBeforeProxy,
	}
	for i, stage := range got {
		if uint8(stage) != uint8(i) {
			t.Fatalf("RequestStage value at index %d = %d, want %d", i, stage, i)
		}
	}
}

func TestRequestStageRegistryRejectsUnknownAndImplementationName(t *testing.T) {
	for _, name := range []string{"", "unknown-plugin", "request_context", "proxy_rewrite"} {
		if _, ok := RequestStageFor(name); ok {
			t.Fatalf("RequestStageFor(%q) = ok, want false", name)
		}
	}
}

func TestRewriteOnlyAdapterPropagatesReplacementRequest(t *testing.T) {
	plugin := newExecutorLegacyPlugin("request_context", 1, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			replacement := r.WithContext(
				context.WithValue(r.Context(), registryTraceKey{}, "replacement"),
			)
			next.ServeHTTP(w, replacement)
		})
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	var terminalRequest *http.Request
	NewScopedExecutor(BindPlugin(
		"real-ip",
		plugin,
		ScopeGlobal,
		ResourceProvenance{Kind: ResourceRoute, ID: "r-replace"},
	)).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		terminalRequest = r
	})).ServeHTTP(httptest.NewRecorder(), request)
	if terminalRequest == nil ||
		terminalRequest.Context().Value(registryTraceKey{}) != "replacement" {
		t.Fatalf("terminal request did not receive replacement: %#v", terminalRequest)
	}
}

type registryTraceKey struct{}

func TestRewriteOnlyAdapterStopsWhenNextNotCalled(t *testing.T) {
	plugin := newExecutorLegacyPlugin("real_ip", 1, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	})
	request, lifecycle, _ := executorRequest(t)
	terminalCalled := false
	NewScopedExecutor(BindPlugin(
		"real-ip",
		plugin,
		ScopeGlobal,
		ResourceProvenance{Kind: ResourceService, ID: "svc-stop"},
	)).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { terminalCalled = true })).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)
	if terminalCalled {
		t.Fatal("terminal called when rewrite adapter did not call next")
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceEarlyStop)
	}
}

func TestRewriteOnlyAdapterRejectsDoubleNext(t *testing.T) {
	plugin := newExecutorLegacyPlugin("not-proxy-rewrite", 1, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			next.ServeHTTP(w, r)
		})
	})
	var diagnostics []string
	request := withRewriteAdapterDiagnosticRecorder(
		httptest.NewRequest(http.MethodGet, "/", nil),
		func(message string) { diagnostics = append(diagnostics, message) },
	)
	response := httptest.NewRecorder()
	terminalCalled := false
	NewScopedExecutor(BindPlugin(
		"proxy-rewrite",
		plugin,
		ScopeGlobal,
		ResourceProvenance{Kind: ResourcePluginConfig, ID: "pc-double"},
	)).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { terminalCalled = true })).ServeHTTP(
		response,
		request,
	)
	if terminalCalled {
		t.Fatal("terminal called after double-next rejection")
	}
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("double-next status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	for _, want := range []string{"factory=\"proxy-rewrite\"", "plugin=\"not-proxy-rewrite\"", "plugin_config", "pc-double"} {
		if len(diagnostics) != 1 || !strings.Contains(diagnostics[0], want) {
			t.Fatalf("internal double-next diagnostic %q missing %q", diagnostics, want)
		}
	}
	if body := response.Body.String(); body != "Internal Server Error\n" {
		t.Fatalf("double-next response body = %q, want generic body", body)
	}
}

func TestRewriteOnlyAdapterDoesNotExecuteDownstreamTwice(t *testing.T) {
	plugin := newExecutorLegacyPlugin("proxy_rewrite", 1, func(next http.Handler) http.Handler {
		return http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) },
		)
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	terminalCalls := 0
	NewScopedExecutor(BindPlugin(
		"proxy-rewrite",
		plugin,
		ScopeGlobal,
		ResourceProvenance{Kind: ResourceRoute, ID: "r-once"},
	)).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { terminalCalls++ })).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)
	if terminalCalls != 1 {
		t.Fatalf("terminal calls = %d, want 1", terminalCalls)
	}
}

func TestScopedExecutorRejectsUnregisteredRewriteBinding(t *testing.T) {
	plugin := newExecutorLegacyPlugin("legacy-name", 1, func(next http.Handler) http.Handler {
		return http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) },
		)
	})
	var diagnostics []string
	request := withRewriteAdapterDiagnosticRecorder(
		httptest.NewRequest(http.MethodGet, "/", nil),
		func(message string) { diagnostics = append(diagnostics, message) },
	)
	response := httptest.NewRecorder()
	terminalCalled := false
	NewScopedExecutor(Binding{
		Plugin:     plugin,
		Scope:      ScopeGlobal,
		Stage:      RequestStageRewrite,
		Provenance: ResourceProvenance{Kind: ResourceService, ID: "svc-unaudited"},
	}).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { terminalCalled = true })).ServeHTTP(
		response,
		request,
	)
	if terminalCalled {
		t.Fatal("terminal called for unregistered rewrite binding")
	}
	if response.Code != http.StatusInternalServerError ||
		response.Body.String() != "Internal Server Error\n" {
		t.Fatalf(
			"unregistered rewrite response = %d/%q, want generic 500",
			response.Code,
			response.Body.String(),
		)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0], "service/svc-unaudited") {
		t.Fatalf("unregistered rewrite diagnostics = %q, want provenance", diagnostics)
	}
}

func TestRequestStageRegistryConstructorNameDrift(t *testing.T) {
	p := New("request-context")
	if p == nil {
		t.Fatal("New(request-context) returned nil")
	}
	if err := p.Init(); err != nil {
		t.Fatalf("request-context Init() error = %v", err)
	}
	if got := p.GetName(); got != "request_context" {
		t.Fatalf("request-context implementation name = %q, want request_context", got)
	}
	if _, ok := RequestStageFor(p.GetName()); ok {
		t.Fatalf("implementation name %q unexpectedly registered as factory", p.GetName())
	}
	if spec, ok := RequestStageFor("request-context"); !ok || spec.Stage != RequestStageRewrite {
		t.Fatalf("factory request-context stage = %#v/%v, want rewrite/true", spec, ok)
	}
	if _, ok := p.(base.RequestPhasePlugin); !ok {
		t.Fatal("request-context does not implement RequestPhasePlugin")
	}
}
