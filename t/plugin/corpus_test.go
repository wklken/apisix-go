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
	Reason      string `yaml:"reason,omitempty"`
}

type corpusLabelSelection struct {
	Commit string
}

var corpusDispositions = map[string]bool{
	"converted":       true,
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
	fileCommits := make(map[string]string, len(s.Sources))
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
		if previous, ok := fileCommits[source.File]; ok && previous != effectiveCommit {
			return fmt.Errorf(
				"source %q mixes effective commits %s and %s; migrate all rows for one source file together",
				source.File,
				previous,
				effectiveCommit,
			)
		}
		fileCommits[source.File] = effectiveCommit
		if strings.TrimSpace(source.Owner) == "" {
			return fmt.Errorf("source %q owner is required", source.File)
		}
		if !corpusDispositions[source.Disposition] {
			return fmt.Errorf(
				"source %q disposition %q is not allowed; want one of converted, pending, blocked_runtime, blocked_design, non_plugin",
				source.File,
				source.Disposition,
			)
		}
		if source.Disposition == "converted" && strings.TrimSpace(source.Manifest) == "" {
			return fmt.Errorf("source %q is converted but has no manifest", source.File)
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

func TestCorpusScopeRejectsMixedCommitsWithinSourceFile(t *testing.T) {
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
		"    disposition: pending",
		"    reason: historical row",
	}, "\n"))
	_, err := loadCorpusScope("test", data)
	if err == nil || !strings.Contains(err.Error(), "mixes effective commits") {
		t.Fatalf("loadCorpusScope() error = %v, want mixed source-file commit error", err)
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
	data = bytes.ReplaceAll(data, []byte(historicalCommit), []byte(targetCommit))
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
	data = bytes.ReplaceAll(data, []byte(historicalCommit), []byte(targetCommit))
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

func TestFirstWaveSourcesUseCompatibilityTarget(t *testing.T) {
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

func TestCorpusEvidenceMatchesCompatibilityTarget(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}

	qualified := make(map[string]bool)
	for _, pluginName := range manifest.QualifiedPlugins("http-data-plane-v1") {
		qualified[pluginName] = true
	}

	staleClaims, freshClaims := 0, 0
	repoRoot := filepath.Join("..", "..")
	for _, plugin := range manifest.Plugins {
		if !onlyIntegrationManifestRefs(plugin.Evidence.Upstream.Refs) {
			continue
		}
		fresh, freshErr := integrationManifestRefsFresh(
			repoRoot,
			plugin.Evidence.Upstream.Refs,
			manifest.Target.SourceCommit,
		)
		if freshErr != nil {
			t.Errorf("plugin %s converted_upstream refs: %v", plugin.Name, freshErr)
			continue
		}
		if fresh {
			freshClaims++
			if plugin.Evidence.Upstream.State == capability.EvidenceStale {
				t.Logf("plugin %s has target-pinned converted cases awaiting evidence promotion", plugin.Name)
			}
			continue
		}

		staleClaims++
		if plugin.Evidence.Upstream.State != capability.EvidenceStale {
			t.Errorf(
				"plugin %s converted_upstream state = %q, want %q while referenced manifests differ from target %s",
				plugin.Name,
				plugin.Evidence.Upstream.State,
				capability.EvidenceStale,
				manifest.Target.SourceCommit,
			)
		}
		if qualified[plugin.Name] {
			t.Errorf("QualifiedPlugins(http-data-plane-v1) includes %s with stale corpus evidence", plugin.Name)
		}
	}
	if staleClaims == 0 {
		t.Log("all integration-manifest converted_upstream claims use the compatibility target")
	}
	t.Logf(
		"corpus evidence: %d fresh claims and %d stale claims versus compatibility target %s",
		freshClaims,
		staleClaims,
		manifest.Target.SourceCommit,
	)
}

func onlyIntegrationManifestRefs(refs []string) bool {
	if len(refs) == 0 {
		return false
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "t/plugin/") || filepath.Ext(ref) != ".yaml" ||
			filepath.Base(ref) == corpusScopeFile {
			return false
		}
	}
	return true
}

func integrationManifestRefsFresh(repoRoot string, refs []string, targetCommit string) (bool, error) {
	for _, ref := range refs {
		path := filepath.Join(repoRoot, filepath.FromSlash(ref))
		data, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", ref, err)
		}
		manifest, err := loadManifest(filepath.Base(ref), data)
		if err != nil {
			return false, fmt.Errorf("load %s: %w", ref, err)
		}
		for _, source := range manifestSources(manifest) {
			if source.Commit != targetCommit {
				return false, nil
			}
		}
	}
	return true, nil
}

