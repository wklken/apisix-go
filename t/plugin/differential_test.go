package pluginintegration

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	differentialRunEnv                = "APISIX_GO_RUN_DIFFERENTIAL"
	differentialCandidateEnv          = "APISIX_GO_CANDIDATE_BIN"
	differentialCandidateSHA256Env    = "APISIX_GO_CANDIDATE_BINARY_SHA256"
	differentialContainerEnv          = "APISIX_GO_CONTAINER_BIN"
	differentialHostGatewayEnv        = "APISIX_GO_DIFFERENTIAL_HOST_GATEWAY"
	differentialArtifactEnv           = "APISIX_GO_DIFFERENTIAL_ARTIFACT"
	differentialPluginsEnv            = "APISIX_GO_DIFFERENTIAL_PLUGINS"
	differentialCasesEnv              = "APISIX_GO_DIFFERENTIAL_CASES"
	differentialShardIndexEnv         = "APISIX_GO_DIFFERENTIAL_SHARD_INDEX"
	differentialShardCountEnv         = "APISIX_GO_DIFFERENTIAL_SHARD_COUNT"
	differentialOracleFixturePort     = 1980
	differentialOracleFixtureReady    = "/tmp/apisix-go-differential-fixture.ready"
	differentialOracleFixtureRecord   = "/tmp/apisix-go-differential-fixture-request.raw"
	differentialWorkerLimit           = 3
	differentialContainerNameLimit    = 63
	differentialPodmanTimeout         = 10 * time.Second
	differentialRemovalVerifyTimeout  = 5 * time.Second
	differentialRemovalVerifyInterval = 100 * time.Millisecond
	differentialFixtureBindAttempts   = 16
	differentialFixtureProbePath      = "/.apisix-go-differential/fixture-owner"
	differentialFixtureProbeHeader    = "X-Apisix-Go-Differential-Fixture"
	differentialFixtureWireHTTPTCP    = "http-tcp"
	differentialFixtureWireHTTPUDP    = "http-udp"
	differentialFixtureWireTLSTCP     = "tls-tcp"
	differentialFixtureWireT1KV2      = "t1k-v2"
	differentialRawRecordPrefix       = "APISIX-GO-DIFFERENTIAL-RAW/1 "
	differentialRawProbePrefix        = "APISIX-GO-DIFFERENTIAL-PROBE/1 "
	differentialRawMaxFramePayload    = 16 << 20
	differentialT1KMaxFramePayload    = 16 << 20
	differentialMaxConcurrentRequests = 32
	differentialMaxFixtureDelayMillis = 5000
)

const differentialFixtureCertificatePEM = `-----BEGIN CERTIFICATE-----
MIIDMzCCAhugAwIBAgIUDMkuYgqrFpltdBwuNC/7BIG4umEwDQYJKoZIhvcNAQEL
BQAwKTEnMCUGA1UEAwweYXBpc2l4LWdvLWRpZmZlcmVudGlhbC1maXh0dXJlMB4X
DTI2MDgyODE3MDg1OVoXDTM2MDgyNTE3MDg1OVowKTEnMCUGA1UEAwweYXBpc2l4
LWdvLWRpZmZlcmVudGlhbC1maXh0dXJlMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A
MIIBCgKCAQEApd40aioy/728GS2WtAoUbWfBigv2OMUcWNCwQ0M1/saMffBb+O4F
RQSrFahWwnERjQOykuF5Up3O8D1cX2L2VBUlL2n6v8UFxODUCtEvuPjxHlPV0cEr
GVOltdUd1DwHa0yNND8nmC76P36hX07FPx1fMAmMWn8v72NAuUe0P2aVJeJSDQ0o
W0PkjgWqryPky8RYeLkQReP7DHeleTl7v9gRiqELZZg037hhxDmmB+LOIe/VquGI
w35kKbumuLgxE8pdjlkkZlsjdE4H9Cn6t1/JHDUqgIWjEljXkjHZZ3oMjLgWHane
iygBP26gQHF4NkXy5sP3l+ABJRlAMwAlhwIDAQABo1MwUTAdBgNVHQ4EFgQUPGWY
Hz2CoC6+fstTrJnF8gb25cEwHwYDVR0jBBgwFoAUPGWYHz2CoC6+fstTrJnF8gb2
5cEwDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAfVdtFUXxv2og
whaC0/TBTcPTCZd8vF/AdGvcB1u6oMmp8V2pqGMbMSXuu4TR1yke0Y9gG4GqcMmy
PtFQqQXAVnE5pnrx2drd28enWEi/ztLX/IpNgp4BP5hr5fa+nPRrRWvMt/88h1zE
k8mP28nCWbaQ8UcMQwyq+hJPRiHlAbju9bBIGph9u+SU39WGjEd4p8JTs9QTSC8/
uVQXugBRjbscmrvYlh3yuM7ZDdNEyggzJ6AneKYoPfNoyfMwQUvV6iGqSJvYp9MK
42HXJftonZ6FHztEFe/kNGkWUNxeL6dJlVSJBbfHQjtjatxhM2bD72S2A3ETr/C4
sgo7Len50Q==
-----END CERTIFICATE-----
`

const differentialFixturePrivateKeyPEM = `-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQCl3jRqKjL/vbwZ
LZa0ChRtZ8GKC/Y4xRxY0LBDQzX+xox98Fv47gVFBKsVqFbCcRGNA7KS4XlSnc7w
PVxfYvZUFSUvafq/xQXE4NQK0S+4+PEeU9XRwSsZU6W11R3UPAdrTI00PyeYLvo/
fqFfTsU/HV8wCYxafy/vY0C5R7Q/ZpUl4lINDShbQ+SOBaqvI+TLxFh4uRBF4/sM
d6V5OXu/2BGKoQtlmDTfuGHEOaYH4s4h79Wq4YjDfmQpu6a4uDETyl2OWSRmWyN0
Tgf0Kfq3X8kcNSqAhaMSWNeSMdlnegyMuBYdqd6LKAE/bqBAcXg2RfLmw/eX4AEl
GUAzACWHAgMBAAECggEAMpTYFBYJVl70aRMzdXTrdM+iwCfUrsxBUD5XujNZWHgQ
6OjvCzL+rWT2jVS4HHShnwilINCckFqqfC2iKT6DEvId1F8zxd5d24OadjADpxtX
YGG9f0kyjPcqvhAfGBU0R/7gwrGNsAWHb+x8ZpWdZhldaUdII2LM6eoxFy9sIrb2
fSS9iTDIrvwMErRCbv5QgDJ+PvkHdn6P4wYpSgAZJBoDImyfQOnA8sZAfY86IHUS
u2uklEBRAgxUJI4gCucSEpuDlQbix0g6EQCKKKu57kNTHOUBu0k1SSvAlyZ0xL7M
NV8oc9Lal6+xndTiXsV31DCWo1Zn84p2UREndp5DXQKBgQDVLbcbCj843m/lJfY9
50ylXBc6FO9wJ2csFK9XxrcAFd5qYDsrJZCA8Xb4daWmenPlEqbxoAC0HUTipUl+
B+rg+7N4z/T9rk4FNJFBe2/1SKgJFb3trAiuKwahlVKWn31A54LMjfy35cArcMOT
iY4tZjWUF9AY8noCUZQCKN4HRQKBgQDHL6Lw5NBVG005WjwaGY+WGOmhaPcmhtGg
bj+eNT/G4koNybxcrDRVCgRfKHLQO4wc9ohKVRi6ISG/qB3KEW1qnQrUxsMy2oVe
BgLQfFpnrgsqb4NhE5mpTgNASALDSaycJXZ9Jp+c1JqqOtLID6BS8dLp4Tl4v7dZ
WV8aMHZQWwKBgQCooNXjxNJH6OR4TfQf+ZQOhe81mZPhkrmxC9e7xkvB/IqIeQC0
260X4mmqll1neBuvC3cVUOzdjP2NjxO4Zwjr2Q6ZtV5lQPkkcvWn572jOEr7jMBF
fj0LkKtZK+Y9kYGh0sALkRFkYpAFjNiYH0phLSWatM9+vGe459D9eFhRRQKBgFc+
LS83+WwdfjCNrl98LKEAnmwdTotoZ67OOz0vc5TIDsmFP+STZISO06VeUROV0WPq
M33jUeZMlrychRe5lGQrDtBtkpfWkK3DEj6BCRP6bleS6kd9z0MRsWjZYaRpw5nM
6t4cKbMGiAvhoesQtRc/ZjMcfBDAYC1ZcMdGzLubAoGBAKXyLa083t4PthuCse5s
ykiQzfNYpvt5z9VpkJHOivO1L+g2YK9FlhUUhSuo5vFpUZUa7s5S3I9sP55yuGeL
CuToDs0pley+1bbHgQAbpFh1hcG8Gva5VDtU6TqSykhQBzHaeA07lkjPBINBCLAh
S44gPOV8cx74uQrDLlUq2ks7
-----END PRIVATE KEY-----
`

var differentialProxyEnvironmentNames = map[string]struct{}{
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {}, "NO_PROXY": {}, "FTP_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "all_proxy": {}, "no_proxy": {}, "ftp_proxy": {},
}

// APISIX 3.17 batch-processor.lua imports the Prometheus exporter at module
// load time. Its default plugin set includes prometheus, which causes the CLI
// to create the required prometheus-cache shared dictionary. Preserve that
// runtime dependency when a differential selection is intentionally narrow.
var differentialAPISIXBatchProcessorPlugins = map[string]struct{}{
	"clickhouse-logger":    {},
	"datadog":              {},
	"elasticsearch-logger": {},
	"error-log-logger":     {},
	"google-cloud-logging": {},
	"http-logger":          {},
	"kafka-logger":         {},
	"lago":                 {},
	"loggly":               {},
	"loki-logger":          {},
	"rocketmq-logger":      {},
	"skywalking-logger":    {},
	"sls-logger":           {},
	"splunk-hec-logging":   {},
	"syslog":               {},
	"tcp-logger":           {},
	"tencent-cloud-cls":    {},
	"udp-logger":           {},
	"zipkin":               {},
}

func TestPluginDifferential(t *testing.T) {
	if os.Getenv(differentialRunEnv) != "1" {
		t.Skip("set APISIX_GO_RUN_DIFFERENTIAL=1 to run the immutable APISIX oracle")
	}
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	allCases := differentialCases()
	identity, err := loadOracleIdentity(differentialOraclePath(repoRoot))
	if err != nil {
		t.Fatalf("load oracle identity: %v", err)
	}
	policy, err := loadNormalizationPolicy(differentialNormalizationPath(repoRoot))
	if err != nil {
		t.Fatalf("load differential normalization policy: %v", err)
	}
	catalog, err := loadDifferentialCatalog(differentialCatalogPath(repoRoot), allCases)
	if err != nil {
		t.Fatalf("load differential catalog: %v", err)
	}
	selectedCases, selection, err := differentialSelectionFromEnvironment(allCases)
	if err != nil {
		t.Fatalf("parse differential selection: %v", err)
	}
	selectedCatalog, err := selectDifferentialCatalog(catalog, selectedCases)
	if err != nil {
		t.Fatalf("select differential catalog: %v", err)
	}
	candidateBin := strings.TrimSpace(os.Getenv(differentialCandidateEnv))
	if candidateBin == "" {
		t.Fatal(
			"APISIX_GO_CANDIDATE_BIN is required; mutable source-only execution is not a differential run",
		)
	}
	if _, err := os.Stat(candidateBin); err != nil {
		t.Fatalf("candidate binary %s: %v", candidateBin, err)
	}
	candidateSHA256, err := candidateBinarySHA256(candidateBin)
	if err != nil {
		t.Fatal(err)
	}
	containerBin := strings.TrimSpace(os.Getenv(differentialContainerEnv))
	if containerBin == "" {
		containerBin = strings.TrimSpace(os.Getenv("CONTAINER_BIN"))
	}
	if containerBin == "" {
		containerBin = "podman"
	}
	if _, err := exec.LookPath(containerBin); err != nil {
		t.Fatalf("container runtime %s: %v", containerBin, err)
	}
	artifactPath := strings.TrimSpace(os.Getenv(differentialArtifactEnv))
	if artifactPath == "" {
		artifactPath = filepath.Join(t.TempDir(), "differential.json")
	}
	artifactPath, err = filepath.Abs(artifactPath)
	if err != nil {
		t.Fatalf("resolve differential artifact path: %v", err)
	}
	if _, err := os.Stat(artifactPath); err == nil {
		t.Fatalf("differential artifact already exists; use a new attempt path: %s", artifactPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect differential artifact %s: %v", artifactPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o700); err != nil {
		t.Fatalf("create differential artifact directory: %v", err)
	}
	runRoot, err := os.MkdirTemp(filepath.Dir(artifactPath), ".differential-run-*")
	if err != nil {
		t.Fatalf("create differential run root: %v", err)
	}
	runNonce, err := newDifferentialRunNonce()
	if err != nil {
		t.Fatalf("create differential run nonce: %v", err)
	}
	caseWorkDirs := make(map[string]string, len(selectedCases))
	for index, spec := range selectedCases {
		caseWorkDirs[spec.Name] = differentialCaseWorkDir(runRoot, index, spec)
	}
	results := runDifferentialCaseBatch(selectedCases, func(spec DifferentialCase) DifferentialCaseResult {
		return runDifferentialCase(
			repoRoot,
			identity,
			policy,
			candidateBin,
			containerBin,
			runNonce,
			artifactPath,
			caseWorkDirs[spec.Name],
			spec,
		)
	})
	artifact, err := buildSelectedDifferentialArtifact(
		selectedCatalog,
		selectedCases,
		selection,
		DifferentialCandidateID{
			SourceCommit:    candidateSourceCommit(),
			BinarySHA256:    candidateSHA256,
			SecurityProfile: "compat",
		},
		identity,
		results,
	)
	if err != nil {
		t.Fatalf("build differential artifact: %v", err)
	}
	if err := writeDifferentialArtifact(artifactPath, artifact); err != nil {
		t.Fatalf("write first-attempt differential artifact: %v", err)
	}
	t.Logf("differential artifact: %s", artifactPath)
	if artifact.Result == "pass" {
		if err := os.RemoveAll(runRoot); err != nil {
			t.Fatalf("remove successful differential run root: %v", err)
		}
		t.Logf("immutable APISIX differential: %d/%d cases passed", artifact.Passed, len(results))
		return
	}
	t.Fatalf(
		"immutable APISIX differential: %d/%d cases passed; artifact: %s",
		artifact.Passed,
		len(results),
		artifactPath,
	)
}

func differentialSelectionFromEnvironment(all []DifferentialCase) ([]DifferentialCase, DifferentialSelection, error) {
	return parseDifferentialSelectionFromEnvironment(os.LookupEnv, all)
}

type differentialSelectionPreflight struct {
	Plugins           []string `json:"plugins"`
	Cases             []string `json:"cases"`
	ShardIndex        int      `json:"shard_index"`
	ShardCount        int      `json:"shard_count"`
	SelectedCaseCount int      `json:"selected_case_count"`
	FullCatalogRun    bool     `json:"full_catalog_run"`
}

func runDifferentialSelectionPreflight(
	getenv func(string) (string, bool),
	all []DifferentialCase,
	output io.Writer,
) error {
	selected, selection, err := parseDifferentialSelectionFromEnvironment(getenv, all)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(differentialSelectionPreflight{
		Plugins:           selection.Plugins,
		Cases:             selection.Cases,
		ShardIndex:        selection.ShardIndex,
		ShardCount:        selection.ShardCount,
		SelectedCaseCount: len(selected),
		FullCatalogRun: len(selection.Plugins) == 0 && len(selection.Cases) == 0 &&
			selection.ShardIndex == 0 && selection.ShardCount == 1,
	})
	if err != nil {
		return fmt.Errorf("marshal differential selection preflight: %w", err)
	}
	if _, err := fmt.Fprintf(output, "DIFFERENTIAL_SELECTION_JSON=%s\n", payload); err != nil {
		return fmt.Errorf("write differential selection preflight: %w", err)
	}
	return nil
}

func parseDifferentialSelectionFromEnvironment(
	getenv func(string) (string, bool),
	all []DifferentialCase,
) ([]DifferentialCase, DifferentialSelection, error) {
	plugins, err := differentialSelectorEnvironment(getenv, differentialPluginsEnv)
	if err != nil {
		return nil, DifferentialSelection{}, err
	}
	cases, err := differentialSelectorEnvironment(getenv, differentialCasesEnv)
	if err != nil {
		return nil, DifferentialSelection{}, err
	}
	shardIndex, err := differentialIntegerEnvironment(getenv, differentialShardIndexEnv, 0)
	if err != nil {
		return nil, DifferentialSelection{}, err
	}
	shardCount, err := differentialIntegerEnvironment(getenv, differentialShardCountEnv, 1)
	if err != nil {
		return nil, DifferentialSelection{}, err
	}
	return selectDifferentialCases(all, DifferentialSelection{
		Plugins: plugins, Cases: cases, ShardIndex: shardIndex, ShardCount: shardCount,
	})
}

func differentialSelectorEnvironment(getenv func(string) (string, bool), name string) ([]string, error) {
	raw, present := getenv(name)
	if !present {
		return nil, nil
	}
	return strings.Split(raw, ","), nil
}

func differentialIntegerEnvironment(
	getenv func(string) (string, bool),
	name string,
	defaultValue int,
) (int, error) {
	raw, present := getenv(name)
	if !present {
		return defaultValue, nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %q", name, raw)
	}
	return value, nil
}

func candidateBinarySHA256(path string) (string, error) {
	expected := strings.TrimSpace(os.Getenv(differentialCandidateSHA256Env))
	if expected == "" {
		return "", fmt.Errorf("%s is required", differentialCandidateSHA256Env)
	}
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(expected) != expected {
		return "", fmt.Errorf(
			"%s must be 64 lowercase hexadecimal characters",
			differentialCandidateSHA256Env,
		)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open candidate binary %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash candidate binary %s: %w", path, err)
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != expected {
		return "", fmt.Errorf("candidate binary sha256 %s does not match %s", actual, expected)
	}
	return actual, nil
}

type differentialChild struct {
	command                   *exec.Cmd
	done                      chan error
	logPath                   string
	container                 bool
	runtime                   string
	name                      string
	oracleFixtureBootstrapped bool
	stopOnce                  sync.Once
	stopErr                   error
}

func runDifferentialCaseBatch(
	cases []DifferentialCase,
	run func(DifferentialCase) DifferentialCaseResult,
) []DifferentialCaseResult {
	sortedCases := append([]DifferentialCase(nil), cases...)
	sort.Slice(sortedCases, func(i, j int) bool { return sortedCases[i].Name < sortedCases[j].Name })
	results := make([]DifferentialCaseResult, len(sortedCases))
	workerCount := min(differentialWorkerLimit, len(sortedCases))
	if workerCount == 0 {
		return results
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				spec := sortedCases[index]
				results[index] = runDifferentialCaseSafely(spec, run)
			}
		}()
	}
	for index := range sortedCases {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results
}

func runDifferentialCaseSafely(
	spec DifferentialCase,
	run func(DifferentialCase) DifferentialCaseResult,
) (result DifferentialCaseResult) {
	result = DifferentialCaseResult{
		Name: spec.Name, Plugin: spec.Plugin, ComparisonPolicy: spec.ComparisonPolicy, FirstAttempt: true,
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Passed = false
			result.Error = fmt.Sprintf("case panic: %v", recovered)
		}
		if result.Name == "" {
			result.Name = spec.Name
		}
		if result.Plugin == "" {
			result.Plugin = spec.Plugin
		}
		if result.ComparisonPolicy == "" {
			result.ComparisonPolicy = spec.ComparisonPolicy
		}
	}()
	return run(spec)
}

