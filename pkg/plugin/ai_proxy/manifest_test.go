package ai_proxy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	protocolConversionSource  = "t/plugin/ai-proxy-protocol-conversion.t"
	requestBodyOverrideSource = "t/plugin/ai-proxy-request-body-override.t"
)

type requestBodyManifestFixture struct {
	ExpectRequests *int             `yaml:"expect_requests"`
	Expect         []map[string]any `yaml:"expect"`
	Respond        []map[string]any `yaml:"respond"`
}

type requestBodyManifestStep struct {
	Name   string         `yaml:"name"`
	Input  map[string]any `yaml:"input"`
	Output map[string]any `yaml:"output"`
}

type requestBodyManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Config   map[string]any               `yaml:"config"`
	Fixtures []requestBodyManifestFixture `yaml:"fixtures"`
	Steps    []requestBodyManifestStep    `yaml:"steps"`
}

func TestRequestBodyOverrideManifestMapsEveryPinnedBlockToIndependentBehavior(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "ai-proxy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest struct {
		Sources []struct {
			Commit      string `yaml:"commit"`
			File        string `yaml:"file"`
			Tests       int    `yaml:"tests"`
			TestNumbers []int  `yaml:"test_numbers"`
		} `yaml:"sources"`
		Cases []requestBodyManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	foundSource := false
	for _, source := range manifest.Sources {
		if source.File != requestBodyOverrideSource {
			continue
		}
		foundSource = true
		if source.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" ||
			source.Tests != 17 ||
			len(source.TestNumbers) != 17 ||
			source.TestNumbers[0] != 3 ||
			source.TestNumbers[16] != 19 {
			t.Fatalf(
				"pinned source = (%q, %d, %v), want APISIX 3.17 runtime tests 3..19",
				source.Commit,
				source.Tests,
				source.TestNumbers,
			)
		}
	}
	if !foundSource {
		t.Fatalf("sources omit %s", requestBodyOverrideSource)
	}

	var cases []requestBodyManifestCase
	for _, testCase := range manifest.Cases {
		if testCase.Source.File != requestBodyOverrideSource {
			continue
		}
		if len(testCase.Fixtures) != 1 {
			t.Fatalf(
				"case %q fixtures = %d, want exactly one asserted provider fixture",
				testCase.Name,
				len(testCase.Fixtures),
			)
		}
		cases = append(cases, testCase)
	}
	if len(cases) != 17 {
		t.Fatalf("%s cases = %d, want exactly 17 independent runtime cases", requestBodyOverrideSource, len(cases))
	}

	names := make(map[string]struct{}, len(cases))
	for i, testCase := range cases {
		testNumber := i + 3
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != testNumber {
			t.Fatalf(
				"case %d %q source tests = %v, want [%d]",
				testNumber,
				testCase.Name,
				testCase.Source.Tests,
				testNumber,
			)
		}
		name := strings.ToLower(strings.TrimSpace(testCase.Name))
		for _, forbidden := range []string{"source-", "block-", "placeholder", "generic", "probe", "skip"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("case %d has non-behavioral name %q", testNumber, testCase.Name)
			}
		}
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("case %d repeats name %q", testNumber, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if !containsAIProxyRoute(testCase.Config) {
			t.Errorf("case %d %q has no ai-proxy or ai-proxy-multi route config", testNumber, testCase.Name)
		}
		fixture := testCase.Fixtures[0]
		if len(fixture.Respond) == 0 {
			t.Errorf("case %d %q provider fixture has no response", testNumber, testCase.Name)
		}
		if len(fixture.Expect) == 0 || fixture.Expect[0]["body"] == nil {
			t.Errorf("case %d %q provider fixture lacks a body assertion", testNumber, testCase.Name)
		}
		if len(testCase.Steps) != 1 ||
			testCase.Steps[0].Name == "" ||
			testCase.Steps[0].Input["path"] == nil ||
			testCase.Steps[0].Output["status"] == nil ||
			(testCase.Steps[0].Output["body"] == nil && testCase.Steps[0].Output["chunks"] == nil) {
			t.Errorf(
				"case %d %q lacks one behavior-specific real-process request/response assertion",
				testNumber,
				testCase.Name,
			)
		}
	}
}

