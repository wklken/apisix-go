package acl

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ohler55/ojg/jp"

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
	p := newTestPlugin(t, Config{AllowLabels: map[string][]string{"team": {"edge"}}})
	res := performRequest(p, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if got := res.Body.String(); got != "{\"message\":\"Missing authentication.\"}\n" {
		t.Fatalf("body = %q, want APISIX missing-authentication response", got)
	}
}

func TestHandlerTreatsFalseExternalUserAsMissingAuthentication(t *testing.T) {
	p := newTestPlugin(t, Config{DenyLabels: map[string][]string{"team": {"blocked"}}})
	res := performExternalUserRequest(p, false)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
}

func TestHandlerAppliesConsumerAllowAndDenyLabels(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		labels map[string]any
		want   int
	}{
		{
			name:   "allowed",
			config: Config{AllowLabels: map[string][]string{"team": {"edge"}}},
			labels: map[string]any{"team": "edge"},
			want:   http.StatusNoContent,
		},
		{
			name:   "not allowed",
			config: Config{AllowLabels: map[string][]string{"team": {"edge"}}},
			labels: map[string]any{"team": "payments"},
			want:   http.StatusForbidden,
		},
		{
			name: "denied before allow",
			config: Config{
				AllowLabels: map[string][]string{"team": {"edge"}},
				DenyLabels:  map[string][]string{"tier": {"blocked"}},
			},
			labels: map[string]any{"team": "edge", "tier": "blocked"},
			want:   http.StatusForbidden,
		},
		{
			name:   "comma separated",
			config: Config{AllowLabels: map[string][]string{"groups": {"edge"}}},
			labels: map[string]any{"groups": "payments, edge, internal"},
			want:   http.StatusNoContent,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			res := performRequest(newTestPlugin(t, test.config), test.labels)
			if res.Code != test.want {
				t.Fatalf("response code = %d, want %d; body=%s", res.Code, test.want, res.Body.String())
			}
		})
	}
}

func TestHandlerUsesAPISIXRejectionResponse(t *testing.T) {
	p := newTestPlugin(t, Config{
		DenyLabels:   map[string][]string{"tier": {"blocked"}},
		RejectedCode: http.StatusTooManyRequests,
		RejectedMsg:  "blocked tier",
	})
	res := performRequest(p, map[string]any{"tier": "blocked"})
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusTooManyRequests)
	}
	if got := res.Body.String(); got != "{\"message\":\"blocked tier\"}\n" {
		t.Fatalf("body = %q, want custom APISIX rejection", got)
	}
	if got := res.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestHandlerDoesNotMatchStringRulesAgainstNonStringLabels(t *testing.T) {
	p := newTestPlugin(t, Config{DenyLabels: map[string][]string{
		"tenant": {"42"},
		"active": {"true"},
	}})
	res := performRequest(p, map[string]any{"tenant": int64(42), "active": true})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 because Lua equality is type strict", res.Code)
	}
}

