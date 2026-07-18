package pluginintegration

import (
	"strings"
	"testing"
	"time"
)

func TestLoadManifestRejectsUnknownField(t *testing.T) {
	_, err := loadManifest("test.yaml", []byte(validManifestYAML+"unknown: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("loadManifest() error = %v, want unknown field rejection", err)
	}
}

func TestManifestRejectsMissingSourceNumber(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Source.Tests = []int{1, 3}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "missing source test 2") {
		t.Fatalf("validate() error = %v, want missing source test 2", err)
	}
}

func TestManifestRejectsDuplicateSourceNumber(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Source.Tests = []int{1, 2, 2, 3}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "source test 2 is mapped more than once") {
		t.Fatalf("validate() error = %v, want duplicate source test 2", err)
	}
}

func TestManifestAcceptsCompleteSourceCoverage(t *testing.T) {
	manifest := validManifest()
	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsMixedEncodedBodyMatchers(t *testing.T) {
	body := "ok"
	manifest := validManifest()
	manifest.Cases[0].Output.BrotliBody = &Matcher{Equals: &body}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "body, gzip_body, and brotli_body are mutually exclusive") {
		t.Fatalf("validate() error = %v, want encoded body matcher rejection", err)
	}
}

func TestManifestRejectsNonPositiveBodyLengthLimit(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Output.BodyLengthLessThanValue = new(0)

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "body_length_less_than_value must be positive") {
		t.Fatalf("validate() error = %v, want non-positive body length limit rejection", err)
	}
}

func TestManifestRejectsInvalidElapsedRange(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Output.ElapsedAtLeast = time.Second
	manifest.Cases[0].Output.ElapsedLessThan = time.Second

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "elapsed_at_least must be less than elapsed_less_than") {
		t.Fatalf("validate() error = %v, want invalid elapsed range rejection", err)
	}
}

func TestManifestAcceptsHMACSignedInput(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input.HMAC = &HMACSignature{
		KeyID:     "access-key",
		Secret:    "secret-key",
		Algorithm: "hmac-sha256",
		Headers:   []string{"@request-target", "date"},
	}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsHMACInputWithAuthorizationHeader(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input.Headers = map[string]string{"Authorization": "static"}
	manifest.Cases[0].Input.HMAC = &HMACSignature{
		KeyID:   "access-key",
		Secret:  "secret-key",
		Headers: []string{"date"},
	}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "must not both be configured") {
		t.Fatalf("validate() error = %v, want HMAC/Authorization conflict", err)
	}
}

func TestManifestRejectsHMACInputWithAuthorizationHeaderValues(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input.HeaderValues = map[string][]string{"authorization": {"static"}}
	manifest.Cases[0].Input.HMAC = &HMACSignature{
		KeyID:   "access-key",
		Secret:  "secret-key",
		Headers: []string{"date"},
	}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "must not both be configured") {
		t.Fatalf("validate() error = %v, want HMAC/Authorization conflict", err)
	}
}

func TestManifestAcceptsTCPFixture(t *testing.T) {
	payload := "hello"
	response := "ok"
	manifest := validManifest()
	manifest.Cases[0].Config = map[string]any{"routes": []any{}}
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name: "sink",
		Kind: "tcp",
		NetworkExpect: []NetworkAssertion{{
			Payload: &Matcher{Equals: &payload},
		}},
		NetworkRespond: []NetworkResponse{{Payload: response}},
	}}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "send",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsMixedHTTPAndNetworkFixtureFields(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name:    "sink",
		Kind:    "tcp",
		Respond: []HTTPResponse{{Status: 200}},
	}}
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "send",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "tcp fixture must use network_expect/network_respond") {
		t.Fatalf("validate() error = %v, want mixed fixture rejection", err)
	}
}

func TestManifestRejectsUnsafeFileAssertion(t *testing.T) {
	body := "ok"
	path := "relative.txt"
	manifest := validManifest()
	manifest.Cases[0].AfterShutdown = []FileAssertion{{
		Path: &Matcher{Equals: &path},
		Body: &Matcher{Equals: &body},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "path must begin with {{WORK_DIR}}/") {
		t.Fatalf("validate() error = %v, want unsafe path rejection", err)
	}
}

func TestManifestRejectsUDPFixtureClose(t *testing.T) {
	payload := "hello"
	manifest := validManifest()
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name: "sink",
		Kind: "udp",
		NetworkExpect: []NetworkAssertion{{
			Payload: &Matcher{Equals: &payload},
		}},
		NetworkRespond: []NetworkResponse{{Payload: "ok", Close: true}},
	}}
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "send",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "UDP fixture cannot close") {
		t.Fatalf("validate() error = %v, want UDP close rejection", err)
	}
}

