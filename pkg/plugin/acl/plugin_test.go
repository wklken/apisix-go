package acl

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
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

func TestHandlerRejectsMissingAuthentication(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"team": {"edge"},
		},
	})

	res := performRequest(p, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(res.Body.String(), "Missing authentication.") {
		t.Fatalf("body = %q, want missing authentication message", res.Body.String())
	}
}

func TestHandlerAllowsConsumerWithAllowedLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"team": {"edge"},
		},
	})

	res := performRequest(p, map[string]any{"team": "edge"})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsConsumerWithoutAllowedLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"team": {"edge"},
		},
	})

	res := performRequest(p, map[string]any{"team": "payments"})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusForbidden)
	}
	if !strings.Contains(res.Body.String(), "The consumer is forbidden.") {
		t.Fatalf("body = %q, want forbidden message", res.Body.String())
	}
}

func TestHandlerRejectsConsumerWithDeniedLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		DenyLabels: map[string][]string{
			"tier": {"blocked"},
		},
		RejectedCode: http.StatusTooManyRequests,
		RejectedMsg:  "blocked tier",
	})

	res := performRequest(p, map[string]any{"tier": "blocked"})
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusTooManyRequests)
	}
	if !strings.Contains(res.Body.String(), "blocked tier") {
		t.Fatalf("body = %q, want custom rejection message", res.Body.String())
	}
}

func TestHandlerParsesCommaSeparatedConsumerLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	res := performRequest(p, map[string]any{"groups": "payments, edge, internal"})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerMatchesNumericAndBooleanConsumerLabels(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"tenant_id": {"42"},
			"active":    {"true"},
			"groups":    {"7"},
		},
	})

	res := performRequest(p, map[string]any{
		"tenant_id": int64(42),
		"active":    true,
		"groups":    []any{int64(7), false},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerAllowsExternalUserJSONLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "groups",
		ExternalUserLabelFieldParser: "json",
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	ctx.RegisterApisixVar(req, "$external_user", map[string]any{
		"groups": `["payments", "edge"]`,
	})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerAllowsExternalUserRecursiveJSONPathLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$..groups",
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	ctx.RegisterApisixVar(req, "$external_user", map[string]any{
		"profile": map[string]any{"groups": []any{"payments", "edge"}},
	})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerAllowsExternalUserRecursiveNestedJSONPathLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$..profile.groups",
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	ctx.RegisterApisixVar(req, "$external_user", map[string]any{
		"identity": map[string]any{
			"profile": map[string]any{"groups": []any{"payments", "edge"}},
		},
	})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerAllowsExternalUserPrefixedRecursiveJSONPathLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$.orgs..team",
		ExternalUserLabelFieldKey:    "team",
		ExternalUserLabelFieldParser: "table",
		AllowLabels: map[string][]string{
			"team": {"infra"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"orgs": map[string]any{
			"api7": map[string]any{"team": []any{"cloud", "infra"}},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerChecksEveryRecursiveJSONPathMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:          "$..name",
		ExternalUserLabelFieldKey:       "name",
		ExternalUserLabelFieldParser:    "segmented_text",
		ExternalUserLabelFieldSeparator: ",",
		DenyLabels: map[string][]string{
			"name": {"infra"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"teams": []any{
			map[string]any{"name": "cloud"},
			map[string]any{"name": "infra,qa"},
		},
	})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerTableParserRejectsStringValue(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$..team",
		ExternalUserLabelFieldKey:    "team",
		ExternalUserLabelFieldParser: "table",
		AllowLabels: map[string][]string{
			"team": {"cloud"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"orgs": map[string]any{"api7": map[string]any{"team": "cloud"}},
	})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
	}
}

func TestPostInitRejectsInvalidExternalUserJSONPath(t *testing.T) {
	p := &Plugin{config: Config{
		ExternalUserLabelField: "$..([invalid",
		AllowLabels:            map[string][]string{"team": {"cloud"}},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid JSONPath rejected")
	}
}

func TestHandlerAllowsExternalUserSegmentedLabelWithCustomKey(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:          "profile.roles",
		ExternalUserLabelFieldKey:       "roles",
		ExternalUserLabelFieldParser:    "segmented_text",
		ExternalUserLabelFieldSeparator: ",",
		AllowLabels: map[string][]string{
			"roles": {"edge"},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	ctx.RegisterApisixVar(req, "$external_user", map[string]any{
		"profile": map[string]any{"roles": "payments, edge"},
	})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerRejectsExternalUserWithoutAllowedLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField: "groups",
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	ctx.RegisterApisixVar(req, "$external_user", map[string]any{"name": "alice"})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
}

func TestSchemaRequiresExternalUserSeparatorForSegmentedParser(t *testing.T) {
	p := newTestPlugin(t, Config{AllowLabels: map[string][]string{"groups": {"edge"}}})
	config := map[string]any{
		"external_user_label_field_parser": "segmented_text",
		"allow_labels":                     map[string]any{"groups": []any{"edge"}},
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("acl schema should require external_user_label_field_separator for segmented_text")
	}
}

func performRequest(p *Plugin, labels map[string]any) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	if labels != nil {
		ctx.AttachConsumer(req, resource.Consumer{
			Username: "alice",
			Labels:   labels,
		})
	}

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	return rr
}

func performExternalUserRequest(p *Plugin, externalUser map[string]any) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	ctx.RegisterApisixVar(req, "$external_user", externalUser)

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	return rr
}
