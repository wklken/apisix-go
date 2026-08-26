package pluginintegration

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestLoadManifestRejectsUnknownField(t *testing.T) {
	_, err := loadManifest("test.yaml", []byte(validManifestYAML+"unknown: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("loadManifest() error = %v, want unknown field rejection", err)
	}
}

func TestManifestAcceptsTargetPluginExemptionField(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 2
cases:
  - name: exempted
    target_plugin_exempt_reason: intentional negative coverage case
    source:
      tests: [1]
    config:
      routes: []
    input:
      path: /exempted
    output:
      status: 404
  - name: variants
    source:
      tests: [2]
    variants:
      - name: exempted-variant
        target_plugin_exempt_reason: variant does not activate target
        config:
          routes: []
        input:
          path: /variant
        output:
          status: 404
`)

	manifest, err := loadManifest("target-plugin-exemption.yaml", data)
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
	if got := manifest.Cases[0].TargetPluginExemptReason; got != "intentional negative coverage case" {
		t.Fatalf("case target_plugin_exempt_reason = %q", got)
	}
	if got := manifest.Cases[1].Variants[0].TargetPluginExemptReason; got != "variant does not activate target" {
		t.Fatalf("variant target_plugin_exempt_reason = %q", got)
	}
}

func TestManifestRejectsParentTargetPluginExemptionWithVariants(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		wantReject bool
	}{
		{name: "nonblank reason", reason: "set on the variant instead", wantReject: true},
		{name: "whitespace is empty", reason: " \t\n ", wantReject: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest()
			caseSpec := &manifest.Cases[0]
			variant := CaseVariant{
				Name:   "variant",
				Config: caseSpec.Config,
				Input:  caseSpec.Input,
				Output: caseSpec.Output,
			}
			caseSpec.Config = nil
			caseSpec.Input = HTTPInput{}
			caseSpec.Output = HTTPOutput{}
			caseSpec.TargetPluginExemptReason = tt.reason
			caseSpec.Variants = []CaseVariant{variant}

			err := manifest.validate()
			if tt.wantReject {
				if err == nil || !strings.Contains(err.Error(), "target_plugin_exempt_reason") {
					t.Fatalf("validate() error = %v, want parent exemption rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() error = %v, want whitespace reason treated as empty", err)
			}
		})
	}
}

func TestManifestAcceptsCaseLevelSerialFlag(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 2
cases:
  - name: fixed-port
    serial: true
    source:
      tests: [1]
    config:
      routes: []
    input:
      path: /hello
    output:
      status: 200
  - name: ordinary
    source:
      tests: [2]
    config:
      routes: []
    input:
      path: /hello
    output:
      status: 200
`)

	manifest, err := loadManifest("serial.yaml", data)
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
	if !manifest.Cases[0].Serial {
		t.Fatal("serial = false, want true")
	}
	if manifest.Cases[1].Serial {
		t.Fatal("ordinary case serial = true, want false")
	}
}

