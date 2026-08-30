package pluginintegration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"go.yaml.in/yaml/v3"
)

type corpusScope struct {
	Commit  string              `yaml:"commit"`
	Sources []corpusSourceScope `yaml:"sources"`
}

type corpusSourceScope struct {
	File        string `yaml:"file"`
	Commit      string `yaml:"commit,omitempty"`
	TestNumbers []int  `yaml:"test_numbers"`
	Owner       string `yaml:"owner"`
	Disposition string `yaml:"disposition"`
	Manifest    string `yaml:"manifest,omitempty"`
	Evidence    string `yaml:"evidence,omitempty"`
	Reason      string `yaml:"reason,omitempty"`
}

type corpusLabelSelection struct {
	Commit string
}

var corpusDispositions = map[string]bool{
	"converted":       true,
	"package_test":    true,
	"dependency_test": true,
	"platform_test":   true,
	"platform_gap":    true,
	"regression_test": true,
	"post_target":     true,
	"pending":         true,
	"blocked_runtime": true,
	"blocked_design":  true,
	"non_plugin":      true,
}

var gitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

const corpusScopeFile = "corpus_scope.yaml"

func loadCorpusScope(name string, data []byte) (*corpusScope, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var scope corpusScope
	if err := decoder.Decode(&scope); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode %s: multiple YAML documents are not supported", name)
		}
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	if err := scope.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", name, err)
	}
	return &scope, nil
}

func (s *corpusScope) validate() error {
	if strings.TrimSpace(s.Commit) == "" {
		return errors.New("commit is required")
	}
	if !gitCommitPattern.MatchString(s.Commit) {
		return fmt.Errorf("commit %q must be a lowercase 40-character Git object ID", s.Commit)
	}
	seen := make(map[string]map[int]string, len(s.Sources))
	validationCommits := make(map[string]string, len(s.Sources))
	for i := range s.Sources {
		source := &s.Sources[i]
		if strings.TrimSpace(source.File) == "" {
			return fmt.Errorf("source %d file is required", i+1)
		}
		if source.Commit != "" && !gitCommitPattern.MatchString(source.Commit) {
			return fmt.Errorf(
				"source %q commit %q must be a lowercase 40-character Git object ID",
				source.File,
				source.Commit,
			)
		}
		effectiveCommit := s.effectiveCommit(*source)
		if source.Disposition != "regression_test" && source.Disposition != "post_target" {
			if previous, ok := validationCommits[source.File]; ok && previous != effectiveCommit {
				return fmt.Errorf(
					"source %q mixes validation commits %s and %s",
					source.File,
					previous,
					effectiveCommit,
				)
			}
			validationCommits[source.File] = effectiveCommit
		}
		if strings.TrimSpace(source.Owner) == "" {
			return fmt.Errorf("source %q owner is required", source.File)
		}
		if !corpusDispositions[source.Disposition] {
			return fmt.Errorf(
				"source %q disposition %q is not allowed; want one of converted, package_test, dependency_test, platform_test, platform_gap, regression_test, post_target, pending, blocked_runtime, blocked_design, non_plugin",
				source.File,
				source.Disposition,
			)
		}
		if (source.Disposition == "converted" || source.Disposition == "regression_test") &&
			strings.TrimSpace(source.Manifest) == "" {
			return fmt.Errorf("source %q is %s but has no manifest", source.File, source.Disposition)
		}
		if (source.Disposition == "package_test" || source.Disposition == "dependency_test" || source.Disposition == "platform_test") &&
			strings.TrimSpace(source.Evidence) == "" {
			return fmt.Errorf("source %q is %s but has no evidence", source.File, source.Disposition)
		}
		if source.Disposition != "converted" && strings.TrimSpace(source.Reason) == "" {
			return fmt.Errorf("source %q disposition %q requires a reason", source.File, source.Disposition)
		}
		if len(source.TestNumbers) == 0 {
			return fmt.Errorf("source %q test_numbers must not be empty", source.File)
		}
		if seen[source.File] == nil {
			seen[source.File] = make(map[int]string, len(source.TestNumbers))
		}
		for _, number := range source.TestNumbers {
			if number <= 0 {
				return fmt.Errorf("source %q test_numbers must be positive", source.File)
			}
			if previous, ok := seen[source.File][number]; ok {
				return fmt.Errorf("source label %d in %s is duplicated by %q", number, source.File, previous)
			}
			seen[source.File][number] = source.Owner
		}
	}
	return nil
}

