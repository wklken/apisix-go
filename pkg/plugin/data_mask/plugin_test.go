package data_mask

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
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

func TestHandlerMasksQueryHeadersAndURLEncodedBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: []MaskRule{
			{Type: "query", Name: "password", Action: "remove"},
			{Type: "query", Name: "token", Action: "replace", Value: "*****"},
			{Type: "query", Name: "card", Action: "regex", Regex: `(\d+)-\d+-\d+-(\d+)`, Value: "$1-****-****-$2"},
			{Type: "header", Name: "Authorization", Action: "remove"},
			{Type: "header", Name: "X-API-Key", Action: "replace", Value: "[REDACTED]"},
			{Type: "body", BodyFormat: "urlencoded", Name: "secret", Action: "remove"},
			{Type: "body", BodyFormat: "urlencoded", Name: "body_token", Action: "replace", Value: "*****"},
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders?password=secret&token=mytoken&card=1234-5678-9012-3456",
		strings.NewReader("secret=s1&body_token=tok&keep=yes"),
	)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-API-Key", "api-key")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("password") != "" {
			t.Fatalf("password query = %q, want removed", query.Get("password"))
		}
		if query.Get("token") != "*****" {
			t.Fatalf("token query = %q, want masked", query.Get("token"))
		}
		if query.Get("card") != "1234-****-****-3456" {
			t.Fatalf("card query = %q, want regex mask", query.Get("card"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("Authorization header = %q, want removed", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-API-Key") != "[REDACTED]" {
			t.Fatalf("X-API-Key = %q, want redacted", r.Header.Get("X-API-Key"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		values := mustParseQuery(t, string(body))
		if values.Get("secret") != "" {
			t.Fatalf("secret body = %q, want removed", values.Get("secret"))
		}
		if values.Get("body_token") != "*****" {
			t.Fatalf("body_token = %q, want masked", values.Get("body_token"))
		}
		if values.Get("keep") != "yes" {
			t.Fatalf("keep = %q, want preserved", values.Get("keep"))
		}
	})).ServeHTTP(rr, req)
}

func TestHandlerMasksParsedPrefixWhenURLEncodedBodyExceedsArgumentLimit(t *testing.T) {
	p := newTestPlugin(t, Config{
		MaxReqPostArgs: new(1),
		Request: []MaskRule{{
			Type:       "body",
			BodyFormat: "urlencoded",
			Name:       "token",
			Action:     "replace",
			Value:      "*****",
		}},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		strings.NewReader("token=secret&keep=yes"),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		values := mustParseQuery(t, string(body))
		if values.Get("token") != "*****" {
			t.Fatalf("token = %q, want masked parsed prefix", values.Get("token"))
		}
		if values.Get("keep") != "" {
			t.Fatalf("keep = %q, want unparsed suffix omitted", values.Get("keep"))
		}
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestHandlerMasksRequestLineForLoggerFields(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: []MaskRule{{Type: "query", Name: "token", Action: "replace", Value: "*****"}},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders?token=secret&keep=yes", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := apisixlog.GetField(r, "$request_uri"); got != "/orders?keep=yes&token=%2A%2A%2A%2A%2A" {
			t.Fatalf("request_uri log field = %q, want masked URI", got)
		}
		if got := apisixlog.GetField(r, "$request_line"); got != "GET /orders?keep=yes&token=%2A%2A%2A%2A%2A HTTP/1.1" {
			t.Fatalf("request_line log field = %q, want masked request line", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerMasksJSONBodyWithSimpleJSONPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: []MaskRule{
			{Type: "body", BodyFormat: "json", Name: "$.password", Action: "remove"},
			{Type: "body", BodyFormat: "json", Name: "$.users[*].token", Action: "replace", Value: "*****"},
			{
				Type:       "body",
				BodyFormat: "json",
				Name:       "$.users[*].credit.card",
				Action:     "regex",
				Regex:      `(\d+)-\d+-\d+-(\d+)`,
				Value:      "$1-****-****-$2",
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{
		"password": "secret",
		"users": [
			{"name": "alice", "token": "tok-a", "credit": {"card": "1234-5678-9012-3456"}},
			{"name": "bob", "token": "tok-b", "credit": {"card": "9876-5432-1098-7654"}}
		]
	}`))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := data["password"]; ok {
			t.Fatal("password field still exists")
		}
		users := data["users"].([]any)
		for _, user := range users {
			item := user.(map[string]any)
			if item["token"] != "*****" {
				t.Fatalf("token = %v, want masked", item["token"])
			}
		}
		first := users[0].(map[string]any)["credit"].(map[string]any)["card"]
		if first != "1234-****-****-3456" {
			t.Fatalf("first card = %v, want masked", first)
		}
		second := users[1].(map[string]any)["credit"].(map[string]any)["card"]
		if second != "9876-****-****-7654" {
			t.Fatalf("second card = %v, want masked", second)
		}
	})).ServeHTTP(rr, req)
}

func TestHandlerMasksJSONBodyWithRootArrayJSONPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: []MaskRule{{
			Type:       "body",
			BodyFormat: "json",
			Name:       "$[*].token",
			Action:     "replace",
			Value:      "*****",
		}},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		strings.NewReader(`[{"token":"tok-a"},{"token":"tok-b"}]`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var data []map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		for index, item := range data {
			if item["token"] != "*****" {
				t.Fatalf("item %d token = %v, want masked", index, item["token"])
			}
		}
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestHandlerMasksJSONBodyWithPathWithoutDollarPrefix(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: []MaskRule{
			{Type: "body", BodyFormat: "json", Name: "users[*].token", Action: "replace", Value: "*****"},
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		strings.NewReader(`{"users":[{"token":"tok-a"},{"token":"tok-b"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		for _, item := range data["users"].([]any) {
			if got := item.(map[string]any)["token"]; got != "*****" {
				t.Fatalf("token = %v, want masked", got)
			}
		}
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestHandlerMasksJSONBodyWithArrayIndexJSONPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: []MaskRule{
			{Type: "body", BodyFormat: "json", Name: "$.users[0].token", Action: "replace", Value: "*****"},
			{
				Type:       "body",
				BodyFormat: "json",
				Name:       "$.users[1].credit.card",
				Action:     "regex",
				Regex:      `(\d+)-\d+-\d+-(\d+)`,
				Value:      "$1-****-****-$2",
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{
		"users": [
			{"name": "alice", "token": "tok-a", "credit": {"card": "1234-5678-9012-3456"}},
			{"name": "bob", "token": "tok-b", "credit": {"card": "9876-5432-1098-7654"}}
		]
	}`))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		users := data["users"].([]any)
		first := users[0].(map[string]any)
		if first["token"] != "*****" {
			t.Fatalf("first token = %v, want masked", first["token"])
		}
		firstCard := first["credit"].(map[string]any)["card"]
		if firstCard != "1234-5678-9012-3456" {
			t.Fatalf("first card = %v, want preserved", firstCard)
		}
		second := users[1].(map[string]any)
		if second["token"] != "tok-b" {
			t.Fatalf("second token = %v, want preserved", second["token"])
		}
		secondCard := second["credit"].(map[string]any)["card"]
		if secondCard != "9876-****-****-7654" {
			t.Fatalf("second card = %v, want masked", secondCard)
		}
	})).ServeHTTP(rr, req)
}

func TestHandlerMasksJSONBodyWithQuotedBracketPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: []MaskRule{{
			Type:       "body",
			BodyFormat: "json",
			Name:       `$["user"]["token"]`,
			Action:     "replace",
			Value:      "*****",
		}},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		strings.NewReader(`{"user":{"token":"secret"}}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), `"token":"*****"`) {
			t.Fatalf("body = %q, want quoted bracket path masked", body)
		}
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestHandlerMasksJSONBodyWithRecursiveJSONPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: []MaskRule{{
			Type:       "body",
			BodyFormat: "json",
			Name:       "$..token",
			Action:     "replace",
			Value:      "*****",
		}},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		strings.NewReader(`{"left":{"token":"one"},"right":[{"token":"two"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if strings.Contains(string(body), `"token":"one"`) || strings.Contains(string(body), `"token":"two"`) {
			t.Fatalf("body = %q, want recursive token masking", body)
		}
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestPostInitDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.MaxBodySize != 1024*1024 {
		t.Fatalf("max_body_size = %d, want 1048576", p.config.MaxBodySize)
	}
	if p.config.MaxReqPostArgs == nil || *p.config.MaxReqPostArgs != 100 {
		t.Fatalf("max_req_post_args = %v, want 100", p.config.MaxReqPostArgs)
	}
}

func TestPostInitPreservesExplicitZeroMaxReqPostArgs(t *testing.T) {
	p := newTestPlugin(t, Config{MaxReqPostArgs: new(0)})

	if p.config.MaxReqPostArgs == nil || *p.config.MaxReqPostArgs != 0 {
		t.Fatalf("max_req_post_args = %v, want explicit zero", p.config.MaxReqPostArgs)
	}
}

func TestSchemaRequiresConditionalMaskRuleFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, test := range []struct {
		name   string
		config map[string]any
	}{
		{
			name: "body format",
			config: map[string]any{
				"request": []any{map[string]any{
					"type": "body", "name": "secret", "action": "remove",
				}},
			},
		},
		{
			name: "regex fields",
			config: map[string]any{
				"request": []any{map[string]any{
					"type": "header", "name": "Authorization", "action": "regex",
				}},
			},
		},
		{
			name: "replace value",
			config: map[string]any{
				"request": []any{map[string]any{
					"type": "header", "name": "Authorization", "action": "replace",
				}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(test.config, p.GetSchema()); err == nil {
				t.Fatal("Validate() error = nil, want conditional field rejection")
			}
		})
	}
}

func TestHandlerSkipsJSONMaskWhenBodyExceedsLimit(t *testing.T) {
	p := newTestPlugin(t, Config{
		MaxBodySize: 10,
		Request: []MaskRule{{
			Type:       "body",
			BodyFormat: "json",
			Name:       "$.token",
			Action:     "replace",
			Value:      "*****",
		}},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		strings.NewReader(`{"token":"secret"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), `"token":"secret"`) {
			t.Fatalf("body = %q, want oversized body left unchanged", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func mustParseQuery(t *testing.T, raw string) url.Values {
	t.Helper()

	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse query body: %v", err)
	}
	return values
}
