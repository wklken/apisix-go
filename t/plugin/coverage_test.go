package pluginintegration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	documentedPluginName   = regexp.MustCompile("`([^`]+)`")
	sourceTestHeader       = regexp.MustCompile(`^=== TEST\s+([0-9]+)`)
	upstreamSourceAbsences = map[string]string{
		"GM":              "no Apache APISIX t/plugin source at the pinned commit",
		"proxy-buffering": "no Apache APISIX t/plugin source at the pinned commit",
	}
)

const pinnedAPISIXSourceCommit = "c3d7d5ec69774121f53d2e20d29d09c816795dd7"

func TestSupportedPluginManifestSelection(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "plugins.md"))
	if err != nil {
		t.Fatalf("read docs/plugins.md: %v", err)
	}

	plugins, err := supportedPluginNames(data)
	if err != nil {
		t.Fatalf("supportedPluginNames() error = %v", err)
	}
	if got := len(plugins); got != 100 {
		t.Fatalf("supported plugins = %d, want 100", got)
	}

	manifests := make(map[string]bool, len(plugins)-len(upstreamSourceAbsences))
	for _, pluginName := range plugins {
		if _, absent := upstreamSourceAbsences[pluginName]; !absent {
			manifests[pluginName] = true
		}
	}
	if problems := manifestCoverageProblems(plugins, manifests); len(problems) != 0 {
		t.Fatalf("complete manifest set problems = %v", problems)
	}

	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	actual := make(map[string]bool, len(files))
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		if name == "redirect2" {
			name = "redirect"
		}
		actual[name] = true
	}
	if problems := manifestCoverageProblems(plugins, actual); len(problems) != 0 {
		t.Fatalf("checked-in manifest set problems = %v", problems)
	}

	delete(manifests, "redirect")
	problems := manifestCoverageProblems(plugins, manifests)
	if len(problems) != 1 || !strings.Contains(problems[0], "redirect") {
		t.Fatalf("missing manifest problems = %v, want redirect", problems)
	}

	manifests["redirect"] = true
	manifests["not-a-plugin"] = true
	problems = manifestCoverageProblems(plugins, manifests)
	if len(problems) != 1 || !strings.Contains(problems[0], "not-a-plugin") {
		t.Fatalf("extra manifest problems = %v, want not-a-plugin", problems)
	}
}

func TestManifestCorpusValidates(t *testing.T) {
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no manifests found")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := loadManifest(file, data); err != nil {
			t.Fatalf("load %s: %v", file, err)
		}
	}
}

func TestSAMLManifestHasIndependentSingletonCases(t *testing.T) {
	data, err := os.ReadFile("saml-auth.yaml")
	if err != nil {
		t.Fatalf("read saml-auth.yaml: %v", err)
	}
	if strings.Contains(string(data), "<<:") {
		t.Fatal("saml-auth.yaml uses YAML merge aliases instead of independent cases")
	}
	manifest, err := loadManifest("saml-auth.yaml", data)
	if err != nil {
		t.Fatalf("load saml-auth.yaml: %v", err)
	}
	if got := len(manifest.Cases); got != 21 {
		t.Fatalf("SAML cases = %d, want 21 independent source cases", got)
	}
	for _, source := range manifestSources(manifest) {
		got := make([]int, 0, source.Tests)
		for _, testCase := range manifest.Cases {
			if testCase.Source.File != source.File {
				continue
			}
			if len(testCase.Source.Tests) != 1 {
				t.Fatalf(
					"case %q source tests = %v, want one source block",
					testCase.Name,
					testCase.Source.Tests,
				)
			}
			got = append(got, testCase.Source.Tests[0])
		}
		sort.Ints(got)
		want := make([]int, source.Tests)
		for i := range want {
			want[i] = i + 1
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s tests = %v, want %v", source.File, got, want)
		}
	}
}