func TestManifestRejectsSerialOnVariant(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: fixed-port
    source:
      tests: [1]
    variants:
      - name: child
        serial: true
        config:
          routes: []
        output:
          logs:
            matches: ready
`)

	_, err := loadManifest("serial-variant.yaml", data)
	if err == nil || !strings.Contains(err.Error(), "field serial not found") {
		t.Fatalf("loadManifest() error = %v, want variant serial field rejection", err)
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

func TestManifestRejectsConcurrentStepWithBodyCaptures(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:        "parallel-body-capture",
		Repeat:      2,
		Concurrency: 2,
		Input:       HTTPInput{Path: "/capture"},
		Output: HTTPOutput{
			Status: 200,
			BodyCaptures: map[string]BodyCapture{
				"id": {Matches: `^(.+)$`},
			},
		},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "concurrency must not be combined with output captures") {
		t.Fatalf("validate() error = %v, want concurrent body capture rejection", err)
	}
}

func TestManifestAcceptsConcurrentStatusCounts(t *testing.T) {
	data := []byte(`sources:
  - repository: https://github.com/apache/apisix
    commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
    file: t/plugin/example.t
    tests: 1
cases:
  - name: concurrent-status-counts
    source: {tests: [1]}
    config: {routes: []}
    fixtures:
      - name: origin
        kind: http
        respond: [{status: 200}]
    steps:
      - name: exact-mix
        repeat: 5
        concurrency: 5
        input: {path: /hello}
        output:
          status_counts: {200: 1, 503: 4}
`)

	if _, err := loadManifest("status-counts.yaml", data); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestRejectsStatusCountsOutsideConcurrentStep(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Output.Status = 0
	manifest.Cases[0].Output.StatusCounts = map[int]int{http.StatusOK: 1}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "status_counts requires a concurrent step") {
		t.Fatalf("validate() error = %v, want concurrent-step requirement", err)
	}
}

func TestManifestRejectsTopLevelStatusCountsWithSteps(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{
		StatusCounts: map[int]int{http.StatusOK: 1},
	}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "request",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: http.StatusOK},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "steps and fixtures must not be mixed") {
		t.Fatalf("validate() error = %v, want mixed top-level output rejection", err)
	}
}

func TestManifestAcceptsHeldUpstreamProbes(t *testing.T) {
	data := []byte(`sources:
  - repository: https://github.com/apache/apisix
    commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
    file: t/plugin/example.t
    tests: 1
cases:
  - name: held-upstream
    source: {tests: [1]}
    config: {routes: []}
    fixtures:
      - name: origin
        kind: http
        respond: [{status: 200}]
    steps:
      - name: hold-two
        repeat: 2
        concurrency: 2
        hold_upstream:
          fixture: origin
          requests: 2
          probes:
            - input: {path: /probe}
              output: {status: 503}
        input: {path: /hold}
        output: {status: 200}
`)

	if _, err := loadManifest("held-upstream.yaml", data); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestRejectsTopLevelBodyCaptureWithSteps(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{
		BodyCaptures: map[string]BodyCapture{"id": {Matches: `^(.+)$`}},
	}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "request",
		Input:  HTTPInput{Path: "/capture"},
		Output: HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "steps and fixtures must not be mixed") {
		t.Fatalf("validate() error = %v, want top-level body capture rejection", err)
	}
}

func TestConfigProbeRejectsBodyCaptures(t *testing.T) {
	err := validateConfigProbeOutput(HTTPOutput{
		Status:       200,
		BodyCaptures: map[string]BodyCapture{"id": {Matches: `^(.+)$`}},
	})
	if err == nil || !strings.Contains(err.Error(), "supports only") {
		t.Fatalf("validateConfigProbeOutput() error = %v, want body capture rejection", err)
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

func TestManifestAcceptsRFC5424JSONFields(t *testing.T) {
	const manifestYAML = `source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: syslog-rfc5424-json-fields
    source:
      tests: [1]
    config:
      routes: []
    fixtures:
      - name: sink
        kind: tcp
        network_expect:
          - rfc5424_json_fields:
              - path: /request/uri
                value:
                  equals: /hello
        network_respond:
          - payload: ''
    steps:
      - name: request
        input:
          path: /hello
        output:
          status: 200
`
	if _, err := loadManifest("syslog-rfc5424-json-fields.yaml", []byte(manifestYAML)); err != nil {
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

func TestKafkaFixtureConfigValidation(t *testing.T) {
	t.Run("accepts explicit topics metadata error and SASL", func(t *testing.T) {
		spec := FixtureSpec{
			Name: "kafka",
			Kind: "kafka",
			NetworkExpect: []NetworkAssertion{{
				Payload: &Matcher{Equals: new("record")},
			}},
			NetworkRespond: []NetworkResponse{{}},
			Kafka: &KafkaFixtureConfig{
				Topics:            []string{"integration"},
				MetadataErrorCode: 3,
				SASL: &KafkaSASLFixtureConfig{
					Mechanism: "SCRAM-SHA-256",
					Username:  "admin",
					Password:  "secret",
				},
			},
		}
		if err := spec.validate(); err != nil {
			t.Fatalf("validate Kafka fixture: %v", err)
		}
	})

	t.Run("rejects Kafka config on another fixture kind", func(t *testing.T) {
		spec := FixtureSpec{
			Name:  "tcp",
			Kind:  "tcp",
			Kafka: &KafkaFixtureConfig{Topics: []string{"integration"}},
			Count: &FixtureCountAssertion{AtMost: 0},
		}
		if err := spec.validate(); err == nil {
			t.Fatal("validate non-Kafka fixture with kafka config = nil, want error")
		}
	})

	t.Run("rejects unsupported SASL mechanism", func(t *testing.T) {
		spec := FixtureSpec{
			Name: "kafka",
			Kind: "kafka",
			NetworkExpect: []NetworkAssertion{{
				Payload: &Matcher{Equals: new("record")},
			}},
			NetworkRespond: []NetworkResponse{{}},
			Kafka: &KafkaFixtureConfig{SASL: &KafkaSASLFixtureConfig{
				Mechanism: "SCRAM-SHA-1",
				Username:  "admin",
				Password:  "secret",
			}},
		}
		if err := spec.validate(); err == nil {
			t.Fatal("validate Kafka fixture with unsupported SASL mechanism = nil, want error")
		}
	})

	t.Run("accepts authentication-only failure fixture", func(t *testing.T) {
		spec := FixtureSpec{
			Name: "kafka",
			Kind: "kafka",
			Kafka: &KafkaFixtureConfig{SASL: &KafkaSASLFixtureConfig{
				Mechanism: "PLAIN",
				Username:  "admin",
				Password:  "secret",
			}},
		}
		if err := spec.validate(); err != nil {
			t.Fatalf("validate authentication-only Kafka fixture: %v", err)
		}
	})
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

func TestManifestValidatesUnorderedHTTPFixtureExpectations(t *testing.T) {
	fixture := FixtureSpec{
		Name:            "sink",
		Kind:            "http",
		ExpectUnordered: true,
		Expect: []HTTPAssertion{
			{Method: http.MethodPost},
			{Method: http.MethodPost},
		},
		Respond: []HTTPResponse{{Status: http.StatusOK}},
	}
	if err := fixture.validate(); err != nil {
		t.Fatalf("validate unordered HTTP fixture: %v", err)
	}

	fixture.Count = &FixtureCountAssertion{}
	if err := fixture.validate(); err == nil || !strings.Contains(err.Error(), "expect_unordered") {
		t.Fatalf("validate() error = %v, want unordered/count rejection", err)
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

func TestManifestAcceptsWorkDirFileLifecycleActions(t *testing.T) {
	body := "after"
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name: "rotate-and-reopen",
		Actions: []CaseAction{
			{Rename: &FileRenameAction{
				From: "{{WORK_DIR}}/access.log",
				To:   "{{WORK_DIR}}/access.log.old",
			}},
			{Remove: "{{WORK_DIR}}/access.log.old"},
			{Signal: "SIGUSR1"},
			{Wait: 10 * time.Millisecond},
		},
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
		FileAssertions: []FileAssertion{{
			Path: &Matcher{Equals: new("{{WORK_DIR}}/access.log")},
			Body: &Matcher{Equals: &body},
		}},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsUnsafeFileLifecycleAction(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:    "unsafe-remove",
		Actions: []CaseAction{{Remove: "/tmp/outside.log"}},
		Input:   HTTPInput{Path: "/hello"},
		Output:  HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "must begin with {{WORK_DIR}}/") {
		t.Fatalf("validate() error = %v, want unsafe action path rejection", err)
	}
}

func TestManifestRejectsMixedFileLifecycleAction(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name: "mixed-action",
		Actions: []CaseAction{{
			Remove: "{{WORK_DIR}}/access.log",
			Signal: "SIGUSR1",
		}},
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("validate() error = %v, want mixed action rejection", err)
	}
}

func TestManifestRejectsUnsupportedChildSignal(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:    "unsupported-signal",
		Actions: []CaseAction{{Signal: "SIGTERM"}},
		Input:   HTTPInput{Path: "/hello"},
		Output:  HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "only SIGUSR1") {
		t.Fatalf("validate() error = %v, want unsupported signal rejection", err)
	}
}

func TestManifestAcceptsAbsentMidCaseFileAssertion(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "cached-unlinked-file",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
		FileAssertions: []FileAssertion{{
			Path:   &Matcher{Equals: new("{{WORK_DIR}}/access.log")},
			Absent: true,
		}},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestAcceptsJSONLinesFileAssertion(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].AfterShutdown = []FileAssertion{{
		Path: &Matcher{Equals: new("{{WORK_DIR}}/access.log")},
		JSONLines: &FileJSONLinesAssertion{
			Count: 2,
			Records: []FileJSONRecordAssertion{{
				Count:  2,
				Fields: map[string]string{"/route_id": "route-1"},
			}},
		},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestAcceptsTypedBboltJSONAssertionAfterShutdown(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].AfterShutdown = []FileAssertion{{
		Path: &Matcher{Equals: new("{{WORK_DIR}}/apisix-go-store.db")},
		BboltJSON: &FileBboltJSONAssertion{
			Bucket: "routes",
			Key:    "route-1",
			Fields: map[string]string{
				"/plugins/ai-rate-limiting/redis_password": "ciphertext",
			},
			ForbiddenMatches: []string{"plaintext"},
		},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsBboltJSONAssertionOutsideAfterShutdown(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "locked-database",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
		FileAssertions: []FileAssertion{{
			Path: &Matcher{Equals: new("{{WORK_DIR}}/apisix-go-store.db")},
			BboltJSON: &FileBboltJSONAssertion{
				Bucket: "routes",
				Key:    "route-1",
				Fields: map[string]string{"/id": "route-1"},
			},
		}},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "bbolt_json is only supported after shutdown") {
		t.Fatalf("validate() error = %v, want after-shutdown restriction", err)
	}
}

func TestManifestAcceptsTypedRedisAuthenticationWithUnassertedCommands(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name: "redis",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values:                  map[string]string{"quota": "1"},
			Auth:                    []RedisAuthAssertion{{Password: "somepassword"}},
		},
	}}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "request",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestAcceptsTLSRedisClusterFixture(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name: "redis",
		Kind: "redis-cluster",
		Redis: &RedisFixtureAssertion{
			TLS:                     true,
			AllowUnassertedCommands: true,
		},
	}}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "request",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestManifestRejectsTLSForPlainRedisFixture(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name: "redis",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			TLS:                     true,
			AllowUnassertedCommands: true,
		},
	}}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "request",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "does not support Redis TLS") {
		t.Fatalf("validate() error = %v, want Redis TLS kind error", err)
	}
}

func TestManifestRejectsInvalidRedisTTLRange(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name: "redis",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values:                  map[string]string{"quota": "1"},
			TTLSecondsBetween:       map[string]IntRange{"quota": {Min: 60, Max: 59}},
		},
	}}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "request",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "ttl_seconds_between") {
		t.Fatalf("validate() error = %v, want ttl_seconds_between error", err)
	}
}

func TestManifestAcceptsRedisValueKeyMatcher(t *testing.T) {
	for _, kind := range []string{"redis", "redis-cluster", "redis-sentinel"} {
		t.Run(kind, func(t *testing.T) {
			manifest := validManifest()
			manifest.Cases[0].Input = HTTPInput{}
			manifest.Cases[0].Output = HTTPOutput{}
			manifest.Cases[0].Fixtures = []FixtureSpec{{
				Name: "redis",
				Kind: kind,
				Redis: &RedisFixtureAssertion{
					AllowUnassertedCommands: true,
					ValueMatches:            map[string]string{`^quota:\d+$`: "2"},
				},
			}}
			manifest.Cases[0].Steps = []CaseStep{{
				Name:   "request",
				Input:  HTTPInput{Path: "/hello"},
				Output: HTTPOutput{Status: 200},
			}}

			if err := manifest.validate(); err != nil {
				t.Fatalf("validate() error = %v", err)
			}
		})
	}
}

func TestManifestRejectsInvalidRedisValueKeyMatcher(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name: "redis",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			ValueMatches:            map[string]string{"[": "2"},
		},
	}}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "request",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: 200},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "value_matches") {
		t.Fatalf("validate() error = %v, want value_matches error", err)
	}
}

func TestManifestAcceptsRedisHashAssertions(t *testing.T) {
	data := []byte(`sources:
  - repository: https://github.com/apache/apisix
    commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
    file: t/plugin/example.t
    tests: 1
cases:
  - name: redis-hash
    source: {tests: [1]}
    config: {routes: []}
    fixtures:
      - name: redis
        kind: redis
        redis:
          allow_unasserted_commands: true
          hashes:
            plugin-limit-req:route:one:client:
              excess: {equals: "1"}
              last: {matches: '^\d+$'}
    steps:
      - name: request
        input: {path: /hello}
        output: {status: 200}
`)

	if _, err := loadManifest("redis-hash.yaml", data); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestRejectsJSONLinesFileAssertionWithWrongRecordTotal(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].AfterShutdown = []FileAssertion{{
		Path: &Matcher{Equals: new("{{WORK_DIR}}/access.log")},
		JSONLines: &FileJSONLinesAssertion{
			Count: 2,
			Records: []FileJSONRecordAssertion{{
				Count:  1,
				Fields: map[string]string{"/route_id": "route-1"},
			}},
		},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), "record counts total 1, want 2") {
		t.Fatalf("validate() error = %v, want record count rejection", err)
	}
}

func TestManifestRejectsJSONLinesFileAssertionWithInvalidPointer(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].AfterShutdown = []FileAssertion{{
		Path: &Matcher{Equals: new("{{WORK_DIR}}/access.log")},
		JSONLines: &FileJSONLinesAssertion{
			Count: 1,
			Records: []FileJSONRecordAssertion{{
				Count:  1,
				Fields: map[string]string{"route_id": "route-1"},
			}},
		},
	}}

	err := manifest.validate()
	if err == nil || !strings.Contains(err.Error(), `path "route_id"`) {
		t.Fatalf("validate() error = %v, want JSON pointer rejection", err)
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

func TestManifestAcceptsZeroPacketUDPFixture(t *testing.T) {
	manifest := validManifest()
	manifest.Cases[0].Input = HTTPInput{}
	manifest.Cases[0].Output = HTTPOutput{}
	manifest.Cases[0].Steps = []CaseStep{{
		Name:   "request",
		Input:  HTTPInput{Path: "/hello"},
		Output: HTTPOutput{Status: http.StatusOK},
	}}
	manifest.Cases[0].Fixtures = []FixtureSpec{{
		Name:  "sink",
		Kind:  "udp",
		Count: &FixtureCountAssertion{AtLeast: 0, AtMost: 0},
	}}

	if err := manifest.validate(); err != nil {
		t.Fatalf("validate() error = %v, want exact-zero UDP fixture acceptance", err)
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

func TestManifestAcceptsCaseAndVariantEnvironmentUnset(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 2
cases:
  - name: case-environment-unset
    source:
      tests: [1]
    environment_unset:
      - SSL_CERT_FILE
    config:
      routes: []
    output:
      logs:
        matches: ready
  - name: variant-environment-unset
    source:
      tests: [2]
    variants:
      - name: child
        environment_unset:
          - VAULT_TOKEN
        config:
          routes: []
        output:
          logs:
            matches: ready
`)

	manifest, err := loadManifest("environment-unset.yaml", data)
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
	if got := manifest.Cases[0].EnvironmentUnset; !slices.Equal(got, []string{"SSL_CERT_FILE"}) {
		t.Fatalf("case environment_unset = %v, want [SSL_CERT_FILE]", got)
	}
	if got := manifest.Cases[1].Variants[0].caseSpec().EnvironmentUnset; !slices.Equal(got, []string{"VAULT_TOKEN"}) {
		t.Fatalf("variant environment_unset = %v, want [VAULT_TOKEN]", got)
	}
}

