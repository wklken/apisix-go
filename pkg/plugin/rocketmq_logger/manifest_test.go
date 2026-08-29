package rocketmq_logger

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStandaloneManifestMapsEveryPinnedBlockToOneRocketMQPublishCase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "rocketmq-logger.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rocketmq-logger manifest: %v", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode rocketmq-logger YAML syntax tree: %v", err)
	}
	assertRocketMQManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			Commit      string `yaml:"commit"`
			File        string `yaml:"file"`
			Tests       int    `yaml:"tests"`
			TestNumbers []int  `yaml:"test_numbers"`
		} `yaml:"sources"`
		Cases []struct {
			Name   string `yaml:"name"`
			Source struct {
				File  string `yaml:"file"`
				Tests []int  `yaml:"tests"`
			} `yaml:"source"`
			Runtime  map[string]any `yaml:"runtime"`
			Config   map[string]any `yaml:"config"`
			Fixtures []struct {
				Name           string `yaml:"name"`
				Kind           string `yaml:"kind"`
				ExpectRequests *int   `yaml:"expect_requests"`
				Expect         []any  `yaml:"expect"`
				Respond        []any  `yaml:"respond"`
				RocketMQ       any    `yaml:"rocketmq"`
			} `yaml:"fixtures"`
			Steps []struct {
				Name          string         `yaml:"name"`
				Config        map[string]any `yaml:"config"`
				ConfigProbe   map[string]any `yaml:"config_probe"`
				ConfigTimeout string         `yaml:"config_timeout"`
				Input         map[string]any `yaml:"input"`
				Output        map[string]any `yaml:"output"`
			} `yaml:"steps"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode rocketmq-logger manifest: %v", err)
	}

	wantSources := []struct {
		file        string
		testNumbers []int
	}{
		{"t/plugin/rocketmq-logger-log-format.t", []int{1, 2, 3, 4, 5}},
		{
			"t/plugin/rocketmq-logger.t",
			[]int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19},
		},
		{
			"t/plugin/rocketmq-logger2.t",
			[]int{1, 2, 3, 4, 5, 6, 7, 10, 11, 12, 13, 14, 15, 16, 17},
		},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	for i, want := range wantSources {
		source := manifest.Sources[i]
		if source.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", i+1, source.Commit)
		}
		if source.File != want.file || source.Tests != len(want.testNumbers) {
			t.Fatalf(
				"source %d = (%q, %d), want (%q, %d)",
				i+1,
				source.File,
				source.Tests,
				want.file,
				len(want.testNumbers),
			)
		}
		gotTestNumbers := source.TestNumbers
		if len(gotTestNumbers) == 0 {
			gotTestNumbers = make([]int, source.Tests)
			for number := range gotTestNumbers {
				gotTestNumbers[number] = number + 1
			}
		}
		if !slices.Equal(gotTestNumbers, want.testNumbers) {
			t.Fatalf(
				"source %d test_numbers = %v, want %v",
				i+1,
				gotTestNumbers,
				want.testNumbers,
			)
		}
	}
	wantCaseCount := 0
	for _, source := range wantSources {
		wantCaseCount += len(source.testNumbers)
	}
	if len(manifest.Cases) != wantCaseCount {
		t.Fatalf("top-level cases = %d, want exactly %d", len(manifest.Cases), wantCaseCount)
	}

	next := make(map[string]int, len(wantSources))
	wantTests := make(map[string][]int, len(wantSources))
	for _, source := range wantSources {
		wantTests[source.file] = source.testNumbers
	}
	names := make(map[string]struct{}, len(manifest.Cases))
	for i, testCase := range manifest.Cases {
		wantNumbers, ok := wantTests[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		index := next[testCase.Source.File]
		if index >= len(wantNumbers) {
			t.Fatalf("case %d %q exceeds source selection %v", i+1, testCase.Name, wantNumbers)
		}
		want := wantNumbers[index]
		if len(testCase.Source.Tests) != 1 || testCase.Source.Tests[0] != want {
			t.Errorf("case %d %q source tests = %v, want [%d]", i+1, testCase.Name, testCase.Source.Tests, want)
		}
		next[testCase.Source.File] = index + 1

		lowerName := strings.ToLower(strings.TrimSpace(testCase.Name))
		for _, forbidden := range []string{"placeholder", "generic", "probe", "source-", "block-", "skip"} {
			if strings.Contains(lowerName, forbidden) {
				t.Errorf("case %d has placeholder-like name %q", i+1, testCase.Name)
			}
		}
		if _, exists := names[testCase.Name]; exists {
			t.Errorf("case %d repeats name %q", i+1, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if !containsRocketMQLoggerConfig(testCase.Config) {
			t.Errorf("case %d %q has no route rocketmq-logger configuration", i+1, testCase.Name)
		}
		if len(testCase.Fixtures) < 2 || len(testCase.Steps) == 0 {
			t.Errorf("case %d %q must execute an upstream and RocketMQ fixture", i+1, testCase.Name)
		}
		rocketFixtures := 0
		for fixtureIndex, fixture := range testCase.Fixtures {
			switch fixture.Kind {
			case "http", "https":
				if fixture.Name == "" || len(fixture.Respond) == 0 ||
					(fixture.ExpectRequests == nil && len(fixture.Expect) == 0) {
					t.Errorf("case %d %q HTTP fixture %d lacks behavior/assertions", i+1, testCase.Name, fixtureIndex+1)
				}
			case "rocketmq":
				rocketFixtures++
				if fixture.Name == "" || fixture.RocketMQ == nil {
					t.Errorf(
						"case %d %q RocketMQ fixture %d lacks protocol assertions",
						i+1,
						testCase.Name,
						fixtureIndex+1,
					)
				}
			default:
				t.Errorf(
					"case %d %q fixture %d has non-behavioral kind %q",
					i+1,
					testCase.Name,
					fixtureIndex+1,
					fixture.Kind,
				)
			}
		}
		if rocketFixtures != 1 {
			t.Errorf("case %d %q RocketMQ fixtures = %d, want exactly one", i+1, testCase.Name, rocketFixtures)
		}
		for stepIndex, step := range testCase.Steps {
			if step.Name == "" || step.Input["path"] == nil || step.Output["status"] == nil {
				t.Errorf("case %d %q step %d lacks a named request/status assertion", i+1, testCase.Name, stepIndex+1)
			}
		}
		encoded, err := yaml.Marshal(testCase)
		if err != nil {
			t.Fatalf("encode case %d %q: %v", i+1, testCase.Name, err)
		}
		assertRocketMQCaseSemantics(
			t,
			i+1,
			testCase.Name,
			testCase.Source.File,
			testCase.Source.Tests[0],
			string(encoded),
		)
	}
	for _, source := range wantSources {
		if next[source.file] != len(source.testNumbers) {
			t.Fatalf(
				"%s mapped case count = %d, want %d for tests %v",
				source.file,
				next[source.file],
				len(source.testNumbers),
				source.testNumbers,
			)
		}
	}
}

func assertRocketMQCaseSemantics(t *testing.T, index int, name, file string, testNumber int, encoded string) {
	t.Helper()
	required := []string{"nameserver_list", "topic:", "batch_max_size", "rocketmq:"}
	switch file {
	case "t/plugin/rocketmq-logger-log-format.t":
		required = append(required, "log_format")
	case "t/plugin/rocketmq-logger.t":
		switch {
		case testNumber >= 7 && testNumber <= 10:
			required = append(required, "meta_format: origin")
		case testNumber >= 13 && testNumber <= 16:
			required = append(required, "key_absent: true")
		case testNumber >= 17:
			required = append(required, "partitions: 3")
		}
	case "t/plugin/rocketmq-logger2.t":
		switch {
		case testNumber == 1:
			required = append(required, "config_probe", "config_timeout")
		case testNumber == 2:
			required = append(required, "topic_missing: true")
		case testNumber >= 5 && testNumber <= 8:
			required = append(required, "include_req_body")
		case testNumber >= 9 && testNumber <= 16:
			required = append(required, "include_resp_body")
		case testNumber == 17:
			required = append(required, "include_resp_body_expr", "request_length", "http_content_type")
		case testNumber == 18:
			required = append(required, "access_key", "secret_key")
		}
		if testNumber == 13 || testNumber == 14 {
			required = append(required, "Content-Encoding: gzip")
		}
		if testNumber == 15 || testNumber == 16 {
			required = append(required, "Content-Encoding: br")
		}
	}
	for _, marker := range required {
		if !strings.Contains(encoded, marker) {
			t.Errorf("case %d %q lacks required behavioral marker %q", index, name, marker)
		}
	}
}

func containsRocketMQLoggerConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if plugins, ok := typed["plugins"].(map[string]any); ok {
			if _, ok := plugins["rocketmq-logger"]; ok {
				return true
			}
		}
		for _, child := range typed {
			if containsRocketMQLoggerConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsRocketMQLoggerConfig)
	}
	return false
}

func assertRocketMQManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertRocketMQManifestHasNoAliasesOrMerges(t, child)
	}
}