func TestAIRateLimitingManifestMapsExactlyOnePinnedBlockPerBehavioralCase(t *testing.T) {
	const manifestFile = "ai-rate-limiting.yaml"
	data, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatalf("read %s: %v", manifestFile, err)
	}
	manifest, err := loadManifest(manifestFile, data)
	if err != nil {
		t.Fatalf("load %s: %v", manifestFile, err)
	}
	if got := len(manifest.Cases); got != 58 {
		t.Fatalf("%s top-level cases = %d, want exactly 58 pinned behavioral cases", manifestFile, got)
	}

	next := map[string]int{
		"t/plugin/ai-rate-limiting-consumer-isolation.t": 1,
		"t/plugin/ai-rate-limiting-expression.t":         1,
		"t/plugin/ai-rate-limiting.t":                    1,
	}
	for i, testCase := range manifest.Cases {
		if len(testCase.Source.Tests) != 1 {
			t.Fatalf(
				"%s case %d %q maps %d source blocks, want exactly one",
				manifestFile,
				i+1,
				testCase.Name,
				len(testCase.Source.Tests),
			)
		}
		want, ok := next[testCase.Source.File]
		if !ok {
			t.Fatalf("%s case %d has unexpected source %q", manifestFile, i+1, testCase.Source.File)
		}
		if got := testCase.Source.Tests[0]; got != want {
			t.Fatalf(
				"%s case %d %q maps source test %d, want next source test %d",
				manifestFile,
				i+1,
				testCase.Name,
				got,
				want,
			)
		}
		next[testCase.Source.File]++
	}
	for file, got := range next {
		want := 6
		switch file {
		case "t/plugin/ai-rate-limiting-expression.t":
			want = 14
		case "t/plugin/ai-rate-limiting.t":
			want = 41
		}
		if got != want {
			t.Fatalf("%s mapped through test %d, want through test %d", file, got-1, want-1)
		}
	}
}

func TestSourceCoverage(t *testing.T) {
	sourceRoot := apacheAPISIXSourceRoot(t)
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no manifests found")
	}

	for _, manifestFile := range files {
		data, err := os.ReadFile(manifestFile)
		if err != nil {
			t.Fatalf("read %s: %v", manifestFile, err)
		}
		manifest, err := loadManifest(manifestFile, data)
		if err != nil {
			t.Fatalf("load %s: %v", manifestFile, err)
		}
		for _, source := range manifestSources(manifest) {
			if source.Repository != "https://github.com/apache/apisix" {
				t.Errorf(
					"%s source %s repository = %q, want Apache APISIX",
					manifestFile,
					source.File,
					source.Repository,
				)
			}
			if source.Commit != pinnedAPISIXSourceCommit {
				t.Errorf(
					"%s source %s commit = %q, want %s",
					manifestFile,
					source.File,
					source.Commit,
					pinnedAPISIXSourceCommit,
				)
			}
			assertPinnedSourceTests(t, sourceRoot, manifestFile, source)
		}
	}
}

func apacheAPISIXSourceRoot(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("APISIX_SOURCE_DIR")}
	if root := os.Getenv("APISIX_GO_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, ".cache", "apache-apisix"))
	}
	candidates = append(candidates, filepath.Join("..", "..", ".cache", "apache-apisix"))

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(candidate, ".git")); err == nil {
			assertPinnedSourceCheckout(t, candidate)
			return candidate
		}
	}
	t.Skip("pinned Apache APISIX source checkout is unavailable; set APISIX_SOURCE_DIR to run source coverage")
	return ""
}

func assertPinnedSourceCheckout(t *testing.T, root string) {
	t.Helper()
	command := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("read Apache APISIX source revision: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != pinnedAPISIXSourceCommit {
		t.Fatalf("Apache APISIX source revision = %s, want %s", got, pinnedAPISIXSourceCommit)
	}

	command = exec.Command("git", "-C", root, "status", "--short", "--untracked-files=no", "--", "t/plugin")
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect Apache APISIX source status: %v: %s", err, output)
	}
	if len(output) != 0 {
		t.Fatalf("Apache APISIX pinned t/plugin source is modified:\n%s", output)
	}
}

