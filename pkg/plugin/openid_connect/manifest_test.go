package openid_connect

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type openIDManifestCase struct {
	Name   string `yaml:"name"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Environment map[string]string `yaml:"environment"`
	Runtime     map[string]any    `yaml:"runtime"`
	Config      map[string]any    `yaml:"config"`
	Fixtures    []map[string]any  `yaml:"fixtures"`
	Steps       []map[string]any  `yaml:"steps"`
}

func TestStandaloneManifestMapsOneIndependentCasePerPinnedBlock(t *testing.T) {
	path := filepath.Join("..", "..", "..", "t", "plugin", "openid-connect.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", path, err)
	}
	assertOpenIDManifestHasNoAliasesOrMerges(t, &document)
	text := string(data)
	for _, forbidden := range []string{"skip:", "placeholder", "source-complete skip", "/probe"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Errorf("manifest contains forbidden placeholder text %q", forbidden)
		}
	}

	var manifest struct {
		Sources []struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"sources"`
		Cases []openIDManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	// testNumbers lists the exact upstream test numbers that were converted for a
	// source file, in manifest order. Leave nil for sources where every test 1..tests
	// was converted contiguously; set it explicitly when specific test numbers were
	// not converted into standalone cases and therefore have gaps.
	wantSources := []struct {
		file        string
		tests       int
		testNumbers []int
	}{
		{"t/plugin/openid-connect-identity-headers.t", 4, nil},
		{"t/plugin/openid-connect-redis.t", 4, nil},
		{"t/plugin/openid-connect.t", 54, nil},
		{"t/plugin/openid-connect10.t", 11, []int{1, 2, 3, 4, 5, 7, 8, 9, 10, 11, 12}},
		{"t/plugin/openid-connect2.t", 21, nil},
		{"t/plugin/openid-connect3.t", 6, nil},
		{"t/plugin/openid-connect4.t", 6, nil},
		{"t/plugin/openid-connect6.t", 8, nil},
		{"t/plugin/openid-connect7.t", 10, nil},
		{"t/plugin/openid-connect8.t", 8, nil},
		{"t/plugin/openid-connect9.t", 6, nil},
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("sources = %d, want %d", len(manifest.Sources), len(wantSources))
	}
	total := 0
	nextIndex := make(map[string]int, len(wantSources))
	testNumbersByFile := make(map[string][]int, len(wantSources))
	for i, want := range wantSources {
		got := manifest.Sources[i]
		if got.Commit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
			t.Fatalf("source %d commit = %q, want pinned Apache APISIX commit", i+1, got.Commit)
		}
		if got.File != want.file || got.Tests != want.tests {
			t.Fatalf("source %d = (%q, %d), want (%q, %d)", i+1, got.File, got.Tests, want.file, want.tests)
		}
		numbers := want.testNumbers
		if numbers == nil {
			numbers = make([]int, want.tests)
			for j := range numbers {
				numbers[j] = j + 1
			}
		}
		if len(numbers) != want.tests {
			t.Fatalf("source %d %q testNumbers = %d entries, want %d", i+1, want.file, len(numbers), want.tests)
		}
		total += want.tests
		nextIndex[want.file] = 0
		testNumbersByFile[want.file] = numbers
	}
	if len(manifest.Cases) != total {
		t.Fatalf("top-level cases = %d, want exactly %d", len(manifest.Cases), total)
	}

	generic := regexp.MustCompile(`(?i)(block-[0-9]+|source-[0-9]+|placeholder|generic|probe)`)
	names := make(map[string]struct{}, len(manifest.Cases))
	for i, testCase := range manifest.Cases {
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf("case %d %q source tests = %v, want one pinned block", i+1, testCase.Name, testCase.Source.Tests)
		}
		idx, ok := nextIndex[testCase.Source.File]
		if !ok {
			t.Fatalf("case %d %q has unexpected source %q", i+1, testCase.Name, testCase.Source.File)
		}
		numbers := testNumbersByFile[testCase.Source.File]
		if idx >= len(numbers) {
			t.Fatalf("case %d %q has more cases than expected for source %q", i+1, testCase.Name, testCase.Source.File)
		}
		want := numbers[idx]
		if testCase.Source.Tests[0] != want {
			t.Fatalf("case %d %q source test = %d, want %d", i+1, testCase.Name, testCase.Source.Tests[0], want)
		}
		nextIndex[testCase.Source.File] = idx + 1
		if _, exists := names[testCase.Name]; exists {
			t.Errorf("case %d has duplicate behavior name %q", i+1, testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		assertOpenIDCaseIdentity(t, i+1, testCase, generic)
		assertOpenIDCaseHasStandaloneResources(t, i+1, testCase)
		assertOpenIDSensitiveSemantics(t, testCase)
	}
}