func TestManifestRejectsInvalidEnvironmentUnset(t *testing.T) {
	for _, test := range []struct {
		name             string
		environment      Environment
		environmentUnset []string
		want             string
	}{
		{
			name:             "invalid-name",
			environmentUnset: []string{"1INVALID"},
			want:             "nonempty POSIX-style name",
		},
		{
			name:             "duplicate",
			environmentUnset: []string{"SSL_CERT_FILE", "SSL_CERT_FILE"},
			want:             `environment_unset variable "SSL_CERT_FILE" is duplicated`,
		},
		{
			name:             "overlap",
			environment:      Environment{"SSL_CERT_FILE": "fixture.pem"},
			environmentUnset: []string{"SSL_CERT_FILE"},
			want:             `environment variable "SSL_CERT_FILE" must not be both set and unset`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := validManifest()
			manifest.Cases[0].Environment = test.environment
			manifest.Cases[0].EnvironmentUnset = test.environmentUnset

			err := manifest.validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validate() error = %v, want %q", err, test.want)
			}
		})
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

func TestManifestAcceptsExplicitEmptyGRPCMessage(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: empty-grpc-message
    source:
      tests: [1]
    config:
      routes: []
    fixtures:
      - name: grpc
        kind: h2c
        expect:
          - grpc:
              message_base64: ""
        respond:
          - grpc:
              message_base64: CgJvaw==
    steps:
      - name: probe
        input:
          path: /probe
        output:
          status: 200
`)

	if _, err := loadManifest("empty-grpc-message.yaml", data); err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
}

func TestManifestRejectsOmittedGRPCMessage(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: missing-grpc-message
    source:
      tests: [1]
    config:
      routes: []
    fixtures:
      - name: grpc
        kind: h2c
        expect:
          - grpc: {}
        respond:
          - grpc:
              message_base64: CgJvaw==
    steps:
      - name: probe
        input:
          path: /probe
        output:
          status: 200
`)

	if _, err := loadManifest("missing-grpc-message.yaml", data); err == nil {
		t.Fatal("loadManifest() error = nil, want omitted gRPC message rejection")
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

func TestManifestAcceptsPerInputGenerationWait(t *testing.T) {
	data := []byte(`source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/example.t
  tests: 1
cases:
  - name: startup
    source: {tests: [1]}
    config: {routes: []}
    input: {path: /hello, generation_timeout: 5s}
    output: {status: 200}
`)

	manifest, err := loadManifest("startup.yaml", data)
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}
	if got := manifest.Cases[0].Input.GenerationTimeout; got != 5*time.Second {
		t.Fatalf("generation timeout = %s, want 5s", got)
	}
}