func (s *corpusScope) effectiveCommit(source corpusSourceScope) string {
	if source.Commit != "" {
		return source.Commit
	}
	return s.Commit
}

func testCorpusCommit() string {
	return strings.Repeat("a", 40)
}

func TestCorpusScopeRejectsMissingSourceLabel(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    owner: example-plugin",
		"    disposition: pending",
		"    reason: no source label",
	}, "\n"))
	if _, err := loadCorpusScope("test", data); err == nil {
		t.Fatal("loadCorpusScope() accepted a source with no labels")
	}
}

func TestCorpusScopeRejectsDuplicateSourceLabel(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1, 2]",
		"    owner: example-plugin",
		"    disposition: pending",
		"    reason: first row",
		"  - file: t/plugin/example.t",
		"    test_numbers: [2, 3]",
		"    owner: other-plugin",
		"    disposition: pending",
		"    reason: duplicate label",
	}, "\n"))
	_, err := loadCorpusScope("test", data)
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("loadCorpusScope() error = %v, want duplicate label error", err)
	}
}

func TestCorpusScopeRejectsUnknownDisposition(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: done",
		"    reason: not an allowed disposition",
	}, "\n"))
	_, err := loadCorpusScope("test", data)
	if err == nil || !strings.Contains(err.Error(), "disposition") {
		t.Fatalf("loadCorpusScope() error = %v, want unknown disposition error", err)
	}
}

func TestCorpusScopeRequiresManifestForConverted(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: converted",
		"    reason: missing manifest",
	}, "\n"))
	_, err := loadCorpusScope("test", data)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("loadCorpusScope() error = %v, want missing manifest error", err)
	}
}

func TestCorpusScopeAllowsRegressionTestWithManifest(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: regression_test",
		"    manifest: example.yaml",
		"    reason: post-target regression coverage",
	}, "\n"))
	if _, err := loadCorpusScope("test", data); err != nil {
		t.Fatalf("loadCorpusScope() rejected regression test evidence: %v", err)
	}
}

func TestCorpusScopeRequiresEvidenceForPackageTest(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: package_test",
		"    reason: missing evidence",
	}, "\n"))
	_, err := loadCorpusScope("test", data)
	if err == nil || !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("loadCorpusScope() error = %v, want missing package-test evidence error", err)
	}
}

func TestCorpusScopeRequiresEvidenceForDependencyTest(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: dependency_test",
		"    reason: missing evidence",
	}, "\n"))
	_, err := loadCorpusScope("test", data)
	if err == nil || !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("loadCorpusScope() error = %v, want missing dependency-test evidence error", err)
	}
}

func TestCorpusScopeAllowsExplicitPlatformGapWithoutPluginEvidence(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: generation-runtime",
		"    disposition: platform_gap",
		"    reason: config publication lifecycle is not plugin behavior",
	}, "\n"))
	if _, err := loadCorpusScope("test", data); err != nil {
		t.Fatalf("loadCorpusScope() rejected explicit platform gap: %v", err)
	}
}

func TestCorpusScopeRequiresReasonForNonConverted(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: pending",
	}, "\n"))
	_, err := loadCorpusScope("test", data)
	if err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("loadCorpusScope() error = %v, want missing reason error", err)
	}
}

func TestCorpusScopeRejectsMalformedCommit(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: not-a-git-object-id",
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: pending",
		"    reason: malformed commit",
	}, "\n"))
	if _, err := loadCorpusScope("test", data); err == nil {
		t.Fatal("loadCorpusScope() accepted a malformed commit")
	}
}

func TestCorpusScopeAllowsPerSourceMigration(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    commit: " + strings.Repeat("b", 40),
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: converted",
		"    manifest: example.yaml",
	}, "\n"))
	if _, err := loadCorpusScope("test", data); err != nil {
		t.Fatalf("loadCorpusScope() rejected a per-source migration commit: %v", err)
	}
}