func TestIntegrationManifestRefsFreshAtTarget(t *testing.T) {
	historicalCommit := "c3d7d5ec69774121f53d2e20d29d09c816795dd7"
	targetCommit := strings.Repeat("b", 40)
	data, err := os.ReadFile("redirect2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	manifestDir := filepath.Join(root, "t", "plugin")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manifestDir, "redirect2.yaml")
	if err := os.WriteFile(
		path,
		bytes.ReplaceAll(data, []byte(historicalCommit), []byte(targetCommit)),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	fresh, err := integrationManifestRefsFresh(root, []string{"t/plugin/redirect2.yaml"}, targetCommit)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("target-pinned manifest refs are stale")
	}
	fresh, err = integrationManifestRefsFresh(root, []string{"t/plugin/redirect2.yaml"}, historicalCommit)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("manifest refs pinned to another commit are fresh")
	}
}

func checkOfflineCorpusAccounting(t *testing.T, scope *corpusScope) {
	t.Helper()

	// Every manifest-declared source label is converted and points back to a manifest.
	manifestByFile, err := loadManifestSelections(".", scope)
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}
	convertedByFile := make(map[string]map[int]string, len(scope.Sources))
	for i := range scope.Sources {
		source := &scope.Sources[i]
		if source.Disposition != "converted" {
			continue
		}
		if convertedByFile[source.File] == nil {
			convertedByFile[source.File] = make(map[int]string)
		}
		for _, number := range source.TestNumbers {
			convertedByFile[source.File][number] = source.Manifest
		}
	}
	for file, labels := range manifestByFile {
		for label, manifestName := range labels {
			convertedManifest, ok := convertedByFile[file][label]
			if !ok {
				t.Errorf(
					"manifest %s maps label %d in %s but the ledger does not mark it converted",
					manifestName,
					label,
					file,
				)
				continue
			}
			if convertedManifest != manifestName {
				t.Errorf(
					"label %d in %s converted by %s, manifest %s disagrees",
					label,
					file,
					convertedManifest,
					manifestName,
				)
			}
		}
	}

	// Every converted ledger row exists in exactly one manifest.
	for file, labels := range convertedByFile {
		for label, manifestName := range labels {
			owner, ok := manifestByFile[file][label]
			if !ok {
				t.Errorf("ledger converts label %d in %s via %s but no manifest maps it", label, file, manifestName)
				continue
			}
			if owner != manifestName {
				t.Errorf("label %d in %s converted via %s but manifest %s owns it", label, file, manifestName, owner)
			}
		}
	}

	// No duplicate or missing label, including shared security-warning sources.
	if err := corpusScopeLabelsComplete(scope); err != nil {
		t.Errorf("ledger completeness: %v", err)
	}
}

func checkCorpusScopeAgainstSource(t *testing.T, scope *corpusScope, sourceRoot string) {
	t.Helper()

	// The default commit remains the complete inventory baseline. A migrated file keeps its place in that
	// inventory, but its exact labels are checked against its row-level effective commit.
	baselineFiles, err := sourceFilesAtCommit(sourceRoot, scope.Commit)
	if err != nil {
		t.Fatalf("list baseline source files: %v", err)
	}
	ledgerLabels := corpusScopeLabels(scope)
	if len(ledgerLabels) != len(baselineFiles) {
		t.Fatalf("ledger source files = %d, baseline source files = %d", len(ledgerLabels), len(baselineFiles))
	}
	baselineSet := make(map[string]bool, len(baselineFiles))
	for _, file := range baselineFiles {
		baselineSet[file] = true
		if _, ok := ledgerLabels[file]; !ok {
			t.Errorf("ledger is missing baseline source file %s", file)
		}
	}
	for file, labels := range ledgerLabels {
		if !baselineSet[file] {
			t.Errorf("ledger covers %s which is absent from the baseline source inventory", file)
			continue
		}
		commit := sourceCommitsByFile(scope)[file]
		data, err := sourceFileAtCommit(sourceRoot, commit, file)
		if err != nil {
			t.Errorf("ledger source %s: %v", file, err)
			continue
		}
		_, sourceLabels, err := parseSourceTestHeaders(data)
		if err != nil {
			t.Errorf("ledger source %s at %s: %v", file, commit, err)
			continue
		}
		if len(labels) != len(sourceLabels) {
			t.Errorf(
				"ledger source %s at %s has %d labels, source has %d",
				file,
				commit,
				len(labels),
				len(sourceLabels),
			)
		}
		for label := range labels {
			if !sourceLabels[label] {
				t.Errorf("ledger label %d in %s is absent from source commit %s", label, file, commit)
			}
		}
		for label := range sourceLabels {
			if !labels[label] {
				t.Errorf("ledger is missing source label %d in %s at commit %s", label, file, commit)
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
	var pendingBlocks, nonPluginBlocks, convertedBlocks int
	for i := range scope.Sources {
		source := &scope.Sources[i]
		switch source.Disposition {
		case "converted":
			convertedBlocks += len(source.TestNumbers)
		case "non_plugin":
			nonPluginBlocks += len(source.TestNumbers)
		default:
			pluginOwnedPending = append(pluginOwnedPending, source.File)
			pendingBlocks += len(source.TestNumbers)
		}
	}
	sort.Strings(pluginOwnedPending)
	t.Logf(
		"corpus completion: %d converted blocks, %d non-plugin blocks, %d pending/blocked blocks across %d sources",
		convertedBlocks,
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