func TestHTTPScenarioRejectsInvalidGenerationWait(t *testing.T) {
	for _, test := range []struct {
		name   string
		input  HTTPInput
		output HTTPOutput
		want   string
	}{
		{
			name: "negative timeout", input: HTTPInput{Path: "/hello", GenerationTimeout: -time.Second},
			output: HTTPOutput{Status: http.StatusOK}, want: "must not be negative",
		},
		{
			name: "HTTP/2", input: HTTPInput{Path: "/hello", Version: "2", GenerationTimeout: time.Second},
			output: HTTPOutput{Status: http.StatusOK}, want: "supports only HTTP/1.1",
		},
		{
			name: "ambiguous 503", input: HTTPInput{Path: "/hello", GenerationTimeout: time.Second},
			output: HTTPOutput{Status: http.StatusServiceUnavailable}, want: "requires a non-503 exact output status",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateHTTPScenario(test.input, test.output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateHTTPScenario() error = %v, want %q", err, test.want)
			}
		})
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

func TestManifestAcceptsHTTPSConnectFixture(t *testing.T) {
	connectAuthority := "api.openai.com:443"
	providerPath := "/v1/chat/completions"
	fixture := FixtureSpec{
		Name: "provider-proxy",
		Kind: "https-connect",
		Expect: []HTTPAssertion{
			{
				Method: http.MethodConnect,
				Host:   &Matcher{Equals: &connectAuthority},
			},
			{
				Method: http.MethodPost,
				Path:   &Matcher{Equals: &providerPath},
			},
		},
		Respond: []HTTPResponse{
			{Status: http.StatusOK},
			{Status: http.StatusUnauthorized, Body: "Unauthorized"},
		},
	}

	if err := fixture.validate(); err != nil {
		t.Fatalf("validate HTTPS CONNECT fixture: %v", err)
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
	otlpTraces := &OTLPTracesAssertion{}
	tests := []struct {
		name      string
		assertion HTTPAssertion
	}{
		{name: "body and Loki", assertion: HTTPAssertion{Body: body, LokiPush: loki}},
		{name: "body and SkyWalking", assertion: HTTPAssertion{Body: body, SkyWalkingLogs: skyWalking}},
		{name: "body and OTLP traces", assertion: HTTPAssertion{Body: body, OTLPTraces: otlpTraces}},
		{name: "Loki and SkyWalking", assertion: HTTPAssertion{LokiPush: loki, SkyWalkingLogs: skyWalking}},
		{
			name:      "SkyWalking and OTLP traces",
			assertion: HTTPAssertion{SkyWalkingLogs: skyWalking, OTLPTraces: otlpTraces},
		},
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

func TestOTLPTracesAssertionMatchesDecodedResourcesSpansAndAttributes(t *testing.T) {
	body := marshalOTLPFixture(t,
		[]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		[]byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20},
	)
	assertion := OTLPTracesAssertion{
		SpanCount:      2,
		UniqueTraceIDs: true,
		Spans: []OTLPSpanAssertion{
			{
				Name:    Matcher{Equals: new("GET /orders")},
				Scope:   &Matcher{Equals: new("github.com/riandyrn/otelchi")},
				Kind:    "server",
				TraceID: &Matcher{Equals: new("0102030405060708090a0b0c0d0e0f10")},
				SpanID:  &Matcher{Matches: new(`^[0-9a-f]{16}$`)},
				ResourceAttributes: map[string]OTLPAttributeAssertion{
					"service.name": {Type: "string", Matcher: Matcher{Equals: new("gateway")}},
				},
				Attributes: map[string]OTLPAttributeAssertion{
					"http.status_code": {Type: "int", Matcher: Matcher{Equals: new("200")}},
					"http.method":      {Type: "string", Matcher: Matcher{Equals: new("GET")}},
				},
			},
			{
				Name:    Matcher{Equals: new("GET /orders")},
				Scope:   &Matcher{Equals: new("github.com/riandyrn/otelchi")},
				Kind:    "server",
				TraceID: &Matcher{Equals: new("1112131415161718191a1b1c1d1e1f20")},
				SpanID:  &Matcher{Matches: new(`^[0-9a-f]{16}$`)},
				ResourceAttributes: map[string]OTLPAttributeAssertion{
					"service.name": {Type: "string", Matcher: Matcher{Equals: new("gateway")}},
				},
				Attributes: map[string]OTLPAttributeAssertion{
					"http.status_code": {Type: "int", Matcher: Matcher{Equals: new("200")}},
				},
			},
		},
	}

	if err := assertion.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	if err := assertion.match(body); err != nil {
		t.Fatalf("match() error = %v", err)
	}
}

func TestOTLPTracesAssertionRejectsMalformedPayloadTypeMismatchAndSharedTrace(t *testing.T) {
	body := marshalOTLPFixture(t,
		[]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		[]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
	)
	assertion := OTLPTracesAssertion{
		SpanCount:      2,
		UniqueTraceIDs: true,
		Spans: []OTLPSpanAssertion{{
			Name: Matcher{Equals: new("GET /orders")},
			Attributes: map[string]OTLPAttributeAssertion{
				"http.status_code": {Type: "string", Matcher: Matcher{Equals: new("200")}},
			},
		}},
	}

	if err := assertion.match("\x00not-protobuf"); err == nil || !strings.Contains(err.Error(), "decode OTLP") {
		t.Fatalf("malformed match error = %v, want decode failure", err)
	}
	if err := assertion.match(body); err == nil || !strings.Contains(err.Error(), "unique trace IDs") {
		t.Fatalf("shared trace match error = %v, want unique trace rejection", err)
	}
	assertion.UniqueTraceIDs = false
	if err := assertion.match(body); err == nil || !strings.Contains(err.Error(), `attribute "http.status_code" type`) {
		t.Fatalf("type mismatch error = %v, want attribute type rejection", err)
	}
}

func marshalOTLPFixture(t *testing.T, traceIDs ...[]byte) string {
	t.Helper()
	spans := make([]*tracepb.Span, 0, len(traceIDs))
	for i, traceID := range traceIDs {
		spans = append(spans, &tracepb.Span{
			TraceId: traceID,
			SpanId:  []byte{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, byte(0x28 + i)},
			Name:    "GET /orders",
			Kind:    tracepb.Span_SPAN_KIND_SERVER,
			Attributes: []*commonpb.KeyValue{
				{
					Key: "http.status_code",
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_IntValue{IntValue: 200},
					},
				},
				{
					Key: "http.method",
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValue{StringValue: "GET"},
					},
				},
			},
		})
	}
	payload, err := proto.Marshal(&collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key: "service.name",
				Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "gateway"},
				},
			}}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "github.com/riandyrn/otelchi"},
				Spans: spans,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal OTLP fixture: %v", err)
	}
	return string(payload)
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