func TestCorpusScopeRejectsMalformedPerSourceCommit(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    commit: not-a-git-object-id",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: converted",
		"    manifest: example.yaml",
	}, "\n"))
	if _, err := loadCorpusScope("test", data); err == nil {
		t.Fatal("loadCorpusScope() accepted a malformed per-source commit")
	}
}

func TestCorpusScopeAllowsHistoricalRegressionLabelsBesideMigratedTarget(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    commit: " + strings.Repeat("b", 40),
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: pending",
		"    reason: migrated row",
		"  - file: t/plugin/example.t",
		"    test_numbers: [2]",
		"    owner: example-plugin",
		"    disposition: regression_test",
		"    manifest: example.yaml",
		"    reason: historical row",
	}, "\n"))
	if _, err := loadCorpusScope("test", data); err != nil {
		t.Fatalf("loadCorpusScope() rejected disjoint target and regression commits: %v", err)
	}
}

func TestCorpusScopeRejectsMixedValidationCommitsWithinSourceFile(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    commit: " + strings.Repeat("b", 40),
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: pending",
		"    reason: first validation row",
		"  - file: t/plugin/example.t",
		"    test_numbers: [2]",
		"    owner: example-plugin",
		"    disposition: pending",
		"    reason: second validation row",
	}, "\n"))
	_, err := loadCorpusScope("test", data)
	if err == nil || !strings.Contains(err.Error(), "mixes validation commits") {
		t.Fatalf("loadCorpusScope() error = %v, want mixed validation commit error", err)
	}
}

func TestManifestSelectionsUseEffectiveCorpusCommit(t *testing.T) {
	historicalCommit := "c3d7d5ec69774121f53d2e20d29d09c816795dd7"
	targetCommit := strings.Repeat("b", 40)
	scope := &corpusScope{
		Commit: historicalCommit,
		Sources: []corpusSourceScope{{
			File:        "t/plugin/redirect2.t",
			Commit:      targetCommit,
			TestNumbers: []int{1, 2, 3},
			Owner:       "redirect",
			Disposition: "converted",
			Manifest:    "redirect2.yaml",
		}},
	}
	data, err := os.ReadFile("redirect2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadManifest("redirect2.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte(manifest.Source.Commit), []byte(targetCommit))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "redirect2.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	selections, err := loadManifestSelections(root, scope)
	if err != nil {
		t.Fatalf("loadManifestSelections() rejected a migrated manifest: %v", err)
	}
	if got := selections["t/plugin/redirect2.t"][3]; got != "redirect2.yaml" {
		t.Fatalf("selection owner = %q, want redirect2.yaml", got)
	}
}

func TestManifestSelectionsExcludeRegressionOnlySources(t *testing.T) {
	const (
		targetCommit = "1111111111111111111111111111111111111111"
		sourceFile   = "t/plugin/example.t"
	)
	scope := &corpusScope{
		Commit: targetCommit,
		Sources: []corpusSourceScope{{
			File:        sourceFile,
			TestNumbers: []int{1},
			Owner:       "example",
			Disposition: "converted",
			Manifest:    "example.yaml",
		}},
	}
	manifest := []byte(`sources:
  - repository: https://github.com/apache/apisix
    commit: 1111111111111111111111111111111111111111
    file: t/plugin/example.t
    tests: 1
    test_numbers: [1]
  - repository: https://github.com/apache/apisix
    commit: 2222222222222222222222222222222222222222
    file: t/plugin/example.t
    tests: 1
    test_numbers: [2]
    regression_only: true
cases:
  - name: target
    source: {file: t/plugin/example.t, tests: [1]}
    config: {routes: []}
    input: {path: /target}
    output: {status: 200}
  - name: regression
    source: {file: t/plugin/example.t, tests: [2]}
    config: {routes: []}
    input: {path: /regression}
    output: {status: 200}
`)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "example.yaml"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	selections, err := loadManifestSelections(root, scope)
	if err != nil {
		t.Fatalf("loadManifestSelections() error = %v", err)
	}
	if got := selections[sourceFile][1]; got != "example.yaml" {
		t.Fatalf("target selection = %q, want example.yaml", got)
	}
	if _, ok := selections[sourceFile][2]; ok {
		t.Fatal("regression-only source label was counted as validation evidence")
	}
	regressions, err := loadManifestRegressionSelections(root, scope)
	if err != nil {
		t.Fatalf("loadManifestRegressionSelections() error = %v", err)
	}
	if _, ok := regressions[sourceFile][2]; ok {
		t.Fatal("post-migration regression label absent from the validation ledger was imported")
	}
}

