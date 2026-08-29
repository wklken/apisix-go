package pluginintegration

import (
	"bytes"
	"errors"
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

	"github.com/wklken/apisix-go/pkg/capability"
	"go.yaml.in/yaml/v3"
)

var (
	sourceTestHeader           = regexp.MustCompile(`^=== TEST\s+([0-9]+)`)
	manifestTargetPluginGroups = map[string][]string{
		"ai-proxy": {"ai-proxy-multi"},
	}
)

func manifestYAMLFiles() ([]string, error) {
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(files, func(file string) bool {
		return file == corpusScopeFile
	}), nil
}

func TestCapabilityManifestSelection(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	files, err := manifestYAMLFiles()
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	problems, factoryCount := capabilityManifestSelectionProblems(manifest, files)
	if len(problems) != 0 {
		t.Fatalf("capability manifest selection problems = %v", problems)
	}
	t.Logf("capability selection: %d manifest files cover %d factory keys", len(files), factoryCount)
}

func TestAllPluginProfileHasConvertedEvidenceForEveryFactory(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	profile, ok := manifest.Qualification("apisix-3.17-all-plugins-v1")
	if !ok {
		t.Fatal("apisix-3.17-all-plugins-v1 qualification profile is missing")
	}

	var problems []string
	factoryCount := 0
	for _, pluginName := range profile.RequiredPlugins {
		plugin, found := manifest.Plugin(pluginName)
		if !found {
			problems = append(problems, "missing capability "+pluginName)
			continue
		}
		if plugin.Evidence.Upstream.State != capability.EvidenceVerified &&
			plugin.Evidence.Upstream.State != capability.EvidenceNotApplicable {
			problems = append(
				problems,
				fmt.Sprintf("%s converted_upstream=%s", pluginName, plugin.Evidence.Upstream.State),
			)
		}
		factoryCount += len(plugin.Factories)
		if len(plugin.Factories) <= 1 {
			continue
		}
		aliasEvidence := false
		for _, ref := range plugin.Evidence.Upstream.Refs {
			if strings.HasPrefix(ref, "pkg/plugin/init_test.go#TestNewPreservesHistoricalFactoryAliases") {
				aliasEvidence = true
				break
			}
		}
		if !aliasEvidence {
			problems = append(problems, pluginName+" has multiple factories without direct alias evidence")
		}
	}

	sort.Strings(problems)
	if len(problems) != 0 {
		t.Fatalf("all-plugin converted evidence problems = %v", problems)
	}
	t.Logf(
		"all-plugin converted evidence covers %d capabilities and %d factory keys",
		len(profile.RequiredPlugins),
		factoryCount,
	)
}

func TestCapabilityManifestSelectionRequiresCanonicalManifest(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	files, err := manifestYAMLFiles()
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	files = slices.DeleteFunc(files, func(file string) bool {
		return filepath.Base(file) == "redirect.yaml"
	})

	problems, _ := capabilityManifestSelectionProblems(manifest, files)
	if !slices.Contains(problems, "missing manifest redirect.yaml") {
		t.Fatalf("selection problems = %v, want missing canonical redirect.yaml", problems)
	}
}

func capabilityManifestSelectionProblems(manifest *capability.Manifest, files []string) ([]string, int) {
	expectedManifests := make(map[string]bool)
	expectedFactories := make(map[string]bool)
	factoriesByManifest := make(map[string][]string)
	for _, plugin := range manifest.Plugins {
		for _, ref := range plugin.Evidence.Upstream.Refs {
			if !strings.HasPrefix(ref, "t/plugin/") || filepath.Ext(ref) != ".yaml" ||
				filepath.Base(ref) == corpusScopeFile {
				continue
			}
			manifestName := filepath.Base(ref)
			expectedManifests[manifestName] = true
			for _, factory := range plugin.Factories {
				expectedFactories[factory.Key] = true
				factoriesByManifest[manifestName] = append(factoriesByManifest[manifestName], factory.Key)
			}
		}
	}
	expectedManifests["redirect2.yaml"] = true

	actualManifests := make(map[string]bool, len(files))
	for _, file := range files {
		actualManifests[filepath.Base(file)] = true
	}

	var problems []string
	for manifestName := range expectedManifests {
		if !actualManifests[manifestName] {
			problems = append(problems, "missing manifest "+manifestName)
		}
	}
	for manifestName := range actualManifests {
		if !expectedManifests[manifestName] {
			problems = append(problems, "unexpected manifest "+manifestName)
		}
	}

	actualFactories := make(map[string]bool, len(expectedFactories))
	for manifestName := range actualManifests {
		factoryManifest := manifestName
		if factoryManifest == "redirect2.yaml" {
			factoryManifest = "redirect.yaml"
		}
		for _, factory := range factoriesByManifest[factoryManifest] {
			actualFactories[factory] = true
		}
	}
	for factory := range expectedFactories {
		if !actualFactories[factory] {
			problems = append(problems, "missing factory "+factory)
		}
	}
	for factory := range actualFactories {
		if !expectedFactories[factory] {
			problems = append(problems, "unexpected factory "+factory)
		}
	}
	sort.Strings(problems)
	return problems, len(expectedFactories)
}

