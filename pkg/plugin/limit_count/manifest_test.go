package limit_count

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStandaloneManifestDuplicateBacklogOnlyDecreases(t *testing.T) {
	const (
		// maxHistoricalDuplicateGroups was raised from 26 to 27 when WRONG-7
		// remediation pointed
		// limit-count-redis-sentinel-create-a-limit-count-with-broken-redis-sentinels-test-11
		// at genuinely unreachable sentinels (matching upstream
		// limit-count-redis-sentinel.t TEST 11/12). Both cases now issue the
		// identical real request against the identical broken-sentinel route
		// upstream pairs them against (TEST 11 only exercises the admin PUT in
		// upstream; this harness has no separate admin-API phase), so they are
		// unavoidably identical once test-11 is corrected. maxHistoricalDuplicateCases
		// dropped from 142 to 120 in the same change, a net improvement.
		maxHistoricalDuplicateGroups = 27
		maxHistoricalDuplicateCases  = 120
	)
	currentSourcePrefixes := []string{
		"limit-count-consumer-isolation-",
		"limit-count-redis-cluster2-",
		"limit-count-redis-cluster3-",
	}

	path := filepath.Join("..", "..", "..", "t", "plugin", "limit-count.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var manifest struct {
		Cases []map[string]any `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	groups := make(map[string][]string)
	for i, testCase := range manifest.Cases {
		name, _ := testCase["name"].(string)
		normalized := normalizeLimitCountBehavior(testCase)
		encoded, err := json.Marshal(normalized)
		if err != nil {
			t.Fatalf("marshal normalized case %d %q: %v", i+1, name, err)
		}
		groups[string(encoded)] = append(groups[string(encoded)], name)
	}
	duplicateGroups := 0
	duplicateCases := 0
	largest := []string(nil)
	currentSourceDuplicates := []string(nil)
	for _, names := range groups {
		if len(names) < 2 {
			continue
		}
		duplicateGroups++
		duplicateCases += len(names)
		if len(names) > len(largest) {
			largest = names
		}
		for _, name := range names {
			if slices.ContainsFunc(currentSourcePrefixes, func(prefix string) bool {
				return strings.HasPrefix(name, prefix)
			}) {
				currentSourceDuplicates = append(currentSourceDuplicates, name)
			}
		}
	}
	if len(currentSourceDuplicates) > 0 {
		t.Fatalf(
			"%s cases still have duplicated normalized behavior: %v",
			strings.Join(currentSourcePrefixes, ", "),
			currentSourceDuplicates,
		)
	}
	if duplicateGroups > maxHistoricalDuplicateGroups || duplicateCases > maxHistoricalDuplicateCases {
		t.Fatalf(
			"historical duplicate backlog grew to %d groups/%d cases, maximum %d/%d; largest group (%d): %v",
			duplicateGroups,
			duplicateCases,
			maxHistoricalDuplicateGroups,
			maxHistoricalDuplicateCases,
			len(largest),
			largest,
		)
	}
}

func normalizeLimitCountBehavior(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			switch key {
			case "name", "source", "id":
				continue
			default:
				result[key] = normalizeLimitCountBehavior(child)
			}
		}
		return result
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[fmt.Sprint(key)] = normalizeLimitCountBehavior(child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i, child := range typed {
			result[i] = normalizeLimitCountBehavior(child)
		}
		return result
	default:
		return value
	}
}

type limitCountManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Config   map[string]any `yaml:"config"`
	Fixtures []struct {
		Name           string           `yaml:"name"`
		Kind           string           `yaml:"kind"`
		ExpectRequests *int             `yaml:"expect_requests"`
		Count          map[string]any   `yaml:"count"`
		Redis          map[string]any   `yaml:"redis"`
		NetworkExpect  []map[string]any `yaml:"network_expect"`
		NetworkRespond []map[string]any `yaml:"network_respond"`
	} `yaml:"fixtures"`
	Steps []struct {
		Name   string         `yaml:"name"`
		Input  map[string]any `yaml:"input"`
		Output map[string]any `yaml:"output"`
	} `yaml:"steps"`
}

func TestStandaloneManifestMapsEveryPinnedBlockInOrder(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "limit-count.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertLimitCountManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			Commit      string `yaml:"commit"`
			File        string `yaml:"file"`
			Tests       int    `yaml:"tests"`
			TestNumbers []int  `yaml:"test_numbers"`
		} `yaml:"sources"`
		Cases []limitCountManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	wantSources := []struct {
		file  string
		tests int
	}{
		{"t/plugin/limit-count-consumer-group-credentials.t", 8},
		{"t/plugin/limit-count-consumer-isolation.t", 5},
		{"t/plugin/limit-count-redis-cluster.t", 18},
		{"t/plugin/limit-count-redis-cluster2.t", 2},
		{"t/plugin/limit-count-redis-cluster3.t", 6},
		{"t/plugin/limit-count-redis-delayed-sync.t", 5},
		{"t/plugin/limit-count-redis-delayed-sync2.t", 9},
		{"t/plugin/limit-count-redis-sentinel.t", 15},
		{"t/plugin/limit-count-redis.t", 22},
		{"t/plugin/limit-count-redis2.t", 10},
		{"t/plugin/limit-count-redis3.t", 11},
		{"t/plugin/limit-count-redis4.t", 5},
		{"t/plugin/limit-count-redis5.t", 3},
		{"t/plugin/limit-count-rules.t", 22},
		{"t/plugin/limit-count-sliding.t", 8},
		{"t/plugin/limit-count-variable.t", 13},
		{"t/plugin/limit-count.t", 41},
		{"t/plugin/limit-count2.t", 22},
		{"t/plugin/limit-count3.t", 13},
		{"t/plugin/limit-count4.t", 5},
		{"t/plugin/limit-count5.t", 8},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}

	total := 0
	// sequence holds, per source file, the ordered list of upstream test
	// numbers this manifest maps to independent cases. Most files map every
	// test 1..Tests contiguously; a source may instead declare an explicit
	// test_numbers list (skipping numbers blocked in corpus_scope.yaml, e.g.
	// limit-count.t test 23 is blocked_design and intentionally absent).
	sequence := make(map[string][]int, len(wantSources))
	cursor := make(map[string]int, len(wantSources))
	for i, want := range wantSources {
		source := manifest.Sources[i]
		if source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", i+1, source.Commit)
		}
		if source.File != want.file || source.Tests != want.tests {
			t.Fatalf(
				"source %d = (%q, %d), want (%q, %d)",
				i+1,
				source.File,
				source.Tests,
				want.file,
				want.tests,
			)
		}
		numbers := source.TestNumbers
		if len(numbers) == 0 {
			numbers = make([]int, want.tests)
			for n := range numbers {
				numbers[n] = n + 1
			}
		} else if len(numbers) != want.tests {
			t.Fatalf(
				"source %d %q declares %d test_numbers, want %d to match tests",
				i+1,
				source.File,
				len(numbers),
				want.tests,
			)
		}
		total += want.tests
		sequence[want.file] = numbers
		cursor[want.file] = 0
	}
	genericName := regexp.MustCompile(`(?i)(block-[0-9]+|source-[0-9]+|placeholder|generic|probe|lifecycle)`)
	names := make(map[string]struct{}, len(manifest.Cases))
	covered := 0
	for i, testCase := range manifest.Cases {
		numbers, ok := sequence[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unknown source %q", i+1, testCase.Name, testCase.Source.File)
		}
		position := cursor[testCase.Source.File]
		if position >= len(numbers) {
			t.Fatalf(
				"case %d %q has more cases than declared test_numbers for %q",
				i+1,
				testCase.Name,
				testCase.Source.File,
			)
		}
		caseTests := testCase.Source.Tests
		if len(caseTests) == 0 || position+len(caseTests) > len(numbers) ||
			!slices.Equal(caseTests, numbers[position:position+len(caseTests)]) {
			t.Fatalf(
				"case %d %q source tests = %v, want next tests from %v",
				i+1,
				testCase.Name,
				caseTests,
				numbers[position:],
			)
		}
		cursor[testCase.Source.File] += len(caseTests)
		covered += len(caseTests)
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("case %d has duplicate behavior name %q", i+1, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if genericName.MatchString(testCase.Name) {
			t.Errorf("case %d has generic behavior name %q", i+1, testCase.Name)
		}
		assertLimitCountCaseIdentity(t, i+1, testCase, genericName)
		assertLimitCountCaseResources(t, i+1, testCase)
	}
	if covered != total {
		t.Fatalf("covered source tests = %d, want exactly %d", covered, total)
	}
	for _, source := range wantSources {
		if got := cursor[source.file]; got != source.tests {
			t.Fatalf("%s mapped through block %d, want %d", source.file, got, source.tests)
		}
	}
}

func assertLimitCountCaseIdentity(
	t *testing.T,
	index int,
	testCase limitCountManifestCase,
	genericName *regexp.Regexp,
) {
	t.Helper()
	source := strings.TrimSuffix(filepath.Base(testCase.Source.File), ".t")
	testNumbers := testCase.Source.Tests
	suffix := "-test-" + strconv.Itoa(testNumbers[0])
	if len(testNumbers) > 1 {
		parts := make([]string, len(testNumbers))
		for i, testNumber := range testNumbers {
			parts[i] = strconv.Itoa(testNumber)
		}
		suffix = "-tests-" + strings.Join(parts, "-")
	}
	if !strings.HasPrefix(testCase.Name, source+"-") || !strings.HasSuffix(testCase.Name, suffix) {
		t.Errorf(
			"case %d %q does not encode source %q and tests %v",
			index,
			testCase.Name,
			source,
			testNumbers,
		)
	}
	behavior := strings.TrimSuffix(strings.TrimPrefix(testCase.Name, source+"-"), suffix)
	if strings.TrimSpace(behavior) == "" {
		t.Errorf("case %d %q has no behavior identity", index, testCase.Name)
	}
	for stepIndex, step := range testCase.Steps {
		if strings.TrimSpace(step.Name) == "" || genericName.MatchString(step.Name) {
			t.Errorf(
				"case %d %q step %d has generic behavior name %q",
				index,
				testCase.Name,
				stepIndex+1,
				step.Name,
			)
		}
		if step.Input["path"] == nil ||
			(step.Output["status"] == nil && step.Output["status_counts"] == nil) {
			t.Errorf(
				"case %d %q step %d lacks real request or status assertion",
				index,
				testCase.Name,
				stepIndex+1,
			)
		}
	}
}

func assertLimitCountCaseResources(t *testing.T, index int, testCase limitCountManifestCase) {
	t.Helper()
	if !containsLimitCountConfig(testCase.Config) {
		t.Errorf(
			"case %d %q has no route, service, consumer, or global rule that configures limit-count",
			index,
			testCase.Name,
		)
	}
	if len(testCase.Fixtures) == 0 {
		t.Errorf("case %d %q has no real fixture", index, testCase.Name)
	} else {
		hasHTTP := false
		for fixtureIndex, fixture := range testCase.Fixtures {
			switch fixture.Kind {
			case "http", "https":
				hasHTTP = true
				if fixture.ExpectRequests == nil && len(fixture.Count) == 0 {
					t.Errorf(
						"case %d %q HTTP fixture %d lacks exact upstream request-count assertion",
						index,
						testCase.Name,
						fixtureIndex+1,
					)
				}
			case "redis", "redis-cluster", "redis-sentinel":
				if len(fixture.Redis) == 0 &&
					(len(fixture.NetworkExpect) == 0 || len(fixture.NetworkRespond) == 0) {
					t.Errorf(
						"case %d %q Redis fixture %d lacks state or command assertions",
						index,
						testCase.Name,
						fixtureIndex+1,
					)
				}
			default:
				t.Errorf(
					"case %d %q fixture %d has unsupported kind %q",
					index,
					testCase.Name,
					fixtureIndex+1,
					fixture.Kind,
				)
			}
		}
		if !hasHTTP {
			t.Errorf("case %d %q has no real HTTP upstream fixture", index, testCase.Name)
		}
	}
	if len(testCase.Steps) == 0 {
		t.Errorf("case %d %q has no real request step", index, testCase.Name)
	}
}

func containsLimitCountConfig(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if plugins, ok := typed["plugins"].(map[string]any); ok {
			if _, ok := plugins["limit-count"]; ok {
				return true
			}
		}
		for _, child := range typed {
			if containsLimitCountConfig(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, containsLimitCountConfig)
	}
	return false
}

func assertLimitCountManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML anchor or alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertLimitCountManifestHasNoAliasesOrMerges(t, child)
	}
}