func TestManifestSelectionsRejectMixedEffectiveCommits(t *testing.T) {
	historicalCommit := "c3d7d5ec69774121f53d2e20d29d09c816795dd7"
	targetCommit := strings.Repeat("b", 40)
	scope := &corpusScope{
		Commit: historicalCommit,
		Sources: []corpusSourceScope{
			{
				File:        "t/plugin/redirect2.t",
				Commit:      targetCommit,
				TestNumbers: []int{1, 2},
				Owner:       "redirect",
				Disposition: "converted",
				Manifest:    "redirect2.yaml",
			},
			{
				File:        "t/plugin/redirect2.t",
				TestNumbers: []int{3},
				Owner:       "redirect",
				Disposition: "converted",
				Manifest:    "redirect2.yaml",
			},
		},
	}
	data, err := os.ReadFile("redirect2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadManifest("redirect2.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte(manifest.Source.Commit), []byte(targetCommit))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "redirect2.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = loadManifestSelections(root, scope)
	if err == nil || !strings.Contains(err.Error(), "label 3 commit") {
		t.Fatalf("loadManifestSelections() error = %v, want mixed effective commit rejection", err)
	}
}

func TestCorpusScopeRejectsUnknownFields(t *testing.T) {
	data := []byte(strings.Join([]string{
		"commit: " + testCorpusCommit(),
		"sources:",
		"  - file: t/plugin/example.t",
		"    test_numbers: [1]",
		"    owner: example-plugin",
		"    disposition: pending",
		"    reason: strict fields",
		"    surprise: true",
	}, "\n"))
	if _, err := loadCorpusScope("test", data); err == nil {
		t.Fatal("loadCorpusScope() accepted an unknown field")
	}
}

func TestCorpusScope(t *testing.T) {
	data, err := os.ReadFile(corpusScopeFile)
	if err != nil {
		t.Fatalf("read %s: %v", corpusScopeFile, err)
	}
	scope, err := loadCorpusScope(corpusScopeFile, data)
	if err != nil {
		t.Fatalf("load %s: %v", corpusScopeFile, err)
	}
	if len(scope.Sources) == 0 {
		t.Fatal("ledger has no sources")
	}
}

func TestFirstWaveSourcesUsePinnedAPISIXTarget(t *testing.T) {
	scope, err := loadCorpusScopeFile(t)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	commits := sourceCommitsByFile(scope)
	files := []string{
		"t/plugin/basic-auth-anonymous-consumer.t",
		"t/plugin/basic-auth-realm.t",
		"t/plugin/basic-auth.t",
		"t/plugin/cors.t",
		"t/plugin/cors2.t",
		"t/plugin/cors3.t",
		"t/plugin/cors4.t",
		"t/plugin/jwt-auth-anonymous-consumer.t",
		"t/plugin/jwt-auth-more-algo.t",
		"t/plugin/jwt-auth-realm.t",
		"t/plugin/jwt-auth.t",
		"t/plugin/jwt-auth2.t",
		"t/plugin/jwt-auth3.t",
		"t/plugin/jwt-auth4.t",
		"t/plugin/key-auth-anonymous-consumer.t",
		"t/plugin/key-auth-realm.t",
		"t/plugin/key-auth-upstream-domain-node.t",
		"t/plugin/key-auth.t",
		"t/plugin/prometheus-metric-expire.t",
		"t/plugin/prometheus.t",
		"t/plugin/prometheus2.t",
		"t/plugin/prometheus3.t",
		"t/plugin/prometheus4.t",
		"t/plugin/request-id.t",
		"t/plugin/request-id2.t",
		"t/plugin/request-id3.t",
	}
	for _, file := range files {
		if got := commits[file]; got != manifest.Target.SourceCommit {
			t.Errorf("%s effective commit = %s, want compatibility target %s", file, got, manifest.Target.SourceCommit)
		}
	}
}

