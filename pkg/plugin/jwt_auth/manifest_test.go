package jwt_auth

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type jwtManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Environment   map[string]string `yaml:"environment"`
	Runtime       map[string]any    `yaml:"runtime"`
	Config        map[string]any    `yaml:"config"`
	Fixtures      []map[string]any  `yaml:"fixtures"`
	Steps         []jwtStep         `yaml:"steps"`
	AfterShutdown []any             `yaml:"after_shutdown"`
}

type jwtStep struct {
	Name string `yaml:"name"`
}

func TestStandaloneManifestMapsOneIndependentCasePerPinnedBlock(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "jwt-auth.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertJWTManifestHasNoAliasesOrMerges(t, &document)

	var manifest struct {
		Sources []struct {
			Commit      string `yaml:"commit"`
			File        string `yaml:"file"`
			Tests       int    `yaml:"tests"`
			TestNumbers []int  `yaml:"test_numbers"`
		} `yaml:"sources"`
		Cases []jwtManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	// testNumbers is nil when every pinned test 1..tests is converted with no
	// gaps. A non-nil list names the exact pinned upstream test numbers that
	// are converted, in ascending order, allowing blocked/removed numbers
	// (e.g. Ed448 in jwt-auth4.t TEST 10) to leave a gap instead of requiring
	// contiguous 1..N coverage.
	wantSources := []struct {
		file        string
		tests       int
		testNumbers []int
	}{
		{"t/plugin/jwt-auth-anonymous-consumer.t", 7, nil},
		{"t/plugin/jwt-auth-more-algo.t", 16, []int{1, 2, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}},
		{"t/plugin/jwt-auth-realm.t", 6, nil},
		{
			"t/plugin/jwt-auth.t",
			58,
			[]int{
				1,
				2,
				3,
				4,
				5,
				6,
				7,
				8,
				9,
				10,
				11,
				12,
				13,
				14,
				15,
				16,
				17,
				18,
				19,
				20,
				21,
				22,
				23,
				24,
				25,
				27,
				28,
				29,
				30,
				31,
				32,
				33,
				34,
				35,
				36,
				37,
				38,
				39,
				40,
				41,
				42,
				43,
				44,
				45,
				46,
				47,
				48,
				49,
				50,
				51,
				52,
				53,
				54,
				55,
				56,
				57,
				58,
				59,
			},
		},
		{"t/plugin/jwt-auth2.t", 9, nil},
		{"t/plugin/jwt-auth3.t", 21, nil},
		{"t/plugin/jwt-auth4.t", 10, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 11}},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	total := 0
	// sequence holds, per source file, the ordered list of pinned upstream
	// test numbers that must appear (in that order) across manifest.Cases.
	sequence := make(map[string][]int, len(wantSources))
	position := make(map[string]int, len(wantSources))
	for i, want := range wantSources {
		got := manifest.Sources[i]
		if got.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", i+1, got.Commit)
		}
		wantNumbers := want.testNumbers
		if wantNumbers == nil {
			wantNumbers = make([]int, want.tests)
			for n := range wantNumbers {
				wantNumbers[n] = n + 1
			}
		}
		if got.File != want.file || got.Tests != want.tests || !slices.Equal(got.TestNumbers, want.testNumbers) {
			t.Fatalf(
				"source %d = (%q, %d, %v), want (%q, %d, %v)",
				i+1, got.File, got.Tests, got.TestNumbers, want.file, want.tests, want.testNumbers,
			)
		}
		total += want.tests
		sequence[want.file] = wantNumbers
		position[want.file] = 0
	}
	if len(manifest.Cases) != total {
		t.Fatalf("top-level cases = %d, want exactly %d", len(manifest.Cases), total)
	}
	names := make(map[string]struct{}, len(manifest.Cases))
	genericName := regexp.MustCompile(`(?i)(block-[0-9]+|source-block|placeholder|generic|probe)`)
	for i, testCase := range manifest.Cases {
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf("case %d %q source tests = %v, want one pinned block", i+1, testCase.Name, testCase.Source.Tests)
		}
		wantNumbers, ok := sequence[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unexpected source %q", i+1, testCase.Name, testCase.Source.File)
		}
		pos := position[testCase.Source.File]
		if pos >= len(wantNumbers) {
			t.Fatalf("case %d %q has more blocks than %s declares", i+1, testCase.Name, testCase.Source.File)
		}
		want := wantNumbers[pos]
		if testCase.Source.Tests[0] != want {
			t.Fatalf("case %d %q source test = %d, want %d", i+1, testCase.Name, testCase.Source.Tests[0], want)
		}
		position[testCase.Source.File]++
		if _, exists := names[testCase.Name]; exists {
			t.Errorf("case %d has duplicate behavior name %q", i+1, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if genericName.MatchString(testCase.Name) {
			t.Errorf("case %d has generic behavior name %q", i+1, testCase.Name)
		}
		assertJWTCaseIdentity(t, i+1, testCase, genericName)
		assertJWTCaseHasStandaloneResources(t, i+1, testCase)
		assertJWTSensitiveCaseSemantics(t, testCase)
	}
	for _, want := range wantSources {
		if got := position[want.file]; got != want.tests {
			t.Fatalf("%s mapped through block %d, want %d", want.file, got, want.tests)
		}
	}
}

func assertJWTCaseIdentity(t *testing.T, index int, testCase jwtManifestCase, genericName *regexp.Regexp) {
	t.Helper()
	testNumber := testCase.Source.Tests[0]
	sourceName := strings.TrimSuffix(filepath.Base(testCase.Source.File), ".t")
	suffix := "-test-" + strconv.Itoa(testNumber)
	if !strings.HasPrefix(testCase.Name, sourceName+"-") || !strings.HasSuffix(testCase.Name, suffix) {
		t.Errorf("case %d %q does not encode source and pinned test %d", index, testCase.Name, testNumber)
	}
	behavior := strings.TrimSuffix(strings.TrimPrefix(testCase.Name, sourceName+"-"), suffix)
	if behavior == "" {
		t.Errorf("case %d %q has no behavior identity", index, testCase.Name)
	}
	if len(testCase.Steps) == 0 {
		t.Errorf("case %d %q has no real request", index, testCase.Name)
	}
	for stepIndex, step := range testCase.Steps {
		if step.Name == "" || genericName.MatchString(step.Name) {
			t.Errorf("case %d %q step %d has generic name %q", index, testCase.Name, stepIndex+1, step.Name)
		}
		wantStep := "exercise-" + behavior + suffix
		if step.Name != wantStep {
			t.Errorf("case %d %q step %d name = %q, want %q", index, testCase.Name, stepIndex+1, step.Name, wantStep)
		}
	}
}

func assertJWTCaseHasStandaloneResources(t *testing.T, index int, testCase jwtManifestCase) {
	t.Helper()
	consumers, ok := testCase.Config["consumers"].([]any)
	if !ok || len(consumers) == 0 {
		t.Errorf("case %d %q has no standalone consumer", index, testCase.Name)
	}
	routes, ok := testCase.Config["routes"].([]any)
	if !ok || len(routes) == 0 {
		t.Errorf("case %d %q has no standalone route", index, testCase.Name)
		return
	}
	hasJWTAuth := false
	for _, rawRoute := range routes {
		route, ok := rawRoute.(map[string]any)
		if !ok {
			continue
		}
		plugins, ok := route["plugins"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := plugins["jwt-auth"]; ok {
			hasJWTAuth = true
			break
		}
	}
	if !hasJWTAuth {
		t.Errorf("case %d %q has no route exercising jwt-auth", index, testCase.Name)
	}
	if len(testCase.Fixtures) == 0 {
		t.Errorf("case %d %q has no fixture", index, testCase.Name)
	}
}

func assertJWTSensitiveCaseSemantics(t *testing.T, testCase jwtManifestCase) {
	t.Helper()
	encoded, err := yaml.Marshal(testCase)
	if err != nil {
		t.Fatalf("encode case %q for semantic assertions: %v", testCase.Name, err)
	}
	text := string(encoded)
	testNumber := testCase.Source.Tests[0]

	switch testCase.Source.File {
	case "t/plugin/jwt-auth2.t":
		if testNumber <= 7 || testNumber == 9 {
			assertJWTCaseContains(t, testCase, text, "jwt-header", "jwt-cookie", "jwt-query")
		}
	case "t/plugin/jwt-auth3.t":
		switch testNumber {
		case 3, 4, 5, 6:
			assertJWTCaseContains(t, testCase, text, "hide_credentials: false")
		case 7, 8, 9, 11, 12:
			assertJWTCaseContains(t, testCase, text, "hide_credentials: true", "absent: true")
		case 10:
			assertJWTCaseContains(t, testCase, text, "hide_credentials: true", "equals: /jwt-auth3-10")
		case 14:
			assertJWTCaseContains(t, testCase, text, "enable_encrypt_fields: true", "keyring:")
			if len(testCase.AfterShutdown) == 0 {
				t.Errorf("case %q does not assert encrypted storage after shutdown", testCase.Name)
			}
		case 15, 16, 17:
			assertJWTCaseContains(
				t,
				testCase,
				text,
				"$secret://vault/test1/jack/secret",
				"name: vault",
				"X-Vault-Token",
			)
		case 18, 19:
			assertJWTCaseContains(t, testCase, text,
				"$secret://vault/test1/rsa1/secret",
				"$secret://vault/test1/rsa1/public_key",
				"algorithm: RS256",
				"name: vault",
			)
		case 20, 21:
			assertJWTCaseContains(t, testCase, text,
				"$ENV://VAULT_TOKEN",
				"$secret://vault/test1/jack/secret",
				"name: vault",
			)
			if testCase.Environment["VAULT_TOKEN"] == "" {
				t.Errorf("case %q has no VAULT_TOKEN environment value", testCase.Name)
			}
		}
	case "t/plugin/jwt-auth4.t":
		if testNumber >= 6 && testNumber <= 9 {
			assertJWTCaseContains(t, testCase, text, "proxy-rewrite", "X-JWT-Payload", "$jwt_auth_payload")
			if testNumber <= 7 {
				assertJWTCaseContains(t, testCase, text, "store_in_ctx: false", "absent: true")
			} else {
				assertJWTCaseContains(t, testCase, text, "store_in_ctx: true", "key:user-key")
			}
		}
	}
}

func assertJWTCaseContains(t *testing.T, testCase jwtManifestCase, text string, required ...string) {
	t.Helper()
	for _, value := range required {
		if !strings.Contains(text, value) {
			t.Errorf("case %q does not contain required semantic marker %q", testCase.Name, value)
		}
	}
}

func assertJWTManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Kind == yaml.AliasNode {
		t.Fatalf("manifest contains YAML alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertJWTManifestHasNoAliasesOrMerges(t, child)
	}
}
