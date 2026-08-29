package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestDifferentialDataMaskCasesCapturePinnedAPISIX317AccessLogMasking(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialDataMaskCases()
	if len(cases) != 1 {
		t.Fatalf("differentialDataMaskCases() returned %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "data-mask-sanitizes-logged-request-line" || spec.Plugin != "data-mask" ||
		spec.RouteID != "differential-data-mask-request-line" ||
		spec.ComparisonPolicy != differentialDataMaskRequestLinePolicy ||
		spec.SecurityDecision != "" || len(spec.Steps) != 1 {
		t.Fatalf("case identity/policy/steps = %#v", spec)
	}
	step := spec.Steps[0]
	if step.Request.Method != http.MethodGet ||
		step.Request.Path != "/hello?password=secret&token=mytoken" ||
		step.Request.Host != "gateway.example.test" ||
		step.SecurityDecision != "not_applicable" {
		t.Fatalf("gateway step = %#v", step)
	}
	if spec.Fixture.Name != "origin-and-data-mask-log" || spec.Fixture.ExpectedCalls != 2 ||
		!spec.Fixture.CaptureAllCalls || spec.Fixture.CollectTimeoutMillis != 6000 ||
		len(spec.Fixture.SemanticHeaders) != 1 || spec.Fixture.SemanticHeaders[0] != "Content-Type" ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "done" {
		t.Fatalf("fixture contract = %#v", spec.Fixture)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	plugins := route["plugins"].(map[string]any)
	dataMask := plugins["data-mask"].(map[string]any)
	wantRules := []any{
		map[string]any{"type": "query", "name": "password", "action": "remove"},
		map[string]any{"type": "query", "name": "token", "action": "replace", "value": "*****"},
	}
	if got := dataMask["request"]; !reflect.DeepEqual(got, wantRules) {
		t.Fatalf("data-mask request rules = %#v, want %#v", got, wantRules)
	}
	httpLogger := plugins["http-logger"].(map[string]any)
	if httpLogger["uri"] != "http://"+differentialFixturePlaceholder+"/logs" ||
		httpLogger["batch_max_size"] != 1 || httpLogger["max_retry_count"] != 0 ||
		httpLogger["log_format"].(map[string]any)["request_line"] != "$request_line" {
		t.Fatalf("http-logger observer config = %#v", httpLogger)
	}

	config := string(mustYAML(t, spec.Config))
	for _, token := range []string{
		"data-mask", "http-logger", "password", "token", "remove", "replace",
		differentialFixturePlaceholder, "/logs", "$request_line",
	} {
		if !strings.Contains(config, token) {
			t.Fatalf("standalone config does not contain %q:\n%s", token, config)
		}
	}
}