func TestByteIdenticalSourcesUsePinnedAPISIXTarget(t *testing.T) {
	scope, err := loadCorpusScopeFile(t)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	sourceRoot := apacheAPISIXRepository(t)
	var stale []string
	for file, commit := range sourceCommitsByFile(scope) {
		if commit == manifest.Target.SourceCommit {
			continue
		}
		targetData, targetErr := sourceFileAtCommit(sourceRoot, manifest.Target.SourceCommit, file)
		if targetErr != nil {
			continue
		}
		currentData, currentErr := sourceFileAtCommit(sourceRoot, commit, file)
		if currentErr != nil {
			t.Fatalf("read ledger source %s at %s: %v", file, commit, currentErr)
		}
		if bytes.Equal(targetData, currentData) {
			stale = append(stale, file)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		limit := min(len(stale), 20)
		t.Fatalf(
			"%d byte-identical sources still use a non-target commit (first %d: %v)",
			len(stale),
			limit,
			stale[:limit],
		)
	}
}

func TestUpstreamCorpusAccounting(t *testing.T) {
	scope, err := loadCorpusScopeFile(t)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	checkOfflineCorpusAccounting(t, scope)

	// A local Apache APISIX repository adds per-commit source-label comparison but is not required for
	// ledger/manifest consistency validation.
	if sourceRoot, ok := optionalApacheAPISIXRepository(); ok {
		checkCorpusScopeAgainstSource(t, scope, sourceRoot)
	}
}

func TestUpstreamCorpusAccountingWithoutSourceCheckout(t *testing.T) {
	scope, err := loadCorpusScopeFile(t)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	checkOfflineCorpusAccounting(t, scope)
}

func checkOfflineCorpusAccounting(t *testing.T, scope *corpusScope) {
	t.Helper()

	// Validation and post-target regression labels are tracked independently so additional
	// regression coverage cannot promote a compatibility claim.
	manifestByFile, err := loadManifestSelections(".", scope)
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}
	regressionManifestByFile, err := loadManifestRegressionSelections(".", scope)
	if err != nil {
		t.Fatalf("load regression manifests: %v", err)
	}
	convertedByFile := make(map[string]map[int]string, len(scope.Sources))
	regressionByFile := make(map[string]map[int]string, len(scope.Sources))
	for i := range scope.Sources {
		source := &scope.Sources[i]
		var selections map[string]map[int]string
		switch source.Disposition {
		case "converted":
			selections = convertedByFile
		case "regression_test":
			selections = regressionByFile
		default:
			continue
		}
		if selections[source.File] == nil {
			selections[source.File] = make(map[int]string)
		}
		for _, number := range source.TestNumbers {
			selections[source.File][number] = source.Manifest
		}
	}
	checkManifestSelectionsMatchLedger(t, "validation", manifestByFile, convertedByFile)
	checkManifestSelectionsMatchLedger(t, "regression", regressionManifestByFile, regressionByFile)

	for i := range scope.Sources {
		source := &scope.Sources[i]
		if source.Disposition == "package_test" || source.Disposition == "platform_test" {
			checkGoTestEvidence(t, source.Evidence)
		}
		if source.Disposition == "dependency_test" {
			checkDependencyTestEvidence(t, source.Evidence)
		}
	}

	// No duplicate or missing label, including shared security-warning sources.
	if err := corpusScopeLabelsComplete(scope); err != nil {
		t.Errorf("ledger completeness: %v", err)
	}
}