func TestHandlerParsesExternalUserValues(t *testing.T) {
	tests := []struct {
		name      string
		parser    string
		separator string
		value     any
	}{
		{name: "json", parser: "json", value: `["payments","edge"]`},
		{name: "table", parser: "table", value: []any{"payments", "edge"}},
		{name: "segmented", parser: "segmented_text", separator: `\|`, value: "payments|edge"},
		{name: "default comma", value: "payments,edge"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:          "groups",
				ExternalUserLabelFieldParser:    test.parser,
				ExternalUserLabelFieldSeparator: test.separator,
				AllowLabels:                     map[string][]string{"groups": {"edge"}},
			})
			res := performExternalUserRequest(p, map[string]any{"groups": test.value})
			if res.Code != http.StatusNoContent {
				t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerUsesOnlyFirstJSONPathMatchLikeAPISIX317(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:          "$.teams[*].name",
		ExternalUserLabelFieldKey:       "name",
		ExternalUserLabelFieldParser:    "segmented_text",
		ExternalUserLabelFieldSeparator: ",",
		DenyLabels:                      map[string][]string{"name": {"blocked"}},
	})
	res := performExternalUserRequest(p, map[string]any{
		"teams": []any{
			map[string]any{"name": "allowed"},
			map[string]any{"name": "blocked"},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf(
			"response code = %d, want 204 because APISIX 3.17 uses jp.value()'s first match; body=%s",
			res.Code,
			res.Body.String(),
		)
	}
}

func TestHandlerUsesFirstRecursiveJSONPathMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$.orgs..team",
		ExternalUserLabelFieldKey:    "team",
		ExternalUserLabelFieldParser: "table",
		AllowLabels:                  map[string][]string{"team": {"edge"}},
	})
	res := performExternalUserRequest(p, map[string]any{
		"orgs": []any{
			map[string]any{"team": []any{"edge"}},
			map[string]any{"team": []any{"other"}},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestJSONPathLibraryCoversAPISIX317ACLPaths(t *testing.T) {
	paths := []string{
		"groups",
		"team",
		"$.orgs..team",
		"$..team",
		"$.profiles[*].groups",
		`$["profile.data"]['team-name']`,
		"$.profiles[0,2].groups",
		"$.profiles[:4:2].groups",
		`$.profiles[?(@.score>=10 && @.score<20)].groups`,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			if _, err := jp.ParseString(path); err != nil {
				t.Fatalf("jp.ParseString(%q) error = %v", path, err)
			}
		})
	}
}

func TestHandlerJSONParserRequiresLiteralArrayPrefix(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "groups",
		ExternalUserLabelFieldParser: "json",
		AllowLabels:                  map[string][]string{"groups": {"edge"}},
	})
	res := performExternalUserRequest(p, map[string]any{"groups": ` ["edge"]`})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want 403 because APISIX checks the raw prefix", res.Code)
	}
}

func TestHandlerTableParserRejectsStringValue(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "team",
		ExternalUserLabelFieldParser: "table",
		AllowLabels:                  map[string][]string{"team": {"cloud"}},
	})
	res := performExternalUserRequest(p, map[string]any{"team": "cloud"})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want 403", res.Code)
	}
}

func TestHandlerRejectsExternalUserWithoutAllowedLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField: "groups",
		AllowLabels:            map[string][]string{"groups": {"edge"}},
	})
	res := performExternalUserRequest(p, map[string]any{"name": "alice"})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want 403", res.Code)
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
		t.Fatal("PostInit() error = nil, want invalid JSONPath rejection")
	}
}

func TestSchemaMatchesAPISIX317Limits(t *testing.T) {
	p := newTestPlugin(t, Config{AllowLabels: map[string][]string{"groups": {"edge"}}})
	tests := []struct {
		name   string
		config map[string]any
		valid  bool
	}{
		{
			name: "segmented parser requires separator",
			config: map[string]any{
				"allow_labels":                     map[string]any{"groups": []any{"edge"}},
				"external_user_label_field_parser": "segmented_text",
			},
			valid: false,
		},
		{
			name: "status above 599",
			config: map[string]any{
				"allow_labels":  map[string]any{"groups": []any{"edge"}},
				"rejected_code": 600,
			},
			valid: true,
		},
		{
			name: "invalid parser",
			config: map[string]any{
				"allow_labels":                     map[string]any{"groups": []any{"edge"}},
				"external_user_label_field_parser": "csv",
			},
			valid: false,
		},
		{
			name: "empty field key",
			config: map[string]any{
				"allow_labels":                  map[string]any{"groups": []any{"edge"}},
				"external_user_label_field_key": "",
			},
			valid: false,
		},
		{
			name: "non-string separator",
			config: map[string]any{
				"allow_labels":                        map[string]any{"groups": []any{"edge"}},
				"external_user_label_field_separator": map[string]any{},
			},
			valid: false,
		},
		{
			name: "path above former local cap",
			config: map[string]any{
				"allow_labels":              map[string]any{"groups": []any{"edge"}},
				"external_user_label_field": "$" + strings.Repeat(".a", 2050),
			},
			valid: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := util.Validate(test.config, p.GetSchema())
			if test.valid && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate() error = nil, want rejection")
			}
		})
	}
}

func performRequest(p *Plugin, labels map[string]any) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	if labels != nil {
		ctx.AttachConsumer(req, resource.Consumer{Username: "alice", Labels: labels})
	}
	return servePlugin(p, req)
}

func performExternalUserRequest(p *Plugin, externalUser any) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	ctx.RegisterApisixVar(req, "$external_user", externalUser)
	return servePlugin(p, req)
}

func servePlugin(p *Plugin, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	return rr
}