func assertPinnedSourceTests(t *testing.T, sourceRoot, manifestFile string, source SourceSpec) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(source.File)))
	if err != nil {
		t.Errorf("%s source %s: %v", manifestFile, source.File, err)
		return
	}
	headers, labels, err := parseSourceTestHeaders(data)
	if err != nil {
		t.Errorf("%s source %s: %v", manifestFile, source.File, err)
		return
	}
	if len(source.TestNumbers) == 0 {
		if headers != source.Tests {
			t.Errorf(
				"%s source %s declares %d tests, pinned source has %d TEST blocks",
				manifestFile,
				source.File,
				source.Tests,
				headers,
			)
		}
		return
	}
	for _, number := range source.TestNumbers {
		if !labels[number] {
			t.Errorf("%s source %s has no pinned TEST label %d", manifestFile, source.File, number)
		}
	}
}

func parseSourceTestHeaders(data []byte) (int, map[int]bool, error) {
	headers := 0
	labels := make(map[int]bool)
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "=== TEST ") {
			continue
		}
		headers++
		match := sourceTestHeader.FindStringSubmatch(line)
		if len(match) != 2 {
			return 0, nil, fmt.Errorf("cannot parse TEST header %q", line)
		}
		number, err := strconv.Atoi(match[1])
		if err != nil {
			return 0, nil, fmt.Errorf("parse TEST label %q: %w", match[1], err)
		}
		labels[number] = true
	}
	if headers == 0 {
		return 0, nil, fmt.Errorf("no TEST blocks found")
	}
	return headers, labels, nil
}

func TestParseSourceTestHeaders(t *testing.T) {
	headers, labels, err := parseSourceTestHeaders([]byte(strings.Join([]string{
		"=== TEST 1: first",
		"=== TEST 24b: letter suffix",
		"=== TEST 30 : space before colon",
	}, "\n")))
	if err != nil {
		t.Fatalf("parseSourceTestHeaders() error = %v", err)
	}
	if headers != 3 || !labels[1] || !labels[24] || !labels[30] {
		t.Fatalf("parseSourceTestHeaders() = (%d, %v), want three source labels", headers, labels)
	}

	if _, _, err := parseSourceTestHeaders([]byte("no source blocks")); err == nil {
		t.Fatal("parseSourceTestHeaders() accepted a source without TEST blocks")
	}
}

func manifestSources(manifest *Manifest) []SourceSpec {
	if len(manifest.Sources) > 0 {
		return manifest.Sources
	}
	return []SourceSpec{manifest.Source}
}