func TestProtocolConversionManifestMapsEveryPinnedBlockToIndependentBehavior(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "ai-proxy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest struct {
		Sources []struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"sources"`
		Cases []requestBodyManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	foundSource := false
	for _, source := range manifest.Sources {
		if source.File != protocolConversionSource {
			continue
		}
		foundSource = true
		if source.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" || source.Tests != 28 {
			t.Fatalf("pinned source = (%q, %d), want APISIX 3.17 target with 28 tests", source.Commit, source.Tests)
		}
	}
	if !foundSource {
		t.Fatalf("sources omit %s", protocolConversionSource)
	}

	var cases []requestBodyManifestCase
	for _, testCase := range manifest.Cases {
		if testCase.Source.File != protocolConversionSource {
			continue
		}
		if len(testCase.Fixtures) != 1 {
			t.Fatalf(
				"case %q fixtures = %d, want exactly one asserted provider fixture",
				testCase.Name,
				len(testCase.Fixtures),
			)
		}
		cases = append(cases, testCase)
	}
	if len(cases) != 28 {
		t.Fatalf("%s cases = %d, want exactly 28 independent cases", protocolConversionSource, len(cases))
	}

	names := make(map[string]struct{}, len(cases))
	for i, testCase := range cases {
		testNumber := i + 1
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != testNumber {
			t.Fatalf(
				"case %d %q source tests = %v, want [%d]",
				testNumber,
				testCase.Name,
				testCase.Source.Tests,
				testNumber,
			)
		}
		name := strings.ToLower(strings.TrimSpace(testCase.Name))
		for _, forbidden := range []string{"source-", "block-", "placeholder", "generic", "probe", "skip"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("case %d has non-behavioral name %q", testNumber, testCase.Name)
			}
		}
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("case %d repeats name %q", testNumber, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if !containsAIProxyRoute(testCase.Config) {
			t.Errorf("case %d %q has no ai-proxy route config", testNumber, testCase.Name)
		}
		fixture := testCase.Fixtures[0]
		if len(fixture.Respond) == 0 {
			t.Errorf("case %d %q provider fixture has no response", testNumber, testCase.Name)
		}
		invalidClientRequest := testNumber >= 3 && testNumber <= 6
		if invalidClientRequest {
			if fixture.ExpectRequests == nil || *fixture.ExpectRequests != 0 {
				t.Errorf("case %d %q must assert invalid input makes zero provider requests", testNumber, testCase.Name)
			}
		} else if len(fixture.Expect) == 0 || fixture.Expect[0]["body"] == nil {
			t.Errorf("case %d %q provider fixture lacks a converted body assertion", testNumber, testCase.Name)
		}
		if len(testCase.Steps) != 1 ||
			testCase.Steps[0].Name == "" ||
			testCase.Steps[0].Input["path"] == nil ||
			testCase.Steps[0].Output["status"] == nil ||
			(testCase.Steps[0].Output["body"] == nil && testCase.Steps[0].Output["chunks"] == nil) {
			t.Errorf(
				"case %d %q lacks one behavior-specific real-process request/response assertion",
				testNumber,
				testCase.Name,
			)
		}
	}
}

func TestBuiltinVariablesManifestPreservesPinnedStreamingToolSemantics(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "ai-proxy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest struct {
		Cases []struct {
			Name   string `yaml:"name"`
			Source struct {
				File  string `yaml:"file"`
				Tests []int  `yaml:"tests"`
			} `yaml:"source"`
			Fixtures []struct {
				Respond []struct {
					Chunks []string `yaml:"chunks"`
				} `yaml:"respond"`
			} `yaml:"fixtures"`
			Steps []struct {
				Output struct {
					Headers map[string]struct {
						Equals *string `yaml:"equals"`
					} `yaml:"headers"`
				} `yaml:"output"`
			} `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	expectations := map[int]string{
		13: "llm-vars-streaming-tool-calls",
		16: "llm-vars-responses-streaming-tool-call",
	}

	for number, wantName := range expectations {
		var fixtureChunks []string
		var headers map[string]struct {
			Equals *string `yaml:"equals"`
		}
		found := false
		for i := range manifest.Cases {
			testCase := &manifest.Cases[i]
			if testCase.Source.File != "t/plugin/ai-proxy3.t" {
				continue
			}
			containsNumber := false
			for _, sourceNumber := range testCase.Source.Tests {
				if sourceNumber == number {
					containsNumber = true
				}
			}
			if !containsNumber {
				continue
			}
			found = true
			if testCase.Name != wantName {
				t.Errorf("case for test %d has name %q, want %q", number, testCase.Name, wantName)
			}
			for _, fixture := range testCase.Fixtures {
				for _, respond := range fixture.Respond {
					fixtureChunks = append(fixtureChunks, respond.Chunks...)
				}
			}
			if len(testCase.Steps) != 1 {
				t.Errorf("case %q steps = %d, want 1", testCase.Name, len(testCase.Steps))
				continue
			}
			headers = testCase.Steps[0].Output.Headers
		}
		if !found {
			t.Fatalf("%s: no case maps ai-proxy3.t test %d", path, number)
		}
		if len(fixtureChunks) == 0 {
			t.Errorf("case %q has no streaming fixture chunks", wantName)
		}
		hasToolCalls := headers["X-LLM-Has-Tool-Calls"]
		toolCount := headers["X-LLM-Tool-Count"]
		if hasToolCalls.Equals == nil || *hasToolCalls.Equals != "true" {
			t.Errorf("case %q X-LLM-Has-Tool-Calls equals = %v, want true", wantName, hasToolCalls.Equals)
		}
		if toolCount.Equals == nil || *toolCount.Equals != "0" {
			t.Errorf("case %q X-LLM-Tool-Count equals = %v, want pinned streaming count 0", wantName, toolCount.Equals)
		}
	}
}