func TestManifestCorpusValidates(t *testing.T) {
	files, err := manifestYAMLFiles()
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
	if strings.Contains(string(data), "<<:") {
		t.Fatalf("%s contains YAML merge keys", manifestFile)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode %s syntax tree: %v", manifestFile, err)
	}
	if node := firstYAMLAnchorOrAlias(&document); node != nil {
		t.Fatalf("%s contains YAML anchor or alias %q", manifestFile, node.Value)
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

func firstYAMLAnchorOrAlias(node *yaml.Node) *yaml.Node {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return node
	}
	for _, child := range node.Content {
		if found := firstYAMLAnchorOrAlias(child); found != nil {
			return found
		}
	}
	return nil
}

func TestSourceCoverage(t *testing.T) {
	scope, err := loadCorpusScopeFile(t)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	sourceRoot := apacheAPISIXRepository(t)
	for _, commit := range sourceCoverageCommits(scope) {
		assertSourceCommit(t, sourceRoot, commit)
	}
	sourceCommits := sourceCommitsByFile(scope)
	files, err := manifestYAMLFiles()
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
			if source.RegressionOnly {
				assertSourceCommit(t, sourceRoot, source.Commit)
				assertSourceTestsAtCommit(t, sourceRoot, manifestFile, source)
				continue
			}
			expectedCommit, ok := sourceCommits[source.File]
			if !ok {
				t.Errorf("%s source %s is absent from the corpus ledger", manifestFile, source.File)
				continue
			}
			if source.Commit != expectedCommit {
				t.Errorf(
					"%s source %s commit = %q, want %s",
					manifestFile,
					source.File,
					source.Commit,
					expectedCommit,
				)
			}
			assertSourceTestsAtCommit(t, sourceRoot, manifestFile, source)
		}
	}
}

func sourceCommitsByFile(scope *corpusScope) map[string]string {
	commits := make(map[string]string, len(scope.Sources))
	for _, source := range scope.Sources {
		if source.Disposition == "regression_test" || source.Disposition == "post_target" {
			continue
		}
		commits[source.File] = scope.effectiveCommit(source)
	}
	return commits
}

func sourceCoverageCommits(scope *corpusScope) []string {
	commits := map[string]bool{scope.Commit: true}
	for _, source := range scope.Sources {
		commits[scope.effectiveCommit(source)] = true
	}
	result := make([]string, 0, len(commits))
	for commit := range commits {
		result = append(result, commit)
	}
	sort.Strings(result)
	return result
}

func TestSourceCoverageCommitsIncludeMigratedSources(t *testing.T) {
	scope := &corpusScope{
		Commit: strings.Repeat("a", 40),
		Sources: []corpusSourceScope{
			{File: "t/plugin/legacy.t", TestNumbers: []int{1}},
			{File: "t/plugin/migrated.t", Commit: strings.Repeat("b", 40), TestNumbers: []int{1}},
		},
	}
	want := []string{strings.Repeat("a", 40), strings.Repeat("b", 40)}
	if got := sourceCoverageCommits(scope); !slices.Equal(got, want) {
		t.Fatalf("sourceCoverageCommits() = %v, want %v", got, want)
	}
}