func checkManifestSelectionsMatchLedger(
	t *testing.T,
	kind string,
	manifestByFile map[string]map[int]string,
	ledgerByFile map[string]map[int]string,
) {
	t.Helper()
	for file, labels := range manifestByFile {
		for label, manifestName := range labels {
			ledgerManifest, ok := ledgerByFile[file][label]
			if !ok {
				t.Errorf(
					"manifest %s maps %s label %d in %s but the ledger does not account for it",
					manifestName,
					kind,
					label,
					file,
				)
				continue
			}
			if ledgerManifest != manifestName {
				t.Errorf(
					"%s label %d in %s accounted by %s, manifest %s disagrees",
					kind,
					label,
					file,
					ledgerManifest,
					manifestName,
				)
			}
		}
	}

	for file, labels := range ledgerByFile {
		for label, manifestName := range labels {
			owner, ok := manifestByFile[file][label]
			if !ok {
				t.Errorf(
					"ledger accounts for %s label %d in %s via %s but no manifest maps it",
					kind,
					label,
					file,
					manifestName,
				)
				continue
			}
			if owner != manifestName {
				t.Errorf(
					"%s label %d in %s accounted via %s but manifest %s owns it",
					kind,
					label,
					file,
					manifestName,
					owner,
				)
			}
		}
	}
}

func checkGoTestEvidence(t *testing.T, evidence string) {
	t.Helper()
	path, testName, ok := strings.Cut(evidence, "#")
	if !ok || !strings.HasPrefix(path, "pkg/") || !strings.HasSuffix(path, "_test.go") ||
		!strings.HasPrefix(testName, "Test") || strings.Contains(testName, "#") {
		t.Errorf("Go test evidence %q must be pkg/..._test.go#TestName", evidence)
		return
	}
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
	if err != nil {
		t.Errorf("read Go test evidence %q: %v", evidence, err)
		return
	}
	pattern := regexp.MustCompile(`(?m)^func\s+` + regexp.QuoteMeta(testName) + `\s*\(`)
	if !pattern.Match(data) {
		t.Errorf("Go test evidence %q does not name a test function", evidence)
	}
}

func checkDependencyTestEvidence(t *testing.T, evidence string) {
	t.Helper()
	if !strings.HasPrefix(evidence, "scripts/validation/") || !strings.HasSuffix(evidence, ".sh") {
		t.Errorf("dependency test evidence %q must be scripts/validation/*.sh", evidence)
		return
	}
	info, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(evidence)))
	if err != nil {
		t.Errorf("read dependency test evidence %q: %v", evidence, err)
		return
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("dependency test evidence %q is not executable", evidence)
	}
}

