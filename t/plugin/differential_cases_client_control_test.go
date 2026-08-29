package pluginintegration

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestDifferentialClientControlCasesCoverPinnedAPISIX317ContentLengthBlocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialClientControlCases()
	if len(cases) != 2 {
		t.Fatalf("differentialClientControlCases() = %d cases, want 2", len(cases))
	}

	byName := make(map[string]DifferentialCase, len(cases))
	for _, spec := range cases {
		if spec.Plugin != "client-control" {
			t.Fatalf("case %q plugin = %q, want client-control", spec.Name, spec.Plugin)
		}
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if spec.Request.Method != http.MethodPost || spec.Request.Path != "/hello" {
			t.Fatalf("case %q request = %s %s, want POST /hello", spec.Name, spec.Request.Method, spec.Request.Path)
		}

		routes, ok := spec.Config["routes"].([]any)
		if !ok || len(routes) != 1 {
			t.Fatalf("case %q routes = %#v, want one route", spec.Name, spec.Config["routes"])
		}
		route, ok := routes[0].(map[string]any)
		if !ok {
			t.Fatalf("case %q route = %#v, want map", spec.Name, routes[0])
		}
		plugins, ok := route["plugins"].(map[string]any)
		if !ok {
			t.Fatalf("case %q plugins = %#v, want map", spec.Name, route["plugins"])
		}
		clientControl, ok := plugins["client-control"].(map[string]any)
		if !ok || clientControl["max_body_size"] != 5 {
			t.Fatalf("case %q client-control config = %#v, want max_body_size 5", spec.Name, plugins["client-control"])
		}
		config := string(mustYAML(t, spec.Config))
		if !strings.Contains(config, differentialFixturePlaceholder) {
			t.Fatalf("case %q config does not contain fixture placeholder:\n%s", spec.Name, config)
		}

		raw, request, err := renderDifferentialOracleRequest(spec)
		if err != nil {
			t.Fatalf("render case %q request: %v", spec.Name, err)
		}
		if request.ContentLength != int64(len(spec.Request.Body)) {
			t.Fatalf("case %q ContentLength = %d, want %d", spec.Name, request.ContentLength, len(spec.Request.Body))
		}
		wantHeader := []byte("Content-Length: " + strconv.Itoa(len(spec.Request.Body)) + "\r\n")
		if !bytes.Contains(raw, wantHeader) {
			t.Fatalf("case %q raw request lacks %q:\n%s", spec.Name, wantHeader, raw)
		}
		if bytes.Contains(bytes.ToLower(raw), []byte("transfer-encoding: chunked")) {
			t.Fatalf("case %q unexpectedly uses chunked transfer encoding", spec.Name)
		}
		if _, duplicate := byName[spec.Name]; duplicate {
			t.Fatalf("duplicate case name %q", spec.Name)
		}
		byName[spec.Name] = spec
	}

	// APISIX 3.17 client-control.t TEST 1 creates max_body_size=5 and
	// TEST 2 proves a declared six-byte body is rejected before upstream.
	reject, ok := byName[differentialClientControlContentLengthTooLargeCase]
	if !ok {
		t.Fatalf("missing stable semantic-policy case %q", differentialClientControlContentLengthTooLargeCase)
	}
	if reject.Request.Body != "123456" || reject.Fixture.ExpectedCalls != 0 || reject.SecurityDecision != "deny" {
		t.Fatalf("content-length reject semantics = %#v", reject)
	}
	if reject.ComparisonPolicy != differentialComparisonPlatformOwnedErrorRepresentation {
		t.Fatalf("content-length reject comparison policy = %q", reject.ComparisonPolicy)
	}

	// APISIX 3.17 client-control.t TEST 3 creates the same limit and TEST 4
	// proves an exact five-byte body reaches the upstream once.
	allow, ok := byName[differentialClientControlContentLengthExactLimitCase]
	if !ok {
		t.Fatalf("missing exact-limit case %q", differentialClientControlContentLengthExactLimitCase)
	}
	if allow.Request.Body != "12345" || allow.Fixture.ExpectedCalls != 1 || allow.SecurityDecision != "allow" {
		t.Fatalf("exact-limit allow semantics = %#v", allow)
	}
	if allow.ComparisonPolicy != "" {
		t.Fatalf("exact-limit allow comparison policy = %q, want exact", allow.ComparisonPolicy)
	}
	if allow.Fixture.Response.Status != http.StatusOK || allow.Fixture.Response.Body != "done" {
		t.Fatalf("exact-limit fixture response = %#v, want 200/done", allow.Fixture.Response)
	}
}
