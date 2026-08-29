package acl

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
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
	if got := res.Body.String(); got != "{\"message\":\"The consumer is forbidden.\"}\n" {
		t.Fatalf("body = %q, want APISIX 3.17 rejection body", got)
	}
	if got := res.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX 3.17 response type", got)
	}
	if got := res.Header().Get("X-Content-Type-Options"); got != "" {
		t.Fatalf("X-Content-Type-Options = %q, want absent", got)
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

func TestHandlerDoesNotMatchStringRulesAgainstNumericOrBooleanConsumerLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]any
		rules  map[string][]string
	}{
		{
			name: "scalar values",
			labels: map[string]any{
				"tenant_id": int64(42),
			},
			rules: map[string][]string{"tenant_id": {"42"}},
		},
		{
			name: "table values",
			labels: map[string]any{
				"groups": []any{int64(42), true},
			},
			rules: map[string][]string{"groups": {"42", "true"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allow := newTestPlugin(t, Config{
				AllowLabels: test.rules,
			})
			if res := performRequest(allow, test.labels); res.Code != http.StatusForbidden {
				t.Fatalf(
					"allow response code = %d, want %d because Lua label equality is type-strict; body=%s",
					res.Code,
					http.StatusForbidden,
					res.Body.String(),
				)
			}

			deny := newTestPlugin(t, Config{
				DenyLabels: test.rules,
			})
			if res := performRequest(deny, test.labels); res.Code != http.StatusNoContent {
				t.Fatalf(
					"deny response code = %d, want %d because Lua label equality is type-strict; body=%s",
					res.Code,
					http.StatusNoContent,
					res.Body.String(),
				)
			}
		})
	}
}