func TestSourceFileAtCommitReadsExactRevision(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "--quiet")
	runGit("config", "user.email", "corpus-test@example.invalid")
	runGit("config", "user.name", "Corpus Test")
	path := filepath.Join(root, "t", "plugin", "example.t")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("=== TEST 1: historical\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "t/plugin/example.t")
	runGit("commit", "--quiet", "-m", "historical")
	historicalCommit := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte("=== TEST 1: migrated\n=== TEST 2: added\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addedPath := filepath.Join(root, "t", "plugin", "added.t")
	if err := os.WriteFile(addedPath, []byte("=== TEST 1: added source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "t/plugin/example.t", "t/plugin/added.t")
	runGit("commit", "--quiet", "-m", "migrated")
	migratedCommit := runGit("rev-parse", "HEAD")

	historical, err := sourceFileAtCommit(root, historicalCommit, "t/plugin/example.t")
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := sourceFileAtCommit(root, migratedCommit, "t/plugin/example.t")
	if err != nil {
		t.Fatal(err)
	}
	if string(historical) != "=== TEST 1: historical\n" {
		t.Fatalf("historical source = %q", historical)
	}
	if string(migrated) != "=== TEST 1: migrated\n=== TEST 2: added\n" {
		t.Fatalf("migrated source = %q", migrated)
	}
	historicalFiles, err := sourceFilesAtCommit(root, historicalCommit)
	if err != nil {
		t.Fatal(err)
	}
	migratedFiles, err := sourceFilesAtCommit(root, migratedCommit)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"t/plugin/example.t"}; !slices.Equal(historicalFiles, want) {
		t.Fatalf("historical source files = %v, want %v", historicalFiles, want)
	}
	if want := []string{"t/plugin/added.t", "t/plugin/example.t"}; !slices.Equal(migratedFiles, want) {
		t.Fatalf("migrated source files = %v, want %v", migratedFiles, want)
	}
}

func sourceFileAtCommit(root, commit, file string) ([]byte, error) {
	command := exec.Command("git", "-C", root, "show", commit+":"+file)
	output, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("read %s at %s: %w: %s", file, commit, err, exitErr.Stderr)
		}
		return nil, fmt.Errorf("read %s at %s: %w", file, commit, err)
	}
	return output, nil
}

func sourceFilesAtCommit(root, commit string) ([]string, error) {
	command := exec.Command("git", "-C", root, "ls-tree", "-r", "--name-only", "-z", commit, "--", "t/plugin")
	output, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("list t/plugin at %s: %w: %s", commit, err, exitErr.Stderr)
		}
		return nil, fmt.Errorf("list t/plugin at %s: %w", commit, err)
	}
	files := make([]string, 0)
	for raw := range bytes.SplitSeq(output, []byte{0}) {
		file := string(raw)
		if filepath.Ext(file) == ".t" {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	return files, nil
}

func apacheAPISIXRepository(t *testing.T) string {
	t.Helper()
	if sourceRoot, ok := optionalApacheAPISIXRepository(); ok {
		return sourceRoot
	}
	t.Skip("Apache APISIX source repository is unavailable; set APISIX_SOURCE_DIR to run source coverage")
	return ""
}

func optionalApacheAPISIXRepository() (string, bool) {
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
			return candidate, true
		}
	}
	return "", false
}

func assertSourceCommit(t *testing.T, root, commit string) {
	t.Helper()
	command := exec.Command("git", "-C", root, "cat-file", "-e", commit+"^{commit}")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Apache APISIX source commit %s is unavailable: %v: %s", commit, err, output)
	}
}