func TestManifestMultipleSources(t *testing.T) {
	body := "ok"
	manifest := &Manifest{
		Sources: []SourceSpec{
			{
				Repository: "https://github.com/apache/apisix",
				Commit:     "c3d7d5ec69774121f53d2e20d29d09c816795dd7",
				File:       "t/plugin/example.t",
				Tests:      1,
			},
			{
				Repository: "https://github.com/apache/apisix",
				Commit:     "c3d7d5ec69774121f53d2e20d29d09c816795dd7",
				File:       "t/plugin/example2.t",
				Tests:      1,
			},
		},
		Cases: []Case{
			{
				Name:   "first",
				Source: CaseSource{File: "t/plugin/example.t", Tests: []int{1}},
				Config: map[string]any{"routes": []any{}},
				Input:  HTTPInput{Path: "/first"},
				Output: HTTPOutput{Status: 200, Body: &Matcher{Equals: &body}},
			},
			{
				Name:   "second",
				Source: CaseSource{File: "t/plugin/example2.t", Tests: []int{1}},
				Config: map[string]any{"routes": []any{}},
				Input:  HTTPInput{Path: "/second"},
				Output: HTTPOutput{Status: 200, Body: &Matcher{Equals: &body}},
			},
		},
	}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsMissingSourceFile(t *testing.T) {
	manifest := validManifest()
	manifest.Sources = []SourceSpec{
		manifest.Source,
		{
			Repository: manifest.Source.Repository,
			Commit:     manifest.Source.Commit,
			File:       "t/plugin/example2.t",
			Tests:      1,
		},
	}
	manifest.Source = SourceSpec{}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "source file is required when multiple sources are configured") {
		t.Fatalf("validate() error = %v, want missing source file rejection", err)
	}
}

func TestManifestRejectsDuplicateSourceNumberAcrossCases(t *testing.T) {
	manifest := validManifest()
	manifest.Sources = []SourceSpec{manifest.Source}
	manifest.Source = SourceSpec{}
	manifest.Cases[0].Source.File = "t/plugin/example.t"
	manifest.Cases[0].Source.Tests = []int{1, 2, 3}
	duplicate := manifest.Cases[0]
	duplicate.Name = "duplicate"
	duplicate.Source.Tests = []int{2}
	manifest.Cases = append(manifest.Cases, duplicate)

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "source test 2 in t/plugin/example.t is mapped more than once") {
		t.Fatalf("validate() error = %v, want duplicate source test rejection", err)
	}
}

func TestManifestAcceptsMultipleStandaloneVariantsForOneSourceCase(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: invalid-values
    source:
      tests: [1]
    variants:
      - name: first
        config:
          routes: []
        output:
          logs:
            matches: first
      - name: second
        config:
          routes: []
        output:
          logs:
            matches: second
`)

	manifest, err := loadManifest("variants.yaml", data)
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
	if got := len(manifest.Cases[0].Variants); got != 2 {
		t.Fatalf("variants = %d, want 2", got)
	}
}

func TestManifestAcceptsStepsAndNamedFixtures(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: sequence
    source:
      tests: [1]
    config:
      routes: []
    fixtures:
      - name: primary
        kind: http
        respond:
          - status: 200
            body: ok
    steps:
      - name: first
        input:
          path: /hello
        output:
          status: 200
        wait: 200ms
`)

	manifest, err := loadManifest("steps.yaml", data)
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
	if got := manifest.Cases[0].Steps[0].Wait.String(); got != "200ms" {
		t.Fatalf("step wait = %s, want 200ms", got)
	}
}