func newDifferentialRunNonce() (string, error) {
	var bytes [16]byte
	if _, err := cryptorand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("read random differential run nonce: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func differentialCaseWorkDir(runRoot string, index int, spec DifferentialCase) string {
	digest := sha256.Sum256([]byte(spec.Name))
	prefix := sanitizeDifferentialResourceName(spec.Name)
	if len(prefix) > 24 {
		prefix = prefix[:24]
	}
	return filepath.Join(runRoot, fmt.Sprintf("%04d-%s-%s", index, prefix, hex.EncodeToString(digest[:])[:12]))
}

func differentialOracleContainerName(runNonce, caseName, workDir string) string {
	runDigest := sha256.Sum256([]byte(runNonce))
	caseDigest := sha256.Sum256([]byte(runNonce + "\x00" + caseName + "\x00" + workDir))
	const runDigestLength = 12
	const caseDigestLength = 16
	fixedLength := len("apisix-go-diff---") + runDigestLength + caseDigestLength
	prefixLimit := differentialContainerNameLimit - fixedLength
	prefix := sanitizeDifferentialResourceName(caseName)
	if len(prefix) > prefixLimit {
		prefix = prefix[:prefixLimit]
	}
	return fmt.Sprintf(
		"apisix-go-diff-%s-%s-%s",
		prefix,
		hex.EncodeToString(runDigest[:])[:runDigestLength],
		hex.EncodeToString(caseDigest[:])[:caseDigestLength],
	)
}

func sanitizeDifferentialResourceName(value string) string {
	var result strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '-',
			char == '_',
			char == '.':
			result.WriteRune(char)
		default:
			result.WriteByte('-')
		}
	}
	if result.Len() == 0 {
		return "case"
	}
	return result.String()
}

func differentialRequiredPluginNames(cases []DifferentialCase) []string {
	seen := make(map[string]struct{}, len(cases))
	for _, spec := range cases {
		seen[spec.Plugin] = struct{}{}
		collectDifferentialConfiguredPlugins(spec.Config, seen)
	}
	for plugin := range seen {
		if _, requiresPrometheus := differentialAPISIXBatchProcessorPlugins[plugin]; requiresPrometheus {
			seen["prometheus"] = struct{}{}
			break
		}
	}
	plugins := make([]string, 0, len(seen))
	for plugin := range seen {
		plugins = append(plugins, plugin)
	}
	sort.Strings(plugins)
	return plugins
}

func collectDifferentialConfiguredPlugins(value any, seen map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "plugins" {
				if plugins, ok := child.(map[string]any); ok {
					for plugin := range plugins {
						seen[plugin] = struct{}{}
					}
				}
			}
			collectDifferentialConfiguredPlugins(child, seen)
		}
	case []any:
		for _, child := range typed {
			collectDifferentialConfiguredPlugins(child, seen)
		}
	}
}

func appendDifferentialError(result *DifferentialCaseResult, prefix string, err error) {
	if err == nil {
		return
	}
	if result.Error == "" {
		result.Error = prefix + err.Error()
		return
	}
	result.Error += "; " + prefix + err.Error()
}

func writeDifferentialDiagnosticLog(path, message string) error {
	if err := os.WriteFile(path, []byte(message+"\n"), 0o600); err != nil {
		return fmt.Errorf("write diagnostic log %s: %w", path, err)
	}
	return nil
}

func differentialLogReference(artifactPath, logPath string) string {
	relative, err := filepath.Rel(filepath.Dir(artifactPath), logPath)
	if err != nil || relative == "" {
		return filepath.Base(logPath)
	}
	return filepath.ToSlash(relative)
}

func runDifferentialPodmanCommand(
	containerBin string,
	timeout time.Duration,
	stdin io.Reader,
	extraEnv []string,
	args ...string,
) ([]byte, error) {
	if timeout <= 0 || timeout > differentialPodmanTimeout {
		timeout = differentialPodmanTimeout
	}
	commandContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(commandContext, containerBin, args...)
	command.Env = clearProxyEnvironment(os.Environ())
	command.Env = append(command.Env, extraEnv...)
	command.Stdin = stdin
	output, err := command.CombinedOutput()
	if commandContext.Err() != nil {
		return output, fmt.Errorf("container command timed out after %s: %w", timeout, commandContext.Err())
	}
	return output, err
}

func startDifferentialCandidate(
	workDir, logPath, binary, configPath string,
) (*differentialChild, error) {
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create candidate log: %w", err)
	}
	command := exec.Command(binary, "-c", configPath)
	command.Dir = workDir
	command.Env = clearProxyEnvironment(os.Environ())
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start candidate: %w", err)
	}
	child := &differentialChild{command: command, done: make(chan error, 1), logPath: logPath}
	go func() {
		err := command.Wait()
		_ = logFile.Close()
		child.done <- err
	}()
	return child, nil
}

func startDifferentialCandidateUnderStartupLock(
	workDir, logPath, binary, configPath string,
	plugins []string,
	runtimeOverlay map[string]any,
) (*differentialChild, int, int, int, error) {
	integrationStartupMu.Lock()
	defer integrationStartupMu.Unlock()
	candidatePort, err := reservePort()
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("reserve candidate port: %w", err)
	}
	statusPort, err := reserveDifferentialPortExcluding(candidatePort)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("reserve candidate status port: %w", err)
	}
	controlPort, err := reserveDifferentialPortExcluding(candidatePort, statusPort)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("reserve candidate control port: %w", err)
	}
	runtime, err := renderDifferentialCandidateRuntimeWithOverlay(
		candidatePort, statusPort, controlPort, workDir, plugins, runtimeOverlay,
	)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("render candidate runtime: %w", err)
	}
	if err := os.WriteFile(configPath, runtime, 0o600); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("write candidate runtime %s: %w", configPath, err)
	}
	child, err := startDifferentialCandidate(workDir, logPath, binary, configPath)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if err := waitDifferentialCandidateListeners(child, candidatePort, statusPort, controlPort); err != nil {
		return child, candidatePort, statusPort, controlPort, err
	}
	return child, candidatePort, statusPort, controlPort, nil
}

func reserveDifferentialPortExcluding(excluded ...int) (int, error) {
	for range differentialFixtureBindAttempts {
		port, err := reservePort()
		if err != nil {
			return 0, err
		}
		if !slices.Contains(excluded, port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("reserve distinct differential port after %d attempts", differentialFixtureBindAttempts)
}

type differentialOracleFixtureLaunch struct {
	responseHex    string
	delayMillis    int
	program        string
	captureHTTP    bool
	certificateHex string
	privateKeyHex  string
}

func differentialOracleBootstrapFixture(fixture DifferentialFixture) bool {
	switch fixture.WireProtocol {
	case differentialFixtureWireHTTPTCP, differentialFixtureWireHTTPUDP, differentialFixtureWireTLSTCP:
		return true
	default:
		return fixture.WireProtocol == "" && fixture.RequestWindowQuietMillis > 0
	}
}

func differentialOracleRunArgs(
	identity OracleIdentity,
	name string,
	runtimePath string,
	standalonePath string,
	bootstrap *differentialOracleFixtureLaunch,
) ([]string, error) {
	return differentialOracleRunArgsWithFileMount(
		identity, name, runtimePath, standalonePath, bootstrap, "",
	)
}

func differentialOracleRunArgsWithFileMount(
	identity OracleIdentity,
	name string,
	runtimePath string,
	standalonePath string,
	bootstrap *differentialOracleFixtureLaunch,
	fileHostDir string,
) ([]string, error) {
	args := []string{
		"run", "--rm", "--detach", "--name", name, "--pull=never", "--platform", "linux/amd64",
		"--network", "bridge",
		"--env", "APISIX_STAND_ALONE=true",
		"--volume", runtimePath + ":/usr/local/apisix/conf/config.yaml:ro",
		"--volume", standalonePath + ":/usr/local/apisix/conf/apisix.yaml:ro",
	}
	hostGateway, err := differentialOracleHostGatewayAddress()
	if err != nil {
		return nil, err
	}
	if hostGateway != "" {
		args = append(args, "--add-host", differentialOracleHostGateway+":"+hostGateway)
	}
	if volume := differentialOracleFileVolume(fileHostDir); volume != "" {
		args = append(args, "--volume", volume)
	}
	if bootstrap == nil {
		return append(args, identity.ImageReference()), nil
	}
	args = append(
		args,
		"--entrypoint", "perl",
		"--env", "APISIX_GO_FIXTURE_RESPONSE_HEX="+bootstrap.responseHex,
		"--env", "APISIX_GO_FIXTURE_DELAY_MILLIS="+strconv.Itoa(bootstrap.delayMillis),
		"--env", "APISIX_GO_FIXTURE_CAPTURE_HTTP="+strconv.FormatBool(bootstrap.captureHTTP),
		identity.ImageReference(),
		"-MIO::Socket::INET",
		"-e",
		renderDifferentialOracleBootstrapProgram(
			bootstrap.program,
			differentialOracleFixtureReady,
			"/docker-entrypoint.sh",
			"docker-start",
		),
	)
	if bootstrap.certificateHex != "" {
		imageIndex := len(args) - 4
		tlsEnvironment := []string{
			"--env", "APISIX_GO_FIXTURE_CERTIFICATE_HEX=" + bootstrap.certificateHex,
			"--env", "APISIX_GO_FIXTURE_PRIVATE_KEY_HEX=" + bootstrap.privateKeyHex,
		}
		args = append(args[:imageIndex], append(tlsEnvironment, args[imageIndex:]...)...)
	}
	return args, nil
}

func differentialOracleHostGatewayAddress() (string, error) {
	raw := strings.TrimSpace(os.Getenv(differentialHostGatewayEnv))
	if raw == "" {
		return "", nil
	}
	ip := net.ParseIP(raw)
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("%s must be an IPv4 address, got %q", differentialHostGatewayEnv, raw)
	}
	return ip.To4().String(), nil
}

func renderDifferentialOracleBootstrapProgram(
	fixtureProgram string,
	readyPath string,
	entrypointPath string,
	entrypointCommand string,
) string {
	return `my $fixture_pid = fork(); die "fork fixture: $!\n" unless defined $fixture_pid; ` +
		`if ($fixture_pid == 0) { ` + fixtureProgram + `; exit 0; } ` +
		`my $fixture_ready = ` + strconv.Quote(readyPath) + `; my $deadline = time() + 3; ` +
		`while (!-e $fixture_ready) { my $result = waitpid($fixture_pid, 1); ` +
		`die "fixture exited before ready\n" if $result == $fixture_pid; ` +
		`die "fixture ready timeout\n" if time() >= $deadline; ` +
		`select(undef, undef, undef, 0.05); } ` +
		`exec ` + strconv.Quote(entrypointPath) + `, ` + strconv.Quote(entrypointCommand) + `; ` +
		`die "exec oracle entrypoint: $!\n";`
}

func startDifferentialOracle(
	containerBin string,
	identity OracleIdentity,
	name string,
	runtimePath string,
	standalonePath string,
	bootstrapFixture *DifferentialFixture,
) (*differentialChild, error) {
	return startDifferentialOracleWithFileMount(
		containerBin,
		identity,
		name,
		runtimePath,
		standalonePath,
		bootstrapFixture,
		"",
	)
}

func startDifferentialOracleWithFileMount(
	containerBin string,
	identity OracleIdentity,
	name string,
	runtimePath string,
	standalonePath string,
	bootstrapFixture *DifferentialFixture,
	fileHostDir string,
) (*differentialChild, error) {
	child := &differentialChild{
		done: make(chan error, 1), container: true, runtime: containerBin, name: name,
	}
	var bootstrap *differentialOracleFixtureLaunch
	if bootstrapFixture != nil {
		launch, err := prepareDifferentialOracleFixtureLaunch(*bootstrapFixture)
		if err != nil {
			return child, err
		}
		bootstrap = &launch
	}
	args, err := differentialOracleRunArgsWithFileMount(
		identity, name, runtimePath, standalonePath, bootstrap, fileHostDir,
	)
	if err != nil {
		return child, err
	}
	output, err := runDifferentialPodmanCommand(
		containerBin,
		differentialPodmanTimeout,
		nil,
		nil,
		args...,
	)
	if err != nil {
		return child, fmt.Errorf(
			"start oracle container: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	if bootstrap != nil {
		if err := waitDifferentialOracleFile(child, differentialOracleFixtureReady, 3*time.Second); err != nil {
			return child, fmt.Errorf("wait for bootstrapped oracle fixture: %w", err)
		}
		child.oracleFixtureBootstrapped = true
	}
	return child, nil
}

func (child *differentialChild) logs() (string, error) {
	if child.container {
		output, err := runDifferentialPodmanCommand(
			child.runtime, differentialPodmanTimeout, nil, nil, "logs", child.name,
		)
		if err != nil {
			return string(output), err
		}
		return string(output), nil
	}
	data, err := os.ReadFile(child.logPath)
	return string(data), err
}

func finalizeDifferentialOracle(
	result *DifferentialCaseResult,
	child *differentialChild,
	logPath string,
) {
	if child == nil {
		return
	}
	if !result.Passed {
		logs, logErr := child.logs()
		message := strings.TrimSpace(logs)
		if logErr != nil {
			message = "oracle logs unavailable: " + logErr.Error()
			if output := strings.TrimSpace(logs); output != "" {
				message += "\ncontainer output: " + output
			}
		} else if message == "" {
			message = "oracle logs unavailable: container returned no output"
		}
		if err := writeDifferentialDiagnosticLog(logPath, message); err != nil {
			appendDifferentialError(result, "oracle logs: ", err)
		}
	}
	if err := child.stop(); err != nil {
		result.Passed = false
		appendDifferentialError(result, "oracle stop: ", err)
	}
}

func (child *differentialChild) stop() error {
	if child == nil {
		return nil
	}
	child.stopOnce.Do(func() {
		child.stopErr = child.stopInternal()
	})
	return child.stopErr
}

func (child *differentialChild) stopInternal() error {
	if child.container {
		output, err := runDifferentialPodmanCommand(
			child.runtime, differentialPodmanTimeout, nil, nil, "rm", "--force", child.name,
		)
		if err != nil && !differentialContainerAbsent(output, err) {
			absent, verifyErr := waitDifferentialContainerAbsent(
				child.runtime,
				child.name,
				differentialRemovalVerifyTimeout,
				differentialRemovalVerifyInterval,
			)
			if verifyErr == nil && absent {
				return nil
			}
			if verifyErr != nil {
				return fmt.Errorf(
					"remove oracle container: %w: %s; verify absence: %v",
					err,
					strings.TrimSpace(string(output)),
					verifyErr,
				)
			}
			return fmt.Errorf(
				"remove oracle container: %w: %s",
				err,
				strings.TrimSpace(string(output)),
			)
		}
		return nil
	}
	if child.command == nil || child.command.Process == nil {
		return nil
	}
	if err := child.command.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		select {
		case waitErr := <-child.done:
			return waitErr
		default:
			return err
		}
	}
	select {
	case err := <-child.done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	case <-time.After(5 * time.Second):
		if err := child.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		if err := waitDifferentialCandidateDoneAfterKill(child.done, 5*time.Second); err != nil {
			return fmt.Errorf("candidate killed after graceful-stop timeout: %w", err)
		}
		return errors.New("candidate did not stop within 5s")
	}
}

func waitDifferentialCandidateDoneAfterKill(done <-chan error, timeout time.Duration) error {
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("candidate did not exit after kill")
	}
}

