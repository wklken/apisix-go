package pluginintegration

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	brotli "github.com/andybalholm/brotli"
	"go.yaml.in/yaml/v3"
)

func TestOracleIdentity(t *testing.T) {
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := loadOracleIdentity(differentialOraclePath(repoRoot))
	if err != nil {
		t.Fatalf("loadOracleIdentity() error = %v", err)
	}
	if got := identity.ImageReference(); got != compatibilityOracleRepository+"@"+compatibilityOracleImageDigest {
		t.Fatalf("ImageReference() = %q, want digest-qualified reference", got)
	}
	if identity.ImageTag == identity.ImageReference() {
		t.Fatal("oracle execution identity unexpectedly uses the mutable tag")
	}
}

func TestOracleIdentityRejectsMutableOrWrongIdentity(t *testing.T) {
	tests := []struct {
		name string
		edit func(*OracleIdentity)
		want string
	}{
		{
			name: "mutable digest missing",
			edit: func(identity *OracleIdentity) { identity.ImageLinuxAMD64 = "" },
			want: "linux/amd64 digest",
		},
		{
			name: "wrong source",
			edit: func(identity *OracleIdentity) { identity.SourceCommit = "main" },
			want: "source_commit",
		},
		{
			name: "wrong image",
			edit: func(identity *OracleIdentity) { identity.ImageTag = "apache/apisix:latest" },
			want: "image_tag",
		},
	}
	base := OracleIdentity{
		SchemaVersion:    1,
		ImageTag:         compatibilityOracleImage,
		ImageRepository:  compatibilityOracleRepository,
		ImageIndexDigest: "sha256:0e5377839f4ff5e322a5686ab6ce6797ba768008aca1bfc9b71149c3b326c4df",
		ImageLinuxAMD64:  compatibilityOracleImageDigest,
		SourceRepository: "https://github.com/apache/apisix",
		SourceCommit:     compatibilityOracleSourceCommit,
		ExpectedVersion:  compatibilityOracleVersion,
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := base
			tt.edit(&identity)
			if err := identity.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDifferentialCasesAreOneHundredTwentyFourPinnedLogicalCases(t *testing.T) {
	cases := differentialCases()
	if len(cases) != 124 {
		t.Fatalf("differentialCases() = %d cases, want 124", len(cases))
	}
	seenNames := make(map[string]struct{}, len(cases))
	seenPlugins := make(map[string]struct{}, len(cases))
	for _, spec := range cases {
		if spec.Name == "" || spec.Plugin == "" || spec.RouteID == "" {
			t.Fatalf("case has incomplete identity: %#v", spec)
		}
		if _, exists := seenNames[spec.Name]; exists {
			t.Fatalf("duplicate differential case %q", spec.Name)
		}
		seenNames[spec.Name] = struct{}{}
		seenPlugins[spec.Plugin] = struct{}{}
		configYAML := string(mustYAML(t, spec.Config))
		hasFixtureEndpoint := strings.Contains(configYAML, differentialFixturePlaceholder) ||
			(strings.Contains(configYAML, differentialFixtureHostPlaceholder) &&
				strings.Contains(configYAML, differentialFixturePortPlaceholder))
		if !hasFixtureEndpoint && spec.Fixture.ExpectedCalls != 0 {
			t.Fatalf("case %s has no deterministic fixture endpoint placeholder", spec.Name)
		}
		if spec.Fixture.CollectTimeoutMillis < 0 ||
			spec.Fixture.CollectTimeoutMillis > int(differentialPodmanTimeout/time.Millisecond) {
			t.Fatalf(
				"case %s fixture collect timeout = %dms, want 0..%dms",
				spec.Name,
				spec.Fixture.CollectTimeoutMillis,
				int(differentialPodmanTimeout/time.Millisecond),
			)
		}
		if isDifferentialMQTTProxyCase(spec) {
			continue
		}
		if len(spec.Steps) == 0 {
			if spec.Request.Host == "" {
				t.Fatalf("case %s has no explicit request Host", spec.Name)
			}
		} else {
			for index, step := range spec.Steps {
				if len(step.ConcurrentRequests) != 0 {
					for requestIndex, request := range step.ConcurrentRequests {
						if request.Host == "" {
							t.Fatalf(
								"case %s step %d concurrent request %d has no explicit Host",
								spec.Name, index, requestIndex,
							)
						}
					}
					continue
				}
				if step.Request.Host == "" {
					t.Fatalf("case %s step %d has no explicit request Host", spec.Name, index)
				}
			}
		}
	}
	if got := len(seenPlugins); got != 111 {
		t.Fatalf("differential plugin count = %d, want 111", got)
	}
}

func TestDifferentialCasesIncludePreparedRealIPAndRequestIDCases(t *testing.T) {
	want := map[string]string{
		"real-ip-trusted-peer-rewrites-from-forwarded-for": "real-ip",
		"real-ip-untrusted-peer-ignores-forwarded-for":     "real-ip",
		"request-id-preserves-client-id-in-response":       "request-id",
		"request-id-omits-client-id-from-response":         "request-id",
	}
	for _, spec := range differentialCases() {
		if plugin, exists := want[spec.Name]; exists {
			if spec.Plugin != plugin {
				t.Fatalf("case %q plugin = %q, want %q", spec.Name, spec.Plugin, plugin)
			}
			delete(want, spec.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("prepared differential cases are not registered: %v", want)
	}
}

func TestDifferentialCatalogDeclaresOneHundredTwentyFourCaseOneHundredElevenPluginSuite(t *testing.T) {
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "validation", "differential-cases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		SchemaVersion int    `yaml:"schema_version"`
		Suite         string `yaml:"suite"`
		Cases         []struct {
			Plugin     string `yaml:"plugin"`
			Obligation string `yaml:"obligation"`
			Case       string `yaml:"case"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.SchemaVersion != 2 || catalog.Suite != differentialSuite {
		t.Fatalf(
			"catalog identity = schema %d suite %q",
			catalog.SchemaVersion,
			catalog.Suite,
		)
	}
	if len(catalog.Cases) != 124 {
		t.Fatalf(
			"catalog coverage = %d cases, want 124",
			len(catalog.Cases),
		)
	}
	covered := make(map[string]struct{})
	for _, row := range catalog.Cases {
		if row.Plugin == "" || row.Obligation == "" || row.Case == "" {
			t.Fatalf("incomplete catalog row: %#v", row)
		}
		covered[row.Plugin] = struct{}{}
	}
	if len(covered) != 111 {
		t.Fatalf("catalog covered plugins = %d, want 111", len(covered))
	}
}

func TestDifferentialCatalogRejectsHandMaintainedRequiredPlugins(t *testing.T) {
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(differentialCatalogPath(repoRoot))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	inserted := false
	for index, line := range lines {
		if strings.HasPrefix(line, "suite:") {
			lines = slices.Insert(lines, index+1, "required_plugins: [acl]")
			inserted = true
			break
		}
	}
	if !inserted {
		t.Fatal("catalog suite field not found")
	}
	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDifferentialCatalog(path, differentialCases()); err == nil ||
		!strings.Contains(err.Error(), "field required_plugins not found") {
		t.Fatalf("loadDifferentialCatalog() error = %v, want derived-field rejection", err)
	}
}

func TestDifferentialCatalogAndArtifactReportOneHundredElevenOf111(t *testing.T) {
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := loadDifferentialCatalog(differentialCatalogPath(repoRoot), differentialCases())
	if err != nil {
		t.Fatalf("loadDifferentialCatalog() error = %v", err)
	}
	results := make([]DifferentialCaseResult, 0, len(differentialCases()))
	for _, spec := range differentialCases() {
		results = append(results, DifferentialCaseResult{
			Name: spec.Name, Plugin: spec.Plugin, ComparisonPolicy: spec.ComparisonPolicy,
			FirstAttempt: true, Passed: true,
			CandidateHash: "sha256:" + strings.Repeat("1", 64),
			OracleHash:    "sha256:" + strings.Repeat("1", 64),
		})
	}
	artifact, err := buildDifferentialArtifact(catalog, DifferentialCandidateID{
		SourceCommit: "candidate", BinarySHA256: strings.Repeat("2", 64),
	}, OracleIdentity{SourceCommit: compatibilityOracleSourceCommit}, results)
	if err != nil {
		t.Fatalf("buildDifferentialArtifact() error = %v", err)
	}
	if artifact.SchemaVersion != 3 || artifact.Suite != differentialSuite {
		t.Fatalf(
			"artifact identity = schema %d suite %q",
			artifact.SchemaVersion,
			artifact.Suite,
		)
	}
	if !artifact.Selection.FullCatalogRun || artifact.Selection.SelectedCaseCount != 124 ||
		artifact.Selection.ShardIndex != 0 || artifact.Selection.ShardCount != 1 ||
		artifact.Selection.Plugins == nil || len(artifact.Selection.Plugins) != 0 ||
		artifact.Selection.Cases == nil || len(artifact.Selection.Cases) != 0 {
		t.Fatalf("full-run artifact selection = %#v", artifact.Selection)
	}
	if artifact.Coverage.RequiredCount != 111 || artifact.Coverage.CoveredCount != 111 ||
		len(
			artifact.Coverage.RequiredPlugins,
		) != 111 || len(artifact.Coverage.CoveredPlugins) != 111 {
		t.Fatalf("artifact coverage = %#v, want exactly 111/111", artifact.Coverage)
	}
	if len(artifact.Plugins) != 111 || artifact.Plugins[0].Plugin != "acl" ||
		artifact.Plugins[0].Obligations != 1 || !artifact.Plugins[0].FirstAttempt || artifact.Plugins[0].Passed != 1 ||
		artifact.Plugins[1].Plugin != "ai-aliyun-content-moderation" ||
		artifact.Plugins[1].Obligations != 1 || !artifact.Plugins[1].FirstAttempt || artifact.Plugins[1].Passed != 1 {
		t.Fatalf("artifact plugin aggregates = %#v", artifact.Plugins)
	}
	if artifact.CatalogSHA256 == "" || !strings.HasPrefix(artifact.CatalogSHA256, "sha256:") {
		t.Fatalf("artifact catalog hash = %q", artifact.CatalogSHA256)
	}
}

func TestDifferentialNetworkLoggerPoliciesAreRegistered(t *testing.T) {
	want := map[string]string{
		differentialDatadogSixDatagramsPolicy:      "datadog",
		differentialTCPLoggerFixtureDeliveryPolicy: "tcp-logger",
		differentialUDPLoggerFixtureDeliveryPolicy: "udp-logger",
		differentialSyslogTCPDeliveryPolicy:        "syslog",
		differentialSLSLoggerTLSDeliveryPolicy:     "sls-logger",
	}
	for policy, plugin := range want {
		registration, exists := differentialComparatorRegistry[policy]
		if !exists {
			t.Errorf("comparison policy %q is not registered", policy)
			continue
		}
		if registration.compare == nil || len(registration.allowedPlugins) != 1 {
			t.Errorf("comparison policy %q registration = %#v", policy, registration)
			continue
		}
		if _, allowed := registration.allowedPlugins[plugin]; !allowed {
			t.Errorf("comparison policy %q does not allow plugin %q", policy, plugin)
		}
	}
}

func TestSelectDifferentialCasesFiltersByPluginAndCase(t *testing.T) {
	all := []DifferentialCase{
		{Name: "z-client", Plugin: "client-control"},
		{Name: "m-csrf", Plugin: "csrf"},
		{Name: "a-client", Plugin: "client-control"},
	}
	selected, normalized, err := selectDifferentialCases(all, DifferentialSelection{
		Plugins:    []string{" client-control "},
		Cases:      []string{"z-client", "m-csrf"},
		ShardIndex: 0,
		ShardCount: 1,
	})
	if err != nil {
		t.Fatalf("selectDifferentialCases() error = %v", err)
	}
	if got := differentialCaseNames(selected); !reflect.DeepEqual(got, []string{"z-client"}) {
		t.Fatalf("selected cases = %#v, want exact plugin/case intersection", got)
	}
	if !reflect.DeepEqual(normalized.Plugins, []string{"client-control"}) ||
		!reflect.DeepEqual(normalized.Cases, []string{"m-csrf", "z-client"}) {
		t.Fatalf("normalized selection = %#v", normalized)
	}

	invalid := []struct {
		name      string
		selection DifferentialSelection
		want      string
	}{
		{
			name:      "blank plugin selector",
			selection: DifferentialSelection{Plugins: []string{"client-control", ""}, ShardCount: 1},
			want:      "blank plugin selector",
		},
		{
			name:      "duplicate trimmed selector",
			selection: DifferentialSelection{Cases: []string{"z-client", " z-client "}, ShardCount: 1},
			want:      "duplicate case selector",
		},
		{
			name:      "unknown plugin",
			selection: DifferentialSelection{Plugins: []string{"missing"}, ShardCount: 1},
			want:      "unknown plugin selector",
		},
		{
			name:      "unknown case",
			selection: DifferentialSelection{Cases: []string{"missing"}, ShardCount: 1},
			want:      "unknown case selector",
		},
		{
			name: "empty intersection",
			selection: DifferentialSelection{
				Plugins: []string{"csrf"}, Cases: []string{"z-client"}, ShardCount: 1,
			},
			want: "selects no differential cases",
		},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := selectDifferentialCases(all, tt.selection); err == nil ||
				!strings.Contains(err.Error(), tt.want) {
				t.Fatalf("selectDifferentialCases() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSelectDifferentialCasesUsesStableZeroBasedShards(t *testing.T) {
	all := []DifferentialCase{
		{Name: "f", Plugin: "plugin"},
		{Name: "e", Plugin: "plugin"},
		{Name: "d", Plugin: "plugin"},
		{Name: "c", Plugin: "plugin"},
		{Name: "b", Plugin: "plugin"},
		{Name: "a", Plugin: "plugin"},
	}
	wants := [][]string{{"a", "d"}, {"b", "e"}, {"c", "f"}}
	for shardIndex, want := range wants {
		selected, _, err := selectDifferentialCases(all, DifferentialSelection{
			ShardIndex: shardIndex,
			ShardCount: 3,
		})
		if err != nil {
			t.Fatalf("selectDifferentialCases(shard %d) error = %v", shardIndex, err)
		}
		if got := differentialCaseNames(selected); !reflect.DeepEqual(got, want) {
			t.Fatalf("shard %d = %#v, want %#v", shardIndex, got, want)
		}
	}

	for _, selection := range []DifferentialSelection{
		{ShardIndex: 0, ShardCount: 0},
		{ShardIndex: 0, ShardCount: -1},
		{ShardIndex: -1, ShardCount: 1},
		{ShardIndex: 1, ShardCount: 1},
	} {
		if _, _, err := selectDifferentialCases(all, selection); err == nil {
			t.Fatalf("invalid shard coordinates accepted: %#v", selection)
		}
	}
}

func TestSelectedDifferentialArtifactRecordsExactSelectionAndFailures(t *testing.T) {
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	all := differentialCases()
	fullCatalog, err := loadDifferentialCatalog(differentialCatalogPath(repoRoot), all)
	if err != nil {
		t.Fatalf("loadDifferentialCatalog() error = %v", err)
	}
	selected, normalized, err := selectDifferentialCases(all, DifferentialSelection{
		Plugins:    []string{" csrf "},
		Cases:      []string{"csrf-safe-get-issues-cookie"},
		ShardIndex: 0,
		ShardCount: 1,
	})
	if err != nil {
		t.Fatalf("selectDifferentialCases() error = %v", err)
	}
	selectedCatalog, err := selectDifferentialCatalog(fullCatalog, selected)
	if err != nil {
		t.Fatalf("selectDifferentialCatalog() error = %v", err)
	}
	if selectedCatalog.CatalogSHA256 != fullCatalog.CatalogSHA256 ||
		len(selectedCatalog.RequiredPlugins) != differentialRequiredPluginCount ||
		len(selectedCatalog.Cases) != 1 {
		t.Fatalf("selected catalog lost full identity or exact rows: %#v", selectedCatalog)
	}

	failedResult := DifferentialCaseResult{
		Name:             selected[0].Name,
		Plugin:           selected[0].Plugin,
		ComparisonPolicy: selected[0].ComparisonPolicy,
		FirstAttempt:     true,
		Passed:           false,
		Error:            "candidate rejected the request",
	}
	artifact, err := buildSelectedDifferentialArtifact(
		selectedCatalog,
		selected,
		normalized,
		DifferentialCandidateID{SourceCommit: "candidate"},
		OracleIdentity{SourceCommit: compatibilityOracleSourceCommit},
		[]DifferentialCaseResult{failedResult},
	)
	if err != nil {
		t.Fatalf("buildSelectedDifferentialArtifact() error = %v", err)
	}
	if artifact.SchemaVersion != 3 ||
		!reflect.DeepEqual(artifact.Selection.Plugins, []string{"csrf"}) ||
		!reflect.DeepEqual(artifact.Selection.Cases, []string{"csrf-safe-get-issues-cookie"}) ||
		artifact.Selection.ShardIndex != 0 || artifact.Selection.ShardCount != 1 ||
		artifact.Selection.SelectedCaseCount != 1 || artifact.Selection.FullCatalogRun {
		t.Fatalf("artifact selection = %#v", artifact.Selection)
	}
	if artifact.Passed != 0 || artifact.Failed != 1 || artifact.Result != "fail" ||
		len(artifact.Cases) != 1 || artifact.Cases[0].Error != failedResult.Error {
		t.Fatalf("artifact did not retain factual failure: %#v", artifact)
	}

	for _, tt := range []struct {
		name    string
		results []DifferentialCaseResult
		want    string
	}{
		{name: "missing result", results: nil, want: "cover 0/1"},
		{
			name: "wrong plugin",
			results: []DifferentialCaseResult{{
				Name: failedResult.Name, Plugin: "client-control",
				ComparisonPolicy: failedResult.ComparisonPolicy,
			}},
			want: "runtime plugin",
		},
		{
			name: "wrong comparison policy",
			results: []DifferentialCaseResult{{
				Name: failedResult.Name, Plugin: failedResult.Plugin,
				ComparisonPolicy: "",
			}},
			want: "comparison policy",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildSelectedDifferentialArtifact(
				selectedCatalog,
				selected,
				normalized,
				DifferentialCandidateID{},
				OracleIdentity{},
				tt.results,
			); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("buildSelectedDifferentialArtifact() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBuildSelectedDifferentialArtifactRejectsFalseSelectionMetadata(t *testing.T) {
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	all := differentialCases()
	fullCatalog, err := loadDifferentialCatalog(differentialCatalogPath(repoRoot), all)
	if err != nil {
		t.Fatalf("loadDifferentialCatalog() error = %v", err)
	}
	honestSelection := DifferentialSelection{
		Plugins:    []string{"csrf"},
		Cases:      []string{"csrf-safe-get-issues-cookie"},
		ShardIndex: 0,
		ShardCount: 1,
	}
	selected, honestSelection, err := selectDifferentialCases(all, honestSelection)
	if err != nil {
		t.Fatalf("selectDifferentialCases() error = %v", err)
	}
	selectedCatalog, err := selectDifferentialCatalog(fullCatalog, selected)
	if err != nil {
		t.Fatalf("selectDifferentialCatalog() error = %v", err)
	}
	results := []DifferentialCaseResult{{
		Name:             selected[0].Name,
		Plugin:           selected[0].Plugin,
		ComparisonPolicy: selected[0].ComparisonPolicy,
		FirstAttempt:     true,
		Passed:           true,
	}}

	for _, tt := range []struct {
		name      string
		catalog   DifferentialCatalog
		runtime   []DifferentialCase
		selection DifferentialSelection
		results   []DifferentialCaseResult
	}{
		{
			name:      "subset cannot claim empty-selector full run",
			catalog:   selectedCatalog,
			runtime:   selected,
			selection: DifferentialSelection{ShardIndex: 0, ShardCount: 1},
			results:   results,
		},
		{
			name:    "plugin selector must match selected rows",
			catalog: selectedCatalog,
			runtime: selected,
			selection: DifferentialSelection{
				Plugins: []string{"client-control"}, ShardIndex: 0, ShardCount: 1,
			},
			results: results,
		},
		{
			name:      "empty rows and runtime are rejected",
			catalog:   differentialCatalogWithCases(selectedCatalog, nil),
			runtime:   nil,
			selection: honestSelection,
			results:   nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildSelectedDifferentialArtifact(
				tt.catalog,
				tt.runtime,
				tt.selection,
				DifferentialCandidateID{},
				OracleIdentity{},
				tt.results,
			); err == nil || !strings.Contains(err.Error(), "selection") {
				t.Fatalf("buildSelectedDifferentialArtifact() error = %v, want selection binding rejection", err)
			}
		})
	}

	shardSelection := DifferentialSelection{
		Plugins:    []string{"csrf"},
		ShardIndex: 1,
		ShardCount: 2,
	}
	shardCases, shardSelection, err := selectDifferentialCases(all, shardSelection)
	if err != nil {
		t.Fatalf("selectDifferentialCases(shard) error = %v", err)
	}
	shardCatalog, err := selectDifferentialCatalog(fullCatalog, shardCases)
	if err != nil {
		t.Fatalf("selectDifferentialCatalog(shard) error = %v", err)
	}
	shardResult := DifferentialCaseResult{
		Name:             shardCases[0].Name,
		Plugin:           shardCases[0].Plugin,
		ComparisonPolicy: shardCases[0].ComparisonPolicy,
		FirstAttempt:     true,
		Passed:           true,
	}
	artifact, err := buildSelectedDifferentialArtifact(
		shardCatalog,
		shardCases,
		shardSelection,
		DifferentialCandidateID{},
		OracleIdentity{},
		[]DifferentialCaseResult{shardResult},
	)
	if err != nil {
		t.Fatalf("honest selected shard rejected: %v", err)
	}
	if artifact.Selection.FullCatalogRun || artifact.Selection.ShardIndex != 1 ||
		artifact.Selection.ShardCount != 2 || artifact.Selection.SelectedCaseCount != 1 {
		t.Fatalf("honest shard selection = %#v", artifact.Selection)
	}
}

func TestBuildDifferentialArtifactIsStableForPermutedResults(t *testing.T) {
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	all := differentialCases()
	fullCatalog, err := loadDifferentialCatalog(differentialCatalogPath(repoRoot), all)
	if err != nil {
		t.Fatalf("loadDifferentialCatalog() error = %v", err)
	}
	selected, normalized, err := selectDifferentialCases(all, DifferentialSelection{
		Plugins:    []string{"client-control"},
		ShardIndex: 0,
		ShardCount: 1,
	})
	if err != nil {
		t.Fatalf("selectDifferentialCases() error = %v", err)
	}
	selectedCatalog, err := selectDifferentialCatalog(fullCatalog, selected)
	if err != nil {
		t.Fatalf("selectDifferentialCatalog() error = %v", err)
	}
	results := []DifferentialCaseResult{
		{
			Name: selected[0].Name, Plugin: selected[0].Plugin,
			ComparisonPolicy: selected[0].ComparisonPolicy, FirstAttempt: true, Passed: true,
		},
		{
			Name: selected[1].Name, Plugin: selected[1].Plugin,
			ComparisonPolicy: selected[1].ComparisonPolicy, FirstAttempt: true, Passed: true,
		},
	}
	build := func(results []DifferentialCaseResult) []byte {
		t.Helper()
		artifact, err := buildSelectedDifferentialArtifact(
			selectedCatalog,
			selected,
			normalized,
			DifferentialCandidateID{SourceCommit: "candidate"},
			OracleIdentity{SourceCommit: compatibilityOracleSourceCommit},
			results,
		)
		if err != nil {
			t.Fatalf("buildSelectedDifferentialArtifact() error = %v", err)
		}
		data, err := json.Marshal(artifact)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	forward := build(results)
	reverse := build([]DifferentialCaseResult{results[1], results[0]})
	if !bytes.Equal(forward, reverse) {
		t.Fatalf("permuted results changed artifact bytes:\nforward=%s\nreverse=%s", forward, reverse)
	}
}

func TestDifferentialCaseComparisonScopesPlatformOwnedErrorRepresentation(t *testing.T) {
	policy := testNormalizationPolicy()
	spec := DifferentialCase{
		Plugin:           "client-control",
		ComparisonPolicy: differentialComparisonPlatformOwnedErrorRepresentation,
	}
	left := DifferentialObservation{
		Status: 413, Body: "", Headers: map[string][]string{"Date": {"candidate-date"}},
		Host: "gateway.example.test", SecurityDecision: "deny",
		Upstream: DifferentialUpstreamObservation{Received: false},
	}
	right := cloneDifferentialObservation(left)
	right.Body = "<html>nginx generated 413</html>"
	right.Headers = map[string][]string{
		"Content-Type": {"text/html"}, "Content-Length": {"32"}, "Date": {"oracle-date"},
	}
	leftBefore := cloneDifferentialObservation(left)
	rightBefore := cloneDifferentialObservation(right)

	passed, diff, err := compareDifferentialCaseObservations(spec, left, right, policy)
	if err != nil {
		t.Fatalf("compareDifferentialCaseObservations() error = %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf(
			"platform-owned representation difference rejected: passed=%t diff=%q",
			passed,
			diff,
		)
	}
	if !reflect.DeepEqual(left, leftBefore) || !reflect.DeepEqual(right, rightBefore) {
		t.Fatalf("case comparison mutated inputs: left=%#v right=%#v", left, right)
	}

	for _, candidateEdit := range []struct {
		name string
		edit func(*DifferentialObservation)
	}{
		{name: "body", edit: func(observation *DifferentialObservation) {
			observation.Body = "candidate-owned error"
		}},
		{name: "content type", edit: func(observation *DifferentialObservation) {
			observation.Headers["Content-Type"] = []string{"text/plain"}
		}},
	} {
		t.Run("candidate "+candidateEdit.name+" remains strict", func(t *testing.T) {
			changed := cloneDifferentialObservation(left)
			candidateEdit.edit(&changed)
			passed, _, err := compareDifferentialCaseObservations(spec, changed, right, policy)
			if err != nil {
				t.Fatalf("compareDifferentialCaseObservations() error = %v", err)
			}
			if passed {
				t.Fatalf(
					"candidate %s was normalized by the oracle-only policy",
					candidateEdit.name,
				)
			}
		})
	}

	right.Status = 401
	passed, _, err = compareDifferentialCaseObservations(spec, left, right, policy)
	if err != nil {
		t.Fatalf("compareDifferentialCaseObservations() status error = %v", err)
	}
	if passed {
		t.Fatal("platform-owned response policy normalized a status difference")
	}

	spec.ComparisonPolicy = "unknown"
	if _, _, err := compareDifferentialCaseObservations(spec, left, right, policy); err == nil {
		t.Fatal("unknown case comparison policy was accepted")
	}
}

func TestDifferentialCompressedResponseComparisonDecodesEachSideStrictly(t *testing.T) {
	encode := func(t *testing.T, algorithm, body string, variant int) string {
		t.Helper()
		var output bytes.Buffer
		switch algorithm {
		case "gzip":
			writer, err := gzip.NewWriterLevel(&output, gzip.BestSpeed+variant)
			if err != nil {
				t.Fatal(err)
			}
			writer.ModTime = time.Unix(int64(variant+1), 0)
			if _, err := writer.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		case "br":
			writer := brotli.NewWriterLevel(&output, 3+variant)
			if _, err := writer.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unsupported test encoding %q", algorithm)
		}
		return output.String()
	}

	const body = "0123456789\n012345678"
	for _, test := range []struct {
		plugin    string
		algorithm string
	}{
		{plugin: "gzip", algorithm: "gzip"},
		{plugin: "brotli", algorithm: "br"},
	} {
		t.Run(test.plugin, func(t *testing.T) {
			var spec DifferentialCase
			if test.plugin == "gzip" {
				spec = differentialGzipCases()[0]
			} else {
				spec = differentialBrotliCases()[0]
			}
			candidate := DifferentialObservation{
				Status: http.StatusOK,
				Headers: map[string][]string{
					"Content-Encoding": {test.algorithm}, "Content-Length": {"31"},
					"Content-Type": {"text/html"},
				},
				Body: encode(t, test.algorithm, body, 0), Host: spec.Request.Host,
				SecurityDecision: "not_applicable",
				Upstream:         DifferentialUpstreamObservation{Received: true, Fixture: "primary"},
			}
			oracle := cloneDifferentialObservation(candidate)
			oracle.Body = encode(t, test.algorithm, body, 1)
			oracle.Headers["Content-Length"] = []string{"47"}
			oracle.Headers["Content-Type"] = []string{"text/html; charset=utf-8"}
			candidateBefore := cloneDifferentialObservation(candidate)
			oracleBefore := cloneDifferentialObservation(oracle)

			passed, diff, err := compareDifferentialCaseObservations(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err != nil {
				t.Fatalf("compare compressed response: %v", err)
			}
			if !passed || diff != "" {
				t.Fatalf("equivalent compressed bodies rejected: passed=%t diff=%q", passed, diff)
			}
			if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
				t.Fatal("compressed response comparison mutated its observations")
			}

			missingEncoding := cloneDifferentialObservation(candidate)
			delete(missingEncoding.Headers, "Content-Encoding")
			if _, _, err := compareDifferentialCaseObservations(
				spec, missingEncoding, oracle, testNormalizationPolicy(),
			); err == nil {
				t.Fatal("compressed response policy accepted missing Content-Encoding")
			}

			corrupt := cloneDifferentialObservation(candidate)
			corrupt.Body = "not-compressed"
			if _, _, err := compareDifferentialCaseObservations(
				spec, corrupt, oracle, testNormalizationPolicy(),
			); err == nil {
				t.Fatal("compressed response policy accepted corrupt body")
			}

			different := cloneDifferentialObservation(oracle)
			different.Body = encode(t, test.algorithm, body+"changed", 1)
			_, _, err = compareDifferentialCaseObservations(
				spec, candidate, different, testNormalizationPolicy(),
			)
			if err == nil {
				t.Fatal("compressed response policy accepted a decoded body difference")
			}
		})
	}
}

func TestDifferentialChaitinWAFComparisonScopesOnlyElapsedTime(t *testing.T) {
	spec := differentialChaitinWAFCases()[0]
	const body = "{\"code\": 403, \"success\":false, \"message\": \"blocked by Chaitin SafeLine Web Application Firewall\", \"event_id\": \"b3c6ce574dc24f09a01f634a39dca83b\"}\n"
	candidate := DifferentialObservation{
		Status: http.StatusForbidden,
		Headers: map[string][]string{
			"X-APISIX-CHAITIN-WAF":        {"yes"},
			"X-APISIX-CHAITIN-WAF-STATUS": {"403"},
			"X-APISIX-CHAITIN-WAF-ACTION": {"reject"},
			"X-APISIX-CHAITIN-WAF-TIME":   {"1"},
		},
		Body: body, Host: spec.Request.Host, SecurityDecision: "deny",
		UpstreamFixture: "waf", UpstreamAddress: "127.0.0.1:12345",
		Upstream: DifferentialUpstreamObservation{
			Received: true, Fixture: "waf", Method: http.MethodGet, Path: "/hello",
			Host: spec.Request.Host,
		},
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.Headers["X-APISIX-CHAITIN-WAF-TIME"] = []string{"3"}
	candidateBefore := cloneDifferentialObservation(candidate)
	oracleBefore := cloneDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialCaseObservations(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compare Chaitin WAF response: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("elapsed-time-only difference rejected: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("Chaitin WAF comparison mutated its observations")
	}

	for _, value := range []string{"", "not-a-number", "-1"} {
		changed := cloneDifferentialObservation(candidate)
		if value == "" {
			delete(changed.Headers, "X-APISIX-CHAITIN-WAF-TIME")
		} else {
			changed.Headers["X-APISIX-CHAITIN-WAF-TIME"] = []string{value}
		}
		if _, _, err := compareDifferentialCaseObservations(
			spec, changed, oracle, testNormalizationPolicy(),
		); err == nil {
			t.Fatalf("Chaitin WAF policy accepted elapsed time %q", value)
		}
	}

	changed := cloneDifferentialObservation(oracle)
	changed.Headers["X-APISIX-CHAITIN-WAF-ACTION"] = []string{"pass"}
	_, _, err = compareDifferentialCaseObservations(
		spec, candidate, changed, testNormalizationPolicy(),
	)
	if err == nil {
		t.Fatal("Chaitin WAF policy accepted an action difference")
	}
}

func TestDifferentialLimitReqComparisonScopesOnlyPinned503Representation(t *testing.T) {
	spec := differentialLimitReqCases()[1]
	candidate := DifferentialObservation{
		Steps: []DifferentialStepObservation{
			{
				Status: http.StatusOK,
				Headers: map[string][]string{
					"Content-Type":   {"text/plain; charset=utf-8"},
					"Content-Length": {"11"},
				},
				Body:             "hello world",
				Host:             "gateway.example.test",
				SecurityDecision: "allow",
			},
		},
		UpstreamFixture: "primary",
		UpstreamAddress: "127.0.0.1:12345",
		Upstream: DifferentialUpstreamObservation{
			Received: true,
			Fixture:  "primary",
			Method:   http.MethodGet,
			Path:     "/hello",
			Host:     "differential.example.test",
		},
		UpstreamCalls: []DifferentialUpstreamObservation{
			{
				Received: true,
				Fixture:  "primary",
				Method:   http.MethodGet,
				Path:     "/hello",
				Host:     "differential.example.test",
			},
		},
	}
	oracle := cloneDifferentialObservation(candidate)
	for range 3 {
		candidate.Steps = append(candidate.Steps, DifferentialStepObservation{
			Status: http.StatusServiceUnavailable, Headers: map[string][]string{"Content-Length": {"0"}},
			Host: "gateway.example.test", SecurityDecision: "deny",
		})
		oracle.Steps = append(oracle.Steps, DifferentialStepObservation{
			Status: http.StatusServiceUnavailable,
			Headers: map[string][]string{
				"Content-Type": {"text/html; charset=utf-8"}, "Content-Length": {"269"},
			},
			Body: differentialLimitCountOracle503Body, Host: "gateway.example.test", SecurityDecision: "deny",
		})
	}
	candidateBefore := cloneDifferentialObservation(candidate)
	oracleBefore := cloneDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialCaseObservations(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compare limit-req response: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("pinned 503 representation difference rejected: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("limit-req comparison mutated its observations")
	}

	changedOracle := cloneDifferentialObservation(oracle)
	changedOracle.Steps[2].Body = "different platform page"
	if passed, _, _ := compareDifferentialCaseObservations(
		spec, candidate, changedOracle, testNormalizationPolicy(),
	); passed {
		t.Fatal("limit-req policy accepted a different oracle 503 representation")
	}
	changedCandidate := cloneDifferentialObservation(candidate)
	changedCandidate.Steps[2].Body = "candidate-owned error"
	if passed, _, _ := compareDifferentialCaseObservations(
		spec, changedCandidate, oracle, testNormalizationPolicy(),
	); passed {
		t.Fatal("limit-req policy accepted a candidate-owned 503 body")
	}
}

func TestDifferentialAuthzCasdoorUsesPlatformOwnedErrorRepresentationNarrowly(t *testing.T) {
	policy := testNormalizationPolicy()
	spec := DifferentialCase{
		Plugin:           "authz-casdoor",
		ComparisonPolicy: differentialComparisonPlatformOwnedErrorRepresentation,
	}
	candidate := DifferentialObservation{
		Status: http.StatusServiceUnavailable, Headers: map[string][]string{}, Body: "",
		Host: "gateway.example.test", SecurityDecision: "deny",
		Upstream: DifferentialUpstreamObservation{Received: false},
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.Body = "<html>nginx generated 503</html>"
	oracle.Headers = map[string][]string{
		"Content-Type":   {"text/html; charset=utf-8"},
		"Content-Length": {"32"},
	}

	passed, diff, err := compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare authz-casdoor representation: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("authz-casdoor platform representation rejected: passed=%t diff=%q", passed, diff)
	}

	candidate.Body = `{"message":"no session found"}`
	passed, _, err = compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare authz-casdoor candidate body: %v", err)
	}
	if passed {
		t.Fatal("authz-casdoor policy ignored a candidate-owned body")
	}

	candidate.Body = ""
	oracle.Status = http.StatusBadGateway
	passed, _, err = compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare authz-casdoor changed status: %v", err)
	}
	if passed {
		t.Fatal("authz-casdoor policy ignored a status difference")
	}
}

func TestDifferentialOpenIDConnectUsesPlatformOwnedErrorRepresentationNarrowly(t *testing.T) {
	policy := testNormalizationPolicy()
	spec := DifferentialCase{
		Plugin:           "openid-connect",
		ComparisonPolicy: differentialComparisonPlatformOwnedErrorRepresentation,
	}
	candidate := DifferentialObservation{
		Status: http.StatusUnauthorized,
		Headers: map[string][]string{
			"WWW-Authenticate": {`Bearer realm="apisix"`},
		},
		Body: "", Host: "gateway.example.test", SecurityDecision: "deny",
		Upstream: DifferentialUpstreamObservation{Received: false},
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.Body = "<html>nginx generated 401</html>"
	oracle.Headers["Content-Type"] = []string{"text/html; charset=utf-8"}
	oracle.Headers["Content-Length"] = []string{"32"}

	passed, diff, err := compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare openid-connect representation: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("openid-connect platform representation rejected: passed=%t diff=%q", passed, diff)
	}

	candidate.Body = "No bearer token found in request.\n"
	passed, _, err = compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare openid-connect candidate body: %v", err)
	}
	if passed {
		t.Fatal("openid-connect policy ignored a candidate-owned body")
	}
}

func TestDifferentialErrorPageComparisonAllowsOnlyOracleUTF8CharsetParameter(t *testing.T) {
	policy := testNormalizationPolicy()
	spec := DifferentialCase{
		Plugin:           "error-page",
		ComparisonPolicy: "error-page-charset-parameter",
	}
	candidate := DifferentialObservation{
		Status: http.StatusInternalServerError,
		Headers: map[string][]string{
			"Content-Type":   {"text/html"},
			"Content-Length": {"60"},
		},
		Body: "<html><body><h1>500 Internal Server Error</h1></body></html>",
		Host: "gateway.example.test", SecurityDecision: "not_applicable",
		Upstream: DifferentialUpstreamObservation{Received: false},
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.Headers["Content-Type"] = []string{"text/html; charset=utf-8"}

	passed, diff, err := compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare error-page charset: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("error-page charset parameter rejected: passed=%t diff=%q", passed, diff)
	}

	tests := []struct {
		name string
		edit func(*DifferentialObservation, *DifferentialObservation)
	}{
		{name: "different media type", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Headers["Content-Type"] = []string{"text/plain; charset=utf-8"}
		}},
		{name: "candidate parameter", edit: func(left, _ *DifferentialObservation) {
			left.Headers["Content-Type"] = []string{"text/html; charset=utf-8"}
		}},
		{name: "oracle wrong charset", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Headers["Content-Type"] = []string{"text/html; charset=iso-8859-1"}
		}},
		{name: "oracle extra parameter", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Headers["Content-Type"] = []string{"text/html; charset=utf-8; boundary=x"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left := cloneDifferentialObservation(candidate)
			right := cloneDifferentialObservation(oracle)
			tt.edit(&left, &right)
			passed, _, err := compareDifferentialCaseObservations(spec, left, right, policy)
			if err == nil && passed {
				t.Fatalf("error-page policy accepted %s", tt.name)
			}
		})
	}
}

func TestDifferentialForwardAuthComparisonNormalizesOnlyEmptyErrorContentTypeAndFixtureHost(t *testing.T) {
	policy := testNormalizationPolicy()
	spec := DifferentialCase{
		Plugin:           "forward-auth",
		ComparisonPolicy: "forward-auth-empty-error-content-type",
	}
	candidate := DifferentialObservation{
		Status: http.StatusForbidden,
		Headers: map[string][]string{
			"Content-Length": {"0"},
			"Location":       {"http://example.com/auth"},
		},
		Body: "", UpstreamFixture: "primary", UpstreamAddress: "127.0.0.1:49257",
		Host: "gateway.example.test", SecurityDecision: "deny",
		Upstream: DifferentialUpstreamObservation{
			Received: true, Fixture: "primary", Method: http.MethodGet, Path: "/auth",
			Host: "127.0.0.1:49257", Headers: map[string][]string{"Authorization": {"333"}},
		},
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.Headers = map[string][]string{
		"Content-Type": {"text/plain; charset=utf-8"},
		"Location":     {"http://example.com/auth"},
	}
	oracle.UpstreamAddress = "127.0.0.1:1980"
	oracle.Upstream.Host = "127.0.0.1:1980"

	passed, diff, err := compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare forward-auth boundary: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("forward-auth boundary rejected: passed=%t diff=%q", passed, diff)
	}

	tests := []struct {
		name string
		edit func(*DifferentialObservation, *DifferentialObservation)
	}{
		{name: "different status", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Status = http.StatusUnauthorized
		}},
		{name: "nonempty body", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Body = "denied"
		}},
		{name: "candidate content type", edit: func(left, _ *DifferentialObservation) {
			left.Headers["Content-Type"] = []string{"text/plain"}
		}},
		{name: "oracle different content type", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Headers["Content-Type"] = []string{"application/json"}
		}},
		{name: "semantic location", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Headers["Location"] = []string{"http://example.com/other"}
		}},
		{name: "semantic auth header", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Upstream.Headers["Authorization"] = []string{"444"}
		}},
		{name: "custom upstream host", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Upstream.Host = "auth.example.test"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left := cloneDifferentialObservation(candidate)
			right := cloneDifferentialObservation(oracle)
			tt.edit(&left, &right)
			passed, _, err := compareDifferentialCaseObservations(spec, left, right, policy)
			if err == nil && passed {
				t.Fatalf("forward-auth policy accepted %s", tt.name)
			}
		})
	}
}

func TestDifferentialGraphQLHeadErrorContentTypeComparisonIsNarrow(t *testing.T) {
	policy := testNormalizationPolicy()
	candidate := DifferentialObservation{
		Status: http.StatusMethodNotAllowed,
		Headers: map[string][]string{
			"X-Semantic": {"same"},
		},
		Body: "", Host: "gateway.example.test", SecurityDecision: "not_applicable",
		Upstream: DifferentialUpstreamObservation{Received: false},
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.Headers["Content-Type"] = []string{"text/html; charset=utf-8"}

	for _, plugin := range []string{"graphql-limit-count", "graphql-proxy-cache"} {
		t.Run(plugin, func(t *testing.T) {
			spec := DifferentialCase{
				Plugin:           plugin,
				Request:          DifferentialRequest{Method: http.MethodHead},
				ComparisonPolicy: differentialComparisonGraphQLHeadErrorContentType,
			}
			passed, diff, err := compareDifferentialCaseObservations(spec, candidate, oracle, policy)
			if err != nil {
				t.Fatalf("compare GraphQL HEAD boundary: %v", err)
			}
			if !passed || diff != "" {
				t.Fatalf("GraphQL HEAD Content-Type boundary rejected: passed=%t diff=%q", passed, diff)
			}
		})
	}

	for _, spec := range []DifferentialCase{
		{
			Plugin: "degraphql", Request: DifferentialRequest{Method: http.MethodHead},
			ComparisonPolicy: differentialComparisonGraphQLHeadErrorContentType,
		},
		{
			Plugin: "graphql-limit-count", Request: DifferentialRequest{Method: http.MethodGet},
			ComparisonPolicy: differentialComparisonGraphQLHeadErrorContentType,
		},
	} {
		if _, _, err := compareDifferentialCaseObservations(spec, candidate, oracle, policy); err == nil {
			t.Fatalf("GraphQL HEAD policy accepted plugin %q method %q", spec.Plugin, spec.Request.Method)
		}
	}

	spec := DifferentialCase{
		Plugin: "graphql-limit-count", Request: DifferentialRequest{Method: http.MethodHead},
		ComparisonPolicy: differentialComparisonGraphQLHeadErrorContentType,
	}
	tests := []struct {
		name string
		edit func(*DifferentialObservation, *DifferentialObservation)
	}{
		{name: "status", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Status = http.StatusBadRequest
		}},
		{name: "body", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Body = "method not allowed"
		}},
		{name: "oracle media", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Headers["Content-Type"] = []string{"text/plain; charset=utf-8"}
		}},
		{name: "candidate content type", edit: func(left, _ *DifferentialObservation) {
			left.Headers["Content-Type"] = []string{"text/html; charset=utf-8"}
		}},
		{name: "upstream", edit: func(_ *DifferentialObservation, right *DifferentialObservation) {
			right.Upstream.Received = true
		}},
	}
	for _, tt := range tests {
		t.Run("rejects changed "+tt.name, func(t *testing.T) {
			left := cloneDifferentialObservation(candidate)
			right := cloneDifferentialObservation(oracle)
			tt.edit(&left, &right)
			passed, _, err := compareDifferentialCaseObservations(spec, left, right, policy)
			if err == nil && passed {
				t.Fatalf("GraphQL HEAD policy accepted changed %s", tt.name)
			}
		})
	}
}

func TestDifferentialRedirectComparisonScopesPlatformOwnedBody(t *testing.T) {
	policy := testNormalizationPolicy()
	spec := DifferentialCase{
		Name:             "redirect-fixed-uri-301",
		Plugin:           "redirect",
		ComparisonPolicy: "platform-owned-redirect-representation",
	}
	candidate := DifferentialObservation{
		Status: 301,
		Headers: map[string][]string{
			"Content-Length": {"0"},
			"Location":       {"/test/add"},
		},
		SecurityDecision: "not_applicable",
		Upstream:         DifferentialUpstreamObservation{Received: false},
	}
	oracle := candidate
	oracle.Body = "<html>NGINX-generated redirect page</html>"
	oracle.Headers = map[string][]string{
		"Content-Length": {"43"},
		"Content-Type":   {"text/html"},
		"Location":       {"/test/add"},
	}

	passed, diff, err := compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare redirect representation: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("platform-owned redirect body rejected: passed=%t diff=%q", passed, diff)
	}

	oracle.Headers["Location"] = []string{"/different"}
	passed, _, err = compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare redirect Location: %v", err)
	}
	if passed {
		t.Fatal("redirect comparison ignored semantic Location difference")
	}
}

func TestDifferentialFixtureOwnedUpstreamEndpointNormalizesOnlyProjectionAndQueryOrder(t *testing.T) {
	policy := testNormalizationPolicy()
	spec := DifferentialCase{
		Name:             "openwhisk-json-action-response",
		Plugin:           "openwhisk",
		ComparisonPolicy: differentialComparisonFixtureOwnedUpstreamEndpoint,
	}
	candidate := DifferentialObservation{
		Status:           http.StatusOK,
		Headers:          map[string][]string{"Content-Type": {"application/json"}},
		Body:             `{"hello":"world"}`,
		UpstreamFixture:  "primary",
		UpstreamAddress:  "127.0.0.1:54321",
		Host:             "gateway.example.test",
		SecurityDecision: "allow",
		Upstream: DifferentialUpstreamObservation{
			Received: true,
			Fixture:  "primary",
			Method:   http.MethodPost,
			Path:     "/api/v1/namespaces/guest/actions/test-params?blocking=true&result=true&timeout=3000",
			Host:     "127.0.0.1:54321",
		},
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.UpstreamAddress = "127.0.0.1:1980"
	oracle.Upstream.Host = "127.0.0.1:1980"
	oracle.Upstream.Path = "/api/v1/namespaces/guest/actions/test-params?timeout=3000&result=true&blocking=true"

	passed, diff, err := compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare fixture-owned authority: %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("fixture-owned authority rejected: passed=%t diff=%q", passed, diff)
	}

	oracle.Upstream.Host = "unrelated.example.test:1980"
	passed, _, err = compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err == nil {
		t.Fatal("non-fixture upstream Host was accepted without a policy error")
	}
	if passed {
		t.Fatal("non-fixture upstream Host was normalized")
	}

	oracle.Upstream.Host = oracle.UpstreamAddress
	oracle.Upstream.Path = "/api/v1/namespaces/guest/actions/test-params?timeout=3001&result=true&blocking=true"
	passed, _, err = compareDifferentialCaseObservations(spec, candidate, oracle, policy)
	if err != nil {
		t.Fatalf("compare changed query value: %v", err)
	}
	if passed {
		t.Fatal("fixture endpoint policy normalized a semantic query value difference")
	}
}

func TestDifferentialComparatorRegistryBindsPolicyAndPreservesInputs(t *testing.T) {
	policy := testNormalizationPolicy()
	left := DifferentialObservation{
		Status: 413,
		Headers: map[string][]string{
			"Date":       {"candidate-date"},
			"X-Semantic": {"same"},
		},
		Host:             "gateway.example.test",
		SecurityDecision: "deny",
		Upstream: DifferentialUpstreamObservation{
			Received: false,
			Headers:  map[string][]string{"X-Nested": {"same"}},
		},
	}
	right := cloneDifferentialObservation(left)
	right.Body = "<html>nginx generated 413</html>"
	right.Headers["Content-Type"] = []string{"text/html"}
	right.Headers["Content-Length"] = []string{"32"}
	leftBefore := cloneDifferentialObservation(left)
	rightBefore := cloneDifferentialObservation(right)

	passed, diff, err := compareDifferentialCaseObservations(DifferentialCase{
		Plugin:           "client-control",
		ComparisonPolicy: differentialComparisonPlatformOwnedErrorRepresentation,
	}, left, right, policy)
	if err != nil {
		t.Fatalf("compareDifferentialCaseObservations() error = %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("registered comparator rejected owned representation: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(left, leftBefore) || !reflect.DeepEqual(right, rightBefore) {
		t.Fatalf("registered comparator mutated caller inputs: left=%#v right=%#v", left, right)
	}

	for _, spec := range []DifferentialCase{
		{Plugin: "csrf", ComparisonPolicy: differentialComparisonPlatformOwnedErrorRepresentation},
		{Plugin: "client-control", ComparisonPolicy: differentialCSRFIssuedCookieComparisonPolicy},
		{Plugin: "client-control", ComparisonPolicy: "unknown"},
	} {
		if _, _, err := compareDifferentialCaseObservations(spec, left, right, policy); err == nil {
			t.Fatalf("comparison policy %q accepted for plugin %q", spec.ComparisonPolicy, spec.Plugin)
		}
	}

	strictRight := cloneDifferentialObservation(left)
	strictRight.Body = "different"
	passed, _, err = compareDifferentialCaseObservations(
		DifferentialCase{Plugin: "client-control"},
		left,
		strictRight,
		policy,
	)
	if err != nil {
		t.Fatalf("strict comparison error = %v", err)
	}
	if passed {
		t.Fatal("empty comparison policy did not remain strict")
	}
}

func TestDifferentialCSRFCookieComparisonNormalizesOnlyDynamicTokenFields(t *testing.T) {
	policy := testNormalizationPolicy()
	spec := differentialCSRFCases()[0]
	const key = "userkey"
	const ttl int64 = 1000000000
	sign := func(random float64, expires int64) string {
		t.Helper()
		signature := sha256.Sum256(fmt.Appendf(nil,
			"{expires:%d,random:%v,key:%s}", expires, random, key,
		))
		return hex.EncodeToString(signature[:])
	}
	token := func(random float64, expires int64) string {
		t.Helper()
		payload := fmt.Sprintf(
			`{"random":%g,"expires":%d,"sign":%q}`,
			random,
			expires,
			sign(random, expires),
		)
		return base64.StdEncoding.EncodeToString([]byte(payload))
	}
	cookie := func(random float64, expires int64) string {
		t.Helper()
		return "apisix-csrf-token=" + token(random, expires) +
			"; Path=/; Expires=" + time.Unix(expires+ttl, 0).UTC().Format(http.TimeFormat) +
			"; SameSite=Lax"
	}
	left := DifferentialObservation{
		Status: 200, Body: "same", Host: "gateway.example.test", SecurityDecision: "allow",
		Headers:  map[string][]string{"Set-Cookie": {cookie(0.25, 1700000000)}},
		Upstream: DifferentialUpstreamObservation{Received: true},
	}
	right := cloneDifferentialObservation(left)
	right.Headers["Set-Cookie"] = []string{cookie(0.75, 1800000000)}
	leftBefore := cloneDifferentialObservation(left)
	rightBefore := cloneDifferentialObservation(right)

	passed, diff, err := compareDifferentialCaseObservations(spec, left, right, policy)
	if err != nil {
		t.Fatalf("compareDifferentialCaseObservations() error = %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("dynamic CSRF token fields rejected: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(left, leftBefore) || !reflect.DeepEqual(right, rightBefore) {
		t.Fatal("CSRF cookie comparison mutated its input observations")
	}

	changed := cloneDifferentialObservation(right)
	changed.Headers["Set-Cookie"] = []string{
		strings.Replace(cookie(0.75, 1800000000), "Path=/", "Path=/other", 1),
	}
	passed, _, err = compareDifferentialCaseObservations(spec, left, changed, policy)
	if err != nil {
		t.Fatalf("compareDifferentialCaseObservations() attribute error = %v", err)
	}
	if passed {
		t.Fatal("CSRF cookie policy normalized a Path difference")
	}

	missing := cloneDifferentialObservation(right)
	delete(missing.Headers, "Set-Cookie")
	if _, _, err := compareDifferentialCaseObservations(spec, left, missing, policy); err == nil {
		t.Fatal("CSRF cookie policy accepted a missing cookie")
	}

	invalidSignature := cloneDifferentialObservation(right)
	invalidPayload := fmt.Sprintf(
		`{"random":%g,"expires":%d,"sign":%q}`,
		0.75,
		1800000000,
		strings.Repeat("b", 64),
	)
	invalidSignature.Headers["Set-Cookie"] = []string{
		"apisix-csrf-token=" + base64.StdEncoding.EncodeToString([]byte(invalidPayload)) +
			"; Path=/; Expires=" +
			time.Unix(1800000000+ttl, 0).UTC().Format(http.TimeFormat) +
			"; SameSite=Lax",
	}
	if _, _, err := compareDifferentialCaseObservations(spec, left, invalidSignature, policy); err == nil {
		t.Fatal("CSRF cookie policy accepted an invalid token signature")
	}

	unknownField := cloneDifferentialObservation(right)
	unknownFieldPayload := fmt.Sprintf(
		`{"random":%g,"expires":%d,"sign":%q,"key":%q}`,
		0.75,
		1800000000,
		sign(0.75, 1800000000),
		key,
	)
	unknownField.Headers["Set-Cookie"] = []string{
		"apisix-csrf-token=" + base64.StdEncoding.EncodeToString([]byte(unknownFieldPayload)) +
			"; Path=/; Expires=" +
			time.Unix(1800000000+ttl, 0).UTC().Format(http.TimeFormat) +
			"; SameSite=Lax",
	}
	if _, _, err := compareDifferentialCaseObservations(spec, left, unknownField, policy); err == nil {
		t.Fatal("CSRF cookie policy accepted an unknown token payload field")
	}

	for _, random := range []float64{-0.1, 1} {
		outOfRange := cloneDifferentialObservation(right)
		outOfRange.Headers["Set-Cookie"] = []string{
			"apisix-csrf-token=" + token(random, 1800000000) +
				"; Path=/; Expires=" +
				time.Unix(1800000000+ttl, 0).UTC().Format(http.TimeFormat) +
				"; SameSite=Lax",
		}
		if _, _, err := compareDifferentialCaseObservations(spec, left, outOfRange, policy); err == nil {
			t.Fatalf("CSRF cookie policy accepted random %v outside [0,1)", random)
		}
	}

	missingExpires := cloneDifferentialObservation(right)
	missingExpires.Headers["Set-Cookie"] = []string{
		"apisix-csrf-token=" + token(0.75, 1800000000) + "; Path=/; SameSite=Lax",
	}
	if _, _, err := compareDifferentialCaseObservations(spec, left, missingExpires, policy); err == nil {
		t.Fatal("CSRF cookie policy accepted a missing Expires attribute")
	}

	wrongExpires := cloneDifferentialObservation(right)
	wrongExpires.Headers["Set-Cookie"] = []string{
		"apisix-csrf-token=" + token(0.75, 1800000000) +
			"; Path=/; Expires=" +
			time.Unix(1800000000+ttl+2, 0).UTC().Format(http.TimeFormat) +
			"; SameSite=Lax",
	}
	if _, _, err := compareDifferentialCaseObservations(spec, left, wrongExpires, policy); err == nil {
		t.Fatal("CSRF cookie policy accepted an expiry unrelated to the token timestamp")
	}
}

func TestDifferentialCatalogRejectsDuplicatePluginObligation(t *testing.T) {
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(differentialCatalogPath(repoRoot))
	if err != nil {
		t.Fatal(err)
	}
	data = append(
		data,
		[]byte(
			"\n  - plugin: key-auth\n    obligation: allow-valid-api-key\n    case: key-auth-valid\n",
		)...)
	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDifferentialCatalog(path, differentialCases()); err == nil ||
		!strings.Contains(err.Error(), "duplicate plugin/obligation") {
		t.Fatalf("loadDifferentialCatalog() error = %v, want duplicate obligation rejection", err)
	}
}

func TestDifferentialProjectionOnlyChangesFixtureEndpoint(t *testing.T) {
	spec := differentialCases()[0]
	candidate, err := projectDifferentialConfig(spec.Config, "127.0.0.1:1980")
	if err != nil {
		t.Fatalf("candidate projection: %v", err)
	}
	oracle, err := projectDifferentialConfig(spec.Config, "host.containers.internal:1980")
	if err != nil {
		t.Fatalf("oracle projection: %v", err)
	}
	candidateYAML := string(mustYAML(t, candidate))
	oracleYAML := string(mustYAML(t, oracle))
	if strings.Contains(candidateYAML, differentialFixturePlaceholder) ||
		strings.Contains(oracleYAML, differentialFixturePlaceholder) {
		t.Fatal("fixture endpoint placeholder survived projection")
	}
	if !strings.Contains(candidateYAML, "127.0.0.1:1980") ||
		!strings.Contains(oracleYAML, "host.containers.internal:1980") {
		t.Fatalf("endpoint projections missing: candidate=%s oracle=%s", candidateYAML, oracleYAML)
	}
	for _, value := range []string{"differential-proxy-rewrite", "/rewritten", "rewritten.example.test"} {
		if strings.Count(candidateYAML, value) != strings.Count(oracleYAML, value) {
			t.Fatalf("non-endpoint value %q changed between projections", value)
		}
	}
}

func TestDifferentialProjectionAllowsAlreadyProjectedConfigWithoutEndpoint(t *testing.T) {
	config := map[string]any{
		"routes": []any{map[string]any{
			"id":       "already-projected",
			"upstream": map[string]any{"nodes": map[string]any{"127.0.0.1:1980": 1}},
		}},
	}
	projected, err := projectDifferentialConfig(config, "")
	if err != nil {
		t.Fatalf("project already-projected config: %v", err)
	}
	if !reflect.DeepEqual(projected, config) {
		t.Fatalf("projected config = %#v, want unchanged %#v", projected, config)
	}
}

func TestDifferentialProjectionProjectsFixtureHostAndIntegerPort(t *testing.T) {
	config := map[string]any{
		"routes": []any{map[string]any{
			"plugins": map[string]any{
				"example-plugin": map[string]any{
					"i":    11,
					"ip":   "{{FIXTURE.HOST}}",
					"port": "{{FIXTURE.PORT}}",
				},
			},
		}},
	}
	projected, err := projectDifferentialConfig(config, "127.0.0.1:32109")
	if err != nil {
		t.Fatalf("project fixture authority: %v", err)
	}
	route := projected["routes"].([]any)[0].(map[string]any)
	plugin := route["plugins"].(map[string]any)["example-plugin"].(map[string]any)
	if plugin["ip"] != "127.0.0.1" {
		t.Fatalf("projected fixture host = %#v, want 127.0.0.1", plugin["ip"])
	}
	port, ok := plugin["port"].(int)
	if !ok || port != 32109 {
		t.Fatalf("projected fixture port = %#v (%T), want integer 32109", plugin["port"], plugin["port"])
	}
	if configPlugin := config["routes"].([]any)[0].(map[string]any)["plugins"].(map[string]any)["example-plugin"].(map[string]any); configPlugin["ip"] != "{{FIXTURE.HOST}}" ||
		configPlugin["port"] != "{{FIXTURE.PORT}}" {
		t.Fatalf("source config mutated: %#v", configPlugin)
	}
}

func TestDifferentialNormalizationAllowsOnlyReviewedTransportDifferences(t *testing.T) {
	policy := testNormalizationPolicy()
	left := DifferentialObservation{
		Status: 200, Body: "same", UpstreamFixture: "primary", UpstreamAddress: "127.0.0.1:1980",
		RetryCount: 0, Host: "gateway.example.test", SNI: "", SecurityDecision: "allow",
		Headers: map[string][]string{
			"Content-Length": {"4"}, "Date": {"candidate-date"}, "Server": {"candidate-server"},
			"X-Semantic": {"same"}, "Connection": {"keep-alive"},
		},
		RouteObserver: map[string][]string{"X-Route": {"differential-proxy-rewrite"}},
		Upstream: DifferentialUpstreamObservation{
			Received: true, Fixture: "primary", Method: http.MethodGet, Path: "/rewritten",
			Host: "rewritten.example.test", Headers: map[string][]string{"Date": {"one"}}, Body: "",
		},
	}
	right := left
	right.UpstreamAddress = "host.containers.internal:1980"
	right.Headers = map[string][]string{
		"content-length": {"4"}, "date": {"oracle-date"}, "server": {"oracle-server"},
		"x-semantic": {"same"}, "transfer-encoding": {"chunked"},
	}
	right.Upstream.Headers = map[string][]string{"server": {"two"}}
	passed, diff, err := compareNormalizedObservations(left, right, policy)
	if err != nil {
		t.Fatalf("compareNormalizedObservations() error = %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("transport-only differences rejected: passed=%t diff=%q", passed, diff)
	}

	for _, field := range []struct {
		name string
		edit func(*DifferentialObservation)
	}{
		{name: "status", edit: func(observation *DifferentialObservation) { observation.Status = 401 }},
		{name: "body", edit: func(observation *DifferentialObservation) { observation.Body = "different" }},
		{name: "route", edit: func(observation *DifferentialObservation) { observation.RouteObserver["X-Route"] = []string{"other"} }},
		{name: "retry", edit: func(observation *DifferentialObservation) { observation.RetryCount = 1 }},
		{name: "host", edit: func(observation *DifferentialObservation) { observation.Host = "other.example.test" }},
		{name: "security", edit: func(observation *DifferentialObservation) { observation.SecurityDecision = "deny" }},
	} {
		t.Run(field.name, func(t *testing.T) {
			changed := cloneDifferentialObservation(right)
			field.edit(&changed)
			passed, _, err := compareNormalizedObservations(left, changed, policy)
			if err != nil {
				t.Fatalf("compareNormalizedObservations() error = %v", err)
			}
			if passed {
				t.Fatalf("semantic %s difference was normalized", field.name)
			}
		})
	}
}

func TestDifferentialNormalizationAppliesTransportRulesToEverySequenceStep(t *testing.T) {
	policy := testNormalizationPolicy()
	left := DifferentialObservation{Steps: []DifferentialStepObservation{
		{
			Status:           http.StatusOK,
			Headers:          map[string][]string{"Date": {"candidate"}, "Content-Length": {"2"}},
			Body:             "ok",
			Host:             "gateway.example.test",
			SecurityDecision: "allow",
		},
		{
			Status:           http.StatusServiceUnavailable,
			Headers:          map[string][]string{"Server": {"candidate"}},
			Body:             "",
			Host:             "gateway.example.test",
			SecurityDecision: "deny",
		},
	}}
	right := cloneDifferentialObservation(left)
	right.Steps[0].Headers = map[string][]string{"Date": {"oracle"}}
	right.Steps[1].Headers = map[string][]string{"Server": {"oracle"}}

	passed, diff, err := compareNormalizedObservations(left, right, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !passed || diff != "" {
		t.Fatalf("sequence transport differences rejected: passed=%t diff=%q", passed, diff)
	}

	right = cloneDifferentialObservation(left)
	right.Steps[1].Status = http.StatusTooManyRequests
	passed, _, err = compareNormalizedObservations(left, right, policy)
	if err != nil {
		t.Fatal(err)
	}
	if passed {
		t.Fatal("sequence status difference was normalized")
	}
}

func TestDifferentialNormalizationRejectsSemanticIgnore(t *testing.T) {
	policy := testNormalizationPolicy()
	policy.Headers.Ignore = []string{"Status"}
	if err := validateNormalizationPolicy(policy); err == nil ||
		!strings.Contains(err.Error(), "semantic header") {
		t.Fatalf("validateNormalizationPolicy() error = %v, want semantic-field rejection", err)
	}
	policy = testNormalizationPolicy()
	policy.FixtureEndpointMapping = false
	if err := validateNormalizationPolicy(policy); err == nil {
		t.Fatal("normalization without exact fixture mapping was accepted")
	}
}

func TestDifferentialNormalizationRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "normalization.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nstatus: ignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNormalizationPolicy(path); err == nil ||
		!strings.Contains(err.Error(), "field status not found") {
		t.Fatalf("loadNormalizationPolicy() error = %v, want unknown semantic field rejection", err)
	}
}

func TestDifferentialArtifactIsAppendOnlyPerAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence", "differential.json")
	artifact := DifferentialArtifact{
		SchemaVersion: 3, Suite: differentialSuite,
		Attempt: 1, FirstAttempt: true,
		Candidate: DifferentialCandidateID{SourceCommit: "candidate"},
		Oracle: OracleIdentity{
			ImageTag:        compatibilityOracleImage,
			ImageRepository: compatibilityOracleRepository,
			ImageLinuxAMD64: compatibilityOracleImageDigest,
		},
		Cases: []DifferentialCaseResult{
			{Name: "case", Plugin: "redirect", FirstAttempt: true, Passed: true},
		},
		Passed: 1, Result: "pass",
	}
	if err := writeDifferentialArtifact(path, artifact); err != nil {
		t.Fatalf("writeDifferentialArtifact() error = %v", err)
	}
	if err := writeDifferentialArtifact(path, artifact); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second write error = %v, want append-only rejection", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded DifferentialArtifact
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	if !decoded.FirstAttempt || decoded.Attempt != 1 || decoded.Result != "pass" {
		t.Fatalf("decoded artifact = %#v", decoded)
	}
}

func TestWriteDifferentialArtifactCannotOverwriteConcurrentPublish(t *testing.T) {
	previousMaxProcs := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousMaxProcs) })

	directory := t.TempDir()
	path := filepath.Join(directory, "differential.json")
	artifacts := []DifferentialArtifact{
		{
			SchemaVersion: 3,
			Suite:         differentialSuite,
			Cases: []DifferentialCaseResult{{
				Name: "writer-a", Error: strings.Repeat("a", 8<<20),
			}},
		},
		{
			SchemaVersion: 3,
			Suite:         differentialSuite,
			Cases: []DifferentialCaseResult{{
				Name: "writer-b", Error: strings.Repeat("b", 8<<20),
			}},
		},
	}
	type outcome struct {
		writer int
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(artifacts))
	var writers sync.WaitGroup
	for writer, artifact := range artifacts {
		writers.Go(func() {
			<-start
			outcomes <- outcome{writer: writer, err: writeDifferentialArtifact(path, artifact)}
		})
	}
	close(start)
	writers.Wait()
	close(outcomes)

	successes := make([]int, 0, 1)
	for result := range outcomes {
		if result.err == nil {
			successes = append(successes, result.writer)
			continue
		}
		if !strings.Contains(result.err.Error(), "already exists") {
			t.Fatalf("writer %d error = %v, want already-exists rejection", result.writer, result.err)
		}
	}
	if len(successes) != 1 {
		t.Fatalf("concurrent publish successes = %#v, want exactly one", successes)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(artifacts[successes[0]], "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if !bytes.Equal(got, want) {
		t.Fatal("published bytes do not belong to the successful writer")
	}
	temps, err := filepath.Glob(filepath.Join(directory, ".differential-artifact-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("losing temporary artifacts were not cleaned: %#v", temps)
	}
}

func TestCandidateBinarySHA256RequiresExactArtifactIdentity(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "apisix")
	if err := os.WriteFile(candidate, []byte("candidate-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("candidate-binary"))
	wantHex := hex.EncodeToString(want[:])

	t.Run("matches", func(t *testing.T) {
		t.Setenv(differentialCandidateSHA256Env, wantHex)
		got, err := candidateBinarySHA256(candidate)
		if err != nil {
			t.Fatalf("candidateBinarySHA256() error = %v", err)
		}
		if got != wantHex {
			t.Fatalf("candidateBinarySHA256() = %q, want %q", got, wantHex)
		}
	})

	t.Run("missing", func(t *testing.T) {
		t.Setenv(differentialCandidateSHA256Env, "")
		if _, err := candidateBinarySHA256(candidate); err == nil ||
			!strings.Contains(err.Error(), "is required") {
			t.Fatalf("candidateBinarySHA256() error = %v, want required identity rejection", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		t.Setenv(differentialCandidateSHA256Env, "ABC")
		if _, err := candidateBinarySHA256(candidate); err == nil ||
			!strings.Contains(err.Error(), "64 lowercase hexadecimal") {
			t.Fatalf("candidateBinarySHA256() error = %v, want malformed identity rejection", err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		t.Setenv(differentialCandidateSHA256Env, strings.Repeat("0", 64))
		if _, err := candidateBinarySHA256(candidate); err == nil ||
			!strings.Contains(err.Error(), "does not match") {
			t.Fatalf("candidateBinarySHA256() error = %v, want mismatched identity rejection", err)
		}
	})
}

func TestRenderDifferentialRuntimeConfigurations(t *testing.T) {
	dataDir := t.TempDir()
	candidate, err := renderDifferentialCandidateRuntime(
		19080,
		17085,
		19090,
		dataDir,
		[]string{"redirect", "prometheus"},
	)
	if err != nil {
		t.Fatalf("render candidate runtime: %v", err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(candidate, &config); err != nil {
		t.Fatalf("decode candidate runtime: %v", err)
	}
	if _, ok := config["apisix_go"]; !ok {
		t.Fatal("candidate runtime lacks isolated apisix_go paths")
	}
	apisix, ok := config["apisix"].(map[string]any)
	if !ok {
		t.Fatalf("candidate apisix = %#v, want object", config["apisix"])
	}
	control, ok := apisix["control"].(map[string]any)
	if !ok || apisix["enable_control"] != true || control["ip"] != "127.0.0.1" || control["port"] != 19090 {
		t.Fatalf("candidate control = %#v/%#v, want enabled loopback:19090", apisix["enable_control"], control)
	}
	pluginAttr, ok := config["plugin_attr"].(map[string]any)
	if !ok {
		t.Fatalf("plugin_attr = %#v, want isolated prometheus configuration", config["plugin_attr"])
	}
	prometheusAttr, ok := pluginAttr["prometheus"].(map[string]any)
	if !ok || prometheusAttr["enable_export_server"] != false || prometheusAttr["refresh_interval"] != 0.1 {
		t.Fatalf(
			"plugin_attr.prometheus = %#v, want disabled export server with fast refresh",
			pluginAttr["prometheus"],
		)
	}
	oracle, err := renderDifferentialOracleRuntime([]string{"error-page", "exit-transformer", "key-auth", "prometheus"})
	if err != nil {
		t.Fatalf("render oracle runtime: %v", err)
	}
	if strings.Contains(string(oracle), "apisix_go") {
		t.Fatal("oracle runtime projection contains candidate-only apisix-go configuration")
	}
	config = nil
	if err := yaml.Unmarshal(oracle, &config); err != nil {
		t.Fatalf("decode oracle runtime: %v", err)
	}
	if got := fmt.Sprint(config["plugins"]); got != "[error-page exit-transformer key-auth prometheus]" {
		t.Fatalf("oracle plugins = %s, want selected plugin set", got)
	}
	apisix, ok = config["apisix"].(map[string]any)
	if !ok {
		t.Fatalf("oracle apisix = %#v, want object", config["apisix"])
	}
	control, ok = apisix["control"].(map[string]any)
	if !ok || apisix["enable_control"] != true || control["ip"] != "127.0.0.1" || control["port"] != 9090 {
		t.Fatalf("oracle control = %#v/%#v, want enabled loopback:9090", apisix["enable_control"], control)
	}
	pluginAttr, ok = config["plugin_attr"].(map[string]any)
	if !ok {
		t.Fatalf("oracle plugin_attr = %#v, want isolated prometheus configuration", config["plugin_attr"])
	}
	prometheusAttr, ok = pluginAttr["prometheus"].(map[string]any)
	if !ok || prometheusAttr["enable_export_server"] != false || prometheusAttr["refresh_interval"] != 0.1 {
		t.Fatalf(
			"oracle plugin_attr.prometheus = %#v, want disabled export server with fast refresh",
			pluginAttr["prometheus"],
		)
	}
}

func TestRenderDifferentialRuntimeConfigurationsMergeLogRotateSideOverlay(t *testing.T) {
	candidate, err := renderDifferentialCandidateRuntimeWithOverlay(
		19080,
		17085,
		19090,
		t.TempDir(),
		[]string{"log-rotate", "prometheus"},
		differentialLogRotateRuntimeOverlay("/candidate-side"),
	)
	if err != nil {
		t.Fatalf("render candidate runtime with log-rotate overlay: %v", err)
	}
	assertDifferentialLogRotateRuntimeOverlay(
		t, candidate, "/candidate-side/logs/access.log", 19090,
	)

	oracle, err := renderDifferentialOracleRuntimeWithOverlay(
		[]string{"log-rotate", "prometheus"},
		differentialLogRotateRuntimeOverlay(differentialOracleFileDirectory),
	)
	if err != nil {
		t.Fatalf("render oracle runtime with log-rotate overlay: %v", err)
	}
	assertDifferentialLogRotateRuntimeOverlay(
		t,
		oracle,
		differentialOracleFileDirectory+"/logs/access.log",
		differentialOracleControlPort,
	)
}

func TestRenderDifferentialRuntimeConfigurationRejectsHarnessOverride(t *testing.T) {
	_, err := renderDifferentialCandidateRuntimeWithOverlay(
		19080,
		17085,
		19090,
		t.TempDir(),
		[]string{"log-rotate"},
		map[string]any{"apisix": map[string]any{"enable_admin": true}},
	)
	if err == nil || !strings.Contains(err.Error(), "apisix") {
		t.Fatalf("harness override error = %v, want rejected apisix overlay", err)
	}
}

func assertDifferentialLogRotateRuntimeOverlay(
	t *testing.T,
	raw []byte,
	wantAccessLog string,
	wantControlPort int,
) {
	t.Helper()
	var config map[string]any
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatalf("decode differential runtime: %v", err)
	}
	apisix := config["apisix"].(map[string]any)
	control := apisix["control"].(map[string]any)
	if apisix["enable_control"] != true || control["port"] != wantControlPort {
		t.Fatalf("runtime control = %#v/%#v", apisix["enable_control"], control)
	}
	pluginAttr := config["plugin_attr"].(map[string]any)
	if _, ok := pluginAttr["prometheus"].(map[string]any); !ok {
		t.Fatalf("prometheus plugin attr was not preserved: %#v", pluginAttr)
	}
	logRotate := pluginAttr["log-rotate"].(map[string]any)
	if logRotate["max_size"] != differentialLogRotatePlan().MaxSize || logRotate["max_kept"] != 1 ||
		logRotate["enable_compression"] != true {
		t.Fatalf("log-rotate plugin attr = %#v", logRotate)
	}
	nginx := config["nginx_config"].(map[string]any)
	httpConfig := nginx["http"].(map[string]any)
	if httpConfig["access_log"] != wantAccessLog || httpConfig["enable_access_log"] != true {
		t.Fatalf("nginx access log config = %#v", httpConfig)
	}
}

func TestDifferentialOracleRequestRoundTripPreservesSemantics(t *testing.T) {
	spec := DifferentialCase{Request: DifferentialRequest{
		Method: http.MethodPost,
		Path:   "/oracle?mode=exact",
		Host:   "gateway.example.test",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"X-Trace":      "trace-value",
		},
		Body: `{"message":"hello"}`,
	}}
	raw, request, err := renderDifferentialOracleRequest(spec)
	if err != nil {
		t.Fatalf("renderDifferentialOracleRequest() error = %v", err)
	}
	parsed, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatalf("parse rendered request: %v", err)
	}
	body, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatalf("read rendered request body: %v", err)
	}
	if parsed.Method != http.MethodPost || parsed.URL.RequestURI() != "/oracle?mode=exact" {
		t.Fatalf("rendered request target = %s %s", parsed.Method, parsed.URL.RequestURI())
	}
	if parsed.Host != "gateway.example.test" || parsed.Header.Get("X-Trace") != "trace-value" {
		t.Fatalf(
			"rendered request headers = Host %q, X-Trace %q",
			parsed.Host,
			parsed.Header.Get("X-Trace"),
		)
	}
	if string(body) != spec.Request.Body || request.Host != spec.Request.Host {
		t.Fatalf("rendered request body/host = %q/%q", body, request.Host)
	}

	response, err := parseDifferentialOracleResponse(request, []byte(
		"HTTP/1.1 202 Accepted\r\nX-Result: exact\r\nSet-Cookie: one=1\r\nSet-Cookie: two=2\r\nContent-Length: 2\r\n\r\nok",
	))
	if err != nil {
		t.Fatalf("parseDifferentialOracleResponse() error = %v", err)
	}
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read parsed response body: %v", err)
	}
	if response.StatusCode != http.StatusAccepted || response.Header.Get("X-Result") != "exact" {
		t.Fatalf("parsed response = status %d headers %#v", response.StatusCode, response.Header)
	}
	if got := response.Header.Values("Set-Cookie"); len(got) != 2 || got[0] != "one=1" ||
		got[1] != "two=2" {
		t.Fatalf("parsed Set-Cookie = %#v", got)
	}
	if string(responseBody) != "ok" {
		t.Fatalf("parsed response body = %q", responseBody)
	}
}

func TestDifferentialOracleRequestPortUsesControlListener(t *testing.T) {
	dataPort, err := differentialOracleRequestPort(DifferentialRequest{})
	if err != nil || dataPort != 9080 {
		t.Fatalf("default oracle request port = %d/%v, want 9080", dataPort, err)
	}
	controlPort, err := differentialOracleRequestPort(DifferentialRequest{
		Target: DifferentialRequestTargetControl,
	})
	if err != nil || controlPort != 9090 {
		t.Fatalf("control oracle request port = %d/%v, want 9090", controlPort, err)
	}
}

func TestDifferentialOracleFixtureRoundTripPreservesSemantics(t *testing.T) {
	fixture := DifferentialFixture{
		Name: "primary",
		Response: DifferentialFixtureResponse{
			Status: http.StatusCreated,
			Headers: map[string]string{
				"X-Fixture": "exact",
			},
			Body: "created body",
		},
		ExpectedCalls: 1,
	}
	rawResponse, err := renderDifferentialOracleFixtureResponse(fixture.Response)
	if err != nil {
		t.Fatalf("renderDifferentialOracleFixtureResponse() error = %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(rawResponse)), request)
	if err != nil {
		t.Fatalf("parse rendered fixture response: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read rendered fixture response: %v", err)
	}
	if response.StatusCode != http.StatusCreated || response.Header.Get("X-Fixture") != "exact" ||
		response.Header.Get(
			"Content-Type",
		) != "text/plain; charset=utf-8" || string(body) != "created body" {
		t.Fatalf(
			"rendered fixture response = status %d headers %#v body %q",
			response.StatusCode,
			response.Header,
			body,
		)
	}

	rawRequest := []byte(
		"POST /captured?mode=exact HTTP/1.1\r\nHost: rewritten.example.test\r\nX-Route-Observer: route-1\r\nContent-Length: 4\r\n\r\nbody",
	)
	captured, err := parseDifferentialOracleFixtureRequest(rawRequest)
	if err != nil {
		t.Fatalf("parseDifferentialOracleFixtureRequest() error = %v", err)
	}
	if captured.Method != http.MethodPost || captured.Path != "/captured?mode=exact" ||
		captured.Host != "rewritten.example.test" {
		t.Fatalf(
			"captured request target = %s %s host %s",
			captured.Method,
			captured.Path,
			captured.Host,
		)
	}
	if captured.Headers.Get("X-Route-Observer") != "route-1" || captured.Body != "body" {
		t.Fatalf("captured request headers/body = %#v/%q", captured.Headers, captured.Body)
	}
}

func TestParseDifferentialOracleFixtureRequestsReturnsEveryCallInOrder(t *testing.T) {
	rawRequests := [][]byte{
		[]byte("GET /first HTTP/1.1\r\nHost: upstream.example.test\r\n\r\n"),
		[]byte("POST /second HTTP/1.1\r\nHost: upstream.example.test\r\nContent-Length: 4\r\n\r\nbody"),
	}

	captured, err := parseDifferentialOracleFixtureRequests(rawRequests)
	if err != nil {
		t.Fatalf("parseDifferentialOracleFixtureRequests() error = %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("captured request count = %d, want 2", len(captured))
	}
	if captured[0].Method != http.MethodGet || captured[0].Path != "/first" {
		t.Fatalf("first captured request = %#v", captured[0])
	}
	if captured[1].Method != http.MethodPost || captured[1].Path != "/second" ||
		captured[1].Body != "body" {
		t.Fatalf("second captured request = %#v", captured[1])
	}
}

func TestDifferentialSequenceUpstreamCallsCompareInOrder(t *testing.T) {
	left := DifferentialObservation{
		Upstream: DifferentialUpstreamObservation{Received: true, Path: "/second"},
		UpstreamCalls: []DifferentialUpstreamObservation{
			{Received: true, Path: "/first", Headers: map[string][]string{"Date": {"left-one"}}},
			{Received: true, Path: "/second", Headers: map[string][]string{"Date": {"left-two"}}},
		},
	}
	right := cloneDifferentialObservation(left)
	right.UpstreamCalls[0].Headers["Date"] = []string{"right-one"}
	right.UpstreamCalls[1].Headers["Date"] = []string{"right-two"}

	passed, diff, err := compareNormalizedObservations(left, right, testNormalizationPolicy())
	if err != nil {
		t.Fatalf("compareNormalizedObservations() error = %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("transport-only call header difference rejected: passed=%t diff=%q", passed, diff)
	}

	right.UpstreamCalls[0], right.UpstreamCalls[1] = right.UpstreamCalls[1], right.UpstreamCalls[0]
	passed, _, err = compareNormalizedObservations(left, right, testNormalizationPolicy())
	if err != nil {
		t.Fatalf("ordered comparison error = %v", err)
	}
	if passed {
		t.Fatal("sequence upstream call order was ignored")
	}

	right.UpstreamCalls = right.UpstreamCalls[:1]
	if _, _, err := compareNormalizedObservations(left, right, testNormalizationPolicy()); err == nil {
		t.Fatal("sequence upstream call count mismatch did not fail loud")
	}
}

func TestDifferentialLimitCountFixedWindowResponseComparison(t *testing.T) {
	spec, candidate, oracle := differentialLimitCountFixedWindowTestObservations()
	candidateBefore := cloneDifferentialObservation(candidate)
	oracleBefore := cloneDifferentialObservation(oracle)
	passed, diff, err := compareDifferentialCaseObservations(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compareDifferentialCaseObservations() error = %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("fixed-window response rejected: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("fixed-window comparison mutated caller observations")
	}
}

func TestDifferentialLimitCountFixedWindowResponseRejectsSemanticDifferences(t *testing.T) {
	assertRejected := func(
		t *testing.T,
		edit func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation),
	) {
		t.Helper()
		spec, candidate, oracle := differentialLimitCountFixedWindowTestObservations()
		edit(&spec, &candidate, &oracle)
		passed, _, _ := compareDifferentialCaseObservations(
			spec,
			candidate,
			oracle,
			testNormalizationPolicy(),
		)
		if passed {
			t.Fatal("semantic difference was normalized")
		}
	}

	t.Run("reset increases", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.Steps[0].Headers["X-RateLimit-Reset"] = []string{"59"}
			oracle.Steps[1].Headers["X-RateLimit-Reset"] = []string{"60"}
		})
	})
	for _, value := range []string{"0", "01", "+1", "61", "not-an-integer"} {
		t.Run("invalid reset "+value, func(t *testing.T) {
			assertRejected(
				t,
				func(_ *DifferentialCase, candidate *DifferentialObservation, _ *DifferentialObservation) {
					candidate.Steps[0].Headers["X-RateLimit-Reset"] = []string{value}
				},
			)
		})
	}
	t.Run("remaining changes on one side", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.Steps[0].Headers["X-RateLimit-Remaining"] = []string{"0"}
		})
	})
	t.Run("remaining is wrong on both sides", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
			candidate.Steps[0].Headers["X-RateLimit-Remaining"] = []string{"0"}
			oracle.Steps[0].Headers["X-RateLimit-Remaining"] = []string{"0"}
		})
	})
	t.Run("oracle 503 is not pinned platform representation", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.Steps[2].Body = "another 503 page"
			oracle.Steps[2].Headers["Content-Length"] = []string{"16"}
		})
	})
	t.Run("candidate 503 owns a body", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate *DifferentialObservation, _ *DifferentialObservation) {
			candidate.Steps[2].Body = "candidate error"
			candidate.Steps[2].Headers["Content-Length"] = []string{"15"}
		})
	})
	t.Run("upstream call order changes", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.UpstreamCalls[0], oracle.UpstreamCalls[1] = oracle.UpstreamCalls[1], oracle.UpstreamCalls[0]
		})
	})
	t.Run("policy is used by another plugin", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			spec.Plugin = "limit-req"
		})
	})
	t.Run("case does not have four steps", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			spec.Steps = spec.Steps[:3]
		})
	})
	t.Run("case config is not the pinned fixed window", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			route := spec.Config["routes"].([]any)[0].(map[string]any)
			plugins := route["plugins"].(map[string]any)
			plugins["limit-count"].(map[string]any)["time_window"] = 30
		})
	})
}

func TestDifferentialCASAuthCallbackNosniffComparison(t *testing.T) {
	spec, candidate, oracle := differentialCASAuthCallbackNosniffTestObservations()
	candidateBefore := cloneDifferentialObservation(candidate)
	oracleBefore := cloneDifferentialObservation(oracle)
	passed, diff, err := compareDifferentialCaseObservations(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compareDifferentialCaseObservations() error = %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("CAS callback nosniff difference rejected: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("CAS callback comparison mutated caller observations")
	}
}

func TestDifferentialCASAuthCallbackNosniffRejectsSemanticDifferences(t *testing.T) {
	assertRejected := func(
		t *testing.T,
		edit func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation),
	) {
		t.Helper()
		spec, candidate, oracle := differentialCASAuthCallbackNosniffTestObservations()
		edit(&spec, &candidate, &oracle)
		passed, _, _ := compareDifferentialCaseObservations(
			spec,
			candidate,
			oracle,
			testNormalizationPolicy(),
		)
		if passed {
			t.Fatal("CAS callback semantic difference was normalized")
		}
	}

	t.Run("body", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
			candidate.Body = "wrong\n"
			oracle.Body = "wrong\n"
		})
	})
	t.Run("status", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
			candidate.Status = http.StatusForbidden
			oracle.Status = http.StatusForbidden
		})
	})
	t.Run("content type", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
			candidate.Headers["Content-Type"] = []string{"application/json"}
			oracle.Headers["Content-Type"] = []string{"application/json"}
		})
	})
	t.Run("set cookie", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
			candidate.Headers["Set-Cookie"] = []string{"session=unexpected"}
		})
	})
	t.Run("upstream", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
			candidate.Upstream.Received = true
			oracle.Upstream.Received = true
		})
	})
	t.Run("extra header", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.Headers["X-Unrelated"] = []string{"extra"}
		})
	})
	t.Run("candidate nosniff is absent", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
			delete(candidate.Headers, "X-Content-Type-Options")
		})
	})
	t.Run("candidate nosniff has multiple values", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
			candidate.Headers["X-Content-Type-Options"] = []string{"nosniff", "nosniff"}
		})
	})
	t.Run("oracle owns nosniff", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.Headers["X-Content-Type-Options"] = []string{"nosniff"}
		})
	})
	t.Run("policy allowlist", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			spec.Plugin = "openid-connect"
		})
	})
	t.Run("source case identity", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			spec.Name = "another-cas-case"
		})
	})
	t.Run("callback method", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			spec.Request.Method = http.MethodPost
		})
	})
	t.Run("callback host", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			spec.Request.Host = "gateway.example.test"
		})
	})
	t.Run("fixture call", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			spec.Fixture.ExpectedCalls = 1
		})
	})
}

func differentialCASAuthCallbackNosniffTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	const body = "{\"message\":\"invalid callback state\"}\n"
	spec := differentialCASAuthCases()[0]
	spec.ComparisonPolicy = "cas-auth-callback-nosniff"
	candidate := DifferentialObservation{
		Status: http.StatusUnauthorized,
		Headers: map[string][]string{
			"Content-Length":         {"37"},
			"Content-Type":           {"text/plain; charset=utf-8"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body, Host: "127.0.0.3", SecurityDecision: "deny",
		Upstream: DifferentialUpstreamObservation{Received: false},
	}
	oracle := cloneDifferentialObservation(candidate)
	delete(oracle.Headers, "Content-Length")
	delete(oracle.Headers, "X-Content-Type-Options")
	return spec, candidate, oracle
}

func differentialLimitCountFixedWindowTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	const oracle503Body = "<html>\r\n" +
		"<head><title>503 Service Temporarily Unavailable</title></head>\r\n" +
		"<body>\r\n" +
		"<center><h1>503 Service Temporarily Unavailable</h1></center>\r\n" +
		"<hr><center>openresty</center>\r\n" +
		"<p><em>Powered by <a href=\"https://apisix.apache.org/\">APISIX</a>.</em></p></body>\r\n" +
		"</html>\r\n"
	spec := differentialLimitCountCases()[0]
	spec.ComparisonPolicy = "limit-count-fixed-window-response"
	step := func(
		status int,
		body string,
		decision string,
		remaining string,
		reset string,
	) DifferentialStepObservation {
		headers := map[string][]string{
			"Content-Length":        {fmt.Sprint(len(body))},
			"X-RateLimit-Limit":     {"2"},
			"X-RateLimit-Remaining": {remaining},
			"X-RateLimit-Reset":     {reset},
		}
		if status == http.StatusOK {
			headers["Content-Type"] = []string{"text/plain; charset=utf-8"}
		}
		return DifferentialStepObservation{
			Status: status, Headers: headers, Body: body,
			Host: "gateway.example.test", SecurityDecision: decision,
		}
	}
	candidate := DifferentialObservation{
		Steps: []DifferentialStepObservation{
			step(http.StatusOK, "hello world", "allow", "1", "60"),
			step(http.StatusOK, "hello world", "allow", "0", "60"),
			step(http.StatusServiceUnavailable, "", "deny", "0", "60"),
			step(http.StatusServiceUnavailable, "", "deny", "0", "60"),
		},
		UpstreamFixture: "primary", UpstreamAddress: "127.0.0.1:49152",
		Upstream: DifferentialUpstreamObservation{
			Received: true, Fixture: "primary", Method: http.MethodGet,
			Path: "/hello?call=2", Host: "differential.example.test",
		},
		UpstreamCalls: []DifferentialUpstreamObservation{
			{
				Received: true,
				Fixture:  "primary",
				Method:   http.MethodGet,
				Path:     "/hello?call=1",
				Host:     "differential.example.test",
			},
			{
				Received: true,
				Fixture:  "primary",
				Method:   http.MethodGet,
				Path:     "/hello?call=2",
				Host:     "differential.example.test",
			},
		},
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.UpstreamAddress = "127.0.0.1:1980"
	oracle.Steps[1].Headers["X-RateLimit-Reset"] = []string{"59"}
	for index := 2; index < 4; index++ {
		oracle.Steps[index].Body = oracle503Body
		oracle.Steps[index].Headers["Content-Type"] = []string{"text/html; charset=utf-8"}
		oracle.Steps[index].Headers["Content-Length"] = []string{fmt.Sprint(len(oracle503Body))}
		oracle.Steps[index].Headers["X-RateLimit-Reset"] = []string{"59"}
	}
	oracle.Steps[3].Headers["X-RateLimit-Reset"] = []string{"58"}
	return spec, candidate, oracle
}

func testNormalizationPolicy() NormalizationPolicy {
	return NormalizationPolicy{
		SchemaVersion: 1,
		Headers: HeaderNormalizationPolicy{
			CanonicalizeNames: true, Ignore: []string{"Date", "Server"},
			StripHopByHop: true, ContentLengthIfBodyEqual: true,
		},
		FixtureEndpointMapping: true,
	}
}

func cloneDifferentialObservation(observation DifferentialObservation) DifferentialObservation {
	observation.Steps = append([]DifferentialStepObservation(nil), observation.Steps...)
	for index := range observation.Steps {
		observation.Steps[index].Headers = cloneDifferentialHeaders(observation.Steps[index].Headers)
	}
	observation.Headers = cloneDifferentialHeaders(observation.Headers)
	observation.RouteObserver = cloneDifferentialHeaders(observation.RouteObserver)
	observation.Upstream.Headers = cloneDifferentialHeaders(observation.Upstream.Headers)
	observation.UpstreamCalls = append(
		[]DifferentialUpstreamObservation(nil),
		observation.UpstreamCalls...,
	)
	for index := range observation.UpstreamCalls {
		observation.UpstreamCalls[index].Headers = cloneDifferentialHeaders(
			observation.UpstreamCalls[index].Headers,
		)
	}
	return observation
}

func cloneDifferentialHeaders(headers map[string][]string) map[string][]string {
	if headers == nil {
		return nil
	}
	cloned := make(map[string][]string, len(headers))
	for name, values := range headers {
		cloned[name] = append([]string(nil), values...)
	}
	return cloned
}

func differentialCaseNames(cases []DifferentialCase) []string {
	names := make([]string, 0, len(cases))
	for _, spec := range cases {
		names = append(names, spec.Name)
	}
	return names
}

func differentialCatalogWithCases(
	catalog DifferentialCatalog,
	cases []DifferentialCatalogCase,
) DifferentialCatalog {
	catalog.Cases = cases
	return catalog
}

func mustYAML(t *testing.T, value any) []byte {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
