package pluginintegration

import (
	"net/http"
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

func TestManifestRejectsConcurrentStepWithResponseCaptures(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:        "parallel-capture",
		Repeat:      2,
		Concurrency: 2,
		Input:       HTTPInput{Path: "/capture"},
		Output: HTTPOutput{
			Status: 200,
			Captures: map[string]HeaderCapture{
				"id": {Header: "X-ID", Matches: `^(.+)$`},
			},
		},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "concurrency must not be combined with output captures") {
		t.Fatalf("validate() error = %v, want concurrent capture rejection", err)
	}
}

func TestManifestAcceptsNetworkJSONFields(t *testing.T) {
	const manifestYAML = `source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: udp-json-fields
    source:
      tests: [1]
    config:
      routes: []
    fixtures:
      - name: sink
        kind: udp
        network_expect:
          - json_fields:
              - path: /request/body
                value:
                  equals: '{"sample_payload":"hello"}'
        network_respond:
          - payload: ''
    steps:
      - name: request
        input:
          path: /hello
        output:
          status: 200
`
	if _, err := loadManifest("udp-json-fields.yaml", []byte(manifestYAML)); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestAcceptsNetworkJSONRFC3339(t *testing.T) {
	const manifestYAML = `source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: udp-json-rfc3339
    source:
      tests: [1]
    config:
      routes: []
    fixtures:
      - name: sink
        kind: udp
        network_expect:
          - json_fields:
              - path: /@timestamp
                rfc3339: true
        network_respond:
          - payload: ''
    steps:
      - name: request
        input:
          path: /hello
        output:
          status: 200
`
	if _, err := loadManifest("udp-json-rfc3339.yaml", []byte(manifestYAML)); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestNetworkJSONFieldRejectsMixedMatcherModes(t *testing.T) {
	value := "2026-07-18T12:30:00Z"
	err := (NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{{
		Path:    "/@timestamp",
		Value:   Matcher{Equals: &value},
		RFC3339: true,
	}}}).validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of value or rfc3339") {
		t.Fatalf("validate() error = %v, want mixed matcher rejection", err)
	}
}