func differentialContainerAbsent(output []byte, err error) bool {
	message := strings.ToLower(string(output))
	if err != nil {
		message += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(message, "no such container") ||
		strings.Contains(message, "no container with name") ||
		strings.Contains(message, "container does not exist")
}

func differentialContainerExists(runtime, name string) (bool, error) {
	output, err := runDifferentialPodmanCommand(
		runtime, differentialPodmanTimeout, nil, nil, "container", "exists", name,
	)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf(
		"query oracle container %q: %w: %s",
		name,
		err,
		strings.TrimSpace(string(output)),
	)
}

func waitDifferentialContainerAbsent(
	runtime string,
	name string,
	timeout time.Duration,
	interval time.Duration,
) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		exists, err := differentialContainerExists(runtime, name)
		if err != nil {
			return false, err
		}
		if !exists {
			return true, nil
		}
		if time.Now().Add(interval).After(deadline) {
			return false, nil
		}
		time.Sleep(interval)
	}
}

func runDifferentialCase(
	repoRoot string,
	identity OracleIdentity,
	policy NormalizationPolicy,
	candidateBin string,
	containerBin string,
	runNonce string,
	artifactPath string,
	workDir string,
	spec DifferentialCase,
) (result DifferentialCaseResult) {
	result = DifferentialCaseResult{
		Name: spec.Name, Plugin: spec.Plugin, ComparisonPolicy: spec.ComparisonPolicy, FirstAttempt: true,
	}
	candidateLogPath := filepath.Join(workDir, "candidate.log")
	oracleLogPath := filepath.Join(workDir, "oracle.log")
	var fixture *differentialFixtureServer
	var candidate *differentialChild
	var oracle *differentialChild
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Passed = false
			result.Error = fmt.Sprintf("case panic: %v", recovered)
		}
		if err := candidate.stop(); err != nil {
			result.Passed = false
			appendDifferentialError(&result, "candidate stop: ", err)
		}
		finalizeDifferentialOracle(&result, oracle, oracleLogPath)
		if fixture != nil {
			fixture.close()
		}
		if !result.Passed {
			if err := os.MkdirAll(workDir, 0o700); err == nil {
				if _, err := os.Stat(candidateLogPath); errors.Is(err, os.ErrNotExist) {
					if err := writeDifferentialDiagnosticLog(
						candidateLogPath,
						"candidate was not started",
					); err != nil {
						appendDifferentialError(&result, "candidate logs: ", err)
					}
				}
				if _, err := os.Stat(oracleLogPath); errors.Is(err, os.ErrNotExist) {
					if err := writeDifferentialDiagnosticLog(oracleLogPath, "oracle was not started"); err != nil {
						appendDifferentialError(&result, "oracle logs: ", err)
					}
				}
			}
			result.Error = strings.TrimSpace(result.Error + "; candidate_log=" +
				differentialLogReference(artifactPath, candidateLogPath) +
				" oracle_log=" + differentialLogReference(artifactPath, oracleLogPath))
		}
	}()

	if err := os.MkdirAll(workDir, 0o700); err != nil {
		result.Error = fmt.Sprintf("create differential case directory: %v", err)
		return result
	}
	if err := writeDifferentialDiagnosticLog(candidateLogPath, "candidate was not started"); err != nil {
		result.Error = err.Error()
		return result
	}
	if err := writeDifferentialDiagnosticLog(oracleLogPath, "oracle was not started"); err != nil {
		result.Error = err.Error()
		return result
	}
	fixture, err := newDifferentialFixture(spec.Fixture)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	fixturePort := fixture.port()
	configDir := filepath.Join(workDir, "conf")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		result.Error = fmt.Sprintf("create differential config directory: %v", err)
		return result
	}
	defaultConfig, err := os.ReadFile(filepath.Join(repoRoot, "conf", "config-default.yaml"))
	if err != nil {
		result.Error = fmt.Sprintf("read candidate default config: %v", err)
		return result
	}
	if err := os.WriteFile(filepath.Join(configDir, "config-default.yaml"), defaultConfig, 0o600); err != nil {
		result.Error = fmt.Sprintf("write candidate default config: %v", err)
		return result
	}
	candidateConfig := spec.Config
	oracleConfig := spec.Config
	var fileSides differentialFileSides
	var logRotateSides differentialLogRotateSides
	var candidateRuntimeOverlay map[string]any
	var oracleRuntimeOverlay map[string]any
	mqttCase := isDifferentialMQTTProxyCase(spec)
	oracleFileHostDir := ""
	if spec.File != nil {
		fileSides, err = prepareDifferentialFileSides(workDir, *spec.File)
		if err != nil {
			result.Error = fmt.Sprintf("prepare differential side files: %v", err)
			return result
		}
		candidateConfig, err = projectDifferentialSideFile(spec.Config, fileSides.CandidatePath)
		if err != nil {
			result.Error = fmt.Sprintf("project candidate side file: %v", err)
			return result
		}
		oracleConfig, err = projectDifferentialSideFile(spec.Config, fileSides.OracleConfigPath)
		if err != nil {
			result.Error = fmt.Sprintf("project oracle side file: %v", err)
			return result
		}
		oracleFileHostDir = fileSides.OracleHostDir
	}
	if spec.ComparisonPolicy == differentialLogRotatePolicy {
		if spec.File != nil {
			result.Error = "log-rotate differential case may not use the generic side-file capture"
			return result
		}
		logRotateSides, err = prepareDifferentialLogRotateSides(
			workDir, differentialLogRotatePlan(),
		)
		if err != nil {
			result.Error = fmt.Sprintf("prepare differential log-rotate sides: %v", err)
			return result
		}
		candidateConfig, err = projectDifferentialLogRotateConfig(
			spec.Config, logRotateSides.CandidateDir,
		)
		if err != nil {
			result.Error = fmt.Sprintf("project candidate log-rotate config: %v", err)
			return result
		}
		oracleConfig, err = projectDifferentialLogRotateConfig(
			spec.Config, logRotateSides.OracleConfigDir,
		)
		if err != nil {
			result.Error = fmt.Sprintf("project oracle log-rotate config: %v", err)
			return result
		}
		candidateRuntimeOverlay = differentialLogRotateRuntimeOverlay(logRotateSides.CandidateDir)
		oracleRuntimeOverlay = differentialLogRotateRuntimeOverlay(logRotateSides.OracleConfigDir)
		oracleFileHostDir = logRotateSides.OracleHostDir
	}
	var projectedCandidate map[string]any
	if !mqttCase {
		projectedCandidate, err = projectDifferentialConfig(
			candidateConfig,
			net.JoinHostPort("127.0.0.1", strconv.Itoa(fixturePort)),
		)
		if err != nil {
			result.Error = fmt.Sprintf("project candidate config: %v", err)
			return result
		}
	}
	projectedOracle, err := projectDifferentialConfig(
		oracleConfig,
		differentialOracleFixtureEndpoint(spec.Fixture, fixturePort),
	)
	if err != nil {
		result.Error = fmt.Sprintf("project oracle config: %v", err)
		return result
	}
	if mqttCase {
		if err := projectDifferentialMQTTListenPort(
			projectedOracle, differentialMQTTOracleListenPort,
		); err != nil {
			result.Error = fmt.Sprintf("project oracle MQTT config: %v", err)
			return result
		}
	}
	plugins := differentialRequiredPluginNames([]DifferentialCase{spec})
	oracleRuntime, err := renderDifferentialOracleRuntimeWithOverlay(plugins, oracleRuntimeOverlay)
	if err != nil {
		result.Error = fmt.Sprintf("render oracle runtime: %v", err)
		return result
	}
	if mqttCase {
		oracleRuntime, err = renderDifferentialMQTTRuntime(
			oracleRuntime,
			net.JoinHostPort("127.0.0.1", strconv.Itoa(differentialMQTTOracleListenPort)),
		)
		if err != nil {
			result.Error = fmt.Sprintf("render oracle MQTT runtime: %v", err)
			return result
		}
	}
	var candidateStandalone []byte
	if !mqttCase {
		candidateStandalone, err = renderDifferentialStandalone(projectedCandidate, "")
		if err != nil {
			result.Error = fmt.Sprintf("render candidate standalone: %v", err)
			return result
		}
	}
	oracleStandalone, err := renderDifferentialStandalone(projectedOracle, "")
	if err != nil {
		result.Error = fmt.Sprintf("render oracle standalone: %v", err)
		return result
	}
	candidateRuntimePath := filepath.Join(configDir, "config.yaml")
	candidateStandalonePath := filepath.Join(configDir, "apisix.yaml")
	oracleRuntimePath := filepath.Join(workDir, "oracle-config.yaml")
	oracleStandalonePath := filepath.Join(workDir, "oracle-apisix.yaml")
	files := map[string][]byte{
		oracleRuntimePath:    oracleRuntime,
		oracleStandalonePath: oracleStandalone,
	}
	if !mqttCase {
		files[candidateStandalonePath] = candidateStandalone
	}
	for path, data := range files {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			result.Error = fmt.Sprintf("write differential config %s: %v", path, err)
			return result
		}
	}

	fixture.resetForSide(false)
	var candidatePort, statusPort, controlPort int
	if mqttCase {
		candidate, candidatePort, statusPort, controlPort, _, projectedCandidate, err = startDifferentialMQTTCandidateUnderStartupLock(
			workDir,
			candidateLogPath,
			candidateBin,
			candidateRuntimePath,
			candidateStandalonePath,
			candidateConfig,
			net.JoinHostPort("127.0.0.1", strconv.Itoa(fixturePort)),
			plugins,
		)
	} else {
		candidate, candidatePort, statusPort, controlPort, err = startDifferentialCandidateUnderStartupLock(
			workDir,
			candidateLogPath,
			candidateBin,
			candidateRuntimePath,
			plugins,
			candidateRuntimeOverlay,
		)
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if err := waitDifferentialCandidateGeneration(candidate, statusPort); err != nil {
		result.Error = err.Error()
		return result
	}
	candidateObservationSpec := spec
	if mqttCase {
		candidateObservationSpec.Config = projectedCandidate
	}
	candidateObservation, candidateErr := observeDifferentialSideWithPortsAndFileRoot(
		fixture,
		candidateObservationSpec,
		candidatePort,
		controlPort,
		"127.0.0.1:"+strconv.Itoa(fixturePort),
		logRotateSides.CandidateDir,
	)
	if candidateErr == nil && spec.File != nil {
		fileObservation, fileErr := collectDifferentialFileObservation(fileSides.CandidatePath, *spec.File)
		if fileErr != nil {
			candidateErr = fileErr
		} else {
			candidateObservation.File = &fileObservation
		}
	}
	if candidateErr == nil && spec.ComparisonPolicy == differentialLogRotatePolicy {
		fileObservation, fileErr := collectDifferentialLogRotateObservation(
			logRotateSides.CandidateDir, true, differentialLogRotatePlan().WaitTimeout,
		)
		if fileErr != nil {
			candidateErr = fileErr
		} else {
			candidateObservation.File = &fileObservation
		}
	}
	stopErr := candidate.stop()
	if candidateErr != nil {
		result.Error = "candidate: " + candidateErr.Error()
		if stopErr != nil {
			result.Error += "; stop: " + stopErr.Error()
		}
		return result
	}
	if stopErr != nil {
		result.Error = "candidate stop: " + stopErr.Error()
		return result
	}
	result.Candidate = &candidateObservation
	result.CandidateHash, err = hashObservation(candidateObservation, policy)
	if err != nil {
		result.Error = "candidate hash: " + err.Error()
		return result
	}

	fixture.resetForSide(true)
	containerName := differentialOracleContainerName(runNonce, spec.Name, workDir)
	var bootstrapFixture *DifferentialFixture
	if differentialOracleBootstrapFixture(spec.Fixture) {
		bootstrapFixture = &spec.Fixture
	}
	oracle, err = startDifferentialOracleWithFileMount(
		containerBin,
		identity,
		containerName,
		oracleRuntimePath,
		oracleStandalonePath,
		bootstrapFixture,
		oracleFileHostDir,
	)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if err := waitDifferentialOracle(oracle); err != nil {
		result.Error = err.Error()
		return result
	}
	oracleObservationSpec := spec
	if mqttCase {
		oracleObservationSpec.Config = projectedOracle
	}
	oracleObservation, oracleErr := observeDifferentialOracleSideWithFileRoot(
		fixture,
		oracleObservationSpec,
		oracle,
		differentialOracleFixtureEndpoint(spec.Fixture, fixturePort),
		logRotateSides.OracleHostDir,
	)
	if oracleErr == nil && spec.File != nil {
		fileObservation, fileErr := collectDifferentialFileObservation(fileSides.OracleHostPath, *spec.File)
		if fileErr != nil {
			oracleErr = fileErr
		} else {
			oracleObservation.File = &fileObservation
		}
	}
	if oracleErr == nil && spec.ComparisonPolicy == differentialLogRotatePolicy {
		fileObservation, stopped, fileErr := collectDifferentialLogRotateOracleObservation(
			oracle,
			oracleLogPath,
			logRotateSides.OracleHostDir,
			differentialLogRotatePlan().WaitTimeout,
		)
		if stopped {
			oracle = nil
		}
		if fileErr != nil {
			oracleErr = fileErr
		} else {
			oracleObservation.File = &fileObservation
		}
	}
	if oracleErr != nil {
		result.Error = "oracle: " + oracleErr.Error()
		return result
	}
	result.Oracle = &oracleObservation
	result.OracleHash, err = hashObservation(oracleObservation, policy)
	if err != nil {
		result.Error = "oracle hash: " + err.Error()
		return result
	}
	passed, diff, err := compareDifferentialCaseObservations(
		spec,
		candidateObservation,
		oracleObservation,
		policy,
	)
	if err != nil {
		result.Error = "compare: " + err.Error()
		return result
	}
	if !passed {
		result.Error = diff
		return result
	}
	result.Passed = true
	return result
}

func waitDifferentialCandidateListeners(child *differentialChild, port, statusPort, controlPort int) error {
	if err := waitDifferentialListener(child, port, 5*time.Second); err != nil {
		return err
	}
	if err := waitDifferentialListener(child, statusPort, 5*time.Second); err != nil {
		return err
	}
	return waitDifferentialListener(child, controlPort, 5*time.Second)
}

func waitDifferentialCandidateGeneration(child *differentialChild, statusPort int) error {
	ready, err := waitForInitialGeneration(
		"127.0.0.1:"+strconv.Itoa(statusPort), child.logs, 10*time.Second,
	)
	if err != nil {
		return fmt.Errorf("candidate generation: %w", err)
	}
	if !ready {
		return errors.New("candidate initial generation was not ready")
	}
	return nil
}

func waitDifferentialOracle(child *differentialChild) error {
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		response, err := executeDifferentialOracleRequestWithTimeout(
			child,
			DifferentialCase{Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/__differential_ready__",
				Host:   "localhost",
			}},
			remaining,
		)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("wait for oracle HTTP listener inside container %s: %w", child.name, lastErr)
}

func waitDifferentialListener(child *differentialChild, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if child.command != nil {
			select {
			case err := <-child.done:
				select {
				case child.done <- err:
				default:
				}
				if err == nil {
					return errors.New("child exited before readiness")
				}
				return fmt.Errorf("child exited before readiness: %w", err)
			default:
			}
		}
		connection, err := net.DialTimeout(
			"tcp",
			net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
			100*time.Millisecond,
		)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		time.Sleep(30 * time.Millisecond)
	}
	return fmt.Errorf("child did not listen on 127.0.0.1:%d within %s", port, timeout)
}

func observeDifferentialSide(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	port int,
	upstreamAddress string,
) (DifferentialObservation, error) {
	return observeDifferentialSideWithPorts(fixture, spec, port, 0, upstreamAddress)
}

func differentialTargetPort(target DifferentialRequestTarget, dataPort, controlPort int) (int, error) {
	switch target {
	case DifferentialRequestTargetData:
		return dataPort, nil
	case DifferentialRequestTargetControl:
		if controlPort <= 0 {
			return 0, errors.New("differential control request port is not configured")
		}
		return controlPort, nil
	default:
		return 0, fmt.Errorf("unsupported differential request target %q", target)
	}
}

func observeDifferentialSideWithPorts(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	dataPort int,
	controlPort int,
	upstreamAddress string,
) (DifferentialObservation, error) {
	return observeDifferentialSideWithPortsAndFileRoot(
		fixture, spec, dataPort, controlPort, upstreamAddress, "",
	)
}