func checkCorpusScopeAgainstSource(t *testing.T, scope *corpusScope, sourceRoot string) {
	t.Helper()

	// The default commit remains the complete HTTP plugin inventory baseline. A migrated file may keep
	// post-target regression labels at the baseline commit while its validation labels move to the
	// compatibility target. Explicit stream-plugin sources supplement that baseline.
	baselineFiles, err := sourceFilesAtCommit(sourceRoot, scope.Commit)
	if err != nil {
		t.Fatalf("list baseline source files: %v", err)
	}
	ledgerLabels := corpusScopeLabels(scope)
	baselineSet := make(map[string]bool, len(baselineFiles))
	for _, file := range baselineFiles {
		baselineSet[file] = true
		if _, ok := ledgerLabels[file]; !ok {
			t.Errorf("ledger is missing baseline source file %s", file)
		}
	}
	for file := range ledgerLabels {
		if !baselineSet[file] {
			if !strings.HasPrefix(file, "t/stream-plugin/") {
				t.Errorf("ledger covers %s which is absent from the baseline source inventory", file)
			}
		}
	}

	// Every row must name labels that exist at its own historical or target commit.
	labelsByFileCommit := make(map[string]map[string]map[int]bool)
	validationLabels := make(map[string]map[int]bool)
	for i := range scope.Sources {
		source := &scope.Sources[i]
		commit := scope.effectiveCommit(*source)
		if labelsByFileCommit[source.File] == nil {
			labelsByFileCommit[source.File] = make(map[string]map[int]bool)
		}
		if labelsByFileCommit[source.File][commit] == nil {
			labelsByFileCommit[source.File][commit] = make(map[int]bool)
		}
		for _, label := range source.TestNumbers {
			labelsByFileCommit[source.File][commit][label] = true
		}
		if source.Disposition != "regression_test" && source.Disposition != "post_target" {
			if validationLabels[source.File] == nil {
				validationLabels[source.File] = make(map[int]bool)
			}
			for _, label := range source.TestNumbers {
				validationLabels[source.File][label] = true
			}
		}
	}
	for file, commits := range labelsByFileCommit {
		for commit, labels := range commits {
			data, readErr := sourceFileAtCommit(sourceRoot, commit, file)
			if readErr != nil {
				t.Errorf("ledger source %s at %s: %v", file, commit, readErr)
				continue
			}
			_, sourceLabels, parseErr := parseSourceTestHeaders(data)
			if parseErr != nil {
				t.Errorf("ledger source %s at %s: %v", file, commit, parseErr)
				continue
			}
			for label := range labels {
				if !sourceLabels[label] {
					t.Errorf("ledger label %d in %s is absent from source commit %s", label, file, commit)
				}
			}
		}
	}

	// Validation rows, unlike historical regression rows, must account for the complete source
	// at their effective compatibility commit.
	for file, labels := range validationLabels {
		commit := sourceCommitsByFile(scope)[file]
		data, readErr := sourceFileAtCommit(sourceRoot, commit, file)
		if readErr != nil {
			t.Errorf("validation source %s at %s: %v", file, commit, readErr)
			continue
		}
		_, sourceLabels, parseErr := parseSourceTestHeaders(data)
		if parseErr != nil {
			t.Errorf("validation source %s at %s: %v", file, commit, parseErr)
			continue
		}
		if len(labels) != len(sourceLabels) {
			t.Errorf(
				"validation source %s at %s has %d labels, source has %d",
				file,
				commit,
				len(labels),
				len(sourceLabels),
			)
		}
		for label := range sourceLabels {
			if !labels[label] {
				t.Errorf("validation ledger is missing source label %d in %s at commit %s", label, file, commit)
			}
		}
	}
}

func TestUpstreamCorpusCompletion(t *testing.T) {
	scope, err := loadCorpusScopeFile(t)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	var pluginOwnedPending []string
	var pendingBlocks, nonPluginBlocks, convertedBlocks, packageTestBlocks, dependencyTestBlocks, platformTestBlocks, platformGapBlocks, regressionTestBlocks, postTargetBlocks int
	for i := range scope.Sources {
		source := &scope.Sources[i]
		switch source.Disposition {
		case "converted":
			convertedBlocks += len(source.TestNumbers)
		case "package_test":
			packageTestBlocks += len(source.TestNumbers)
		case "dependency_test":
			dependencyTestBlocks += len(source.TestNumbers)
		case "platform_test":
			platformTestBlocks += len(source.TestNumbers)
		case "platform_gap":
			platformGapBlocks += len(source.TestNumbers)
		case "regression_test":
			regressionTestBlocks += len(source.TestNumbers)
		case "post_target":
			postTargetBlocks += len(source.TestNumbers)
		case "non_plugin":
			nonPluginBlocks += len(source.TestNumbers)
		default:
			pluginOwnedPending = append(pluginOwnedPending, source.File)
			pendingBlocks += len(source.TestNumbers)
		}
	}
	sort.Strings(pluginOwnedPending)
	t.Logf(
		"corpus completion: %d real-process validation blocks, %d package-test blocks, %d dependency-test blocks, %d platform-test blocks, %d platform-gap blocks, %d post-target regression blocks, %d excluded post-target blocks, %d non-plugin blocks, %d pending/blocked plugin blocks across %d sources",
		convertedBlocks,
		packageTestBlocks,
		dependencyTestBlocks,
		platformTestBlocks,
		platformGapBlocks,
		regressionTestBlocks,
		postTargetBlocks,
		nonPluginBlocks,
		pendingBlocks,
		len(pluginOwnedPending),
	)
	if os.Getenv("APISIX_GO_REQUIRE_FULL_CORPUS") != "1" {
		t.Log("APISIX_GO_REQUIRE_FULL_CORPUS is not set; completion is opt-in")
		return
	}
	if len(pluginOwnedPending) > 0 {
		t.Fatalf(
			"full corpus required: %d plugin-owned blocks are not converted (first sources: %v)",
			pendingBlocks,
			pluginOwnedPending,
		)
	}
}