func TestHandlerDoesNotMatchStringRulesAgainstNumericOrBooleanExternalUserLabels(t *testing.T) {
	tests := []struct {
		name   string
		parser string
		value  any
	}{
		{name: "table", parser: "table", value: []any{float64(42), true}},
		{name: "json", parser: "json", value: `[42,true]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       "groups",
				ExternalUserLabelFieldParser: test.parser,
				DenyLabels: map[string][]string{
					"groups": {"42", "true"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{"groups": test.value})
			if res.Code != http.StatusNoContent {
				t.Fatalf(
					"response code = %d, want %d because Lua label equality is type-strict; body=%s",
					res.Code,
					http.StatusNoContent,
					res.Body.String(),
				)
			}
		})
	}
}

func TestHandlerDefaultParserPreservesJSONArrayElementTypes(t *testing.T) {
	tests := []struct {
		name  string
		value string
		rules []string
		want  int
	}{
		{
			name:  "numeric array is not its source string",
			value: `[42]`, rules: []string{`[42]`}, want: http.StatusForbidden,
		},
		{
			name:  "boolean array is not its source string",
			value: `[true]`, rules: []string{`[true]`}, want: http.StatusForbidden,
		},
		{
			name:  "string in mixed array remains matchable",
			value: `["edge",42,true]`, rules: []string{"edge"}, want: http.StatusNoContent,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				AllowLabels: map[string][]string{"groups": test.rules},
			})
			res := performRequest(p, map[string]any{"groups": test.value})
			if res.Code != test.want {
				t.Fatalf("response code = %d, want %d; body=%s", res.Code, test.want, res.Body.String())
			}
		})
	}
}

func TestHandlerDefaultParserDoesNotMatchEncodedNumericExternalUserLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField: "groups",
		DenyLabels: map[string][]string{
			"groups": {`[42]`},
		},
	})

	res := performExternalUserRequest(p, map[string]any{"groups": `[42]`})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerDefaultParserPreservesTypesAcrossAllJSONPathMatches(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:    "$.profiles[*].groups",
		ExternalUserLabelFieldKey: "groups",
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"profiles": []any{
			map[string]any{"groups": `[42]`},
			map[string]any{"groups": `["edge",true]`},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerSkipsDirectNilAcrossAllJSONPathMatches(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:    "$.profiles[*].groups",
		ExternalUserLabelFieldKey: "groups",
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"profiles": []any{
			map[string]any{"groups": nil},
			map[string]any{"groups": "edge"},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestDirectNilJSONPathMatchesConsumeTerminalBudget(t *testing.T) {
	matches := make(externalUserMatches, maxExternalUserLabelValues+1)
	budget := &externalUserLabelBudget{}
	if _, err := extractValuesWithParser(matches, "", "", budget); err == nil {
		t.Fatal("extractValuesWithParser() error = nil, want terminal budget rejection")
	}
	if budget.values != maxExternalUserLabelValues {
		t.Fatalf("consumed terminal values = %d, want %d", budget.values, maxExternalUserLabelValues)
	}
}

func TestHandlerDoesNotCountMissingLabelKeysAsNilTerminals(t *testing.T) {
	denyLabels := make(map[string][]string, maxExternalUserLabelValues+1)
	for index := 0; index <= maxExternalUserLabelValues; index++ {
		denyLabels["missing-"+strconv.Itoa(index)] = []string{"blocked"}
	}
	p := newTestPlugin(t, Config{
		ExternalUserLabelField: "groups",
		DenyLabels:             denyLabels,
	})

	res := performExternalUserRequest(p, map[string]any{"groups": "safe"})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerDefaultParserTreatsMalformedJSONArrayAsNoMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		DenyLabels: map[string][]string{
			"groups": {`[edge]`},
		},
	})

	res := performRequest(p, map[string]any{"groups": `[edge]`})
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

func TestHandlerSupportsAPISIX317JSONPathSelectors(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		externalUser map[string]any
	}{
		{
			name: "bracket child",
			path: "$['profile']['groups']",
			externalUser: map[string]any{
				"profile": map[string]any{"groups": []any{"payments", "edge"}},
			},
		},
		{
			name: "zero based array index",
			path: "$.profiles[1].groups",
			externalUser: map[string]any{
				"profiles": []any{
					map[string]any{"groups": []any{"payments"}},
					map[string]any{"groups": []any{"edge"}},
				},
			},
		},
		{
			name: "array wildcard",
			path: "$.profiles[*].groups",
			externalUser: map[string]any{
				"profiles": []any{
					map[string]any{"groups": []any{"payments"}},
					map[string]any{"groups": []any{"edge"}},
				},
			},
		},
		{
			name: "object wildcard",
			path: "$.profiles.*.groups",
			externalUser: map[string]any{
				"profiles": map[string]any{
					"primary":   map[string]any{"groups": []any{"payments"}},
					"secondary": map[string]any{"groups": []any{"edge"}},
				},
			},
		},
		{
			name: "quoted keys",
			path: `$["profile.data"]['team-name']`,
			externalUser: map[string]any{
				"profile.data": map[string]any{"team-name": []any{"payments", "edge"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				AllowLabels: map[string][]string{
					"groups": {"edge"},
				},
			})

			res := performExternalUserRequest(p, tt.externalUser)
			if res.Code != http.StatusNoContent {
				t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerSupportsLuaJSONPathUnionSelectors(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		externalUser map[string]any
	}{
		{
			name: "quoted object keys",
			path: "$.profiles['primary','secondary'].groups",
			externalUser: map[string]any{
				"profiles": map[string]any{
					"primary":   map[string]any{"groups": []any{"allowed"}},
					"secondary": map[string]any{"groups": []any{"blocked"}},
				},
			},
		},
		{
			name: "zero based array indexes",
			path: "$.profiles[0,2].groups",
			externalUser: map[string]any{
				"profiles": []any{
					map[string]any{"groups": []any{"allowed"}},
					map[string]any{"groups": []any{"ignored"}},
					map[string]any{"groups": []any{"blocked"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			})

			res := performExternalUserRequest(p, tt.externalUser)
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerSupportsLuaJSONPathSliceSelectors(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "open start and positive step", path: "$.profiles[:4:2].groups"},
		{name: "negative bounds", path: "$.profiles[-2:].groups"},
		{name: "negative step", path: "$.profiles[3:0:-1].groups"},
		{name: "union of slices", path: "$.profiles[0:1,2:3].groups"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{
				"profiles": []any{
					map[string]any{"groups": []any{"allowed"}},
					map[string]any{"groups": []any{"allowed"}},
					map[string]any{"groups": []any{"blocked"}},
					map[string]any{"groups": []any{"allowed"}},
				},
			})
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerSupportsLuaJSONPathFilterSelectors(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "presence", path: "$.profiles[?(@.blocked)].groups"},
		{name: "compound expression", path: `$.profiles[?(@.score>=10 && @.score<20 AND @.tier=="restricted")].groups`},
		{name: "quoted variable", path: `$.profiles[?(@['team-name']=="restricted")].groups`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{
				"profiles": []any{
					map[string]any{
						"blocked":   false,
						"score":     8,
						"tier":      "standard",
						"team-name": "standard",
						"groups":    []any{"allowed"},
					},
					map[string]any{
						"blocked":   true,
						"score":     12,
						"tier":      "restricted",
						"team-name": "restricted",
						"groups":    []any{"blocked"},
					},
				},
			})
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerSupportsLuaJSONPathScriptSubscript(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$.profiles[((@.length * 2 / 2) - 1)].groups",
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"profiles": []any{
			map[string]any{"groups": []any{"payments"}},
			map[string]any{"groups": []any{"edge"}},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestExternalUserJSONPathResultsUseLuaPathSortOrder(t *testing.T) {
	profiles := make([]any, 12)
	for index := range profiles {
		profiles[index] = strconv.Itoa(index)
	}

	value, ok, err := externalUserField(map[string]any{"profiles": profiles}, "$.profiles[*]")
	if err != nil {
		t.Fatalf("externalUserField() error = %v", err)
	}
	if !ok {
		t.Fatal("externalUserField() matched = false, want true")
	}
	want := externalUserMatches{"0", "1", "10", "11", "2", "3", "4", "5", "6", "7", "8", "9"}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("externalUserField() = %#v, want Lua path order %#v", value, want)
	}
}

func TestParseExternalUserJSONPathSupportsLuaJSONPath10Grammar(t *testing.T) {
	valid := []string{
		"$",
		"$ \t\n",
		"$.store",
		"store",
		"Request.prototype.end",
		"$.store.book[*].author",
		"$..author",
		"$.store.*",
		"$.store..price",
		"$..book[(@.length-1)]",
		"$..book[0,1]",
		"$..book[0:2]",
		"$..book[?(@.isbn)]",
		"$..book[?( @.price<10 )]",
		"$..[*]",
		"$.store.book.0",
		"$.store.book..0",
		`$["store"]`,
		"$['store']",
		"$.store.book[0:1,1:2,2:3]",
		"$.store.book[0:4:2]",
		"$.store['book','bicycle']",
		"$.store.book[1,2,3]['title']",
		"$.store.book[0:1,1:2]['title','author','price']",
		"$..[0,1]",
		`$..book[?( @.length && (@.price.max - @.price.min + 20 > 50 || false) )]`,
		`$.profiles[?(@.score=0xC OR @.score==1.2e1)].groups`,
		`$.profiles[?(@.score!=0 AND @.score<>10)].groups`,
		`$.profiles[?(@.score<=20 && @.score>=10)].groups`,
		`$.profiles[?(@.score<20 || @.score>30)].groups`,
		"$.profiles[0:2:0]",
	}
	for _, path := range valid {
		t.Run(path, func(t *testing.T) {
			if _, err := parseExternalUserJSONPath(path); err != nil {
				t.Fatalf("parseExternalUserJSONPath(%q) error = %v", path, err)
			}
		})
	}

	invalid := []string{
		"",
		"   ",
		"\r$.store",
		".store",
		"..store",
		"()",
		"store.book...",
		"$.['team']",
		"$.team-name",
		"$.profiles[?()]",
		"$.profiles[?(os.exit())]",
		"$.profiles[0:1,]",
	}
	for _, path := range invalid {
		t.Run("invalid "+path, func(t *testing.T) {
			if _, err := parseExternalUserJSONPath(path); err == nil {
				t.Fatalf("parseExternalUserJSONPath(%q) error = nil, want rejection", path)
			}
		})
	}
}

func TestHandlerEvaluatesLuaJSONPath10OperatorVariants(t *testing.T) {
	paths := []string{
		`$.profiles[?(@.score<=12 && @.score>10)].groups`,
		`$.profiles[?(@.tier!="standard" OR false)].groups`,
		`$.profiles[?(@.tier<>"standard" || false)].groups`,
		`$.profiles[?(@.score="12")].groups`,
		`$.profiles[?(@.score==0xC)].groups`,
		`$.profiles[?(@.score>=1.2e1 AND @.score<13)].groups`,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{
				"profiles": []any{
					map[string]any{"score": 8, "tier": "standard", "groups": []any{"allowed"}},
					map[string]any{"score": 12, "tier": "restricted", "groups": []any{"blocked"}},
				},
			})
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
			}
		})
	}

	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$.profiles[((@.length + 2) % 3)].groups",
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		DenyLabels: map[string][]string{
			"groups": {"blocked"},
		},
	})
	res := performExternalUserRequest(p, map[string]any{
		"profiles": []any{
			map[string]any{"groups": []any{"allowed"}},
			map[string]any{"groups": []any{"blocked"}},
		},
	})
	if res.Code != http.StatusForbidden {
		t.Fatalf("script arithmetic response code = %d, want 403; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerFailsClosedOnLuaJSONPathEvaluationError(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		externalUser map[string]any
	}{
		{
			name: "zero slice step",
			path: "$.profiles[0:2:0].groups",
			externalUser: map[string]any{
				"profiles": []any{
					map[string]any{"groups": []any{"allowed"}},
				},
			},
		},
		{
			name: "non numeric arithmetic operand",
			path: "$.profiles[?(@.score+1>10)].groups",
			externalUser: map[string]any{
				"profiles": []any{
					map[string]any{"score": "not-a-number", "groups": []any{"allowed"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			})

			res := performExternalUserRequest(p, tt.externalUser)
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want fail-closed 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerFailsClosedOnNonFiniteLuaJSONPathNumbers(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		externalUser map[string]any
		allow        bool
	}{
		{
			name: "allow string NaN numeric coercion",
			path: `$.profiles[?(0<=@.score)].groups`,
			externalUser: map[string]any{
				"profiles": []any{map[string]any{"score": "NaN", "groups": []any{"edge"}}},
			},
			allow: true,
		},
		{
			name: "deny string Inf numeric coercion",
			path: `$.profiles[?(0>@.score)].groups`,
			externalUser: map[string]any{
				"profiles": []any{map[string]any{"score": "Inf", "groups": []any{"blocked"}}},
			},
		},
		{
			name: "allow string positive Inf numeric coercion",
			path: `$.profiles[?(0<=@.score)].groups`,
			externalUser: map[string]any{
				"profiles": []any{map[string]any{"score": "+Inf", "groups": []any{"edge"}}},
			},
			allow: true,
		},
		{
			name: "deny string negative Inf numeric coercion",
			path: `$.profiles[?(0<@.score)].groups`,
			externalUser: map[string]any{
				"profiles": []any{map[string]any{"score": "-Inf", "groups": []any{"blocked"}}},
			},
		},
		{
			name: "allow arithmetic NaN",
			path: `$.profiles[?((0/0)>=0)].groups`,
			externalUser: map[string]any{
				"profiles": []any{map[string]any{"groups": []any{"edge"}}},
			},
			allow: true,
		},
		{
			name: "deny arithmetic positive infinity",
			path: `$.profiles[?((1/0)<0)].groups`,
			externalUser: map[string]any{
				"profiles": []any{map[string]any{"groups": []any{"blocked"}}},
			},
		},
		{
			name: "deny arithmetic NaN",
			path: `$.profiles[?((0/0)>0)].groups`,
			externalUser: map[string]any{
				"profiles": []any{map[string]any{"groups": []any{"blocked"}}},
			},
		},
		{
			name: "allow arithmetic negative infinity",
			path: `$.profiles[?((-1/0)<0)].groups`,
			externalUser: map[string]any{
				"profiles": []any{map[string]any{"groups": []any{"edge"}}},
			},
			allow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
			}
			if tt.allow {
				cfg.AllowLabels = map[string][]string{"groups": {"edge"}}
			} else {
				cfg.DenyLabels = map[string][]string{"groups": {"blocked"}}
			}
			p := newTestPlugin(t, cfg)

			res := performExternalUserRequest(p, tt.externalUser)
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want fail-closed 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestPostInitRejectsNonFiniteLuaJSONPathLiteral(t *testing.T) {
	p := &Plugin{config: Config{
		ExternalUserLabelField: `$.profiles[?(1e999>0)].groups`,
		DenyLabels:             map[string][]string{"groups": {"blocked"}},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want non-finite numeric literal rejected")
	}
}

func TestHandlerFailsClosedWhenLuaWouldStringifyReferenceValue(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "object", value: map[string]any{"key": "value"}},
		{name: "array", value: []any{"value"}},
		{name: "function", value: func() {}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       `$.profiles[?(""!=@.meta)].groups`,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{
				"profiles": []any{map[string]any{"meta": tt.value, "groups": []any{"blocked"}}},
			})
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want fail-closed 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerPreservesLuaScalarStringCoercion(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		value any
	}{
		{name: "false becomes empty string", path: `$.profiles[?(""==@.meta)].groups`, value: false},
		{name: "true becomes true string", path: `$.profiles[?("true"==@.meta)].groups`, value: true},
		{name: "number becomes decimal string", path: `$.profiles[?("12"==@.meta)].groups`, value: 12},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{
				"profiles": []any{map[string]any{"meta": tt.value, "groups": []any{"blocked"}}},
			})
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestExternalUserJSONPathDeduplicatesConcreteRecursiveMatches(t *testing.T) {
	value, ok, err := externalUserField(map[string]any{
		"x": map[string]any{
			"x": map[string]any{
				"x": "edge",
			},
		},
	}, "$..x..x")
	if err != nil {
		t.Fatalf("externalUserField() error = %v", err)
	}
	if !ok {
		t.Fatal("externalUserField() matched = false, want true")
	}
	want := externalUserMatches{map[string]any{"x": "edge"}, "edge"}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("externalUserField() = %#v, want deduplicated concrete paths %#v", value, want)
	}
}

func TestPostInitRejectsLuaJSONPathComplexityBudgets(t *testing.T) {
	union := make([]string, 257)
	for index := range union {
		union[index] = strconv.Itoa(index)
	}
	tests := []struct {
		name string
		path string
	}{
		{name: "selector steps", path: "$" + strings.Repeat(".node", 65)},
		{name: "expression nodes", path: "$.profiles[?(" + strings.Repeat("1+", 129) + "1>0)].groups"},
		{name: "expression members", path: "$.profiles[?(@" + strings.Repeat(".a", 257) + ")].groups"},
		{
			name: "expression nesting",
			path: "$.profiles[?(" + strings.Repeat("(", 65) + "true" +
				strings.Repeat(")", 65) + ")].groups",
		},
		{name: "union terms", path: "$.profiles[" + strings.Join(union, ",") + "].groups"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Plugin{config: Config{
				ExternalUserLabelField: tt.path,
				DenyLabels:             map[string][]string{"groups": {"blocked"}},
			}}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err == nil {
				t.Fatal("PostInit() error = nil, want complexity budget rejection")
			}
		})
	}
}

func TestHandlerFailsClosedOnLuaJSONPathRuntimeBudgets(t *testing.T) {
	deep := map[string]any{"leaf": "edge"}
	for range 65 {
		deep = map[string]any{"node": deep}
	}

	wide := make([]any, 4097)
	for index := range wide {
		wide[index] = map[string]any{"other": index}
	}
	wide[len(wide)-1] = map[string]any{"groups": []any{"edge"}}

	results := make([]any, 1025)
	for index := range results {
		results[index] = map[string]any{"groups": []any{"other"}}
	}
	results[len(results)-1] = map[string]any{"groups": []any{"edge"}}

	tests := []struct {
		name         string
		path         string
		externalUser map[string]any
	}{
		{name: "recursive depth", path: "$..leaf", externalUser: deep},
		{name: "visited nodes", path: "$..groups", externalUser: map[string]any{"profiles": wide}},
		{name: "result count", path: "$.profiles[*].groups", externalUser: map[string]any{"profiles": results}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				AllowLabels: map[string][]string{
					"groups": {"edge"},
				},
			})

			res := performExternalUserRequest(p, tt.externalUser)
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want budget fail-closed 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerLuaJSONPathBudgetsAreRequestLocal(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$.profiles[*].groups",
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})
	overBudget := make([]any, 1025)
	for index := range overBudget {
		overBudget[index] = map[string]any{"groups": []any{"edge"}}
	}

	const requests = 24
	var group sync.WaitGroup
	errors := make(chan string, requests)
	for index := range requests {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			user := map[string]any{"profiles": []any{map[string]any{"groups": []any{"edge"}}}}
			want := http.StatusNoContent
			if index%2 == 0 {
				user["profiles"] = overBudget
				want = http.StatusForbidden
			}
			res := performExternalUserRequest(p, user)
			if res.Code != want {
				errors <- "response code = " + strconv.Itoa(res.Code) + ", want " + strconv.Itoa(want)
			}
		}(index)
	}
	group.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
}

func TestHandlerTreatsMissingLuaJSONPathFilterVariableAsNoMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$.profiles[?(@.blocked)].groups",
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		DenyLabels: map[string][]string{
			"groups": {"blocked"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"profiles": []any{
			map[string]any{"groups": []any{"blocked"}},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want missing variable to be a non-match; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerTreatsFailedLuaJSONPathEqualityCoercionAsNoMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       `$.profiles[?(@.score=="not-a-number")].groups`,
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		DenyLabels: map[string][]string{
			"groups": {"blocked"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"profiles": []any{
			map[string]any{"score": 12, "groups": []any{"blocked"}},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf(
			"response code = %d, want failed numeric equality coercion to be a non-match; body=%s",
			res.Code,
			res.Body.String(),
		)
	}
}

func TestHandlerMatchesNumericObjectKeyLikeLuaJSONPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:    "$.labels.0",
		ExternalUserLabelFieldKey: "team",
		DenyLabels: map[string][]string{
			"team": {"blocked"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"labels": map[string]any{"0": "blocked"},
	})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerMatchesQuotedNumericArrayIndexLikeLuaJSONPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$.profiles['1'].groups",
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		DenyLabels: map[string][]string{
			"groups": {"blocked"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"profiles": []any{
			map[string]any{"groups": []any{"allowed"}},
			map[string]any{"groups": []any{"blocked"}},
		},
	})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerDoesNotNormalizeNonCanonicalLuaJSONPathIndexes(t *testing.T) {
	for _, path := range []string{"$.profiles[+1].groups", "$.profiles[01].groups"} {
		t.Run(path, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				AllowLabels: map[string][]string{
					"groups": {"edge"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{
				"profiles": []any{
					map[string]any{"groups": []any{"payments"}},
					map[string]any{"groups": []any{"edge"}},
				},
			})
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want 403; body=%s", res.Code, res.Body.String())
			}
		})
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

func TestHandlerAppliesSegmentedParserToEveryExternalUserJSONPathMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		ExternalUserLabelField:          "$.orgs..team",
		ExternalUserLabelFieldKey:       "team",
		ExternalUserLabelFieldParser:    "segmented_text",
		ExternalUserLabelFieldSeparator: `\|`,
		AllowLabels: map[string][]string{
			"team": {"cloud", "infra"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"orgs": map[string]any{
			"api7":   map[string]any{"team": "cloud|infra"},
			"apache": map[string]any{"team": []any{"devops", "qa"}},
		},
	})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerPost317AllMatchParsesOnlyStringMatches(t *testing.T) {
	tests := []struct {
		name   string
		parser string
		first  string
	}{
		{name: "segmented text", parser: "segmented_text", first: "allowed,other"},
		{name: "json", parser: "json", first: `["allowed","other"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				ExternalUserLabelField:       "$.profiles[*].groups",
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: tt.parser,
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			}
			if tt.parser == "segmented_text" {
				cfg.ExternalUserLabelFieldSeparator = ","
			}
			p := newTestPlugin(t, cfg)

			res := performExternalUserRequest(p, map[string]any{
				"profiles": []any{
					map[string]any{"groups": tt.first},
					map[string]any{"groups": []any{"blocked"}},
				},
			})
			if res.Code != http.StatusForbidden {
				t.Fatalf("response code = %d, want mixed-match deny 403; body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlerSingleActualMatchRetainsConfiguredParser(t *testing.T) {
	tests := []struct {
		name       string
		allow      bool
		wantStatus int
	}{
		{name: "allow remains unmatched", allow: true, wantStatus: http.StatusForbidden},
		{name: "deny remains unmatched", allow: false, wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				ExternalUserLabelField:          "$.profiles[*].groups",
				ExternalUserLabelFieldKey:       "groups",
				ExternalUserLabelFieldParser:    "segmented_text",
				ExternalUserLabelFieldSeparator: ",",
			}
			if tt.allow {
				cfg.AllowLabels = map[string][]string{"groups": {"allowed"}}
			} else {
				cfg.DenyLabels = map[string][]string{"groups": {"allowed"}}
			}
			p := newTestPlugin(t, cfg)

			res := performExternalUserRequest(p, map[string]any{
				"profiles": []any{map[string]any{"groups": []any{"allowed"}}},
			})
			if res.Code != tt.wantStatus {
				t.Fatalf(
					"response code = %d, want single-match configured-parser status %d; body=%s",
					res.Code, tt.wantStatus, res.Body.String(),
				)
			}
		})
	}
}

func TestHandlerFailsClosedOnUnsafeScriptSelectorReference(t *testing.T) {
	tests := []struct {
		name     string
		selector any
	}{
		{name: "map", selector: map[string]any{"target": true}},
		{name: "function", selector: func() {}},
		{name: "nested reference", selector: []any{[]any{"target"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       "$.profiles[(@.selector)].groups",
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				DenyLabels: map[string][]string{
					"groups": {"blocked"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{
				"profiles": map[string]any{
					"selector": tt.selector,
					"target":   map[string]any{"groups": []any{"blocked"}},
				},
			})
			if res.Code != http.StatusForbidden {
				t.Fatalf(
					"response code = %d, want unsafe script reference fail-closed 403; body=%s",
					res.Code,
					res.Body.String(),
				)
			}
		})
	}
}

func TestHandlerFailsClosedWhenScriptComponentExpansionExceedsBudget(t *testing.T) {
	components := func(count int) []any {
		values := make([]any, count)
		for index := range values {
			values[index] = "ignored-" + strconv.Itoa(index)
		}
		values[len(values)-1] = "target"
		return values
	}
	parent := func(selector []any) map[string]any {
		return map[string]any{
			"selector": selector,
			"target":   map[string]any{"groups": []any{"allowed"}},
		}
	}
	tests := []struct {
		name string
		path string
		user map[string]any
	}{
		{
			name: "single expansion count",
			path: "$.profiles[(@.selector)].groups",
			user: map[string]any{"profiles": parent(components(4097))},
		},
		{
			name: "request cumulative count",
			path: "$.collections[*][(@.selector)].groups",
			user: map[string]any{"collections": []any{
				parent(components(2049)),
				parent(components(2049)),
			}},
		},
		{
			name: "component bytes",
			path: "$.profiles[(@.selector)].groups",
			user: map[string]any{"profiles": parent([]any{
				strings.Repeat("x", 256*1024+1), "target",
			})},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:       tt.path,
				ExternalUserLabelFieldKey:    "groups",
				ExternalUserLabelFieldParser: "table",
				AllowLabels: map[string][]string{
					"groups": {"allowed"},
				},
			})

			res := performExternalUserRequest(p, tt.user)
			if res.Code != http.StatusForbidden {
				t.Fatalf(
					"response code = %d, want script expansion fail-closed 403; body=%s",
					res.Code, res.Body.String(),
				)
			}
		})
	}
}

func TestHandlerFailsClosedWhenTerminalLabelExpansionExceedsBudget(t *testing.T) {
	tooMany := make([]any, 4097)
	for index := range tooMany {
		tooMany[index] = "edge"
	}
	tests := []struct {
		name      string
		value     any
		parser    string
		separator string
	}{
		{name: "label count", value: tooMany, parser: "table"},
		{
			name:      "label bytes",
			value:     strings.Repeat("edge,", 65537) + "edge",
			parser:    "segmented_text",
			separator: ",",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ExternalUserLabelField:          "groups",
				ExternalUserLabelFieldKey:       "groups",
				ExternalUserLabelFieldParser:    tt.parser,
				ExternalUserLabelFieldSeparator: tt.separator,
				AllowLabels: map[string][]string{
					"groups": {"edge"},
				},
			})

			res := performExternalUserRequest(p, map[string]any{"groups": tt.value})
			if res.Code != http.StatusForbidden {
				t.Fatalf(
					"response code = %d, want terminal expansion fail-closed 403; body=%s",
					res.Code,
					res.Body.String(),
				)
			}
		})
	}
}