func TestManifestExercisesTargetPlugin(t *testing.T) {
	tests := []struct {
		name     string
		manifest *Manifest
		plugin   string
		want     bool
	}{
		{
			name: "route plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"acl": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   true,
		},
		{
			name: "global plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"global_rules": []any{map[string]any{"plugins": map[string]any{"error-page": map[string]any{}}}},
			}}}},
			plugin: "error-page",
			want:   true,
		},
		{
			name: "control plugin",
			manifest: &Manifest{Cases: []Case{{Runtime: map[string]any{
				"plugins": []any{"node-status"},
			}}}},
			plugin: "node-status",
			want:   true,
		},
		{
			name: "variant plugin",
			manifest: &Manifest{Cases: []Case{{Variants: []CaseVariant{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"ua-restriction": map[string]any{}}}},
			}}}}}},
			plugin: "ua-restriction",
			want:   true,
		},
		{
			name: "fixture proxy placeholder",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"uri": "/*", "upstream": map[string]any{}}},
			}}}},
			plugin: "saml-auth",
			want:   false,
		},
		{
			name: "unrelated plugin",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"mocking": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := manifestExercisesPlugin(test.manifest, test.plugin); got != test.want {
				t.Fatalf("manifestExercisesPlugin() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestManifestCorpusExercisesTargetPlugins(t *testing.T) {
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	for _, file := range files {
		pluginName := manifestPluginName(file)
		t.Run(pluginName, func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			manifest, err := loadManifest(file, data)
			if err != nil {
				t.Fatalf("load %s: %v", file, err)
			}
			assertManifestExercisesTargetPlugin(t, file, manifest, pluginName)
		})
	}
}

func manifestPluginName(file string) string {
	pluginName := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if pluginName == "redirect2" {
		return "redirect"
	}
	return pluginName
}

func assertManifestExercisesTargetPlugin(t *testing.T, file string, manifest *Manifest, pluginName string) {
	t.Helper()
	if !manifestExercisesPlugin(manifest, pluginName) {
		t.Errorf("%s never activates target plugin %q", file, pluginName)
	}
	for caseIndex := range manifest.Cases {
		caseSpec := &manifest.Cases[caseIndex]
		if len(caseSpec.Variants) == 0 {
			if !scenarioExercisesPlugin(caseSpec.Runtime, caseSpec.Config, pluginName) {
				t.Errorf("%s case %q never activates target plugin %q", file, caseSpec.Name, pluginName)
			}
			continue
		}
		for variantIndex := range caseSpec.Variants {
			variant := &caseSpec.Variants[variantIndex]
			if !scenarioExercisesPlugin(variant.Runtime, variant.Config, pluginName) {
				t.Errorf(
					"%s case %q variant %q never activates target plugin %q",
					file,
					caseSpec.Name,
					variant.Name,
					pluginName,
				)
			}
		}
	}
}

func manifestExercisesPlugin(manifest *Manifest, pluginName string) bool {
	for i := range manifest.Cases {
		caseSpec := &manifest.Cases[i]
		if scenarioExercisesPlugin(caseSpec.Runtime, caseSpec.Config, pluginName) {
			return true
		}
		for j := range caseSpec.Variants {
			variant := &caseSpec.Variants[j]
			if scenarioExercisesPlugin(variant.Runtime, variant.Config, pluginName) {
				return true
			}
		}
	}
	return false
}

func scenarioExercisesPlugin(runtime, config map[string]any, pluginName string) bool {
	switch plugins := runtime["plugins"].(type) {
	case []any:
		for _, configured := range plugins {
			if configured == pluginName {
				return true
			}
		}
	case []string:
		if slices.Contains(plugins, pluginName) {
			return true
		}
	}
	return configContainsPlugin(config, pluginName)
}

func configContainsPlugin(value any, pluginName string) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			if key == "plugins" {
				if plugins, ok := nested.(map[string]any); ok {
					if _, configured := plugins[pluginName]; configured {
						return true
					}
				}
			}
			if configContainsPlugin(nested, pluginName) {
				return true
			}
		}
	case []any:
		for _, nested := range current {
			if configContainsPlugin(nested, pluginName) {
				return true
			}
		}
	}
	return false
}

func supportedPluginNames(data []byte) ([]string, error) {
	var plugins []string
	seen := make(map[string]bool)
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "|---") {
			continue
		}
		fields := strings.Split(strings.Trim(line, "|"), "|")
		if len(fields) < 6 || strings.TrimSpace(fields[3]) != "yes" ||
			!strings.HasPrefix(strings.TrimSpace(fields[5]), "Supported") {
			continue
		}
		match := documentedPluginName.FindStringSubmatch(fields[1])
		if len(match) != 2 {
			return nil, fmt.Errorf("supported plugin row has no backtick name: %s", line)
		}
		if seen[match[1]] {
			return nil, fmt.Errorf("supported plugin %q is duplicated", match[1])
		}
		seen[match[1]] = true
		plugins = append(plugins, match[1])
	}
	if len(plugins) == 0 {
		return nil, fmt.Errorf("no supported plugin rows found")
	}
	return plugins, nil
}

func manifestCoverageProblems(plugins []string, manifests map[string]bool) []string {
	selected := make(map[string]bool, len(plugins))
	var problems []string
	for _, pluginName := range plugins {
		selected[pluginName] = true
		if _, absent := upstreamSourceAbsences[pluginName]; absent {
			if manifests[pluginName] {
				problems = append(problems, fmt.Sprintf("source-absence plugin %s has a manifest", pluginName))
			}
			continue
		}
		if !manifests[pluginName] {
			problems = append(problems, fmt.Sprintf("supported plugin %s has no manifest", pluginName))
		}
	}
	for pluginName := range upstreamSourceAbsences {
		if !selected[pluginName] {
			problems = append(problems, fmt.Sprintf("source-absence plugin %s is not selected", pluginName))
		}
	}
	for pluginName := range manifests {
		if !selected[pluginName] {
			problems = append(problems, fmt.Sprintf("manifest %s is not a supported plugin", pluginName))
		}
	}
	sort.Strings(problems)
	return problems
}