func loadCorpusScopeFile(t *testing.T) (*corpusScope, error) {
	t.Helper()
	data, err := os.ReadFile(corpusScopeFile)
	if err != nil {
		return nil, err
	}
	return loadCorpusScope(corpusScopeFile, data)
}

func corpusScopeLabels(scope *corpusScope) map[string]map[int]bool {
	labels := make(map[string]map[int]bool, len(scope.Sources))
	for i := range scope.Sources {
		source := &scope.Sources[i]
		if labels[source.File] == nil {
			labels[source.File] = make(map[int]bool, len(source.TestNumbers))
		}
		for _, number := range source.TestNumbers {
			labels[source.File][number] = true
		}
	}
	return labels
}

func loadManifestSelections(root string, scope *corpusScope) (map[string]map[int]string, error) {
	return loadManifestSelectionsByMode(root, scope, false)
}

func loadManifestRegressionSelections(root string, scope *corpusScope) (map[string]map[int]string, error) {
	return loadManifestSelectionsByMode(root, scope, true)
}

func loadManifestSelectionsByMode(
	root string,
	scope *corpusScope,
	regressionOnly bool,
) (map[string]map[int]string, error) {
	ledgerSelections := make(map[string]map[int]corpusLabelSelection, len(scope.Sources))
	for _, source := range scope.Sources {
		if ledgerSelections[source.File] == nil {
			ledgerSelections[source.File] = make(map[int]corpusLabelSelection, len(source.TestNumbers))
		}
		for _, number := range source.TestNumbers {
			ledgerSelections[source.File][number] = corpusLabelSelection{
				Commit: scope.effectiveCommit(source),
			}
		}
	}

	files, err := filepath.Glob(filepath.Join(root, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("discover manifests: %w", err)
	}
	selections := make(map[string]map[int]string)
	for _, file := range files {
		if filepath.Base(file) == corpusScopeFile {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		manifest, err := loadManifest(file, data)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", file, err)
		}
		manifestName := filepath.Base(file)
		for _, source := range manifestSources(manifest) {
			if source.RegressionOnly != regressionOnly {
				continue
			}
			if selections[source.File] == nil {
				selections[source.File] = make(map[int]string)
			}
			blocks := source.TestNumbers
			if len(blocks) == 0 {
				blocks = make([]int, source.Tests)
				for i := range blocks {
					blocks[i] = i + 1
				}
			}
			for _, number := range blocks {
				ledgerSelection, ok := ledgerSelections[source.File][number]
				if !ok {
					if regressionOnly {
						continue
					}
					return nil, fmt.Errorf(
						"%s selects label %d in %s which is absent from the corpus ledger",
						manifestName,
						number,
						source.File,
					)
				}
				if source.Commit != ledgerSelection.Commit {
					return nil, fmt.Errorf(
						"%s source %s label %d commit = %q, want effective corpus commit %s",
						manifestName,
						source.File,
						number,
						source.Commit,
						ledgerSelection.Commit,
					)
				}
				if previous, ok := selections[source.File][number]; ok {
					return nil, fmt.Errorf(
						"label %d in %s is selected by both %s and %s",
						number,
						source.File,
						previous,
						manifestName,
					)
				}
				selections[source.File][number] = manifestName
			}
		}
	}
	return selections, nil
}

func corpusScopeLabelsComplete(scope *corpusScope) error {
	seen := make(map[string]map[int]string, len(scope.Sources))
	for i := range scope.Sources {
		source := &scope.Sources[i]
		if seen[source.File] == nil {
			seen[source.File] = make(map[int]string, len(source.TestNumbers))
		}
		for _, number := range source.TestNumbers {
			if previous, ok := seen[source.File][number]; ok {
				return fmt.Errorf("label %d in %s duplicated by %q and %q", number, source.File, previous, source.Owner)
			}
			seen[source.File][number] = source.Owner
		}
	}
	return nil
}