func TestNetworkJSONFieldRejectsInvalidPointerEscape(t *testing.T) {
	value := "ok"
	err := (NetworkAssertion{JSONFields: []NetworkJSONFieldAssertion{{
		Path:  "/invalid~2key",
		Value: Matcher{Equals: &value},
	}}}).validate()
	if err == nil || !strings.Contains(err.Error(), "invalid JSON pointer escape") {
		t.Fatalf("validate() error = %v, want invalid pointer escape rejection", err)
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

func TestManifestAcceptsAllHMACAlgorithmsAndDateModes(t *testing.T) {
	for _, algorithm := range []string{"hmac-sha1", "hmac-sha256", "hmac-sha512"} {
		t.Run(algorithm, func(t *testing.T) {
			manifest := validManifest()
			manifest.Cases[0].Input.HMAC = &HMACSignature{
				KeyID:     "access-key",
				Secret:    "secret-key",
				Algorithm: algorithm,
				Headers:   []string{"date"},
				Date:      "Thu, 24 Sep 2020 06:39:52 GMT",
			}
			if err := manifest.validate(); err != nil {
				t.Fatalf("validate() error = %v", err)
			}
		})
	}

	manifest := validManifest()
	manifest.Cases[0].Input.HMAC = &HMACSignature{
		KeyID:      "access-key",
		Secret:     "secret-key",
		Headers:    []string{"date"},
		DateOffset: -time.Second,
	}
	if err := manifest.validate(); err != nil {
		t.Fatalf("relative date validate() error = %v", err)
	}
}

func TestManifestRejectsConflictingHMACDateModes(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input.HMAC = &HMACSignature{
		KeyID:      "access-key",
		Secret:     "secret-key",
		Headers:    []string{"date"},
		Date:       "Thu, 24 Sep 2020 06:39:52 GMT",
		DateOffset: -time.Second,
	}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "date and date_offset must not both be configured") {
		t.Fatalf("validate() error = %v, want conflicting date modes", err)
	}
}

func TestManifestAcceptsHMACWithoutSignedHeaders(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input.HMAC = &HMACSignature{
		KeyID:   "access-key",
		Secret:  "secret-key",
		Headers: []string{},
		Date:    "now",
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
			Payload:          &Matcher{Equals: &payload},
			ForbiddenMatches: []string{"forbidden"},
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

func TestManifestRejectsInvalidNetworkForbiddenMatch(t *testing.T) {
	payload := "hello"
	assertion := NetworkAssertion{
		Payload:          &Matcher{Equals: &payload},
		ForbiddenMatches: []string{"["},
	}

	err := assertion.validate()
	if err == nil || !strings.Contains(err.Error(), "forbidden match 1") {
		t.Fatalf("validate() error = %v, want invalid forbidden regex rejection", err)
	}
}

func TestManifestAcceptsExplicitZeroHTTPFixtureRequests(t *testing.T) {
	zero := 0
	manifest := validManifest()
	manifest.Cases[0].Config = map[string]any{"routes": []any{}}
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name:           "auth",
		Kind:           "http",
		ExpectRequests: &zero,
		Respond:        []HTTPResponse{{Status: http.StatusOK}},
	}}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "reject-before-auth",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: http.StatusRequestEntityTooLarge},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsHTTPFixtureRequestCountMismatch(t *testing.T) {
	zero := 0
	fixture := FixtureSpec{
		Name:           "auth",
		Kind:           "http",
		ExpectRequests: &zero,
		Expect:         []HTTPAssertion{{Method: http.MethodGet}},
		Respond:        []HTTPResponse{{Status: http.StatusOK}},
	}

	err := fixture.validate()
	if err == nil || !strings.Contains(err.Error(), "expect_requests must equal") {
		t.Fatalf("validate() error = %v, want request-count mismatch rejection", err)
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

func TestManifestAcceptsCaseAndVariantEnvironment(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 2
cases:
  - name: case-environment
    source:
      tests: [1]
    environment:
      CLICK_HOUSE_USER: fixture-user
    config:
      routes: []
    output:
      logs:
        matches: ready
  - name: variant-environment
    source:
      tests: [2]
    variants:
      - name: child
        environment:
          CLICK_HOUSE_USER: fixture-user
        config:
          routes: []
        output:
          logs:
            matches: ready
`)

	if _, err := loadManifest("environment.yaml", data); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestRejectsInvalidEnvironment(t *testing.T) {
	for _, test := range []struct {
		name        string
		environment string
	}{
		{name: "invalid-name", environment: "1INVALID: value"},
		{name: "non-string-value", environment: "VALID: 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := []byte("source:\n" +
				"  repository: https://github.com/apache/apisix\n" +
				"  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7\n" +
				"  file: t/plugin/example.t\n" +
				"  tests: 1\n" +
				"cases:\n" +
				"  - name: invalid-environment\n" +
				"    source:\n" +
				"      tests: [1]\n" +
				"    environment:\n" +
				"      " + test.environment + "\n" +
				"    config:\n" +
				"      routes: []\n" +
				"    output:\n" +
				"      logs:\n" +
				"        matches: ready\n")

			if _, err := loadManifest("invalid-environment.yaml", data); err == nil {
				t.Fatal("loadManifest() error = nil, want environment validation failure")
			}
		})
	}
}

func TestManifestRejectsCaseEnvironmentWithVariants(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: mixed-environment
    source:
      tests: [1]
    environment:
      CLICK_HOUSE_USER: fixture-user
    variants:
      - name: child
        config:
          routes: []
        output:
          logs:
            matches: ready
`)

	if _, err := loadManifest("mixed-environment.yaml", data); err == nil {
		t.Fatal("loadManifest() error = nil, want case environment and variants rejection")
	}
}

func TestManifestAcceptsHTTPFixtureResponseDelay(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: delayed-sink
    source:
      tests: [1]
    config:
      routes: []
    fixtures:
      - name: sink
        kind: http
        respond:
          - delay: 100ms
            status: 200
    steps:
      - name: request
        input:
          path: /probe
        output:
          status: 200
`)

	if _, err := loadManifest("delayed-sink.yaml", data); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestRejectsInvalidHTTPFixtureResponseDelay(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay string
	}{
		{name: "negative", delay: "-1s"},
		{name: "too-long", delay: "6s"},
		{name: "not-a-duration", delay: "not-a-duration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := []byte("source:\n" +
				"  repository: https://github.com/apache/apisix\n" +
				"  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7\n" +
				"  file: t/plugin/example.t\n" +
				"  tests: 1\n" +
				"cases:\n" +
				"  - name: invalid-delay\n" +
				"    source:\n" +
				"      tests: [1]\n" +
				"    config:\n" +
				"      routes: []\n" +
				"    fixtures:\n" +
				"      - name: sink\n" +
				"        kind: http\n" +
				"        respond:\n" +
				"          - delay: " + test.delay + "\n" +
				"            status: 200\n" +
				"    steps:\n" +
				"      - name: request\n" +
				"        input:\n" +
				"          path: /probe\n" +
				"        output:\n" +
				"          status: 200\n")

			if _, err := loadManifest("invalid-delay.yaml", data); err == nil {
				t.Fatal("loadManifest() error = nil, want fixture response delay rejection")
			}
		})
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

func TestManifestRejectsUnsupportedConfigProbeTransportOptions(t *testing.T) {
	tests := []struct {
		name   string
		input  HTTPInput
		setTLS bool
		want   string
	}{
		{
			name:  "explicit HTTP version",
			input: HTTPInput{Path: "/ready", Version: "1.1"},
			want:  "input version",
		},
		{
			name:   "HTTPS even with frontend TLS",
			input:  HTTPInput{Path: "/ready", Scheme: "https"},
			setTLS: true,
			want:   "input scheme",
		},
		{
			name:  "absolute HTTPS URL",
			input: HTTPInput{Path: "https://example.test/ready"},
			want:  "input path",
		},
		{
			name:  "cookie transport option",
			input: HTTPInput{Path: "/ready", WithoutCookies: true},
			want:  "without_cookies",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest()
			manifest.Cases[0].Steps = []CaseStep{{
				Name:   "update",
				Config: map[string]any{"routes": []any{}},
				ConfigProbe: &ConfigProbe{
					Input:  tt.input,
					Output: HTTPOutput{Status: 204},
				},
				Input:  HTTPInput{Path: "/hello"},
				Output: HTTPOutput{Status: 200},
			}}
			manifest.Cases[0].Input = HTTPInput{}
			manifest.Cases[0].Output = HTTPOutput{}
			if tt.setTLS {
				manifest.Cases[0].TLS = &FrontendTLS{SNI: "example.test"}
			}

			err := manifest.validate()
			if err == nil || !strings.Contains(err.Error(), `step "update" config_probe`) ||
				!strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validate() error = %v, want source-identifying %q rejection", err, tt.want)
			}
		})
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

func TestSkyWalkingLogsAssertionMatchesDecodedEnvelopeAndPayload(t *testing.T) {
	assertion := SkyWalkingLogsAssertion{Entries: []SkyWalkingLogAssertion{{
		Service:         Matcher{Equals: new("APISIX")},
		ServiceInstance: Matcher{Matches: new(`^[^$].+$`)},
		Endpoint:        Matcher{Equals: new("/opentracing")},
		TraceContext: &SkyWalkingTraceContextAssertion{
			TraceID:        "trace-id",
			TraceSegmentID: "segment-id",
			SpanID:         1,
		},
		Payload: map[string]Matcher{
			"route_id":     {Equals: new("route-1")},
			"request.body": {Equals: new(`{"sample":"hello"}`)},
		},
		PayloadAbsent: []string{"response.body"},
	}}}
	body := `[{
		"traceContext":{"traceId":"trace-id","traceSegmentId":"segment-id","spanId":1},
		"body":{"json":{"json":"{\"route_id\":\"route-1\",\"request\":{\"body\":\"{\\\"sample\\\":\\\"hello\\\"}\"}}"}},
		"service":"APISIX","serviceInstance":"host-a","endpoint":"/opentracing"
	}]`

	if err := assertion.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	if err := assertion.match(body); err != nil {
		t.Fatalf("match() error = %v", err)
	}
}

func TestSkyWalkingLogsAssertionRejectsTraceAndPayloadMismatch(t *testing.T) {
	assertion := SkyWalkingLogsAssertion{Entries: []SkyWalkingLogAssertion{{
		Service:            Matcher{Equals: new("APISIX")},
		ServiceInstance:    Matcher{Equals: new("host-a")},
		Endpoint:           Matcher{Equals: new("/opentracing")},
		TraceContextAbsent: true,
		Payload:            map[string]Matcher{"route_id": {Equals: new("route-1")}},
	}}}
	body := `[{
		"traceContext":{"traceId":"unexpected","traceSegmentId":"segment-id","spanId":1},
		"body":{"json":{"json":"{\"route_id\":\"route-2\"}"}},
		"service":"APISIX","serviceInstance":"host-a","endpoint":"/opentracing"
	}]`

	if err := assertion.match(body); err == nil || !strings.Contains(err.Error(), "want absent") {
		t.Fatalf("match() error = %v, want trace-context mismatch", err)
	}
	assertion.Entries[0].TraceContextAbsent = false
	if err := assertion.match(body); err == nil || !strings.Contains(err.Error(), `payload "route_id"`) {
		t.Fatalf("match() error = %v, want nested payload mismatch", err)
	}
}

func TestSkyWalkingLogsAssertionRequiresSemanticExpectations(t *testing.T) {
	assertion := SkyWalkingLogsAssertion{Entries: []SkyWalkingLogAssertion{{}}}
	if err := assertion.validate(); err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatalf("validate() error = %v, want required semantic matcher", err)
	}
}

func TestHTTPAssertionAllowsOnlyOneTypedBodyAssertion(t *testing.T) {
	body := &Matcher{Equals: new("payload")}
	loki := &LokiPushAssertion{}
	skyWalking := &SkyWalkingLogsAssertion{}
	tests := []struct {
		name      string
		assertion HTTPAssertion
	}{
		{name: "body and Loki", assertion: HTTPAssertion{Body: body, LokiPush: loki}},
		{name: "body and SkyWalking", assertion: HTTPAssertion{Body: body, SkyWalkingLogs: skyWalking}},
		{name: "Loki and SkyWalking", assertion: HTTPAssertion{LokiPush: loki, SkyWalkingLogs: skyWalking}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.assertion.validate()
			if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
				t.Fatalf("validate() error = %v, want typed body assertion conflict", err)
			}
		})
	}
}

func TestMatcherSupportsSemanticJSON(t *testing.T) {
	manifest, err := loadManifest("json.yaml", []byte(`
sources:
  - repository: https://github.com/apache/apisix
    commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
    file: t/plugin/example.t
    tests: 1
cases:
  - name: json-body
    source: {file: t/plugin/example.t, tests: [1]}
    config: {routes: []}
    fixtures:
      - name: primary
        kind: http
        expect:
          - body:
              json_equals: '{"messages":[{"role":"user","content":"hello"}],"model":"gpt-4"}'
        respond: [{status: 200}]
    steps:
      - name: request
        input: {path: /}
        output: {status: 200}
`))
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}

	if err := manifest.Cases[0].Fixtures[0].Expect[0].Body.match(
		`{"model":"gpt-4","messages":[{"content":"hello","role":"user"}]}`,
		true,
	); err != nil {
		t.Fatalf("semantic JSON matcher error = %v", err)
	}
}

func TestSemanticJSONMatcherPreservesArrayOrder(t *testing.T) {
	expected := `{"items":[1,2,3]}`
	matcher := Matcher{JSONEquals: &expected}

	if err := matcher.match(`{"items":[1,3,2]}`, true); err == nil {
		t.Fatal("match() error = nil, want array order mismatch")
	}
}

func TestSemanticJSONMatcherComparesNumbersExactly(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		actual   string
		wantErr  bool
	}{
		{name: "integer and decimal", expected: `{"value":1}`, actual: `{"value":1.0}`},
		{name: "integer and exponent", expected: `{"value":1}`, actual: `{"value":1e0}`},
		{
			name:     "large equivalent exponent",
			expected: `{"value":9007199254740993}`,
			actual:   `{"value":9.007199254740993e15}`,
		},
		{
			name:     "adjacent large integers",
			expected: `{"value":9007199254740992}`,
			actual:   `{"value":9007199254740993}`,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matcher := Matcher{JSONEquals: &tt.expected}
			err := matcher.match(tt.actual, true)
			if tt.wantErr && err == nil {
				t.Fatal("match() error = nil, want numeric mismatch")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("match() error = %v", err)
			}
		})
	}
}

func TestSemanticJSONMatcherRejectsMalformedJSON(t *testing.T) {
	t.Run("expected during validation", func(t *testing.T) {
		expected := `{"value":`
		manifest := validManifest()
		manifest.Cases[0].Output.Body = &Matcher{JSONEquals: &expected}
		err := manifest.validate()
		if err == nil || !strings.Contains(err.Error(), "output body: invalid json_equals") {
			t.Fatalf("validate() error = %v, want invalid expected JSON", err)
		}
	})

	t.Run("actual during matching", func(t *testing.T) {
		expected := `{"value":1}`
		err := (Matcher{JSONEquals: &expected}).match(`{"value":`, true)
		if err == nil || !strings.Contains(err.Error(), "decode actual JSON") {
			t.Fatalf("match() error = %v, want malformed actual JSON diagnostic", err)
		}
	})
}

func TestSemanticJSONMatcherRejectsOtherOperations(t *testing.T) {
	jsonValue := `{"value":1}`
	textValue := "value"
	tests := []struct {
		name    string
		matcher Matcher
	}{
		{name: "equals", matcher: Matcher{JSONEquals: &jsonValue, Equals: &textValue}},
		{name: "matches", matcher: Matcher{JSONEquals: &jsonValue, Matches: &textValue}},
		{name: "not_matches", matcher: Matcher{JSONEquals: &jsonValue, NotMatches: &textValue}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.matcher.validate(matcherBody)
			if err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("validate() error = %v, want matcher exclusivity error", err)
			}
		})
	}
}

func TestSemanticJSONMatcherRejectsNonBodyFields(t *testing.T) {
	jsonValue := `{"value":1}`
	matcher := &Matcher{JSONEquals: &jsonValue}
	filePath := "{{WORK_DIR}}/output.log"
	fileBody := "ok"
	tests := []struct {
		name     string
		want     string
		validate func() error
	}{
		{
			name: "path", want: "upstream request path",
			validate: func() error { return (HTTPAssertion{Path: matcher}).validate() },
		},
		{
			name: "host", want: "upstream request host",
			validate: func() error { return (HTTPAssertion{Host: matcher}).validate() },
		},
		{
			name: "header", want: `upstream request header "X-Test"`,
			validate: func() error {
				return (HTTPAssertion{Headers: map[string]Matcher{"X-Test": *matcher}}).validate()
			},
		},
		{
			name: "logs", want: "output logs",
			validate: func() error {
				return validateHTTPScenario(HTTPInput{Path: "/"}, HTTPOutput{Status: 200, Logs: matcher})
			},
		},
		{
			name: "network payload", want: "network payload",
			validate: func() error {
				return (NetworkAssertion{Payload: matcher}).validate()
			},
		},
		{
			name: "after shutdown path", want: "after_shutdown assertion 1 path",
			validate: func() error {
				return validateAfterShutdown([]FileAssertion{{
					Path: &Matcher{Equals: &filePath, JSONEquals: &jsonValue},
					Body: &Matcher{Equals: &fileBody},
				}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "json_equals") {
				t.Fatalf("validate() error = %v, want contextual json_equals rejection containing %q", err, tt.want)
			}
		})
	}
}

func TestLokiPushAssertionMatchesExactStreamsAndNestedEntries(t *testing.T) {
	assertion := LokiPushAssertion{Streams: []LokiStreamAssertion{
		{
			Stream: map[string]string{"service": "svc-alpha"},
			Values: []LokiValueAssertion{{
				Entry: map[string]any{
					"request":  map[string]any{"headers": map[string]any{"x-service-name": "svc-alpha"}},
					"route_id": "route-1",
				},
			}},
		},
		{
			Stream: map[string]string{"service": ""},
			Values: []LokiValueAssertion{{
				Entry:  map[string]any{"route_id": "route-1"},
				Absent: []string{"request.headers.x-service-name"},
			}},
		},
	}}
	body := `{"streams":[` +
		`{"stream":{"service":"svc-alpha"},"values":[["123","{\"request\":{\"headers\":{\"x-service-name\":\"svc-alpha\"}},\"route_id\":\"route-1\",\"latency\":1}"]]},` +
		`{"stream":{"service":""},"values":[["124","{\"request\":{\"headers\":{}},\"route_id\":\"route-1\"}"]]}` +
		`]}`

	if err := assertion.match(body); err != nil {
		t.Fatalf("match() error = %v", err)
	}
	extraBody := strings.TrimSuffix(body, `]}`) + `,{"stream":{},"values":[["125","{}"]]}]}`
	if err := assertion.match(extraBody); err == nil {
		t.Fatal("match() accepted an extra stream")
	}
	wrongEmptyLabel := strings.Replace(body, `"service":""`, `"other":""`, 1)
	if err := assertion.match(wrongEmptyLabel); err == nil {
		t.Fatal("match() accepted a different empty-valued label")
	}
	withLeakedHeader := strings.Replace(
		body,
		`\"headers\":{}`,
		`\"headers\":{\"x-service-name\":\"svc-alpha\"}`,
		1,
	)
	if err := assertion.match(withLeakedHeader); err == nil {
		t.Fatal("match() accepted a forbidden header path")
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