func TestFixtureFamilyManifestPreservesPinnedErrorBehavior(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "ai-proxy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest struct {
		Cases []struct {
			Name   string `yaml:"name"`
			Source struct {
				File  string `yaml:"file"`
				Tests []int  `yaml:"tests"`
			} `yaml:"source"`
			Fixtures []struct {
				Respond []struct {
					Status int    `yaml:"status"`
					Body   string `yaml:"body"`
				} `yaml:"respond"`
			} `yaml:"fixtures"`
			Steps []struct {
				Output struct {
					Status int `yaml:"status"`
					Body   struct {
						Equals  *string `yaml:"equals"`
						Matches *string `yaml:"matches"`
					} `yaml:"body"`
				} `yaml:"output"`
			} `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	expectations := map[int]struct {
		name        string
		fixtureCode int
		fixtureBody string
		outputCode  int
		outputBody  string
	}{
		4: {
			name:        "fixture-missing-fixture-header-falls-back-to-auth",
			fixtureCode: http.StatusUnauthorized, fixtureBody: "Unauthorized",
			outputCode: http.StatusUnauthorized, outputBody: "Unauthorized",
		},
		5: {
			name:        "fixture-nonexistent-fixture-fails-closed",
			fixtureCode: http.StatusInternalServerError, fixtureBody: "fixture not found",
			outputCode: http.StatusInternalServerError, outputBody: "fixture not found",
		},
		// ai-proxy-fixture.t TEST 12 (path traversal) is blocked_runtime in
		// This source block only exercises the Apache APISIX Lua fixture
		// helper and has no production ai-proxy request surface.
	}

	for number, want := range expectations {
		var found *struct {
			Name   string `yaml:"name"`
			Source struct {
				File  string `yaml:"file"`
				Tests []int  `yaml:"tests"`
			} `yaml:"source"`
			Fixtures []struct {
				Respond []struct {
					Status int    `yaml:"status"`
					Body   string `yaml:"body"`
				} `yaml:"respond"`
			} `yaml:"fixtures"`
			Steps []struct {
				Output struct {
					Status int `yaml:"status"`
					Body   struct {
						Equals  *string `yaml:"equals"`
						Matches *string `yaml:"matches"`
					} `yaml:"body"`
				} `yaml:"output"`
			} `yaml:"steps"`
		}
		for i := range manifest.Cases {
			testCase := &manifest.Cases[i]
			if testCase.Source.File != "t/plugin/ai-proxy-fixture.t" {
				continue
			}
			containsNumber := false
			for _, sourceNumber := range testCase.Source.Tests {
				if sourceNumber == number {
					containsNumber = true
				}
			}
			if !containsNumber {
				continue
			}
			found = testCase
			break
		}
		if found == nil {
			t.Fatalf("%s: no case maps ai-proxy-fixture.t test %d", path, number)
		}
		if found.Name != want.name {
			t.Errorf("case for test %d has name %q, want %q", number, found.Name, want.name)
		}
		var fixtureCode int
		var fixtureBody string
		for _, fixture := range found.Fixtures {
			if len(fixture.Respond) == 0 {
				continue
			}
			response := fixture.Respond[0]
			fixtureCode = response.Status
			fixtureBody = response.Body
		}
		if fixtureCode != want.fixtureCode || !strings.Contains(fixtureBody, want.fixtureBody) {
			t.Errorf(
				"case %q fixture responds (%d, %q), want (%d, %q)",
				found.Name,
				fixtureCode,
				fixtureBody,
				want.fixtureCode,
				want.fixtureBody,
			)
		}
		if len(found.Steps) != 1 {
			t.Errorf("case %q steps = %d, want 1", found.Name, len(found.Steps))
			continue
		}
		output := found.Steps[0].Output
		if output.Status != want.outputCode {
			t.Errorf("case %q output status = %d, want %d", found.Name, output.Status, want.outputCode)
		}
		outputBody := ""
		if output.Body.Equals != nil {
			outputBody = *output.Body.Equals
		} else if output.Body.Matches != nil {
			outputBody = *output.Body.Matches
		}
		if !strings.Contains(outputBody, want.outputBody) {
			t.Errorf("case %q output body = %q, want pinned %q", found.Name, outputBody, want.outputBody)
		}
	}
}

func TestTargetPostResponseObservationsUseLogPhaseLifecycle(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "ai-proxy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest struct {
		Cases []struct {
			Name          string           `yaml:"name"`
			Config        map[string]any   `yaml:"config"`
			AfterShutdown []map[string]any `yaml:"after_shutdown"`
			Steps         []struct {
				FileAssertions []map[string]any `yaml:"file_assertions"`
			} `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	expectations := map[string]bool{
		"openai-fragmented-sse-usage":                                  false,
		"openai-multiple-sse-events-one-chunk":                         false,
		"openai-responses-nonstream-usage-context":                     false,
		"openai-responses-streaming-usage-context":                     false,
		"streaming-request-populates-upstream-address-status-and-time": true,
		"streaming-response-has-nonzero-upstream-response-length":      true,
	}
	found := make(map[string]bool, len(expectations))
	for _, testCase := range manifest.Cases {
		needsLogPhaseConfig, wanted := expectations[testCase.Name]
		if !wanted {
			continue
		}
		found[testCase.Name] = true
		if len(testCase.AfterShutdown) == 0 {
			t.Errorf("case %q has no after_shutdown assertion", testCase.Name)
		}
		for _, step := range testCase.Steps {
			if len(step.FileAssertions) != 0 {
				t.Errorf("case %q observes asynchronous log output before shutdown", testCase.Name)
			}
		}
		if !needsLogPhaseConfig {
			continue
		}
		encoded, err := yaml.Marshal(testCase.Config)
		if err != nil {
			t.Fatalf("encode case %q config: %v", testCase.Name, err)
		}
		config := string(encoded)
		if !strings.Contains(config, "file-logger:") {
			t.Errorf("case %q does not observe pinned upstream variables in log phase", testCase.Name)
		}
		if strings.Contains(config, "response-rewrite:") {
			t.Errorf("case %q observes post-response upstream variables in response headers", testCase.Name)
		}
	}
	for name := range expectations {
		if !found[name] {
			t.Errorf("manifest omits target case %q", name)
		}
	}
}

func containsAIProxyRoute(config map[string]any) bool {
	encoded, err := yaml.Marshal(config)
	return err == nil && (strings.Contains(string(encoded), "ai-proxy:") ||
		strings.Contains(string(encoded), "ai-proxy-multi:"))
}

func TestBuiltinVariablesManifestPreservesPinnedTokenDetails(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "ai-proxy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest struct {
		Cases []struct {
			Name   string `yaml:"name"`
			Source struct {
				File  string `yaml:"file"`
				Tests []int  `yaml:"tests"`
			} `yaml:"source"`
			Fixtures []struct {
				Respond []struct {
					Chunks []string `yaml:"chunks"`
					Body   string   `yaml:"body"`
				} `yaml:"respond"`
			} `yaml:"fixtures"`
			Steps []struct {
				Output struct {
					Headers map[string]struct {
						Equals *string `yaml:"equals"`
					} `yaml:"headers"`
				} `yaml:"output"`
			} `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	type expectation struct {
		number         int
		name           string
		cached         string
		reasoning      string
		cachedDetail   string
		reasoningField string
	}
	expectations := []expectation{
		{
			number: 15, name: "llm-vars-responses-streaming-cache-and-reasoning",
			cached: "10", reasoning: "3",
			cachedDetail: `"cached_tokens":10`, reasoningField: `"reasoning_tokens":3`,
		},
		{
			number: 18, name: "llm-vars-responses-nonstreaming-cache",
			cached: "12", reasoning: "8",
			cachedDetail: `"cached_tokens":12`, reasoningField: `"reasoning_tokens":8`,
		},
	}

	for _, want := range expectations {
		var found *struct {
			Name   string `yaml:"name"`
			Source struct {
				File  string `yaml:"file"`
				Tests []int  `yaml:"tests"`
			} `yaml:"source"`
			Fixtures []struct {
				Respond []struct {
					Chunks []string `yaml:"chunks"`
					Body   string   `yaml:"body"`
				} `yaml:"respond"`
			} `yaml:"fixtures"`
			Steps []struct {
				Output struct {
					Headers map[string]struct {
						Equals *string `yaml:"equals"`
					} `yaml:"headers"`
				} `yaml:"output"`
			} `yaml:"steps"`
		}
		for i := range manifest.Cases {
			testCase := &manifest.Cases[i]
			if testCase.Source.File != "t/plugin/ai-proxy3.t" {
				continue
			}
			containsNumber := false
			for _, number := range testCase.Source.Tests {
				if number == want.number {
					containsNumber = true
				}
			}
			if !containsNumber {
				continue
			}
			found = testCase
			break
		}
		if found == nil {
			t.Fatalf("%s: no case maps ai-proxy3.t test %d", path, want.number)
		}
		if found.Name != want.name {
			t.Errorf("case for test %d has name %q, want %q", want.number, found.Name, want.name)
		}

		var fixtureParts []string
		for _, fixture := range found.Fixtures {
			for _, respond := range fixture.Respond {
				fixtureParts = append(fixtureParts, respond.Body, strings.Join(respond.Chunks, "\n"))
			}
		}
		fixtureText := strings.Join(fixtureParts, "")
		if fixtureText == "" {
			t.Errorf("case %q has no fixture response body or chunks", found.Name)
		}
		if !strings.Contains(fixtureText, want.cachedDetail) {
			t.Errorf("case %q fixture lacks pinned input token detail %s", found.Name, want.cachedDetail)
		}
		if !strings.Contains(fixtureText, want.reasoningField) {
			t.Errorf("case %q fixture lacks pinned output token detail %s", found.Name, want.reasoningField)
		}

		headers := found.Steps[0].Output.Headers
		cached := headers["X-LLM-Cache-Read"]
		reasoning := headers["X-LLM-Reasoning"]
		if cached.Equals == nil || *cached.Equals != want.cached {
			t.Errorf("case %q X-LLM-Cache-Read equals = %v, want %q", found.Name, cached.Equals, want.cached)
		}
		if reasoning.Equals == nil || *reasoning.Equals != want.reasoning {
			t.Errorf("case %q X-LLM-Reasoning equals = %v, want %q", found.Name, reasoning.Equals, want.reasoning)
		}
	}
}
