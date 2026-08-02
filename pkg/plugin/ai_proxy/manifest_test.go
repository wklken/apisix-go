package ai_proxy

import (
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
		if source.File != requestBodyOverrideSource {
			continue
		}
		foundSource = true
		if source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" || source.Tests != 19 {
			t.Fatalf("pinned source = (%q, %d), want APISIX c3d7d5ec with 19 tests", source.Commit, source.Tests)
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
	if len(cases) != 19 {
		t.Fatalf("%s cases = %d, want exactly 19 independent cases", requestBodyOverrideSource, len(cases))
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
			t.Errorf("case %d %q has no ai-proxy or ai-proxy-multi route config", testNumber, testCase.Name)
		}
		fixture := testCase.Fixtures[0]
		if len(fixture.Respond) == 0 {
			t.Errorf("case %d %q provider fixture has no response", testNumber, testCase.Name)
		}
		if testNumber <= 2 {
			if fixture.ExpectRequests == nil || *fixture.ExpectRequests != 0 {
				t.Errorf(
					"case %d %q must assert invalid config makes zero provider requests",
					testNumber,
					testCase.Name,
				)
			}
		} else if len(fixture.Expect) == 0 || fixture.Expect[0]["body"] == nil {
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
		if source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" || source.Tests != 28 {
			t.Fatalf("pinned source = (%q, %d), want APISIX c3d7d5ec with 28 tests", source.Commit, source.Tests)
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