func TestManifestAcceptsFixtureRequestBodyEcho(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: echo
    source:
      tests: [1]
    config:
      routes: []
    fixtures:
      - name: primary
        kind: http
        respond:
          - status: 200
            echo_request_body: true
    steps:
      - name: request
        input:
          path: /hello
        output:
          status: 200
`)

	if _, err := loadManifest("echo.yaml", data); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestAcceptsScenarioFilesAndStandaloneConfigUpdate(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: reload
    source:
      tests: [1]
    config:
      routes: []
    files:
      - path: fixtures/model.conf
        body: model
    steps:
      - name: update
        config:
          routes: []
        config_probe:
          input:
            path: /ready
          output:
            status: 204
        config_timeout: 2s
        input:
          path: /hello
        output:
          status: 200
`)

	manifest, err := loadManifest("reload.yaml", data)
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
	if got := manifest.Cases[0].Files[0].Path; got != "fixtures/model.conf" {
		t.Fatalf("file path = %q, want fixtures/model.conf", got)
	}
	if got := manifest.Cases[0].Steps[0].ConfigTimeout; got != 2*time.Second {
		t.Fatalf("config timeout = %s, want 2s", got)
	}
	if got := manifest.Cases[0].Steps[0].ConfigProbe.Input.Path; got != "/ready" {
		t.Fatalf("config probe path = %q, want /ready", got)
	}
}

func TestManifestRejectsScenarioFileOutsideWorkDirectory(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Files = []ScenarioFile{{Path: "../model.conf", Body: "model"}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "must stay within the scenario work directory") {
		t.Fatalf("validate() error = %v, want work-directory boundary rejection", err)
	}
}

func TestManifestRejectsConfigTimeoutWithoutConfigUpdate(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Steps = []CaseStep{{
		Name:          "request",
		ConfigTimeout: time.Second,
		Input:         HTTPInput{Path: "/hello"},
		Output:        HTTPOutput{Status: 200},
	}}
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "config_timeout requires config") {
		t.Fatalf("validate() error = %v, want config_timeout dependency rejection", err)
	}
}

func TestManifestRequiresReadinessProbeForConfigUpdate(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "update",
		Config: map[string]any{"routes": []any{}},
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "config_probe is required with config") {
		t.Fatalf("validate() error = %v, want readiness probe requirement", err)
	}
}