func TestHandlerFailsClosedWhenNonStringTerminalLabelsExceedRequestBudgetAcrossMatches(t *testing.T) {
	first := make([]any, 2048)
	second := make([]any, 2049)
	for index := range first {
		first[index] = index
	}
	for index := range second {
		second[index] = index%2 == 0
	}

	p := newTestPlugin(t, Config{
		ExternalUserLabelField:       "$.profiles[*].groups",
		ExternalUserLabelFieldKey:    "groups",
		ExternalUserLabelFieldParser: "table",
		DenyLabels: map[string][]string{
			"groups": {"blocked"},
		},
	})

	res := performExternalUserRequest(p, map[string]any{
		"profiles": []any{
			map[string]any{"groups": first},
			map[string]any{"groups": second},
		},
	})
	if res.Code != http.StatusForbidden {
		t.Fatalf(
			"response code = %d, want non-string terminal expansion fail-closed 403; body=%s",
			res.Code,
			res.Body.String(),
		)
	}
}

func TestOversizedExternalUserJSONPathFailsAdmission(t *testing.T) {
	path := "$." + strings.Repeat("a", 4095)
	t.Run("schema", func(t *testing.T) {
		config := map[string]any{
			"external_user_label_field": path,
			"allow_labels":              map[string]any{"groups": []any{"edge"}},
		}
		if err := util.Validate(config, schema); err == nil {
			t.Fatal("schema validation error = nil, want oversized JSONPath rejected")
		}
	})
	t.Run("PostInit", func(t *testing.T) {
		p := &Plugin{config: Config{
			ExternalUserLabelField: path,
			AllowLabels:            map[string][]string{"groups": {"edge"}},
		}}
		if err := p.Init(); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		if err := p.PostInit(); err == nil {
			t.Fatal("PostInit() error = nil, want oversized JSONPath rejected")
		}
	})
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

func TestPostInitRequiresQuotedJSONPathKeysWithPunctuation(t *testing.T) {
	p := &Plugin{config: Config{
		ExternalUserLabelField: "$.team-name",
		AllowLabels:            map[string][]string{"team": {"cloud"}},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want punctuation key to require bracket quotes")
	}
}

func TestPostInitRejectsDotBeforeBracketSelector(t *testing.T) {
	p := &Plugin{config: Config{
		ExternalUserLabelField: "$.['team']",
		AllowLabels:            map[string][]string{"team": {"cloud"}},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want dot before bracket selector rejected")
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

func TestSchemaRejectsInvalidExternalUserOptions(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name   string
		config map[string]any
	}{
		{
			name: "invalid parser",
			config: map[string]any{
				"allow_labels":                     map[string]any{"team": []any{"cloud"}},
				"external_user_label_field":        "team",
				"external_user_label_field_parser": "an-invalid-parser",
			},
		},
		{
			name: "segmented parser without separator",
			config: map[string]any{
				"allow_labels":                     map[string]any{"team": []any{"cloud"}},
				"external_user_label_field":        "team",
				"external_user_label_field_parser": "segmented_text",
			},
		},
		{
			name: "empty field key",
			config: map[string]any{
				"allow_labels":                        map[string]any{"team": []any{"cloud"}},
				"external_user_label_field":           "team",
				"external_user_label_field_parser":    "segmented_text",
				"external_user_label_field_key":       "",
				"external_user_label_field_separator": ",",
			},
		},
		{
			name: "non-string field key",
			config: map[string]any{
				"allow_labels":                        map[string]any{"team": []any{"cloud"}},
				"external_user_label_field":           "team",
				"external_user_label_field_parser":    "segmented_text",
				"external_user_label_field_key":       map[string]any{},
				"external_user_label_field_separator": ",",
			},
		},
		{
			name: "empty separator",
			config: map[string]any{
				"allow_labels":                        map[string]any{"team": []any{"cloud"}},
				"external_user_label_field":           "team",
				"external_user_label_field_parser":    "segmented_text",
				"external_user_label_field_separator": "",
			},
		},
		{
			name: "non-string separator",
			config: map[string]any{
				"allow_labels":                        map[string]any{"team": []any{"cloud"}},
				"external_user_label_field":           "team",
				"external_user_label_field_parser":    "segmented_text",
				"external_user_label_field_separator": map[string]any{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := util.Validate(tt.config, p.GetSchema()); err == nil {
				t.Fatal("Validate() error = nil, want invalid configuration rejected")
			}
		})
	}
}

func TestSchemaRejectsRejectedCodeAboveHTTPMaximum(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"allow_labels":  map[string]any{"team": []any{"edge"}},
		"rejected_code": 1000,
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("rejected_code=1000 should fail schema validation")
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
