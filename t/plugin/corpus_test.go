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
	TestNumbers []int  `yaml:"test_numbers"`
	Owner       string `yaml:"owner"`
	Disposition string `yaml:"disposition"`
	Manifest    string `yaml:"manifest,omitempty"`
	Reason      string `yaml:"reason,omitempty"`
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
	for i := range s.Sources {
		source := &s.Sources[i]
		if strings.TrimSpace(source.File) == "" {
			return fmt.Errorf("source %d file is required", i+1)
		}
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

func TestUpstreamCorpusAccounting(t *testing.T) {
	scope, err := loadCorpusScopeFile(t)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	checkOfflineCorpusAccounting(t, scope)

	// A local checkout of the historical corpus commit adds source-label comparison but is not required for
	// ledger/manifest consistency validation.
	if sourceRoot, ok := optionalApacheAPISIXSourceRoot(t, scope.Commit); ok {
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
	scope, err := loadCorpusScopeFile(t)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	if scope.Commit == manifest.Target.SourceCommit {
		t.Logf("corpus commit already matches compatibility target %s", manifest.Target.SourceCommit)
		return
	}

	qualified := make(map[string]bool)
	for _, pluginName := range manifest.QualifiedPlugins("http-data-plane-v1") {
		qualified[pluginName] = true
	}

	staleClaims := 0
	for _, plugin := range manifest.Plugins {
		if !onlyIntegrationManifestRefs(plugin.Evidence.Upstream.Refs) {
			continue
		}
		staleClaims++
		if plugin.Evidence.Upstream.State != capability.EvidenceStale {
			t.Errorf(
				"plugin %s converted_upstream state = %q, want %q while corpus %s differs from target %s",
				plugin.Name,
				plugin.Evidence.Upstream.State,
				capability.EvidenceStale,
				scope.Commit,
				manifest.Target.SourceCommit,
			)
		}
		if qualified[plugin.Name] {
			t.Errorf("QualifiedPlugins(http-data-plane-v1) includes %s with stale corpus evidence", plugin.Name)
		}
	}
	if staleClaims == 0 {
		t.Fatal("capability manifest has no claims sourced only from integration manifests")
	}
	t.Logf(
		"corpus evidence: %d claims are stale at corpus %s versus compatibility target %s",
		staleClaims,
		scope.Commit,
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

func checkOfflineCorpusAccounting(t *testing.T, scope *corpusScope) {
	t.Helper()

	// Every manifest-declared source label is converted and points back to a manifest.
	manifestByFile, err := loadManifestSelections(scope.Commit)
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

	// Ledger file/label union must equal the pinned checkout when available.
	pinnedLabels := upstreamSourceLabels(t, sourceRoot)
	ledgerLabels := corpusScopeLabels(scope)
	if len(ledgerLabels) != len(pinnedLabels) {
		t.Fatalf("ledger labels = %d, pinned checkout labels = %d", len(ledgerLabels), len(pinnedLabels))
	}
	for file, labels := range pinnedLabels {
		for label := range labels {
			if !ledgerLabels[file][label] {
				t.Errorf("ledger is missing pinned label %d in %s", label, file)
			}
		}
	}
	for file, labels := range ledgerLabels {
		if _, exists := pinnedLabels[file]; !exists {
			t.Errorf("ledger covers %s which has no pinned TEST blocks", file)
			continue
		}
		for label := range labels {
			if !pinnedLabels[file][label] {
				t.Errorf("ledger label %d in %s is absent from the pinned source", label, file)
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

func upstreamSourceLabels(t *testing.T, sourceRoot string) map[string]map[int]bool {
	t.Helper()
	labels := make(map[string]map[int]bool)
	err := filepath.WalkDir(
		filepath.Join(sourceRoot, "t", "plugin"),
		func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".t" {
				return nil
			}
			rel, err := filepath.Rel(sourceRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, found, err := parseSourceTestHeaders(data)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			labels[rel] = found
			return nil
		},
	)
	if err != nil {
		t.Fatalf("walk pinned sources: %v", err)
	}
	return labels
}

func loadManifestSelections(corpusCommit string) (map[string]map[int]string, error) {
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		return nil, fmt.Errorf("discover manifests: %w", err)
	}
	selections := make(map[string]map[int]string)
	for _, file := range files {
		if file == corpusScopeFile {
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
			if source.Commit != corpusCommit {
				return nil, fmt.Errorf(
					"%s source %s commit = %q, want corpus commit %s",
					manifestName,
					source.File,
					source.Commit,
					corpusCommit,
				)
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