func observeDifferentialSideWithPortsAndFileRoot(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	dataPort int,
	controlPort int,
	upstreamAddress string,
	fileRoot string,
) (DifferentialObservation, error) {
	if isDifferentialMQTTProxyCase(spec) {
		return observeDifferentialMQTTProxyCandidate(fixture, spec, upstreamAddress)
	}
	if observation, handled, err := observeDifferentialProtocolCandidate(spec, dataPort); handled {
		if err == nil && spec.ComparisonPolicy == differentialKafkaProxyPubSubPolicy {
			err = attachDifferentialKafkaProxyFixtureEvidence(
				fixture, spec, &observation, upstreamAddress,
				len(differentialKafkaProxyExpectedCandidateBrokerCalls(spec)),
			)
		}
		return observation, err
	}
	if len(spec.Steps) > 0 {
		return observeDifferentialCandidateSequence(
			fixture, spec, dataPort, controlPort, upstreamAddress, fileRoot,
		)
	}
	if spec.Fixture.RequestWindowQuietMillis > 0 {
		return DifferentialObservation{}, errors.New("differential fixture request window requires a sequence case")
	}
	port, err := differentialTargetPort(spec.Request.Target, dataPort, controlPort)
	if err != nil {
		return DifferentialObservation{}, err
	}
	requestURL := "http://127.0.0.1:" + strconv.Itoa(port) + spec.Request.Path
	request, err := http.NewRequest(
		spec.Request.Method,
		requestURL,
		strings.NewReader(spec.Request.Body),
	)
	if err != nil {
		return DifferentialObservation{}, err
	}
	request.Host = spec.Request.Host
	for name, value := range spec.Request.Headers {
		request.Header.Set(name, value)
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	client := &http.Client{
		Timeout: 5 * time.Second, Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		transport.CloseIdleConnections()
		return DifferentialObservation{}, fmt.Errorf("request %s: %w", spec.Name, err)
	}
	observation, err := observeDifferentialResponse(fixture, spec, response, upstreamAddress)
	transport.CloseIdleConnections()
	return observation, err
}

func observeDifferentialCandidateSequence(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	dataPort int,
	controlPort int,
	upstreamAddress string,
	fileRoot string,
) (DifferentialObservation, error) {
	var baseline differentialFixtureWindowBaseline
	quiet := differentialFixtureRequestWindowQuiet(spec.Fixture)
	if quiet > 0 {
		var err error
		baseline, err = fixture.beginRequestWindow(
			quiet, differentialCandidateFixtureCollectTimeout(spec.Fixture),
		)
		if err != nil {
			return DifferentialObservation{}, err
		}
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout: 5 * time.Second, Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	observation := DifferentialObservation{
		Steps: make([]DifferentialStepObservation, 0, len(spec.Steps)),
	}
	for index, step := range spec.Steps {
		if err := waitDifferentialStepDelay(index, step); err != nil {
			return DifferentialObservation{}, err
		}
		stepObservations, err := observeDifferentialStepResponses(
			index,
			step,
			func(requestSpec DifferentialRequest) (*http.Response, error) {
				port, err := differentialTargetPort(requestSpec.Target, dataPort, controlPort)
				if err != nil {
					return nil, err
				}
				requestURL := "http://127.0.0.1:" + strconv.Itoa(port) + requestSpec.Path
				request, err := http.NewRequest(
					requestSpec.Method,
					requestURL,
					strings.NewReader(requestSpec.Body),
				)
				if err != nil {
					return nil, err
				}
				request.Host = requestSpec.Host
				for name, value := range requestSpec.Headers {
					request.Header.Set(name, value)
				}
				return client.Do(request)
			},
		)
		if err != nil {
			return DifferentialObservation{}, fmt.Errorf("request %s step %d: %w", spec.Name, index, err)
		}
		observation.Steps = append(observation.Steps, stepObservations...)
		if err := waitDifferentialLogRotateAfterStep(spec, index, fileRoot); err != nil {
			return DifferentialObservation{}, fmt.Errorf(
				"request %s step %d state barrier: %w", spec.Name, index, err,
			)
		}
	}
	var received []differentialCapturedRequest
	var err error
	if quiet > 0 {
		received, err = fixture.collectRequestWindow(
			baseline,
			spec.Fixture.ExpectedCalls,
			differentialCandidateFixtureCollectTimeout(spec.Fixture),
			quiet,
		)
	} else {
		received, err = fixture.collectWithTimeout(
			spec.Fixture.ExpectedCalls,
			differentialCandidateFixtureCollectTimeout(spec.Fixture),
		)
	}
	if err != nil {
		return DifferentialObservation{}, err
	}
	applyDifferentialSequenceFixtureObservation(
		&observation,
		spec.Fixture,
		received,
		upstreamAddress,
	)
	return observation, nil
}

func observeDifferentialOracleSide(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	child *differentialChild,
	upstreamAddress string,
) (DifferentialObservation, error) {
	return observeDifferentialOracleSideWithFileRoot(
		fixture, spec, child, upstreamAddress, "",
	)
}

func observeDifferentialOracleSideWithFileRoot(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	child *differentialChild,
	upstreamAddress string,
	fileRoot string,
) (DifferentialObservation, error) {
	if isDifferentialMQTTProxyCase(spec) {
		return observeDifferentialMQTTProxyOracle(fixture, spec, child, upstreamAddress)
	}
	if observation, handled, err := observeDifferentialProtocolOracle(spec, child); handled {
		if err == nil && spec.ComparisonPolicy == differentialKafkaProxyPubSubPolicy {
			err = attachDifferentialKafkaProxyFixtureEvidence(
				fixture, spec, &observation, upstreamAddress,
				len(differentialKafkaProxyExpectedOracleBrokerCalls(spec)),
			)
		}
		return observation, err
	}
	if differentialFixtureUsesHostOracle(spec.Fixture) {
		response, err := executeDifferentialOracleRequest(child, spec)
		if err != nil {
			return DifferentialObservation{}, fmt.Errorf("request %s: %w", spec.Name, err)
		}
		return observeDifferentialResponse(fixture, spec, response, upstreamAddress)
	}
	if err := ensureDifferentialOracleFixture(child, spec.Fixture); err != nil {
		return DifferentialObservation{}, err
	}
	if len(spec.Steps) > 0 {
		return observeDifferentialOracleSequence(fixture, spec, child, upstreamAddress, fileRoot)
	}
	if spec.Fixture.RequestWindowQuietMillis > 0 {
		return DifferentialObservation{}, errors.New("differential fixture request window requires a sequence case")
	}
	response, err := executeDifferentialOracleRequest(child, spec)
	if err != nil {
		return DifferentialObservation{}, fmt.Errorf("request %s: %w", spec.Name, err)
	}
	captured, err := collectDifferentialOracleFixturesWithTimeout(
		child,
		spec.Fixture.ExpectedCalls,
		differentialOracleFixtureCollectTimeout(spec.Fixture),
	)
	if err != nil {
		_ = response.Body.Close()
		return DifferentialObservation{}, err
	}
	for _, request := range captured {
		fixture.requests <- request
	}
	return observeDifferentialResponse(fixture, spec, response, upstreamAddress)
}

func observeDifferentialOracleSequence(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	child *differentialChild,
	upstreamAddress string,
	fileRoot string,
) (DifferentialObservation, error) {
	quiet := differentialFixtureRequestWindowQuiet(spec.Fixture)
	baseline := 0
	if quiet > 0 {
		var err error
		baseline, err = beginDifferentialOracleRequestWindow(
			child, quiet, differentialOracleFixtureCollectTimeout(spec.Fixture),
		)
		if err != nil {
			return DifferentialObservation{}, err
		}
	}
	observation := DifferentialObservation{
		Steps: make([]DifferentialStepObservation, 0, len(spec.Steps)),
	}
	for index, step := range spec.Steps {
		if err := waitDifferentialStepDelay(index, step); err != nil {
			return DifferentialObservation{}, err
		}
		stepObservations, err := observeDifferentialStepResponses(
			index,
			step,
			func(requestSpec DifferentialRequest) (*http.Response, error) {
				stepSpec := spec
				stepSpec.Request = requestSpec
				stepSpec.Steps = nil
				stepSpec.SecurityDecision = step.SecurityDecision
				return executeDifferentialOracleRequest(child, stepSpec)
			},
		)
		if err != nil {
			return DifferentialObservation{}, fmt.Errorf("request %s step %d: %w", spec.Name, index, err)
		}
		observation.Steps = append(observation.Steps, stepObservations...)
		if err := waitDifferentialLogRotateAfterStep(spec, index, fileRoot); err != nil {
			return DifferentialObservation{}, fmt.Errorf(
				"request %s step %d state barrier: %w", spec.Name, index, err,
			)
		}
	}
	var captured []differentialCapturedRequest
	var err error
	if quiet > 0 {
		captured, err = collectDifferentialOracleFixtureWindow(
			child,
			baseline,
			spec.Fixture.ExpectedCalls,
			differentialOracleFixtureCollectTimeout(spec.Fixture),
			quiet,
		)
	} else {
		captured, err = collectDifferentialOracleFixturesWithTimeout(
			child,
			spec.Fixture.ExpectedCalls,
			differentialOracleFixtureCollectTimeout(spec.Fixture),
		)
	}
	if err != nil {
		return DifferentialObservation{}, err
	}
	if quiet > 0 {
		applyDifferentialSequenceFixtureObservation(
			&observation,
			spec.Fixture,
			captured,
			upstreamAddress,
		)
		return observation, nil
	}
	for _, request := range captured {
		fixture.requests <- request
	}
	received, err := fixture.collectWithTimeout(
		spec.Fixture.ExpectedCalls,
		differentialCandidateFixtureCollectTimeout(spec.Fixture),
	)
	if err != nil {
		return DifferentialObservation{}, err
	}
	applyDifferentialSequenceFixtureObservation(
		&observation,
		spec.Fixture,
		received,
		upstreamAddress,
	)
	return observation, nil
}

func waitDifferentialStepDelay(index int, step DifferentialStep) error {
	if step.DelayBeforeMillis < 0 || step.DelayBeforeMillis > 5000 {
		return fmt.Errorf(
			"sequence step %d delay_before_millis = %d, want 0..5000",
			index,
			step.DelayBeforeMillis,
		)
	}
	if step.DelayBeforeMillis > 0 {
		time.Sleep(time.Duration(step.DelayBeforeMillis) * time.Millisecond)
	}
	return nil
}

func observeDifferentialStepResponses(
	index int,
	step DifferentialStep,
	request func(DifferentialRequest) (*http.Response, error),
) ([]DifferentialStepObservation, error) {
	requests, concurrent, err := differentialStepRequests(index, step)
	if err != nil {
		return nil, err
	}
	if !concurrent {
		response, err := request(requests[0])
		if err != nil {
			return nil, err
		}
		step.Request = requests[0]
		observation, err := observeDifferentialStepResponse(step, response)
		if err != nil {
			return nil, err
		}
		return []DifferentialStepObservation{observation}, nil
	}

	observations := make([]DifferentialStepObservation, len(requests))
	errorsByIndex := make([]error, len(requests))
	ready := make(chan struct{}, len(requests))
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(len(requests))
	for requestIndex, requestSpec := range requests {
		go func() {
			defer workers.Done()
			ready <- struct{}{}
			<-start
			response, requestErr := request(requestSpec)
			if requestErr != nil {
				errorsByIndex[requestIndex] = requestErr
				return
			}
			requestStep := step
			requestStep.Request = requestSpec
			requestStep.ConcurrentRequests = nil
			observations[requestIndex], errorsByIndex[requestIndex] = observeDifferentialStepResponse(
				requestStep,
				response,
			)
		}()
	}
	for range requests {
		<-ready
	}
	close(start)
	workers.Wait()
	for requestIndex, requestErr := range errorsByIndex {
		if requestErr != nil {
			return nil, fmt.Errorf("concurrent request %d: %w", requestIndex, requestErr)
		}
	}
	return observations, nil
}

func differentialStepRequests(
	index int,
	step DifferentialStep,
) ([]DifferentialRequest, bool, error) {
	if len(step.ConcurrentRequests) == 0 {
		return []DifferentialRequest{step.Request}, false, nil
	}
	if !differentialRequestIsZero(step.Request) {
		return nil, false, fmt.Errorf(
			"sequence step %d cannot set both request and concurrent_requests",
			index,
		)
	}
	if len(step.ConcurrentRequests) < 2 || len(step.ConcurrentRequests) > differentialMaxConcurrentRequests {
		return nil, false, fmt.Errorf(
			"sequence step %d concurrent_requests count = %d, want 2..%d",
			index,
			len(step.ConcurrentRequests),
			differentialMaxConcurrentRequests,
		)
	}
	return append([]DifferentialRequest(nil), step.ConcurrentRequests...), true, nil
}

func differentialRequestIsZero(request DifferentialRequest) bool {
	return request.Method == "" && request.Path == "" && request.Host == "" &&
		request.Target == DifferentialRequestTargetData && request.SNI == "" &&
		len(request.Headers) == 0 && request.Body == ""
}

func observeDifferentialStepResponse(
	step DifferentialStep,
	response *http.Response,
) (DifferentialStepObservation, error) {
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return DifferentialStepObservation{}, fmt.Errorf("read response: %w", readErr)
	}
	if closeErr != nil {
		return DifferentialStepObservation{}, fmt.Errorf("close response: %w", closeErr)
	}
	return DifferentialStepObservation{
		Status: response.StatusCode, Headers: differentialHTTPHeaders(response.Header), Body: string(body),
		Host: step.Request.Host, SNI: step.Request.SNI, SecurityDecision: step.SecurityDecision,
	}, nil
}

func renderDifferentialOracleFixtureResponse(response DifferentialFixtureResponse) ([]byte, error) {
	status := response.Status
	if status == 0 {
		status = http.StatusOK
	}
	reason := http.StatusText(status)
	if reason == "" {
		return nil, fmt.Errorf("unsupported fixture response status %d", status)
	}
	var raw strings.Builder
	fmt.Fprintf(&raw, "HTTP/1.1 %d %s\r\n", status, reason)
	names := make([]string, 0, len(response.Headers))
	hasContentType := false
	for name := range response.Headers {
		names = append(names, name)
		if strings.EqualFold(name, "Content-Type") {
			hasContentType = true
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Connection") {
			continue
		}
		fmt.Fprintf(&raw, "%s: %s\r\n", name, response.Headers[name])
	}
	if response.Body != "" && !hasContentType {
		raw.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	}
	fmt.Fprintf(
		&raw,
		"Content-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(response.Body),
		response.Body,
	)
	return []byte(raw.String()), nil
}

func prepareDifferentialOracleFixtureLaunch(
	fixture DifferentialFixture,
) (differentialOracleFixtureLaunch, error) {
	if fixture.ExpectedCalls < 0 {
		return differentialOracleFixtureLaunch{}, errors.New("oracle fixture expected call count must not be negative")
	}
	if err := validateDifferentialFixtureRequestWindow(fixture); err != nil {
		return differentialOracleFixtureLaunch{}, err
	}
	if _, err := differentialFixtureResponseDelay(fixture.Response); err != nil {
		return differentialOracleFixtureLaunch{}, err
	}
	if err := validateDifferentialFixtureHTTPOriginContract(fixture); err != nil {
		return differentialOracleFixtureLaunch{}, err
	}
	if fixture.WireProtocol != "" && fixture.Response.DelayMillis != 0 {
		return differentialOracleFixtureLaunch{}, fmt.Errorf(
			"oracle fixture response delay_millis is unsupported for wire protocol %q",
			fixture.WireProtocol,
		)
	}
	var response []byte
	var program string
	switch fixture.WireProtocol {
	case "":
		var err error
		response, err = renderDifferentialOracleFixtureResponse(fixture.Response)
		if err != nil {
			return differentialOracleFixtureLaunch{}, err
		}
		program = differentialOracleHTTPFixtureProgram()
	case differentialFixtureWireHTTPTCP:
		var err error
		response, err = renderDifferentialOracleFixtureResponse(fixture.Response)
		if err != nil {
			return differentialOracleFixtureLaunch{}, err
		}
		program = differentialOracleHTTPTCPFixtureProgram()
	case differentialFixtureWireHTTPUDP:
		var err error
		response, err = renderDifferentialOracleFixtureResponse(fixture.Response)
		if err != nil {
			return differentialOracleFixtureLaunch{}, err
		}
		program = differentialOracleHTTPUDPFixtureProgram()
	case differentialFixtureWireTLSTCP:
		program = differentialOracleTLSTCPFixtureProgram()
	case differentialFixtureWireT1KV2:
		response = differentialT1KV2RejectResponse()
		program = differentialOracleT1KV2FixtureProgram()
	default:
		return differentialOracleFixtureLaunch{}, fmt.Errorf(
			"unsupported differential fixture wire protocol %q",
			fixture.WireProtocol,
		)
	}
	launch := differentialOracleFixtureLaunch{
		responseHex: hex.EncodeToString(response),
		delayMillis: fixture.Response.DelayMillis,
		program:     program,
		captureHTTP: differentialFixtureCapturesHTTPOrigin(fixture),
	}
	if fixture.WireProtocol == differentialFixtureWireTLSTCP {
		launch.certificateHex = hex.EncodeToString([]byte(differentialFixtureCertificatePEM))
		launch.privateKeyHex = hex.EncodeToString([]byte(differentialFixturePrivateKeyPEM))
	}
	return launch, nil
}

func ensureDifferentialOracleFixture(child *differentialChild, fixture DifferentialFixture) error {
	if child.oracleFixtureBootstrapped {
		return nil
	}
	return startDifferentialOracleFixture(child, fixture)
}

func startDifferentialOracleFixture(child *differentialChild, fixture DifferentialFixture) error {
	launch, err := prepareDifferentialOracleFixtureLaunch(fixture)
	if err != nil {
		return err
	}
	args := []string{
		"exec", "--detach",
		"--env", "APISIX_GO_FIXTURE_RESPONSE_HEX=" + launch.responseHex,
		"--env", "APISIX_GO_FIXTURE_DELAY_MILLIS=" + strconv.Itoa(launch.delayMillis),
		"--env", "APISIX_GO_FIXTURE_CAPTURE_HTTP=" + strconv.FormatBool(launch.captureHTTP),
	}
	if launch.certificateHex != "" {
		args = append(
			args,
			"--env", "APISIX_GO_FIXTURE_CERTIFICATE_HEX="+launch.certificateHex,
			"--env", "APISIX_GO_FIXTURE_PRIVATE_KEY_HEX="+launch.privateKeyHex,
		)
	}
	args = append(args, child.name, "perl", "-MIO::Socket::INET", "-e", launch.program)
	output, err := runDifferentialPodmanCommand(
		child.runtime,
		differentialPodmanTimeout,
		nil,
		nil,
		args...,
	)
	if err != nil {
		return fmt.Errorf("start oracle fixture: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := waitDifferentialOracleFile(child, differentialOracleFixtureReady, 3*time.Second); err != nil {
		return fmt.Errorf("wait for oracle fixture: %w", err)
	}
	return nil
}

func differentialOracleHTTPFixtureProgram() string {
	return renderDifferentialOracleHTTPFixtureProgram(
		differentialOracleFixturePort,
		differentialOracleFixtureReady,
		differentialOracleFixtureRecord,
	)
}

func renderDifferentialOracleHTTPFixtureProgram(port int, ready, recordBase string) string {
	return `my $ready = "` + ready + `"; my $record_base = "` + recordBase + `"; my $delay_ms = $ENV{"APISIX_GO_FIXTURE_DELAY_MILLIS"}; $delay_ms = 0 unless defined $delay_ms; die "invalid delay\n" unless $delay_ms =~ /\A\d+\z/ && $delay_ms <= ` + strconv.Itoa(
		differentialMaxFixtureDelayMillis,
	) + `; unlink $ready; unlink glob($record_base . ".*"); my $listener = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => ` + strconv.Itoa(
		port,
	) + `, Proto => "tcp", Listen => 16, ReuseAddr => 1) or die "listen: $!\n"; open(my $ready_file, ">", $ready) or die "ready: $!\n"; print {$ready_file} "ready\n"; close($ready_file); my $index = 0; while (1) { my $client = $listener->accept() or die "accept: $!\n"; binmode $client; my $raw = ""; while (index($raw, "\r\n\r\n") < 0) { my $count = sysread($client, my $buffer, 8192); die "read headers: $!\n" unless defined $count; last if $count == 0; $raw .= $buffer; } my ($head, $body) = split(/\r\n\r\n/, $raw, 2); $body = "" unless defined $body; my ($length) = $head =~ /\r\nContent-Length:\s*(\d+)/i; $length = 0 unless defined $length; while (length($body) < $length) { my $count = sysread($client, my $buffer, 8192); die "read body: $!\n" unless defined $count; last if $count == 0; $body .= $buffer; } $raw = $head . "\r\n\r\n" . $body; my $record = $record_base . "." . $index; my $record_tmp = $record . ".tmp-" . $$; open(my $record_file, ">", $record_tmp) or die "record: $!\n"; binmode $record_file; print {$record_file} $raw; close($record_file) or die "record close: $!\n"; rename($record_tmp, $record) or die "record rename: $!\n"; select(undef, undef, undef, $delay_ms / 1000) if $delay_ms > 0; my $response = pack("H*", $ENV{"APISIX_GO_FIXTURE_RESPONSE_HEX"}); my $offset = 0; while ($offset < length($response)) { my $count = syswrite($client, $response, length($response) - $offset, $offset); die "write: $!\n" unless defined $count; $offset += $count; } shutdown($client, 1); close($client); $index += 1; }`
}

func differentialOracleHTTPTCPFixtureProgram() string {
	return renderDifferentialOracleHTTPTCPFixtureProgram(
		differentialOracleFixturePort, differentialOracleFixtureReady, differentialOracleFixtureRecord,
	)
}

func differentialOracleHTTPUDPFixtureProgram() string {
	return renderDifferentialOracleHTTPUDPFixtureProgram(
		differentialOracleFixturePort, differentialOracleFixtureReady, differentialOracleFixtureRecord,
	)
}

func differentialOracleTLSTCPFixtureProgram() string {
	return renderDifferentialOracleTLSTCPFixtureProgram(
		differentialOracleFixturePort, differentialOracleFixtureReady, differentialOracleFixtureRecord,
	)
}

func differentialOracleRawFixturePrelude(ready, recordBase string) string {
	return `my $ready = ` + strconv.Quote(ready) + `; my $record_base = ` + strconv.Quote(recordBase) + `; ` +
		`my $index = 0; my $capture_http = defined($ENV{"APISIX_GO_FIXTURE_CAPTURE_HTTP"}) && $ENV{"APISIX_GO_FIXTURE_CAPTURE_HTTP"} eq "true"; ` +
		`sub write_record { my ($method, $body) = @_; my $record = $record_base . "." . $index; my $tmp = $record . ".tmp-" . $$; open(my $file, ">", $tmp) or die "record: $!\n"; binmode $file; if (defined $method) { print {$file} "` + differentialRawRecordPrefix + `" . $method . " " . length($body) . "\n"; } print {$file} $body; close($file) or die "record close: $!\n"; rename($tmp, $record) or die "record rename: $!\n"; $index += 1; } ` +
		`sub write_response { my ($client) = @_; my $response = pack("H*", $ENV{"APISIX_GO_FIXTURE_RESPONSE_HEX"}); my $offset = 0; while ($offset < length($response)) { my $count = syswrite($client, $response, length($response) - $offset, $offset); die "write response: $!\n" unless defined $count; $offset += $count; } } ` +
		`sub read_http { my ($client, $raw) = @_; while (index($raw, "\r\n\r\n") < 0) { my $count = sysread($client, my $buffer, 8192); die "read HTTP headers: $!\n" unless defined $count; die "truncated HTTP headers\n" if $count == 0; $raw .= $buffer; die "HTTP request too large\n" if length($raw) > ` + strconv.Itoa(differentialRawMaxFramePayload) + `; } my ($head, $body) = split(/\r\n\r\n/, $raw, 2); $body = "" unless defined $body; my ($length) = $head =~ /\r\nContent-Length:\s*(\d+)/i; $length = 0 unless defined $length; while (length($body) < $length) { my $count = sysread($client, my $buffer, $length - length($body)); die "read HTTP body: $!\n" unless defined $count; die "truncated HTTP body\n" if $count == 0; $body .= $buffer; } die "HTTP request has trailing bytes\n" if length($body) != $length; return $head . "\r\n\r\n" . $body; } ` +
		`sub json_complete { my ($raw) = @_; $raw =~ s/^\s+//; return 0 unless $raw =~ /^[\{\[]/; my $depth = 0; my $string = 0; my $escape = 0; for (my $i = 0; $i < length($raw); $i++) { my $char = substr($raw, $i, 1); if ($string) { if ($escape) { $escape = 0; } elsif ($char eq "\\") { $escape = 1; } elsif ($char eq '"') { $string = 0; } next; } if ($char eq '"') { $string = 1; } elsif ($char eq "{" || $char eq "[") { $depth += 1; } elsif ($char eq "}" || $char eq "]") { $depth -= 1; return 0 if $depth < 0; } } return !$string && $depth == 0; } ` +
		`sub read_frame { my ($client, $raw) = @_; while (1) { my $newline = index($raw, "\n"); return substr($raw, 0, $newline + 1) if $newline >= 0; return $raw if json_complete($raw); die "raw frame too large\n" if length($raw) > ` + strconv.Itoa(differentialRawMaxFramePayload) + `; my $count = sysread($client, my $buffer, 8192); die "read raw frame: $!\n" unless defined $count; die "truncated raw frame\n" if $count == 0; $raw .= $buffer; } } ` +
		`unlink $ready; unlink glob($record_base . ".*"); `
}

func differentialOracleHTTPConnectionProgram() string {
	return `sub handle_http { my ($client, $initial) = @_; my $raw = read_http($client, $initial); write_record(undef, $raw) if $capture_http; write_response($client); shutdown($client, 1); close($client); } `
}

func differentialOracleReadyProgram() string {
	return `open(my $ready_file, ">", $ready) or die "ready: $!\n"; print {$ready_file} "ready\n"; close($ready_file); `
}

func renderDifferentialOracleHTTPTCPFixtureProgram(port int, ready, recordBase string) string {
	return differentialOracleRawFixturePrelude(ready, recordBase) +
		differentialOracleHTTPConnectionProgram() +
		`my $listener = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => ` + strconv.Itoa(port) + `, Proto => "tcp", Listen => 16, ReuseAddr => 1) or die "listen: $!\n"; ` +
		differentialOracleReadyProgram() +
		`while (1) { my $client = $listener->accept() or die "accept: $!\n"; binmode $client; my $count = sysread($client, my $initial, 8192); die "read initial frame: $!\n" unless defined $count; if ($count == 0) { close($client); next; } if ($initial =~ /\A[A-Z]+\s+\S+\s+HTTP\/1\.[01]\r\n/) { handle_http($client, $initial); next; } my $frame = read_frame($client, $initial); write_record("TCP", $frame); close($client); }`
}

func renderDifferentialOracleHTTPUDPFixtureProgram(port int, ready, recordBase string) string {
	return `use IO::Select; ` + differentialOracleRawFixturePrelude(ready, recordBase) +
		differentialOracleHTTPConnectionProgram() +
		`my $listener = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => ` + strconv.Itoa(port) + `, Proto => "tcp", Listen => 16, ReuseAddr => 1) or die "listen TCP: $!\n"; my $udp = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => ` + strconv.Itoa(port) + `, Proto => "udp", ReuseAddr => 1) or die "listen UDP: $!\n"; my $select = IO::Select->new($listener, $udp); ` +
		differentialOracleReadyProgram() +
		`while (1) { for my $socket ($select->can_read()) { if ($socket == $udp) { my $peer = recv($udp, my $frame, 65535, 0); die "recv UDP: $!\n" unless defined $peer; write_record("UDP", $frame); next; } my $client = $listener->accept() or die "accept: $!\n"; binmode $client; my $count = sysread($client, my $initial, 8192); die "read HTTP initial: $!\n" unless defined $count; if ($count == 0) { close($client); next; } handle_http($client, $initial); } }`
}

func renderDifferentialOracleTLSTCPFixtureProgram(port int, ready, recordBase string) string {
	return renderDifferentialOracleTLSTCPFixtureProgramWithOpenSSL(
		port, ready, recordBase, "/usr/bin/openssl",
	)
}

func renderDifferentialOracleTLSTCPFixtureProgramWithOpenSSL(
	port int,
	ready string,
	recordBase string,
	opensslPath string,
) string {
	certificatePath := ready + ".crt"
	privateKeyPath := ready + ".key"
	errorPath := ready + ".openssl.err"
	openssl := strconv.Quote(opensslPath)
	return `use IPC::Open3; use IO::Select; ` + differentialOracleRawFixturePrelude(ready, recordBase) +
		`my $openssl = ` + openssl + `; my $certificate = ` + strconv.Quote(certificatePath) + `; my $private_key = ` + strconv.Quote(privateKeyPath) + `; my $openssl_error_path = ` + strconv.Quote(errorPath) + `; unlink $certificate; unlink $private_key; unlink $openssl_error_path; open(my $certificate_file, ">", $certificate) or die "certificate: $!\n"; binmode $certificate_file; print {$certificate_file} pack("H*", $ENV{"APISIX_GO_FIXTURE_CERTIFICATE_HEX"}); close($certificate_file) or die "certificate close: $!\n"; open(my $key_file, ">", $private_key) or die "private key: $!\n"; binmode $key_file; print {$key_file} pack("H*", $ENV{"APISIX_GO_FIXTURE_PRIVATE_KEY_HEX"}); close($key_file) or die "private key close: $!\n"; chmod(0600, $certificate, $private_key) or die "chmod TLS credentials: $!\n"; die "openssl executable is unavailable\n" unless -x $openssl; open(my $openssl_error, ">", $openssl_error_path) or die "openssl error log: $!\n"; my $openssl_pid = IPC::Open3::open3(my $openssl_input, my $openssl_output, $openssl_error, $openssl, "s_server", "-quiet", "-naccept", "1", "-accept", "127.0.0.1:` + strconv.Itoa(port) + `", "-cert", $certificate, "-key", $private_key); binmode $openssl_input; binmode $openssl_output; ` +
		`sub stop_openssl { my ($pid) = @_; kill("TERM", $pid); waitpid($pid, 0); } my $bound = 0; for (1..60) { my $probe = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => ` + strconv.Itoa(port) + `, Proto => "tcp", Listen => 1, ReuseAddr => 0); if ($probe) { close($probe); select(undef, undef, undef, 0.05); next; } my $result = waitpid($openssl_pid, 1); die "openssl TLS listener exited before ready; see $openssl_error_path\n" if $result == $openssl_pid; select(undef, undef, undef, 0.05); $result = waitpid($openssl_pid, 1); die "openssl TLS listener exited before ready; see $openssl_error_path\n" if $result == $openssl_pid; $bound = 1; last; } unless ($bound) { stop_openssl($openssl_pid); die "openssl TLS listener ready timeout\n"; } ` +
		differentialOracleReadyProgram() +
		`my $select = IO::Select->new($openssl_output); my $frame = ""; my $timed_out = 0; local $SIG{"ALRM"} = sub { $timed_out = 1; }; alarm(8); while (index($frame, "\n") < 0 && !$timed_out) { my @readable = $select->can_read(0.1); if (!@readable) { my $result = waitpid($openssl_pid, 1); die "truncated TLS frame; see $openssl_error_path\n" if $result == $openssl_pid; next; } my $count = sysread($openssl_output, my $buffer, 8192); unless (defined $count) { stop_openssl($openssl_pid); die "read openssl TLS frame: $!\n"; } if ($count == 0) { stop_openssl($openssl_pid); die "truncated TLS frame\n"; } $frame .= $buffer; if (length($frame) > ` + strconv.Itoa(differentialRawMaxFramePayload) + `) { stop_openssl($openssl_pid); die "TLS frame too large\n"; } } alarm(0); if ($timed_out) { stop_openssl($openssl_pid); die "TLS frame timeout\n"; } my $newline = index($frame, "\n"); if ($newline != length($frame) - 1) { stop_openssl($openssl_pid); die "extra TLS frame data\n"; } my @extra = $select->can_read(0.15); if (@extra) { my $count = sysread($openssl_output, my $buffer, 8192); if (!defined $count) { stop_openssl($openssl_pid); die "read extra TLS frame data: $!\n"; } if ($count > 0) { stop_openssl($openssl_pid); die "extra TLS frame data\n"; } } write_record("TCP", $frame); stop_openssl($openssl_pid);`
}

func differentialOracleT1KV2FixtureProgram() string {
	return renderDifferentialOracleT1KV2FixtureProgram(
		differentialOracleFixturePort,
		differentialOracleFixtureReady,
		differentialOracleFixtureRecord,
	)
}

func renderDifferentialOracleT1KV2FixtureProgram(port int, ready, recordBase string) string {
	return `sub read_exact { my ($client, $length, $label) = @_; my $data = ""; while (length($data) < $length) { my $count = sysread($client, my $buffer, $length - length($data)); die "read $label: $!\n" unless defined $count; die "truncated $label\n" if $count == 0; $data .= $buffer; } return $data; } sub read_frame { my ($client) = @_; my $header = read_exact($client, 5, "T1K header"); my ($tag, $length) = unpack("CV", $header); die "T1K frame payload too large: $length\n" if $length > ` + strconv.Itoa(
		differentialT1KMaxFramePayload,
	) + `; my $payload = read_exact($client, $length, "T1K payload"); return ($tag, $payload); } my $ready = "` + ready + `"; my $record_base = "` + recordBase + `"; unlink $ready; unlink glob($record_base . ".*"); my $listener = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => ` + strconv.Itoa(
		port,
	) + `, Proto => "tcp", Listen => 16, ReuseAddr => 1) or die "listen: $!\n"; open(my $ready_file, ">", $ready) or die "ready: $!\n"; print {$ready_file} "ready\n"; close($ready_file); my $index = 0; while (1) { my $client = $listener->accept() or die "accept: $!\n"; binmode $client; my ($head_tag, $head) = read_frame($client); die sprintf("T1K HEAD tag 0x%02x, want 0x41\n", $head_tag) unless $head_tag == 0x41; my ($next_tag, $next_payload) = read_frame($client); my $body = ""; if ($next_tag == 0x02) { $body = $next_payload; ($next_tag, $next_payload) = read_frame($client); } die sprintf("T1K VERSION tag 0x%02x, want 0x20\n", $next_tag) unless $next_tag == 0x20; die "T1K VERSION payload mismatch\n" unless $next_payload eq "Proto:2\n"; my ($extra_tag, $extra) = read_frame($client); die sprintf("T1K EXTRA tag 0x%02x, want 0x83\n", $extra_tag) unless $extra_tag == 0x83; die "T1K EXTRA payload is empty\n" unless length($extra) > 0; die "malformed embedded HTTP head\n" unless $head =~ /\A[A-Z]+\s+\S+\s+HTTP\/1\.[01]\r\n/ && $head =~ /\r\nHost:\s*[^\r\n]+/i && $head =~ /\r\n\r\n\z/; my ($content_length) = $head =~ /\r\nContent-Length:\s*(\d+)/i; $content_length = 0 unless defined $content_length; die "embedded HTTP Content-Length mismatch\n" unless $content_length == length($body); my $raw = $head . $body; my $record = $record_base . "." . $index; my $record_tmp = $record . ".tmp-" . $$; open(my $record_file, ">", $record_tmp) or die "record: $!\n"; binmode $record_file; print {$record_file} $raw; close($record_file) or die "record close: $!\n"; rename($record_tmp, $record) or die "record rename: $!\n"; my $response = pack("H*", $ENV{"APISIX_GO_FIXTURE_RESPONSE_HEX"}); my $offset = 0; while ($offset < length($response)) { my $count = syswrite($client, $response, length($response) - $offset, $offset); die "write: $!\n" unless defined $count; $offset += $count; } shutdown($client, 1); close($client); $index += 1; }`
}

func differentialCandidateFixtureCollectTimeout(fixture DifferentialFixture) time.Duration {
	return differentialFixtureCollectTimeout(fixture, 350*time.Millisecond)
}

func differentialOracleFixtureCollectTimeout(fixture DifferentialFixture) time.Duration {
	return differentialFixtureCollectTimeout(fixture, 3*time.Second)
}

func differentialFixtureCollectTimeout(
	fixture DifferentialFixture,
	fallback time.Duration,
) time.Duration {
	if fixture.CollectTimeoutMillis <= 0 {
		return fallback
	}
	return time.Duration(fixture.CollectTimeoutMillis) * time.Millisecond
}

func differentialFixtureRequestWindowQuiet(fixture DifferentialFixture) time.Duration {
	return time.Duration(fixture.RequestWindowQuietMillis) * time.Millisecond
}

func collectDifferentialOracleFixtures(
	child *differentialChild,
	expected int,
) ([]differentialCapturedRequest, error) {
	return collectDifferentialOracleFixturesWithTimeout(child, expected, 3*time.Second)
}

func collectDifferentialOracleFixturesWithTimeout(
	child *differentialChild,
	expected int,
	timeout time.Duration,
) ([]differentialCapturedRequest, error) {
	if expected < 0 {
		return nil, errors.New("oracle fixture expected call count must not be negative")
	}
	if timeout <= 0 {
		return nil, errors.New("oracle fixture collect timeout must be positive")
	}
	rawRequests := make([][]byte, 0, expected+1)
	for index := range expected {
		path := differentialOracleFixtureRecordPath(index)
		if err := waitDifferentialOracleFile(child, path, timeout); err != nil {
			return nil, fmt.Errorf("wait for oracle fixture request %d: %w", index, err)
		}
		raw, err := readDifferentialOracleFixtureRecord(child, path)
		if err != nil {
			return nil, err
		}
		rawRequests = append(rawRequests, raw)
	}
	time.Sleep(150 * time.Millisecond)
	extraPath := differentialOracleFixtureRecordPath(expected)
	extra, err := differentialOracleFileExists(child, extraPath)
	if err != nil {
		return nil, fmt.Errorf("check for extra oracle fixture request: %w", err)
	}
	if extra {
		raw, err := readDifferentialOracleFixtureRecord(child, extraPath)
		if err != nil {
			return nil, err
		}
		rawRequests = append(rawRequests, raw)
	}
	return parseDifferentialOracleFixtureRequests(rawRequests)
}

func beginDifferentialOracleRequestWindow(
	child *differentialChild,
	quiet time.Duration,
	timeout time.Duration,
) (int, error) {
	if quiet <= 0 {
		return 0, errors.New("oracle fixture request-window quiet duration must be positive")
	}
	if timeout <= quiet {
		return 0, errors.New("oracle fixture request-window timeout must exceed quiet duration")
	}
	deadline := time.Now().Add(timeout)
	next := 0
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		exists, err := differentialOracleFileExistsWithTimeout(
			child, differentialOracleFixtureRecordPath(next), remaining,
		)
		if err != nil {
			return 0, fmt.Errorf("check Oracle request-window record %d: %w", next, err)
		}
		if exists {
			next++
			stableSince = time.Now()
			continue
		}
		following, err := differentialOracleFileExistsWithTimeout(
			child, differentialOracleFixtureRecordPath(next+1), remaining,
		)
		if err != nil {
			return 0, fmt.Errorf("check Oracle request-window record %d: %w", next+1, err)
		}
		if following {
			rechecked, err := differentialOracleFileExistsWithTimeout(
				child, differentialOracleFixtureRecordPath(next), time.Until(deadline),
			)
			if err != nil {
				return 0, fmt.Errorf("recheck Oracle request-window record %d: %w", next, err)
			}
			if !rechecked {
				return 0, fmt.Errorf("Oracle fixture records are not contiguous at index %d", next)
			}
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= quiet {
			return next, nil
		}
		time.Sleep(min(differentialRequestWindowPollInterval(quiet), time.Until(deadline)))
	}
	return 0, fmt.Errorf(
		"Oracle fixture did not become quiet for %s within %s",
		quiet, timeout,
	)
}

func collectDifferentialOracleFixtureWindow(
	child *differentialChild,
	baseline int,
	expected int,
	timeout time.Duration,
	quiet time.Duration,
) ([]differentialCapturedRequest, error) {
	if baseline < 0 || expected < 0 {
		return nil, errors.New("oracle fixture request-window indexes must not be negative")
	}
	if timeout <= 0 || quiet <= 0 {
		return nil, errors.New("oracle fixture request-window collect timeout and quiet duration must be positive")
	}
	deadline := time.Now().Add(timeout)
	rawRequests := make([][]byte, 0, expected)
	for offset := range expected {
		index := baseline + offset
		if err := waitDifferentialOracleFixtureRecordContiguous(child, index, deadline); err != nil {
			return nil, err
		}
		raw, err := readDifferentialOracleFixtureRecord(
			child, differentialOracleFixtureRecordPath(index),
		)
		if err != nil {
			return nil, err
		}
		rawRequests = append(rawRequests, raw)
	}

	extraIndex := baseline + expected
	quietDeadline := time.Now().Add(quiet)
	for time.Now().Before(quietDeadline) {
		remaining := time.Until(quietDeadline)
		exists, err := differentialOracleFileExistsWithTimeout(
			child, differentialOracleFixtureRecordPath(extraIndex), remaining,
		)
		if err != nil {
			return nil, fmt.Errorf("check for extra Oracle request-window call: %w", err)
		}
		if exists {
			return nil, fmt.Errorf(
				"Oracle fixture received more than %d request-window calls, want exactly %d",
				expected, expected,
			)
		}
		if !time.Now().Before(quietDeadline) {
			break
		}
		following, err := differentialOracleFileExistsWithTimeout(
			child, differentialOracleFixtureRecordPath(extraIndex+1), time.Until(quietDeadline),
		)
		if err != nil {
			return nil, fmt.Errorf("check Oracle request-window record continuity: %w", err)
		}
		if following {
			rechecked, err := differentialOracleFileExistsWithTimeout(
				child, differentialOracleFixtureRecordPath(extraIndex), time.Until(quietDeadline),
			)
			if err != nil {
				return nil, fmt.Errorf("recheck extra Oracle request-window call: %w", err)
			}
			if rechecked {
				return nil, fmt.Errorf(
					"Oracle fixture received more than %d request-window calls, want exactly %d",
					expected, expected,
				)
			}
			return nil, fmt.Errorf("Oracle fixture records are not contiguous at index %d", extraIndex)
		}
		time.Sleep(min(differentialRequestWindowPollInterval(quiet), time.Until(quietDeadline)))
	}
	return parseDifferentialOracleFixtureRequests(rawRequests)
}

func waitDifferentialOracleFixtureRecordContiguous(
	child *differentialChild,
	index int,
	deadline time.Time,
) error {
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		exists, err := differentialOracleFileExistsWithTimeout(
			child, differentialOracleFixtureRecordPath(index), remaining,
		)
		if err != nil {
			return fmt.Errorf("check Oracle request-window record %d: %w", index, err)
		}
		if exists {
			return nil
		}
		following, err := differentialOracleFileExistsWithTimeout(
			child, differentialOracleFixtureRecordPath(index+1), remaining,
		)
		if err != nil {
			return fmt.Errorf("check Oracle request-window record %d: %w", index+1, err)
		}
		if following {
			rechecked, err := differentialOracleFileExistsWithTimeout(
				child, differentialOracleFixtureRecordPath(index), time.Until(deadline),
			)
			if err != nil {
				return fmt.Errorf("recheck Oracle request-window record %d: %w", index, err)
			}
			if rechecked {
				return nil
			}
			return fmt.Errorf("Oracle fixture records are not contiguous at index %d", index)
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
	return fmt.Errorf("Oracle fixture request-window record %d did not appear within timeout", index)
}

func differentialOracleFixtureRecordPath(index int) string {
	return differentialOracleFixtureRecord + "." + strconv.Itoa(index)
}

func readDifferentialOracleFixtureRecord(child *differentialChild, path string) ([]byte, error) {
	output, err := runDifferentialPodmanCommand(
		child.runtime,
		differentialPodmanTimeout,
		nil,
		nil,
		"exec",
		child.name,
		"perl",
		"-e",
		`open(my $file, "<", $ARGV[0]) or die "open: $!\n"; binmode $file; binmode STDOUT; while (1) { my $count = sysread($file, my $buffer, 8192); die "read: $!\n" unless defined $count; last if $count == 0; print STDOUT $buffer; }`,
		path,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"read oracle fixture request: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return output, nil
}

func differentialOracleFileExists(child *differentialChild, path string) (bool, error) {
	return differentialOracleFileExistsWithTimeout(child, path, differentialPodmanTimeout)
}

func differentialOracleFileExistsWithTimeout(
	child *differentialChild,
	path string,
	timeout time.Duration,
) (bool, error) {
	if timeout <= 0 {
		return false, errors.New("Oracle fixture file check timeout must be positive")
	}
	if timeout < 250*time.Millisecond {
		timeout = 250 * time.Millisecond
	}
	_, err := runDifferentialPodmanCommand(
		child.runtime,
		min(timeout, differentialPodmanTimeout),
		nil,
		nil,
		"exec",
		child.name,
		"perl",
		"-e",
		`exit((-e $ARGV[0]) ? 0 : 1)`,
		path,
	)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func waitDifferentialOracleFile(
	child *differentialChild,
	path string,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		_, err := runDifferentialPodmanCommand(
			child.runtime,
			remaining,
			nil,
			nil,
			"exec",
			child.name,
			"perl",
			"-e",
			`exit((-e $ARGV[0]) ? 0 : 1)`,
			path,
		)
		if err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("file %s did not appear in container %s: %w", path, child.name, lastErr)
}

func parseDifferentialOracleFixtureRequest(raw []byte) (differentialCapturedRequest, error) {
	if bytes.HasPrefix(raw, []byte(differentialRawRecordPrefix)) {
		return parseDifferentialOracleRawFixtureRecord(raw)
	}
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return differentialCapturedRequest{}, fmt.Errorf("parse oracle fixture request: %w", err)
	}
	body, readErr := io.ReadAll(request.Body)
	closeErr := request.Body.Close()
	if readErr != nil {
		return differentialCapturedRequest{}, fmt.Errorf(
			"read oracle fixture request body: %w",
			readErr,
		)
	}
	if closeErr != nil {
		return differentialCapturedRequest{}, fmt.Errorf(
			"close oracle fixture request body: %w",
			closeErr,
		)
	}
	return differentialCapturedRequest{
		Method:  request.Method,
		Path:    request.URL.RequestURI(),
		Host:    request.Host,
		Headers: request.Header.Clone(),
		Body:    string(body),
	}, nil
}

func encodeDifferentialOracleRawFixtureRecord(method string, body []byte) []byte {
	header := fmt.Sprintf("%s%s %d\n", differentialRawRecordPrefix, method, len(body))
	return append([]byte(header), body...)
}

func parseDifferentialOracleRawFixtureRecord(raw []byte) (differentialCapturedRequest, error) {
	header, body, ok := bytes.Cut(raw, []byte{'\n'})
	if !ok {
		return differentialCapturedRequest{}, errors.New("parse oracle raw fixture record: missing header terminator")
	}
	fields := strings.Fields(string(header))
	if len(fields) != 3 || fields[0]+" " != differentialRawRecordPrefix ||
		(fields[1] != "TCP" && fields[1] != "UDP") {
		return differentialCapturedRequest{}, errors.New("parse oracle raw fixture record: invalid header")
	}
	length, err := strconv.Atoi(fields[2])
	if err != nil || length < 0 || length > differentialRawMaxFramePayload {
		return differentialCapturedRequest{}, errors.New("parse oracle raw fixture record: invalid body length")
	}
	if len(body) != length {
		return differentialCapturedRequest{}, fmt.Errorf(
			"parse oracle raw fixture record: body length %d, want %d", len(body), length,
		)
	}
	return differentialCapturedRequest{Method: fields[1], Body: string(body)}, nil
}

func parseDifferentialOracleFixtureRequests(
	rawRequests [][]byte,
) ([]differentialCapturedRequest, error) {
	captured := make([]differentialCapturedRequest, 0, len(rawRequests))
	for index, raw := range rawRequests {
		request, err := parseDifferentialOracleFixtureRequest(raw)
		if err != nil {
			return nil, fmt.Errorf("parse oracle fixture request %d: %w", index, err)
		}
		captured = append(captured, request)
	}
	return captured, nil
}

func renderDifferentialOracleRequest(spec DifferentialCase) ([]byte, *http.Request, error) {
	request, err := http.NewRequest(
		spec.Request.Method,
		"http://127.0.0.1:9080"+spec.Request.Path,
		strings.NewReader(spec.Request.Body),
	)
	if err != nil {
		return nil, nil, err
	}
	request.Host = spec.Request.Host
	request.Close = true
	for name, value := range spec.Request.Headers {
		request.Header.Set(name, value)
	}
	var raw bytes.Buffer
	if err := request.Write(&raw); err != nil {
		return nil, nil, err
	}
	return raw.Bytes(), request, nil
}

func parseDifferentialOracleResponse(request *http.Request, raw []byte) (*http.Response, error) {
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), request)
	if err != nil {
		const previewLimit = 512
		preview := raw
		if len(preview) > previewLimit {
			preview = preview[:previewLimit]
		}
		return nil, fmt.Errorf(
			"parse oracle HTTP response (%d bytes, preview %q): %w",
			len(raw),
			preview,
			err,
		)
	}
	return response, nil
}

func executeDifferentialOracleRequest(
	child *differentialChild,
	spec DifferentialCase,
) (*http.Response, error) {
	return executeDifferentialOracleRequestWithTimeout(child, spec, differentialPodmanTimeout)
}

func executeDifferentialOracleRequestWithTimeout(
	child *differentialChild,
	spec DifferentialCase,
	timeout time.Duration,
) (*http.Response, error) {
	raw, request, err := renderDifferentialOracleRequest(spec)
	if err != nil {
		return nil, fmt.Errorf("render oracle HTTP request: %w", err)
	}
	port, err := differentialOracleRequestPort(spec.Request)
	if err != nil {
		return nil, err
	}
	output, err := runDifferentialPodmanCommand(
		child.runtime,
		timeout,
		bytes.NewReader(raw),
		nil,
		"exec",
		"--interactive",
		child.name,
		"perl",
		"-MIO::Socket::INET",
		"-e",
		`my $socket = IO::Socket::INET->new(PeerAddr => "127.0.0.1", PeerPort => `+
			strconv.Itoa(port)+`, Proto => "tcp", Timeout => 1) or die "connect: $!\n"; `+
			`binmode STDIN; binmode STDOUT; local $/; my $request = <STDIN>; my $offset = 0; `+
			`while ($offset < length($request)) { my $count = syswrite($socket, $request, length($request) - $offset, $offset); die "write: $!\n" unless defined $count; $offset += $count; } `+
			`while (1) { my $count = sysread($socket, my $buffer, 8192); die "read: $!\n" unless defined $count; last if $count == 0; print STDOUT $buffer; }`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"execute oracle HTTP request: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return parseDifferentialOracleResponse(request, output)
}

func differentialOracleRequestPort(request DifferentialRequest) (int, error) {
	return differentialTargetPort(
		request.Target,
		differentialOracleDataPort,
		differentialOracleControlPort,
	)
}

func observeDifferentialResponse(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	response *http.Response,
	upstreamAddress string,
) (DifferentialObservation, error) {
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return DifferentialObservation{}, fmt.Errorf("read response: %w", readErr)
	}
	if closeErr != nil {
		return DifferentialObservation{}, fmt.Errorf("close response: %w", closeErr)
	}
	received, err := fixture.collectWithTimeout(
		spec.Fixture.ExpectedCalls,
		differentialCandidateFixtureCollectTimeout(spec.Fixture),
	)
	if err != nil {
		return DifferentialObservation{}, err
	}
	observation := DifferentialObservation{
		Status:           response.StatusCode,
		Headers:          differentialHTTPHeaders(response.Header),
		Body:             string(body),
		RetryCount:       max(0, len(received)-1),
		Host:             spec.Request.Host,
		SNI:              spec.Request.SNI,
		SecurityDecision: spec.SecurityDecision,
		Upstream:         DifferentialUpstreamObservation{Received: len(received) > 0},
	}
	applyDifferentialFixtureObservation(&observation, spec.Fixture, received, upstreamAddress)
	return observation, nil
}

func applyDifferentialFixtureObservation(
	observation *DifferentialObservation,
	fixture DifferentialFixture,
	received []differentialCapturedRequest,
	upstreamAddress string,
) {
	if fixture.CaptureAllCalls {
		applyDifferentialSequenceFixtureObservation(
			observation,
			fixture,
			received,
			upstreamAddress,
		)
		return
	}
	observation.RetryCount = max(0, len(received)-1)
	observation.Upstream = DifferentialUpstreamObservation{Received: len(received) > 0}
	if len(received) > 0 {
		last := received[len(received)-1]
		observation.UpstreamFixture = fixture.Name
		observation.UpstreamAddress = upstreamAddress
		observation.Upstream = differentialUpstreamObservation(fixture, last)
		observation.RouteObserver = differentialRouteObserver(last.Headers)
	}
}

func applyDifferentialSequenceFixtureObservation(
	observation *DifferentialObservation,
	fixture DifferentialFixture,
	received []differentialCapturedRequest,
	upstreamAddress string,
) {
	observation.RetryCount = 0
	observation.Upstream = DifferentialUpstreamObservation{Received: len(received) > 0}
	observation.UpstreamCalls = make([]DifferentialUpstreamObservation, 0, len(received))
	for _, request := range received {
		observation.UpstreamCalls = append(
			observation.UpstreamCalls,
			differentialUpstreamObservation(fixture, request),
		)
	}
	if len(received) == 0 {
		return
	}
	last := received[len(received)-1]
	observation.UpstreamFixture = fixture.Name
	observation.UpstreamAddress = upstreamAddress
	observation.Upstream = observation.UpstreamCalls[len(observation.UpstreamCalls)-1]
	observation.RouteObserver = differentialRouteObserver(last.Headers)
}

func differentialUpstreamObservation(
	fixture DifferentialFixture,
	request differentialCapturedRequest,
) DifferentialUpstreamObservation {
	return DifferentialUpstreamObservation{
		Received: true,
		Fixture:  fixture.Name,
		Method:   request.Method,
		Path:     request.Path,
		Host:     request.Host,
		Headers:  differentialSemanticUpstreamHeaders(request.Headers, fixture.SemanticHeaders...),
		Body:     request.Body,
	}
}

func clearProxyEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, remove := differentialProxyEnvironmentNames[name]; remove {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func differentialHTTPHeaders(headers http.Header) map[string][]string {
	result := make(map[string][]string, len(headers))
	for name, values := range headers {
		result[name] = append([]string(nil), values...)
	}
	return result
}

func differentialSemanticUpstreamHeaders(headers http.Header, extraNames ...string) map[string][]string {
	result := make(map[string][]string)
	for name, values := range headers {
		retain := strings.EqualFold(name, "Authorization") ||
			strings.EqualFold(name, "X-Consumer-Username") ||
			strings.EqualFold(name, "X-Consumer-Department") ||
			strings.EqualFold(name, "X-Consumer-Company") ||
			strings.EqualFold(name, "X-Consumer-Role") ||
			strings.EqualFold(name, "X-Route-Observer")
		for _, extraName := range extraNames {
			retain = retain || strings.EqualFold(name, extraName)
		}
		if retain {
			result[name] = append([]string(nil), values...)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func differentialRouteObserver(headers http.Header) map[string][]string {
	result := make(map[string][]string)
	for name, values := range headers {
		if strings.EqualFold(name, "X-Route-Observer") {
			result[name] = append([]string(nil), values...)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

type differentialCapturedRequest struct {
	sequence uint64
	Method   string
	Path     string
	Host     string
	Headers  http.Header
	Body     string
}

type differentialFixtureServer struct {
	server       *httptest.Server
	listener     net.Listener
	udp          *net.UDPConn
	requests     chan differentialCapturedRequest
	errors       chan error
	response     DifferentialFixtureResponse
	fixture      string
	probeToken   string
	wire         string
	captureHTTP  bool
	activity     atomic.Uint64
	dropped      atomic.Uint64
	oracleSide   atomic.Bool
	serveWG      sync.WaitGroup
	connectionWG sync.WaitGroup
	closeOnce    sync.Once
}

func newDifferentialFixture(spec DifferentialFixture) (*differentialFixtureServer, error) {
	var lastProbeErr error
	for range differentialFixtureBindAttempts {
		fixture, err := startDifferentialFixture(spec)
		if err != nil {
			return nil, err
		}
		if err := probeDifferentialFixture(fixture); err == nil {
			return fixture, nil
		} else {
			lastProbeErr = err
			fixture.close()
		}
	}
	return nil, fmt.Errorf(
		"claim differential fixture loopback endpoint after %d attempts: %w",
		differentialFixtureBindAttempts,
		lastProbeErr,
	)
}

func startDifferentialFixture(spec DifferentialFixture) (*differentialFixtureServer, error) {
	if err := validateDifferentialFixtureRequestWindow(spec); err != nil {
		return nil, err
	}
	if _, err := differentialFixtureResponseDelay(spec.Response); err != nil {
		return nil, err
	}
	if err := validateDifferentialFixtureHTTPOriginContract(spec); err != nil {
		return nil, err
	}
	if spec.WireProtocol != "" && spec.Response.DelayMillis != 0 {
		return nil, fmt.Errorf(
			"differential fixture response delay_millis is unsupported for wire protocol %q",
			spec.WireProtocol,
		)
	}
	switch spec.WireProtocol {
	case "":
		return startDifferentialHTTPFixture(spec)
	case differentialFixtureWireHTTPTCP:
		return startDifferentialHTTPTCPFixture(spec)
	case differentialFixtureWireHTTPUDP:
		return startDifferentialHTTPUDPFixture(spec)
	case differentialFixtureWireTLSTCP:
		return startDifferentialTLSTCPFixture(spec)
	case differentialFixtureWireT1KV2:
		return startDifferentialT1KV2Fixture(spec)
	case differentialFixtureWireHTTPKafka:
		return startDifferentialHTTPKafkaFixture(spec)
	case differentialFixtureWireGRPCH2C:
		return startDifferentialGRPCH2CFixture(spec)
	case differentialFixtureWireSSEHTTP:
		return startDifferentialProxyBufferingSSEFixture(spec)
	case differentialFixtureWireHTTPDubboFastJSON:
		return startDifferentialHTTPDubboFixture(spec)
	case differentialFixtureWireMQTTCONNECT:
		return startDifferentialMQTTFixture(spec)
	case differentialFixtureWireHTTPRocketMQ:
		return startDifferentialHTTPRocketMQFixture(spec)
	case differentialFixtureWireDubboProxyHessian2:
		return startDifferentialDubboProxyFixture(spec)
	default:
		return nil, fmt.Errorf("unsupported differential fixture wire protocol %q", spec.WireProtocol)
	}
}

func newDifferentialRawFixture(
	spec DifferentialFixture,
	listener net.Listener,
) (*differentialFixtureServer, error) {
	probeToken, err := newDifferentialRunNonce()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("create differential fixture probe token: %w", err)
	}
	return &differentialFixtureServer{
		listener: listener, requests: make(chan differentialCapturedRequest, 16),
		errors: make(chan error, 16), response: spec.Response, fixture: spec.Name,
		probeToken: probeToken, wire: spec.WireProtocol,
		captureHTTP: differentialFixtureCapturesHTTPOrigin(spec),
	}, nil
}

func differentialFixtureCapturesHTTPOrigin(spec DifferentialFixture) bool {
	return !spec.OmitHTTPOriginCall
}

func validateDifferentialFixtureHTTPOriginContract(spec DifferentialFixture) error {
	if !spec.OmitHTTPOriginCall {
		return nil
	}
	if spec.WireProtocol != differentialFixtureWireHTTPTCP &&
		spec.WireProtocol != differentialFixtureWireHTTPUDP {
		return fmt.Errorf(
			"differential fixture omit_http_origin_call is unsupported for wire protocol %q",
			spec.WireProtocol,
		)
	}
	return nil
}

func startDifferentialHTTPTCPFixture(spec DifferentialFixture) (*differentialFixtureServer, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic HTTP/TCP fixture: %w", err)
	}
	fixture, err := newDifferentialRawFixture(spec, listener)
	if err != nil {
		return nil, err
	}
	fixture.serveWG.Add(1)
	go fixture.serveTCP(true)
	return fixture, nil
}

func startDifferentialHTTPUDPFixture(spec DifferentialFixture) (*differentialFixtureServer, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic HTTP/UDP origin: %w", err)
	}
	fixture, err := newDifferentialRawFixture(spec, listener)
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("listen deterministic HTTP/UDP sink: %w", err)
	}
	fixture.udp = udp
	fixture.serveWG.Add(2)
	go fixture.serveTCP(false)
	go fixture.serveUDP()
	return fixture, nil
}

func startDifferentialTLSTCPFixture(spec DifferentialFixture) (*differentialFixtureServer, error) {
	certificate, err := tls.X509KeyPair(
		[]byte(differentialFixtureCertificatePEM), []byte(differentialFixturePrivateKeyPEM),
	)
	if err != nil {
		return nil, fmt.Errorf("load deterministic TLS fixture certificate: %w", err)
	}
	listener, err := tls.Listen("tcp", "0.0.0.0:0", &tls.Config{ //nolint:gosec // test-only self-signed fixture
		Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return nil, fmt.Errorf("listen deterministic TLS/TCP fixture: %w", err)
	}
	fixture, err := newDifferentialRawFixture(spec, listener)
	if err != nil {
		return nil, err
	}
	fixture.serveWG.Add(1)
	go fixture.serveRawTCP()
	return fixture, nil
}

func (fixture *differentialFixtureServer) serveTCP(sniffRaw bool) {
	defer fixture.serveWG.Done()
	for {
		connection, err := fixture.listener.Accept()
		if err != nil {
			return
		}
		fixture.connectionWG.Go(func() {
			defer func() { _ = connection.Close() }()
			_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
			reader := bufio.NewReader(connection)
			if sniffRaw {
				first, peekErr := reader.Peek(1)
				if peekErr != nil {
					fixture.reportError(fmt.Errorf("sniff HTTP/TCP fixture connection: %w", peekErr))
					return
				}
				if first[0] == '{' || first[0] == '[' || first[0] == '<' {
					fixture.captureRawFrame(reader, "TCP")
					return
				}
			}
			fixture.captureHTTPRequest(reader, connection)
		})
	}
}

func (fixture *differentialFixtureServer) serveRawTCP() {
	defer fixture.serveWG.Done()
	for {
		connection, err := fixture.listener.Accept()
		if err != nil {
			return
		}
		fixture.connectionWG.Go(func() {
			defer func() { _ = connection.Close() }()
			_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
			if fixture.captureRawFrame(bufio.NewReader(connection), "TCP") {
				_, err = io.WriteString(connection, differentialRawProbePrefix+fixture.probeToken+"\n")
				if err != nil {
					fixture.reportError(fmt.Errorf("write TLS/TCP fixture probe response: %w", err))
				}
			}
		})
	}
}

func (fixture *differentialFixtureServer) serveUDP() {
	defer fixture.serveWG.Done()
	buffer := make([]byte, 64<<10)
	for {
		count, _, err := fixture.udp.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		fixture.captureRaw(string(buffer[:count]), "UDP")
	}
}

func (fixture *differentialFixtureServer) captureHTTPRequest(
	reader *bufio.Reader,
	writer io.Writer,
) {
	request, err := http.ReadRequest(reader)
	if err != nil {
		fixture.reportError(fmt.Errorf("read HTTP fixture request: %w", err))
		return
	}
	body, readErr := io.ReadAll(request.Body)
	closeErr := request.Body.Close()
	if readErr != nil || closeErr != nil {
		fixture.reportError(fmt.Errorf("read/close HTTP fixture request body: %v / %v", readErr, closeErr))
		return
	}
	isProbe := request.Method == http.MethodGet &&
		request.URL.RequestURI() == differentialFixtureProbePath &&
		request.Header.Get(differentialFixtureProbeHeader) == fixture.probeToken
	if isProbe {
		_, err = fmt.Fprintf(
			writer,
			"HTTP/1.1 204 No Content\r\n%s: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
			differentialFixtureProbeHeader, fixture.probeToken,
		)
	} else {
		if fixture.captureHTTP {
			fixture.capture(differentialCapturedRequest{
				Method: request.Method, Path: request.URL.RequestURI(), Host: request.Host,
				Headers: request.Header.Clone(), Body: string(body),
			})
		}
		response, responseErr := renderDifferentialOracleFixtureResponse(fixture.response)
		if responseErr != nil {
			fixture.reportError(responseErr)
			return
		}
		_, err = writer.Write(response)
	}
	if err != nil {
		fixture.reportError(fmt.Errorf("write HTTP fixture response: %w", err))
	}
}

func (fixture *differentialFixtureServer) captureRawFrame(reader *bufio.Reader, method string) bool {
	body, err := readDifferentialRawFrame(reader)
	if err != nil {
		fixture.reportError(fmt.Errorf("read %s fixture frame: %w", method, err))
		return false
	}
	if string(body) == differentialRawProbePrefix+fixture.probeToken+"\n" {
		return true
	}
	fixture.captureRaw(string(body), method)
	return false
}

func (fixture *differentialFixtureServer) captureRaw(body, method string) {
	fixture.capture(differentialCapturedRequest{Method: method, Body: body})
}

func (fixture *differentialFixtureServer) capture(request differentialCapturedRequest) {
	request.sequence = fixture.activity.Add(1)
	select {
	case fixture.requests <- request:
	default:
		fixture.dropped.Add(1)
		fixture.reportError(fmt.Errorf("%s fixture request buffer is full", fixture.wire))
	}
}

func readDifferentialRawFrame(reader *bufio.Reader) ([]byte, error) {
	frame := make([]byte, 0, 1024)
	for len(frame) <= differentialRawMaxFramePayload {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		frame = append(frame, value)
		if value == '\n' {
			return frame, nil
		}
		trimmed := bytes.TrimSpace(frame)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
			return frame, nil
		}
	}
	return nil, fmt.Errorf("raw fixture frame exceeds %d bytes", differentialRawMaxFramePayload)
}

func startDifferentialHTTPFixture(spec DifferentialFixture) (*differentialFixtureServer, error) {
	probeToken, err := newDifferentialRunNonce()
	if err != nil {
		return nil, fmt.Errorf("create differential fixture probe token: %w", err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic fixture: %w", err)
	}
	fixture := &differentialFixtureServer{
		listener: listener, requests: make(chan differentialCapturedRequest, 16),
		response: spec.Response, fixture: spec.Name, probeToken: probeToken, wire: spec.WireProtocol,
	}
	server := httptest.NewUnstartedServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodGet &&
				request.URL.RequestURI() == differentialFixtureProbePath &&
				request.Header.Get(differentialFixtureProbeHeader) == fixture.probeToken {
				writer.Header().Set(differentialFixtureProbeHeader, fixture.probeToken)
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			body, _ := io.ReadAll(request.Body)
			sequence := fixture.activity.Add(1)
			captured := differentialCapturedRequest{
				sequence: sequence,
				Method:   request.Method, Path: request.URL.RequestURI(), Host: request.Host,
				Headers: request.Header.Clone(), Body: string(body),
			}
			select {
			case fixture.requests <- captured:
			default:
				fixture.dropped.Add(1)
			}
			if fixture.response.DelayMillis > 0 {
				time.Sleep(time.Duration(fixture.response.DelayMillis) * time.Millisecond)
			}
			for name, value := range fixture.response.Headers {
				writer.Header().Set(name, value)
			}
			status := fixture.response.Status
			if status == 0 {
				status = http.StatusOK
			}
			writer.WriteHeader(status)
			_, _ = io.WriteString(writer, fixture.response.Body)
		}),
	)
	server.Listener = listener
	server.Start()
	fixture.server = server
	return fixture, nil
}

func differentialFixtureResponseDelay(response DifferentialFixtureResponse) (time.Duration, error) {
	if response.DelayMillis < 0 || response.DelayMillis > differentialMaxFixtureDelayMillis {
		return 0, fmt.Errorf(
			"differential fixture response delay_millis = %d, want 0..%d",
			response.DelayMillis,
			differentialMaxFixtureDelayMillis,
		)
	}
	return time.Duration(response.DelayMillis) * time.Millisecond, nil
}

func startDifferentialT1KV2Fixture(spec DifferentialFixture) (*differentialFixtureServer, error) {
	probeToken, err := newDifferentialRunNonce()
	if err != nil {
		return nil, fmt.Errorf("create differential fixture probe token: %w", err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen deterministic T1K fixture: %w", err)
	}
	fixture := &differentialFixtureServer{
		listener:   listener,
		requests:   make(chan differentialCapturedRequest, 16),
		errors:     make(chan error, 16),
		response:   spec.Response,
		fixture:    spec.Name,
		probeToken: probeToken,
		wire:       spec.WireProtocol,
	}
	go fixture.serveT1KV2()
	return fixture, nil
}

func (fixture *differentialFixtureServer) serveT1KV2() {
	for {
		connection, err := fixture.listener.Accept()
		if err != nil {
			return
		}
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		captured, readErr := readDifferentialT1KV2Request(connection)
		if readErr != nil {
			_ = connection.Close()
			fixture.reportError(fmt.Errorf("read T1K v2 fixture request: %w", readErr))
			continue
		}
		isProbe := captured.Method == http.MethodGet &&
			captured.Path == differentialFixtureProbePath &&
			captured.Headers.Get(differentialFixtureProbeHeader) == fixture.probeToken
		if !isProbe {
			captured.sequence = fixture.activity.Add(1)
			select {
			case fixture.requests <- captured:
			default:
				fixture.dropped.Add(1)
				fixture.reportError(errors.New("T1K v2 fixture request buffer is full"))
			}
		}
		if _, err := connection.Write(differentialT1KV2RejectResponse()); err != nil {
			fixture.reportError(fmt.Errorf("write T1K v2 fixture response: %w", err))
		}
		_ = connection.Close()
	}
}

func (fixture *differentialFixtureServer) reportError(err error) {
	select {
	case fixture.errors <- err:
	default:
	}
}

type differentialT1KFrame struct {
	tag     byte
	payload []byte
}

func readDifferentialT1KFrame(reader io.Reader) (differentialT1KFrame, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return differentialT1KFrame{}, fmt.Errorf("read frame header: %w", err)
	}
	length := binary.LittleEndian.Uint32(header[1:])
	if length > differentialT1KMaxFramePayload {
		return differentialT1KFrame{}, fmt.Errorf(
			"frame payload length %d exceeds %d",
			length,
			differentialT1KMaxFramePayload,
		)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return differentialT1KFrame{}, fmt.Errorf("read frame payload: %w", err)
	}
	return differentialT1KFrame{tag: header[0], payload: payload}, nil
}

func readDifferentialT1KV2Request(reader io.Reader) (differentialCapturedRequest, error) {
	head, err := readDifferentialT1KFrame(reader)
	if err != nil {
		return differentialCapturedRequest{}, fmt.Errorf("read HEAD frame: %w", err)
	}
	if head.tag != 0x41 {
		return differentialCapturedRequest{}, fmt.Errorf("HEAD frame tag = 0x%02x, want 0x41", head.tag)
	}

	next, err := readDifferentialT1KFrame(reader)
	if err != nil {
		return differentialCapturedRequest{}, fmt.Errorf("read BODY or VERSION frame: %w", err)
	}
	var body []byte
	if next.tag == 0x02 {
		body = next.payload
		next, err = readDifferentialT1KFrame(reader)
		if err != nil {
			return differentialCapturedRequest{}, fmt.Errorf("read VERSION frame: %w", err)
		}
	}
	if next.tag != 0x20 {
		return differentialCapturedRequest{}, fmt.Errorf("VERSION frame tag = 0x%02x, want 0x20", next.tag)
	}
	if string(next.payload) != "Proto:2\n" {
		return differentialCapturedRequest{}, fmt.Errorf("VERSION payload = %q, want Proto:2", next.payload)
	}
	extra, err := readDifferentialT1KFrame(reader)
	if err != nil {
		return differentialCapturedRequest{}, fmt.Errorf("read EXTRA frame: %w", err)
	}
	if extra.tag != 0x83 {
		return differentialCapturedRequest{}, fmt.Errorf("EXTRA frame tag = 0x%02x, want 0x83", extra.tag)
	}
	if len(extra.payload) == 0 {
		return differentialCapturedRequest{}, errors.New("EXTRA frame payload is empty")
	}

	rawRequest := make([]byte, 0, len(head.payload)+len(body))
	rawRequest = append(rawRequest, head.payload...)
	rawRequest = append(rawRequest, body...)
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(rawRequest)))
	if err != nil {
		return differentialCapturedRequest{}, fmt.Errorf("parse embedded HTTP request: %w", err)
	}
	embeddedBody, readErr := io.ReadAll(request.Body)
	closeErr := request.Body.Close()
	if readErr != nil {
		return differentialCapturedRequest{}, fmt.Errorf("read embedded HTTP request body: %w", readErr)
	}
	if closeErr != nil {
		return differentialCapturedRequest{}, fmt.Errorf("close embedded HTTP request body: %w", closeErr)
	}
	if len(request.TransferEncoding) != 0 {
		return differentialCapturedRequest{}, errors.New("embedded HTTP request must not use transfer encoding")
	}
	if !bytes.Equal(embeddedBody, body) {
		return differentialCapturedRequest{}, fmt.Errorf(
			"embedded HTTP Content-Length = %d, BODY length = %d",
			request.ContentLength,
			len(body),
		)
	}
	return differentialCapturedRequest{
		Method:  request.Method,
		Path:    request.URL.RequestURI(),
		Host:    request.Host,
		Headers: request.Header.Clone(),
		Body:    string(embeddedBody),
	}, nil
}

func differentialT1KV2RejectResponse() []byte {
	// APISIX 3.17 source 9ef2ecab, t/lib/chaitin_waf_server.lua reject().
	frames := []differentialT1KFrame{
		{tag: 0x41, payload: []byte("?")},
		{tag: 0x02, payload: []byte("403")},
		{tag: 0x25, payload: []byte(`{"event_id":"b3c6ce574dc24f09a01f634a39dca83b","request_hit_whitelist":false}`)},
		{
			tag:     0x23,
			payload: []byte("Set-Cookie:sl-session=ulgbPfMSuWRNsi/u7Aj9aA==; Domain=; Path=/; Max-Age=86400\n"),
		},
		{tag: 0xa4, payload: []byte("<!-- event_id: b3c6ce574dc24f09a01f634a39dca83b -->")},
	}
	var response bytes.Buffer
	for _, frame := range frames {
		header := []byte{frame.tag, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(header[1:], uint32(len(frame.payload)))
		_, _ = response.Write(header)
		_, _ = response.Write(frame.payload)
	}
	return response.Bytes()
}

func probeDifferentialFixture(fixture *differentialFixtureServer) error {
	switch fixture.wire {
	case differentialFixtureWireMQTTCONNECT:
		return probeDifferentialMQTTFixture(fixture)
	case differentialFixtureWireT1KV2:
		return probeDifferentialT1KV2Fixture(fixture.port(), fixture.probeToken)
	case differentialFixtureWireTLSTCP:
		return probeDifferentialTLSTCPFixture(fixture.port(), fixture.probeToken)
	case differentialFixtureWireGRPCH2C:
		return probeDifferentialGRPCH2CFixture(fixture)
	default:
		return probeDifferentialFixtureLoopback(fixture.port(), fixture.probeToken)
	}
}

func probeDifferentialTLSTCPFixture(port int, token string) error {
	if port <= 0 || token == "" {
		return errors.New("differential TLS/TCP fixture probe requires a port and token")
	}
	connection, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 500 * time.Millisecond},
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		&tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		}, //nolint:gosec // ownership probe for a self-signed test fixture
	)
	if err != nil {
		return fmt.Errorf("dial differential TLS/TCP fixture probe: %w", err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(500 * time.Millisecond))
	want := differentialRawProbePrefix + token + "\n"
	if _, err := io.WriteString(connection, want); err != nil {
		return fmt.Errorf("write differential TLS/TCP fixture probe: %w", err)
	}
	response := make([]byte, len(want))
	if _, err := io.ReadFull(connection, response); err != nil {
		return fmt.Errorf("read differential TLS/TCP fixture probe: %w", err)
	}
	if string(response) != want {
		return fmt.Errorf("loopback endpoint 127.0.0.1:%d is not the TLS/TCP fixture", port)
	}
	return nil
}

func probeDifferentialT1KV2Fixture(port int, token string) error {
	if port <= 0 || token == "" {
		return errors.New("differential T1K fixture probe requires a port and token")
	}
	connection, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		500*time.Millisecond,
	)
	if err != nil {
		return fmt.Errorf("dial differential T1K fixture probe: %w", err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(500 * time.Millisecond))
	head := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: fixture-probe\r\n%s: %s\r\n\r\n",
		differentialFixtureProbePath,
		differentialFixtureProbeHeader,
		token,
	)
	for _, frame := range []differentialT1KFrame{
		{tag: 0x41, payload: []byte(head)},
		{tag: 0x20, payload: []byte("Proto:2\n")},
		{tag: 0x83, payload: []byte("Probe:fixture-owner\n")},
	} {
		header := []byte{frame.tag, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(header[1:], uint32(len(frame.payload)))
		if _, err := connection.Write(append(header, frame.payload...)); err != nil {
			return fmt.Errorf("write differential T1K fixture probe: %w", err)
		}
	}
	want := differentialT1KV2RejectResponse()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(connection, got); err != nil {
		return fmt.Errorf("read differential T1K fixture probe: %w", err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("loopback endpoint 127.0.0.1:%d is not the T1K fixture", port)
	}
	return nil
}

func probeDifferentialFixtureLoopback(port int, token string) error {
	if port <= 0 || token == "" {
		return errors.New("differential fixture probe requires a port and token")
	}
	request, err := http.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:"+strconv.Itoa(port)+differentialFixtureProbePath,
		nil,
	)
	if err != nil {
		return fmt.Errorf("create differential fixture probe: %w", err)
	}
	request.Header.Set(differentialFixtureProbeHeader, token)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: transport}
	response, err := client.Do(request)
	if err != nil {
		transport.CloseIdleConnections()
		return fmt.Errorf("request differential fixture probe: %w", err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	transport.CloseIdleConnections()
	if readErr != nil {
		return fmt.Errorf("read differential fixture probe: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close differential fixture probe: %w", closeErr)
	}
	if response.StatusCode != http.StatusNoContent ||
		response.Header.Get(differentialFixtureProbeHeader) != token {
		return fmt.Errorf(
			"loopback endpoint 127.0.0.1:%d is not owned by the differential fixture",
			port,
		)
	}
	return nil
}

func (fixture *differentialFixtureServer) port() int {
	_, port, _ := net.SplitHostPort(fixture.listener.Addr().String())
	value, _ := strconv.Atoi(port)
	return value
}

func (fixture *differentialFixtureServer) reset() {
	for {
		select {
		case <-fixture.requests:
		default:
			for {
				select {
				case <-fixture.errors:
				default:
					return
				}
			}
		}
	}
}

func (fixture *differentialFixtureServer) resetForSide(oracle bool) {
	fixture.reset()
	fixture.oracleSide.Store(oracle)
}

type differentialFixtureWindowBaseline struct {
	sequence uint64
	dropped  uint64
}

func (fixture *differentialFixtureServer) beginRequestWindow(
	quiet time.Duration,
	timeout time.Duration,
) (differentialFixtureWindowBaseline, error) {
	if quiet <= 0 {
		return differentialFixtureWindowBaseline{}, errors.New("fixture request-window quiet duration must be positive")
	}
	if timeout <= quiet {
		return differentialFixtureWindowBaseline{}, errors.New(
			"fixture request-window timeout must exceed quiet duration",
		)
	}
	poll := differentialRequestWindowPollInterval(quiet)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	stableSequence := fixture.activity.Load()
	stableSince := time.Now()

	for {
		select {
		case request := <-fixture.requests:
			if request.sequence > stableSequence {
				stableSequence = request.sequence
			}
			stableSince = time.Now()
		case err := <-fixture.errors:
			return differentialFixtureWindowBaseline{}, err
		case now := <-ticker.C:
			current := fixture.activity.Load()
			if current != stableSequence {
				stableSequence = current
				stableSince = now
				continue
			}
			if now.Sub(stableSince) < quiet {
				continue
			}
			for {
				select {
				case request := <-fixture.requests:
					if request.sequence > stableSequence {
						stableSequence = request.sequence
					}
					stableSince = time.Now()
				case err := <-fixture.errors:
					return differentialFixtureWindowBaseline{}, err
				default:
					current = fixture.activity.Load()
					if current != stableSequence {
						stableSequence = current
						stableSince = time.Now()
						break
					}
					return differentialFixtureWindowBaseline{
						sequence: current,
						dropped:  fixture.dropped.Load(),
					}, nil
				}
				if time.Since(stableSince) < quiet {
					break
				}
			}
		case <-deadline.C:
			return differentialFixtureWindowBaseline{}, fmt.Errorf(
				"fixture %s did not become quiet for %s within %s",
				fixture.fixture, quiet, timeout,
			)
		}
	}
}

func (fixture *differentialFixtureServer) collectRequestWindow(
	baseline differentialFixtureWindowBaseline,
	expected int,
	timeout time.Duration,
	quiet time.Duration,
) ([]differentialCapturedRequest, error) {
	if expected < 0 {
		return nil, errors.New("fixture expected call count must not be negative")
	}
	if timeout <= 0 || quiet <= 0 {
		return nil, errors.New("fixture request-window collect timeout and quiet duration must be positive")
	}
	requests := make([]differentialCapturedRequest, 0, expected+1)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(differentialRequestWindowPollInterval(quiet))
	defer ticker.Stop()

	for len(requests) < expected {
		select {
		case request := <-fixture.requests:
			if request.sequence <= baseline.sequence {
				continue
			}
			requests = append(requests, request)
		case err := <-fixture.errors:
			return requests, err
		case <-ticker.C:
			if fixture.dropped.Load() != baseline.dropped {
				return requests, fmt.Errorf("fixture %s dropped a request-window call", fixture.fixture)
			}
		case <-deadline.C:
			if fixture.dropped.Load() != baseline.dropped {
				return requests, fmt.Errorf("fixture %s dropped a request-window call", fixture.fixture)
			}
			return requests, fmt.Errorf(
				"fixture %s received %d request-window calls, want %d",
				fixture.fixture, len(requests), expected,
			)
		}
	}

	quietTimer := time.NewTimer(quiet)
	defer quietTimer.Stop()
	for {
		select {
		case request := <-fixture.requests:
			if request.sequence <= baseline.sequence {
				continue
			}
			requests = append(requests, request)
			return requests, fmt.Errorf(
				"fixture %s received %d request-window calls, want exactly %d",
				fixture.fixture, len(requests), expected,
			)
		case err := <-fixture.errors:
			return requests, err
		case <-ticker.C:
			if fixture.dropped.Load() != baseline.dropped {
				return requests, fmt.Errorf("fixture %s dropped a request-window call", fixture.fixture)
			}
		case <-quietTimer.C:
			if fixture.dropped.Load() != baseline.dropped {
				return requests, fmt.Errorf("fixture %s dropped a request-window call", fixture.fixture)
			}
			return requests, nil
		}
	}
}

func differentialRequestWindowPollInterval(quiet time.Duration) time.Duration {
	poll := quiet / 4
	if poll < time.Millisecond {
		return time.Millisecond
	}
	if poll > 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	return poll
}

func (fixture *differentialFixtureServer) collect(
	expected int,
) ([]differentialCapturedRequest, error) {
	return fixture.collectWithTimeout(expected, 350*time.Millisecond)
}

func (fixture *differentialFixtureServer) collectWithTimeout(
	expected int,
	timeout time.Duration,
) ([]differentialCapturedRequest, error) {
	if expected < 0 {
		return nil, errors.New("fixture expected call count must not be negative")
	}
	if timeout <= 0 {
		return nil, errors.New("fixture collect timeout must be positive")
	}
	requests := make([]differentialCapturedRequest, 0, expected+1)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(requests) < expected {
		select {
		case request := <-fixture.requests:
			requests = append(requests, request)
		case err := <-fixture.errors:
			return requests, err
		case <-deadline.C:
			return requests, fmt.Errorf(
				"fixture %s received %d calls, want %d",
				fixture.fixture,
				len(requests),
				expected,
			)
		}
	}
	quiet := time.NewTimer(150 * time.Millisecond)
	defer quiet.Stop()
	for {
		select {
		case request := <-fixture.requests:
			requests = append(requests, request)
			if len(requests) > expected {
				return requests, fmt.Errorf(
					"fixture %s received %d calls, want exactly %d",
					fixture.fixture,
					len(requests),
					expected,
				)
			}
		case err := <-fixture.errors:
			return requests, err
		case <-quiet.C:
			return requests, nil
		}
	}
}

func (fixture *differentialFixtureServer) close() {
	fixture.closeOnce.Do(func() {
		if fixture.server != nil {
			fixture.server.Close()
			return
		}
		if fixture.listener != nil {
			_ = fixture.listener.Close()
		}
		if fixture.udp != nil {
			_ = fixture.udp.Close()
		}
		fixture.serveWG.Wait()
		fixture.connectionWG.Wait()
	})
}
