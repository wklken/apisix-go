package request_validation

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

// These pairs exercise accepted constraints at the request boundary. A failed
// validation must stop the request before downstream runs.
func TestSchemaConstraintsRejectInvalidRequests(t *testing.T) {
	for _, test := range []struct{ name, schema, valid, invalid string }{
		{`required`, `{"type":"object","required":["x"]}`, `{"x":1}`, `{}`},
		{`additional-properties`, `{"type":"object","properties":{"x":{"type":"integer"}},"additionalProperties":false}`, `{"x":1}`, `{"x":1,"extra":2}`},
		{`property-dependency`, `{"type":"object","dependencies":{"x":["y"]}}`, `{"x":1,"y":2}`, `{"x":1}`},
		{`schema-dependency`, `{"type":"object","dependencies":{"x":{"required":["y"]}}}`, `{"x":1,"y":2}`, `{"x":1}`},
		{`numeric-bounds`, `{"type":"number","minimum":1,"maximum":3}`, `2`, `0`},
		{`exclusive-numeric-bound`, `{"type":"number","exclusiveMinimum":1}`, `2`, `1`},
		{`multiple-of`, `{"type":"number","multipleOf":2}`, `4`, `3`},
		{`string-length`, `{"type":"string","minLength":2,"maxLength":4}`, `"abc"`, `"a"`},
		{`string-pattern`, `{"type":"string","pattern":"^good$"}`, `"good"`, `"bad"`},
		{`enum`, `{"enum":["allow","deny"]}`, `"allow"`, `"other"`},
		{`const`, `{"const":"allow"}`, `"allow"`, `"other"`},
		{`array-items`, `{"type":"array","items":{"type":"integer"}}`, `[1,2]`, `[1,"bad"]`},
		{`array-size`, `{"type":"array","minItems":1,"maxItems":2}`, `[1]`, `[]`},
		{`array-unique`, `{"type":"array","uniqueItems":true}`, `[1,2]`, `[1,1]`},
		{`all-of`, `{"allOf":[{"type":"integer"},{"minimum":2}]}`, `2`, `1`},
		{`any-of`, `{"anyOf":[{"type":"integer"},{"enum":["ok"]}]}`, `"ok"`, `"bad"`},
		{`one-of`, `{"oneOf":[{"type":"integer"},{"minimum":2}]}`, `1`, `2`},
		{`not`, `{"not":{"enum":["deny"]}}`, `"allow"`, `"deny"`},
		{`conditional`, `{"type":"object","if":{"required":["x"]},"then":{"required":["y"]}}`, `{"x":1,"y":2}`, `{"x":1}`},
		{`local-reference`, `{"type":"object","definitions":{"integer":{"type":"integer"}},"properties":{"x":{"$ref":"#/definitions/integer"}}}`, `{"x":1}`, `{"x":"bad"}`},
		{`format-email`, `{"type":"object","properties":{"value":{"type":"string","format":"email"}}}`, `{"value":"a@example.com"}`, `{"value":"bad"}`},
		{`format-ipv4`, `{"type":"object","properties":{"value":{"type":"string","format":"ipv4"}}}`, `{"value":"192.0.2.1"}`, `{"value":"999.0.0.1"}`},
		{`format-ipv6`, `{"type":"object","properties":{"value":{"type":"string","format":"ipv6"}}}`, `{"value":"2001:db8::1"}`, `{"value":"bad"}`},
		{`format-hostname`, `{"type":"object","properties":{"value":{"type":"string","format":"hostname"}}}`, `{"value":"example.com"}`, `{"value":"bad host"}`},
		{`format-uuid`, `{"type":"object","properties":{"value":{"type":"string","format":"uuid"}}}`, `{"value":"12345678-1234-1234-1234-123456789abc"}`, `{"value":"bad"}`},
		{`format-date`, `{"type":"object","properties":{"value":{"type":"string","format":"date"}}}`, `{"value":"2026-09-07"}`, `{"value":"2026-13-07"}`},
		{`format-date-time`, `{"type":"object","properties":{"value":{"type":"string","format":"date-time"}}}`, `{"value":"2026-09-07T10:00:00Z"}`, `{"value":"bad"}`},
		{`format-uri`, `{"type":"object","properties":{"value":{"type":"string","format":"uri"}}}`, `{"value":"https://example.com"}`, `{"value":"bad"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal([]byte(test.schema), &schema); err != nil {
				t.Fatal(err)
			}
			p := newTestPlugin(t, Config{BodySchema: schema, RejectedMsg: "schema rejected"})
			for _, input := range []struct {
				name, body            string
				wantStatus, wantCalls int
			}{
				{"valid", test.valid, http.StatusNoContent, 1},
				{"invalid", test.invalid, http.StatusBadRequest, 0},
			} {
				t.Run(input.name, func(t *testing.T) {
					calls := 0
					request := httptest.NewRequest(
						http.MethodPost,
						"http://example.test/validate",
						strings.NewReader(input.body),
					)
					request.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls++
						w.WriteHeader(http.StatusNoContent)
					})).ServeHTTP(response, request)
					if response.Code != input.wantStatus || calls != input.wantCalls {
						t.Fatalf(
							"status=%d downstream calls=%d, want status=%d calls=%d; response=%q",
							response.Code,
							calls,
							input.wantStatus,
							input.wantCalls,
							response.Body.String(),
						)
					}
				})
			}
		})
	}
}

func TestExplicitSchemaDialectDoesNotDisableFormatValidation(t *testing.T) {
	for _, dialect := range []string{
		"http://json-schema.org/draft-07/schema#",
		"https://json-schema.org/draft/2019-09/schema",
		"https://json-schema.org/draft/2020-12/schema",
	} {
		t.Run(dialect, func(t *testing.T) {
			p := newTestPlugin(t, Config{BodySchema: map[string]any{
				"$schema":    dialect,
				"type":       "object",
				"properties": map[string]any{"email": map[string]any{"type": "string", "format": "email"}},
			}, RejectedMsg: "schema rejected"})
			for _, input := range []struct {
				body string
				want int
			}{
				{`{"email":"a@example.com"}`, http.StatusNoContent},
				{`{"email":"bad"}`, http.StatusBadRequest},
			} {
				calls := 0
				req := httptest.NewRequest(
					http.MethodPost,
					"http://example.test/validate",
					strings.NewReader(input.body),
				)
				req.Header.Set("Content-Type", "application/json")
				res := httptest.NewRecorder()
				p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.WriteHeader(http.StatusNoContent)
				})).ServeHTTP(res, req)
				if res.Code != input.want || (input.want == http.StatusBadRequest && calls != 0) {
					t.Fatalf(
						"body=%s status=%d downstream calls=%d, want status=%d",
						input.body,
						res.Code,
						calls,
						input.want,
					)
				}
			}
		})
	}
}