func TestManifestRejectsVariantsMixedWithTopLevelFiles(t *testing.T) {
	manifest := validManifest()
	original := manifest.Cases[0]
	manifest.Cases[0].Config = nil
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Files = []ScenarioFile{{Path: "model.conf", Body: "model"}}
	manifest.Cases[0].Variants = []CaseVariant{{
		Name:   "variant",
		Config: original.Config,
		Input:  original.Input,
		Output: original.Output,
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "case with variants must not declare an inline scenario") {
		t.Fatalf("validate() error = %v, want top-level files mixed-scenario rejection", err)
	}
}

func TestManifestAcceptsHTTP2InputWithFrontendTLS(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: http2
    source:
      tests: [1]
    config:
      routes: []
    frontend_tls:
      sni: example.test
    input:
      scheme: https
      version: "2"
      path: /hello
    output:
      status: 200
`)

	if _, err := loadManifest("http2.yaml", data); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestRejectsSkipField(t *testing.T) {
	data := strings.Replace(validManifestYAML, "    config:\n", "    skip: not executable\n    config:\n", 1)

	_, err := loadManifest("skip.yaml", []byte(data))
	if err == nil || !strings.Contains(err.Error(), "field skip not found") {
		t.Fatalf("loadManifest() error = %v, want skip field rejection", err)
	}
}

func TestManifestRequiresExecutableFields(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Config = nil

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "config is required") {
		t.Fatalf("validate() error = %v, want missing config rejection", err)
	}
}

func TestManifestAcceptsLogOnlyConfigRejection(t *testing.T) {
	pattern := "build route.*fail"
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{Logs: &Matcher{Matches: &pattern}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsMissingHTTPAndLogAssertions(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "HTTP output or log assertion is required") {
		t.Fatalf("validate() error = %v, want missing assertion rejection", err)
	}
}

func TestMatcherSupportsEqualsAndRegex(t *testing.T) {
	equalValue := "hello"
	equals := Matcher{Equals: &equalValue}
	if err := equals.validate(matcherBody); err != nil {
		t.Fatalf("equals.validate() error = %v", err)
	}
	if err := equals.match("hello", true); err != nil {
		t.Fatalf("equals.match() error = %v", err)
	}
	if err := equals.match("world", true); err == nil {
		t.Fatal("equals.match() error = nil, want mismatch")
	}

	pattern := `^request-[0-9]+$`
	matches := Matcher{Matches: &pattern}
	if err := matches.validate(matcherBody); err != nil {
		t.Fatalf("matches.validate() error = %v", err)
	}
	if err := matches.match("request-42", true); err != nil {
		t.Fatalf("matches.match() error = %v", err)
	}
}

func TestMatcherSupportsNegativeRegex(t *testing.T) {
	pattern := `"consumer"|"service"`
	matcher := Matcher{NotMatches: &pattern}
	if err := matcher.validate(matcherBody); err != nil {
		t.Fatalf("not_matches.validate() error = %v", err)
	}
	if err := matcher.match(`{"route":{"id":"1"}}`, true); err != nil {
		t.Fatalf("not_matches.match() error = %v", err)
	}
	if err := matcher.match(`{"consumer":{"username":"test"}}`, true); err == nil {
		t.Fatal("not_matches.match() error = nil, want forbidden match")
	}
}

func TestHeaderMatcherSupportsAbsent(t *testing.T) {
	absent := true
	matcher := Matcher{Absent: &absent}
	if err := matcher.validate(matcherHeader); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	if err := matcher.match("", false); err != nil {
		t.Fatalf("match() error = %v", err)
	}
	if err := matcher.match("", true); err == nil {
		t.Fatal("match() error = nil, want present-header mismatch")
	}
}

func TestHeaderMatcherSupportsOrderedValues(t *testing.T) {
	matcher := Matcher{Values: []string{"upstream", "Accept-Encoding"}}
	if err := matcher.validate(matcherHeader); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	if err := matcher.matchHeader("upstream", []string{"upstream", "Accept-Encoding"}); err != nil {
		t.Fatalf("matchHeader() error = %v", err)
	}
	if err := matcher.matchHeader("upstream", []string{"upstream"}); err == nil {
		t.Fatal("matchHeader() error = nil, want missing value mismatch")
	}
}

func TestMatcherRejectsAbsentForBody(t *testing.T) {
	absent := true
	err := (Matcher{Absent: &absent}).validate(matcherBody)
	if err == nil || !strings.Contains(err.Error(), "absent is only valid for headers") {
		t.Fatalf("validate() error = %v, want absent body rejection", err)
	}
}

func TestMatcherRejectsAmbiguousOperations(t *testing.T) {
	value := "hello"
	pattern := "hello"
	err := (Matcher{Equals: &value, Matches: &pattern}).validate(matcherBody)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("validate() error = %v, want ambiguous matcher rejection", err)
	}
}

func TestUpstreamAssertionValidatesHostMatcher(t *testing.T) {
	invalid := "["
	upstream := UpstreamSpec{Expect: HTTPAssertion{Host: &Matcher{Matches: &invalid}}}

	err := upstream.validate()
	if err == nil || !strings.Contains(err.Error(), "upstream request host") {
		t.Fatalf("validate() error = %v, want invalid host matcher rejection", err)
	}
}

func TestMergeRuntimeConfigPreservesNestedOverrides(t *testing.T) {
	dst := map[string]any{
		"apisix": map[string]any{
			"node_listen": []any{map[string]any{"ip": "127.0.0.1", "port": 9080}},
		},
	}
	src := map[string]any{
		"plugin_attr": map[string]any{
			"redirect": map[string]any{"https_port": 9443},
		},
		"apisix": map[string]any{
			"enable_admin": false,
		},
	}

	mergeMap(dst, src)

	apisix := dst["apisix"].(map[string]any)
	if _, ok := apisix["node_listen"]; !ok {
		t.Fatal("mergeMap() removed runner-owned node_listen")
	}
	if got := apisix["enable_admin"]; got != false {
		t.Fatalf("enable_admin = %v, want false", got)
	}
	pluginAttr := dst["plugin_attr"].(map[string]any)
	redirect := pluginAttr["redirect"].(map[string]any)
	if got := redirect["https_port"]; got != 9443 {
		t.Fatalf("https_port = %v, want 9443", got)
	}
}

func validManifest() *Manifest {
	body := "ok"
	return &Manifest{
		Source: SourceSpec{
			Repository: "https://github.com/apache/apisix",
			Commit:     "c3d7d5ec69774121f53d2e20d29d09c816795dd7",
			File:       "t/plugin/example.t",
			Tests:      3,
		},
		Cases: []Case{
			{
				Name:   "complete",
				Source: CaseSource{Tests: []int{1, 2, 3}},
				Config: map[string]any{"routes": []any{}},
				Input:  HTTPInput{Path: "/hello"},
				Output: HTTPOutput{Status: 200, Body: &Matcher{Equals: &body}},
			},
		},
	}
}

const validManifestYAML = `source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: complete
    source:
      tests: [1]
    config:
      routes: []
    input:
      path: /hello
    output:
      status: 200
`