func assertOpenIDCaseIdentity(
	t *testing.T,
	index int,
	testCase openIDManifestCase,
	generic *regexp.Regexp,
) {
	t.Helper()
	number := testCase.Source.Tests[0]
	source := strings.TrimSuffix(filepath.Base(testCase.Source.File), ".t")
	suffix := "-test-" + strconv.Itoa(number)
	if !strings.HasPrefix(testCase.Name, source+"-") || !strings.HasSuffix(testCase.Name, suffix) {
		t.Errorf("case %d %q does not encode source and pinned test %d", index, testCase.Name, number)
	}
	behavior := strings.TrimSuffix(strings.TrimPrefix(testCase.Name, source+"-"), suffix)
	if behavior == "" || generic.MatchString(behavior) {
		t.Errorf("case %d %q has generic behavior identity", index, testCase.Name)
	}
	if len(testCase.Steps) == 0 {
		t.Errorf("case %d %q has no real request", index, testCase.Name)
	}
	for stepIndex, step := range testCase.Steps {
		name, _ := step["name"].(string)
		if name == "" || generic.MatchString(name) {
			t.Errorf("case %d %q step %d has generic name %q", index, testCase.Name, stepIndex+1, name)
		}
	}
}

func assertOpenIDCaseHasStandaloneResources(t *testing.T, index int, testCase openIDManifestCase) {
	t.Helper()
	routes, ok := testCase.Config["routes"].([]any)
	if !ok || len(routes) == 0 {
		t.Errorf("case %d %q has no standalone route", index, testCase.Name)
		return
	}
	hasPlugin := false
	for _, rawRoute := range routes {
		route, ok := rawRoute.(map[string]any)
		if !ok {
			continue
		}
		plugins, ok := route["plugins"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := plugins["openid-connect"]; ok {
			hasPlugin = true
			break
		}
	}
	if !hasPlugin {
		t.Errorf("case %d %q has no route exercising openid-connect", index, testCase.Name)
	}
	if len(testCase.Fixtures) == 0 {
		t.Errorf("case %d %q has no fixture", index, testCase.Name)
	}
}

func assertOpenIDSensitiveSemantics(t *testing.T, testCase openIDManifestCase) {
	t.Helper()
	encoded, err := yaml.Marshal(testCase)
	if err != nil {
		t.Fatalf("encode case %q: %v", testCase.Name, err)
	}
	text := string(encoded)
	number := testCase.Source.Tests[0]
	contains := func(values ...string) {
		t.Helper()
		for _, value := range values {
			if !strings.Contains(text, value) {
				t.Errorf("case %q does not preserve %q semantics", testCase.Name, value)
			}
		}
	}

	switch testCase.Source.File {
	case "t/plugin/openid-connect-identity-headers.t":
		contains("X-Access-Token")
	case "t/plugin/openid-connect-redis.t":
		contains("storage: redis")
	case "t/plugin/openid-connect.t":
		switch number {
		case 5:
			contains("enable_encrypt_fields: true")
		case 8, 10, 52:
			contains("authorization_endpoint", "token_endpoint", "{{CAPTURE.")
		case 15, 17, 19, 21, 27, 28, 34, 40:
			contains("Authorization: Bearer")
		case 23, 24:
			contains("introspection_endpoint", "Authorization: Bearer")
		case 30, 36, 38:
			contains("logout")
		case 32:
			contains("use_pkce: true", "code_challenge")
		case 54:
			contains("absolute_timeout", "wait:")
		}
	case "t/plugin/openid-connect2.t":
		switch number {
		case 2, 20, 21:
			contains("enable_encrypt_fields: true")
		case 12, 14:
			contains("claim_schema", "{{CAPTURE.")
		case 16, 17, 18, 19:
			contains("Host:")
		}
	case "t/plugin/openid-connect3.t":
		if number <= 2 {
			contains("proxy_opts")
		}
	case "t/plugin/openid-connect4.t":
		if number >= 3 {
			contains("required_scopes")
		}
	case "t/plugin/openid-connect6.t":
		if number >= 4 {
			contains("introspection_addon_headers")
		}
	case "t/plugin/openid-connect7.t":
		contains("claim_validator", "audience")
	case "t/plugin/openid-connect8.t":
		contains("issuer")
	case "t/plugin/openid-connect9.t":
		switch number {
		case 1:
			contains("$ENV://")
		case 2, 3, 4:
			contains("$secret://vault")
		case 5, 6:
			contains("claim_schema")
		}
	}
}

func assertOpenIDManifestHasNoAliasesOrMerges(t *testing.T, node *yaml.Node) {
	t.Helper()
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		t.Fatalf("manifest contains YAML anchor or alias %q", node.Value)
	}
	if node.Kind == yaml.ScalarNode && node.Value == "<<" {
		t.Fatal("manifest contains YAML merge key")
	}
	for _, child := range node.Content {
		assertOpenIDManifestHasNoAliasesOrMerges(t, child)
	}
}