func assertSourceTestsAtCommit(t *testing.T, sourceRoot, manifestFile string, source SourceSpec) {
	t.Helper()
	data, err := sourceFileAtCommit(sourceRoot, source.Commit, source.File)
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
			name: "explicit manifest target alias",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"ai-proxy-multi": map[string]any{}}}},
			}}}},
			plugin: "ai-proxy",
			want:   true,
		},
		{
			name: "canonical factory alias",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"otel": map[string]any{}}}},
			}}}},
			plugin: "opentelemetry",
			want:   true,
		},
		{
			name: "step config plugin",
			manifest: &Manifest{Cases: []Case{{
				Config: map[string]any{
					"routes": []any{map[string]any{"plugins": map[string]any{"mocking": map[string]any{}}}},
				},
				Steps: []CaseStep{{Config: map[string]any{
					"routes": []any{map[string]any{"plugins": map[string]any{"ai-proxy": map[string]any{}}}},
				}}},
			}}},
			plugin: "ai-proxy",
			want:   true,
		},
		{
			name: "manifest target alias is narrow",
			manifest: &Manifest{Cases: []Case{{Config: map[string]any{
				"routes": []any{map[string]any{"plugins": map[string]any{"ai-proxy-multi": map[string]any{}}}},
			}}}},
			plugin: "acl",
			want:   false,
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
			pluginNames := targetPluginActivationNames(t, test.plugin)
			if got := manifestExercisesPlugin(test.manifest, pluginNames); got != test.want {
				t.Fatalf("manifestExercisesPlugin() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestManifestCorpusExercisesTargetPlugins(t *testing.T) {
	files, err := manifestYAMLFiles()
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
	pluginNames := targetPluginActivationNames(t, pluginName)
	if !manifestExercisesPlugin(manifest, pluginNames) {
		t.Errorf("%s never activates target plugin %q", file, pluginName)
	}
	for caseIndex := range manifest.Cases {
		caseSpec := &manifest.Cases[caseIndex]
		if len(caseSpec.Variants) == 0 {
			activates := caseExercisesTargetPlugin(
				caseSpec.Runtime,
				caseSpec.Config,
				caseSpec.Steps,
				pluginNames,
			)
			if err := validateTargetPluginExemption(caseSpec.TargetPluginExemptReason, activates); err != nil {
				t.Errorf("%s case %q target plugin %q: %v", file, caseSpec.Name, pluginName, err)
			}
			continue
		}
		for variantIndex := range caseSpec.Variants {
			variant := &caseSpec.Variants[variantIndex]
			activates := caseExercisesTargetPlugin(
				variant.Runtime,
				variant.Config,
				variant.Steps,
				pluginNames,
			)
			if err := validateTargetPluginExemption(variant.TargetPluginExemptReason, activates); err != nil {
				t.Errorf(
					"%s case %q variant %q target plugin %q: %v",
					file,
					caseSpec.Name,
					variant.Name,
					pluginName,
					err,
				)
			}
		}
	}
}

func validateTargetPluginExemption(reason string, activates bool) error {
	if activates {
		if strings.TrimSpace(reason) != "" {
			return fmt.Errorf("target_plugin_exempt_reason must be empty when the target plugin is activated")
		}
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("target_plugin_exempt_reason is required when the target plugin is not activated")
	}
	return nil
}

func TestTargetPluginExemptionRequiresReasonForInactiveScenario(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{name: "missing reason", want: "target_plugin_exempt_reason is required"},
		{name: "blank reason", reason: "  \t", want: "target_plugin_exempt_reason is required"},
		{name: "valid reason", reason: "intentional negative coverage case"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTargetPluginExemption(tt.reason, false)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validateTargetPluginExemption() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateTargetPluginExemption() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestTargetPluginExemptionRejectedWhenScenarioActivatesTarget(t *testing.T) {
	tests := []struct {
		name     string
		runtime  map[string]any
		config   map[string]any
		steps    []CaseStep
		exempted string
	}{
		{
			name: "case",
			config: map[string]any{
				"routes": []any{map[string]any{
					"plugins": map[string]any{"error-log-logger": map[string]any{}},
				}},
			},
			exempted: "stale exemption",
		},
		{
			name: "variant",
			runtime: map[string]any{
				"plugins": []any{"error-log-logger"},
			},
			exempted: "stale exemption",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pluginNames := targetPluginActivationNames(t, "error-log-logger")
			if !caseExercisesTargetPlugin(tt.runtime, tt.config, tt.steps, pluginNames) {
				t.Fatal("caseExercisesTargetPlugin() = false, want active target plugin")
			}
			err := validateTargetPluginExemption(tt.exempted, true)
			if err == nil || !strings.Contains(err.Error(), "must be empty") {
				t.Fatalf("validateTargetPluginExemption() error = %v, want stale exemption rejection", err)
			}
		})
	}
}

func targetPluginActivationNames(t *testing.T, pluginName string) []string {
	t.Helper()
	capabilities, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	plugin, found := capabilities.Plugin(pluginName)
	if !found {
		t.Fatalf("capability plugin %q is missing", pluginName)
	}
	names := []string{pluginName}
	for _, factory := range plugin.Factories {
		names = append(names, factory.Key)
	}
	names = append(names, manifestTargetPluginGroups[pluginName]...)
	sort.Strings(names)
	return slices.Compact(names)
}

func manifestExercisesPlugin(manifest *Manifest, pluginNames []string) bool {
	for i := range manifest.Cases {
		caseSpec := &manifest.Cases[i]
		if caseExercisesTargetPlugin(caseSpec.Runtime, caseSpec.Config, caseSpec.Steps, pluginNames) {
			return true
		}
		for j := range caseSpec.Variants {
			variant := &caseSpec.Variants[j]
			if caseExercisesTargetPlugin(variant.Runtime, variant.Config, variant.Steps, pluginNames) {
				return true
			}
		}
	}
	return false
}

func caseExercisesTargetPlugin(
	runtime, config map[string]any,
	steps []CaseStep,
	pluginNames []string,
) bool {
	if scenarioExercisesTargetPlugin(runtime, config, pluginNames) {
		return true
	}
	for i := range steps {
		if scenarioExercisesTargetPlugin(nil, steps[i].Config, pluginNames) {
			return true
		}
	}
	return false
}

func scenarioExercisesTargetPlugin(runtime, config map[string]any, pluginNames []string) bool {
	for _, candidate := range pluginNames {
		if scenarioExercisesPlugin(runtime, config, candidate) {
			return true
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
