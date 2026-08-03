# Standalone Plugin Integration Tests Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. The repository forbids subagents unless the user explicitly authorizes them, so execution stays inline.

**Goal:** Build a standalone-mode integration-test runner under `t/plugin`, account for every upstream `TEST` block in `redirect.t`, `proxy-rewrite.t`, and `response-rewrite.t`, fix confirmed Go parity bugs exposed by the executable cases, and deliver one verified PR.

**Architecture:** Pin Apache APISIX commit `c3d7d5ec69774121f53d2e20d29d09c816795dd7` as the canonical source. Each local YAML manifest is a strict derived representation whose source-number coverage is mechanically checked. Each executable case runs the real `cmd.Execute()` path in a fresh child process and temporary standalone working directory, with a local HTTP or HTTPS fixture upstream.

**Tech Stack:** Go 1.26.4, `go.yaml.in/yaml/v3`, Go `testing`, `httptest`, `os/exec`, Cobra command bootstrap, standalone YAML configuration, bbolt, Make, GitHub CLI.

## Global Constraints

- Work in `/Users/wklken/workspace/tx/wklken/apisix-go` on branch `codex/plugin-integration-tests`, based on `origin/master` at `54549447b60d0956a9a69bb39e926f13b4471f7c`.
- Use the current checkout; project `AGENTS.md` forbids `using-git-worktrees` for this medium/bounded-large task.
- Run every Go command from Bash after `source .envrc`.
- Do not add dependencies or run `make dep`.
- Keep upstream `.t` files authoritative and local YAML derived; every source test number must be represented exactly once.
- Native frontend TLS and Admin API/etcd serialization checks remain visible skip cases with concrete reasons; no source block may disappear silently.
- Fix only behavior mismatches proven by the converted cases and focused regression tests.
- Format touched Go files with the repository formatter, inspect the diff, and retain only task-owned changes.
- Final gates are `make test-integration`, `go test ./... -count=1`, `make lint`, `make build`, and `git diff --check`.
- The generated `./apisix` binary and temporary databases must not be committed.

---

### Task 1: Define and validate the manifest contract

**Files:**
- Create: `t/plugin/case.go`
- Create: `t/plugin/case_test.go`

**Interfaces:**
- Consumes: YAML bytes for one plugin manifest.
- Produces: `loadManifest(name string, data []byte) (*Manifest, error)`, `(*Manifest).validate() error`, `Matcher.validate(kind matcherKind) error`, `Matcher.match(value string, present bool) error`, and `mergeMap(dst, src map[string]any)`.

- [ ] **Step 1: Write failing strict-decoding and coverage tests**

Create table tests covering these exact contracts:

```go
func TestLoadManifestRejectsUnknownField(t *testing.T)
func TestManifestRejectsMissingSourceNumber(t *testing.T)
func TestManifestRejectsDuplicateSourceNumber(t *testing.T)
func TestManifestAcceptsCompleteSourceCoverage(t *testing.T)
func TestManifestRequiresSkipReason(t *testing.T)
func TestManifestRequiresExecutableFields(t *testing.T)
```

Use a three-test manifest fixture where `[1, 3]` reports missing test 2,
`[1, 2, 2, 3]` reports duplicate test 2, and `[1, 2, 3]` succeeds.

- [ ] **Step 2: Write failing matcher and runtime-merge tests**

```go
func TestMatcherSupportsEqualsAndRegex(t *testing.T)
func TestHeaderMatcherSupportsAbsent(t *testing.T)
func TestMatcherRejectsAbsentForBody(t *testing.T)
func TestMatcherRejectsAmbiguousOperations(t *testing.T)
func TestMergeRuntimeConfigPreservesNestedOverrides(t *testing.T)
```

The merge test must prove that a case can set
`plugin_attr.redirect.https_port: 9443` while the runner-owned
`apisix.node_listen` and standalone deployment keys remain independently
injectable.

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
source .envrc && go test ./t/plugin -run 'Test(LoadManifest|Manifest|Matcher|HeaderMatcher|MergeRuntime)' -count=1
```

Expected: compilation fails because the manifest types and functions do not yet
exist.

- [ ] **Step 4: Implement the minimal manifest model**

Define these types with strict YAML tags:

```go
type Manifest struct {
    Source SourceSpec `yaml:"source"`
    Cases  []Case     `yaml:"cases"`
}

type SourceSpec struct {
    Repository string `yaml:"repository"`
    Commit     string `yaml:"commit"`
    File       string `yaml:"file"`
    Tests      int    `yaml:"tests"`
}

type Case struct {
    Name     string         `yaml:"name"`
    Source   CaseSource     `yaml:"source"`
    Skip     string         `yaml:"skip,omitempty"`
    Runtime  map[string]any `yaml:"runtime,omitempty"`
    Config   map[string]any `yaml:"config,omitempty"`
    Input    HTTPInput      `yaml:"input,omitempty"`
    Upstream *UpstreamSpec  `yaml:"upstream,omitempty"`
    Output   HTTPOutput     `yaml:"output,omitempty"`
}

type CaseSource struct {
    Tests []int `yaml:"tests"`
}

type HTTPInput struct {
    Method  string            `yaml:"method,omitempty"`
    Path    string            `yaml:"path"`
    Headers map[string]string `yaml:"headers,omitempty"`
    Body    string            `yaml:"body,omitempty"`
}

type UpstreamSpec struct {
    TLS     bool          `yaml:"tls,omitempty"`
    Expect  HTTPAssertion `yaml:"expect,omitempty"`
    Respond HTTPResponse  `yaml:"respond,omitempty"`
}

type HTTPOutput struct {
    Status  int                `yaml:"status"`
    Headers map[string]Matcher `yaml:"headers,omitempty"`
    Body    *Matcher           `yaml:"body,omitempty"`
    Logs    *Matcher           `yaml:"logs,omitempty"`
}

type Matcher struct {
    Equals  *string `yaml:"equals,omitempty"`
    Matches *string `yaml:"matches,omitempty"`
    Absent  *bool   `yaml:"absent,omitempty"`
}
```

Decode with `yaml.Decoder.KnownFields(true)`. Validate source metadata, case
names unique within each manifest, exact `1..Source.Tests` coverage, matcher exclusivity,
regular expressions, request paths, status range 100-599, fixture placeholders,
and skipped/executable field requirements.

- [ ] **Step 5: Run the focused tests and verify GREEN**

Run the command from Step 3. Expected: all manifest tests pass.

---

### Task 2: Build the isolated standalone process runner

**Files:**
- Create: `t/plugin/runner_test.go`
- Modify: `t/plugin/case_test.go`

**Interfaces:**
- Consumes: validated `Manifest` and `Case` values from Task 1.
- Produces: `TestPluginIntegration`, `TestAPISIXProcess`, `runCase`, `startFixture`, `startAPISIX`, `renderRuntimeConfig`, and `renderStandaloneConfig`.

- [ ] **Step 1: Write a failing inline harness smoke test**

Add `TestHarnessRunsStandaloneRoute` with one inline case using a route at
`/smoke`, `{{UPSTREAM_ADDR}}`, client input `GET /smoke`, fixture body `ok`, and
expected status/body `200`/`ok`. Add unit tests proving runtime rendering forces
a loopback listener and standalone YAML provider after merging case overrides.

- [ ] **Step 2: Run the smoke test and verify RED**

```bash
source .envrc && go test ./t/plugin -run 'Test(HarnessRunsStandaloneRoute|RenderRuntimeConfig)' -count=1 -v
```

Expected: compilation fails because runner functions do not exist.

- [ ] **Step 3: Implement helper-process startup and shutdown**

Use `os.Executable()` and start the current test binary with
`-test.run=^TestAPISIXProcess$`. Pass a private environment flag and replace the
child's `os.Args` with:

```go
[]string{"apisix", "-c", "conf/config.yaml"}
```

Then call `cmd.Execute()`. Give every case a `t.TempDir()` working directory,
write runtime/standalone config beneath `conf/`, capture stdout/stderr in a
temporary log file, poll a reserved loopback TCP port for at most five seconds,
and stop with `os.Interrupt`. After another five seconds, kill a child that did
not exit and report the timeout.

- [ ] **Step 4: Implement fixture and assertion behavior**

Start `httptest.NewServer` or `httptest.NewTLSServer` per case. Capture exactly
one upstream request in a buffered channel, including method, escaped request
URI, headers, and body. Apply fixture request assertions after the client
response. Disable redirect following in the client. Stop the child before
reading and matching logs.

- [ ] **Step 5: Run the smoke test and verify GREEN**

Run the Step 2 command. Expected: both harness tests pass and no database or
binary appears in the repository root.

---

### Task 3: Convert every redirect source block and fix confirmed parity gaps

**Files:**
- Create: `t/plugin/redirect.yaml`
- Modify: `pkg/plugin/redirect/plugin.go`
- Modify: `pkg/plugin/redirect/plugin_test.go`
- Test: `t/plugin/runner_test.go`

**Interfaces:**
- Consumes: runner contract from Tasks 1-2 and upstream `redirect.t` at the pinned commit.
- Produces: 30 local cases accounting for source tests 1-48 exactly once.

- [ ] **Step 1: Add the redirect manifest with this exact source mapping**

| Local case | Upstream tests |
| --- | --- |
| schema-sanity | 1 |
| default-ret-code | 2 |
| fixed-uri | 3,4 |
| uri-variable | 5,6 |
| argument-variable | 7,8 |
| literal-dollar | 9,10 |
| escaped-uri-variables | 11,12 |
| missing-variable | 13,14 |
| absolute-https-uri | 15,16 |
| plugin-attr-https-port | 17,18 |
| ssl-listen-port | 19 |
| single-ssl-listen-port | 20 |
| multiple-ssl-listen-ports | 21 |
| default-https-port | 22 |
| http-to-https-ignores-ret-code | 23,24 |
| reject-http-to-https-with-uri | 25 |
| http-to-https-with-upstream | 26,27 |
| frontend-tls-handshake | 28,29 (skip: Go server has no frontend TLS listener) |
| post-http-to-https | 30,31 |
| get-http-to-https | 32 |
| head-http-to-https | 33 |
| regex-uri-match | 34,35 |
| regex-uri-fallthrough | 36 |
| encoded-regex-uri | 37,38 |
| encoded-uri | 39,40 |
| append-query-string | 41,42 |
| append-existing-query-string | 43,44 |
| forwarded-http | 45,46 |
| forwarded-nonstandard-scheme | 47 |
| reject-http-to-https-with-query-append | 48 |

- [ ] **Step 2: Run redirect integration cases and record RED failures**

```bash
source .envrc && go test ./t/plugin -run 'TestPluginIntegration/redirect' -count=1 -v
```

Expected before fixes: failures expose current absolute-location behavior,
missing APISIX variable expansion/escaping, or missing mutually-exclusive
configuration validation.

- [ ] **Step 3: Add focused regression tests before each redirect fix**

Add table tests for:

```go
func TestHandlerExpandsRedirectVariables(t *testing.T)
func TestHandlerPreservesEscapedDollarSyntax(t *testing.T)
func TestHandlerUsesRelativeLocationForRelativeURI(t *testing.T)
func TestPostInitRejectsIncompatibleRedirectOptions(t *testing.T)
```

Each new test must fail for the corresponding integration mismatch before
production code changes.

- [ ] **Step 4: Implement only the proven redirect fixes**

Introduce one local URI-template renderer that recognizes `$name`, `${name}`,
`$$`, and backslash-escaped dollar tokens using existing APISIX request-variable
resolution. Preserve relative redirect targets, append the original query once,
and reject `http_to_https` combined with `uri`, `regex_uri`, or
`append_query_string` in `PostInit()`.

- [ ] **Step 5: Verify redirect unit and integration tests GREEN**

```bash
source .envrc && go test ./pkg/plugin/redirect ./t/plugin -run 'Test(Handler|PostInit|PluginIntegration/redirect)' -count=1 -v
```

Expected: all executable redirect cases pass and the frontend TLS case is the
only redirect skip.

---

### Task 4: Convert every proxy-rewrite source block and fix confirmed parity gaps

**Files:**
- Create: `t/plugin/proxy-rewrite.yaml`
- Modify: `pkg/plugin/proxy_rewrite/plugin.go`
- Modify: `pkg/plugin/proxy_rewrite/plugin_test.go`
- Modify if a request-URI defect is proven: `pkg/route/builder.go`
- Modify if needed for that defect: `pkg/route/builder_test.go`

**Interfaces:**
- Consumes: runner contract and pinned upstream `proxy-rewrite.t`.
- Produces: 36 local cases accounting for source tests 1-57 exactly once.

- [ ] **Step 1: Add the proxy-rewrite manifest with this exact source mapping**

| Local case | Upstream tests |
| --- | --- |
| schema-sanity | 1 |
| add-plugin | 2 |
| update-plugin | 3 |
| disable-plugin | 4 |
| rewrite-host | 5,6 |
| rewrite-scheme-https | 7,8 |
| rewrite-headers | 9,10 |
| add-headers | 11,12 |
| rewrite-empty-headers | 13,14 |
| rewrite-uri-query | 15,16 |
| rewrite-uri-empty-query | 17,18 |
| remove-empty-header | 19,20 |
| regex-uri | 21,22 |
| regex-uri-no-route-match | 23 |
| uri-precedes-regex-uri | 24,25 |
| reject-odd-regex-uri | 26 |
| reject-invalid-regex-pattern | 27 |
| reject-invalid-regex-replacement | 28 |
| reject-invalid-uri | 29 |
| reject-non-string-uri | 30 |
| reject-invalid-header-field | 31 |
| reject-invalid-header-value | 32 |
| rewrite-uri-with-arguments | 33,34 |
| admin-etcd-clean-serialization | 35 (skip: Admin API/etcd representation is outside standalone mode) |
| header-request-variables | 36,37 |
| missing-request-variable | 38,39 |
| context-uri-variable | 40,41 |
| host-with-port-schema | 42 |
| host-with-port-and-rewritten-uri | 43,44 |
| unsafe-uri-with-query | 45,46 |
| unsafe-uri-empty-query | 47,48 |
| unsafe-configured-uri-query-merge | 49,50 |
| unsafe-real-request-uri | 51,52 |
| unsafe-special-query | 53,54 |
| unsafe-multiple-question-marks | 55 |
| safe-configured-uri-query | 56,57 |

- [ ] **Step 2: Run proxy-rewrite integration cases and record RED failures**

```bash
source .envrc && go test ./t/plugin -run 'TestPluginIntegration/proxy-rewrite' -count=1 -v
```

- [ ] **Step 3: Add focused regression tests before confirmed fixes**

Use the existing plugin test seam for schema, header, capture, and variable
bugs. Use `pkg/route/builder_test.go` only if the failure is in final URI/query
application rather than plugin context production. Expected likely regression
tests are empty legacy header removal, invalid URI rejection, and exact query
merging; do not add them unless the integration output proves the mismatch.

- [ ] **Step 4: Implement the smallest confirmed fixes**

Keep URI-template/capture logic inside `proxy_rewrite`. Keep final URL mutation
inside `route.applyProxyRewriteURI`. Do not introduce a shared abstraction with
`redirect`; the escape and query contracts differ.

- [ ] **Step 5: Verify proxy-rewrite unit and integration tests GREEN**

```bash
source .envrc && go test ./pkg/plugin/proxy_rewrite ./pkg/route ./t/plugin -run 'Test(Handler|Headers|PostInit|ApplyProxyRewrite|PluginIntegration/proxy-rewrite)' -count=1 -v
```

Expected: all executable proxy-rewrite cases pass and only the Admin/etcd
serialization case is skipped.

---

### Task 5: Convert every response-rewrite source block and fix confirmed parity gaps

**Files:**
- Create: `t/plugin/response-rewrite.yaml`
- Modify: `pkg/plugin/response_rewrite/plugin.go`
- Modify: `pkg/plugin/response_rewrite/plugin_test.go`

**Interfaces:**
- Consumes: runner contract and pinned upstream `response-rewrite.t`.
- Produces: 20 local cases accounting for source tests 1-27 exactly once.

- [ ] **Step 1: Add the response-rewrite manifest with this exact source mapping**

| Local case | Upstream tests |
| --- | --- |
| schema-sanity | 1 |
| reject-invalid-status | 2 |
| reject-invalid-config | 3 |
| rewrite-headers-and-body | 4,5 |
| rewrite-body-preserve-headers | 6,7 |
| rewrite-location-and-status | 8,9 |
| empty-header-value | 10 |
| reject-array-header-value | 11 |
| base64-body | 12,13 |
| reject-invalid-base64 | 14 |
| admin-etcd-clean-serialization | 15 (skip: Admin API/etcd representation is outside standalone mode) |
| valid-status-expression | 16 |
| reject-invalid-expression | 17 |
| matching-status-expression | 18,19 |
| nonmatching-status-expression | 20 |
| empty-base64-body | 21 |
| nil-base64-body | 22 |
| response-header-variables | 23,24 |
| empty-body | 25,26 |
| add-header-without-colon | 27 |

- [ ] **Step 2: Run response-rewrite integration cases and record RED failures**

```bash
source .envrc && go test ./t/plugin -run 'TestPluginIntegration/response-rewrite' -count=1 -v
```

- [ ] **Step 3: Add focused regression tests before confirmed fixes**

Use the existing response recorder test seam for header deletion, empty/nil
base64 behavior, status expressions, and header-variable resolution. Each
production edit must have a focused failing test tied to an integration case.

- [ ] **Step 4: Implement the smallest confirmed fixes**

Preserve the existing Go-only `body_secret` and filter behavior. Change only
the APISIX-compatible schema/default/header/body semantics proven by the pinned
upstream cases.

- [ ] **Step 5: Verify response-rewrite unit and integration tests GREEN**

```bash
source .envrc && go test ./pkg/plugin/response_rewrite ./t/plugin -run 'Test(Handler|PostInit|PluginIntegration/response-rewrite)' -count=1 -v
```

Expected: all executable response-rewrite cases pass and only the Admin/etcd
serialization case is skipped.

---

### Task 6: Document and expose the integration suite

**Files:**
- Create: `t/plugin/README.md`
- Modify: `Makefile`
- Modify: `docs/superpowers/specs/2026-07-14-plugin-integration-tests-design.md`
- Modify: `docs/superpowers/plans/2026-07-14-plugin-integration-tests.md`

**Interfaces:**
- Consumes: implemented runner and manifests.
- Produces: user-facing conversion instructions and `make test-integration`.

- [ ] **Step 1: Add the Make target**

```make
.PHONY: test-integration
test-integration:
	go test ./t/plugin -count=1 -v
```

- [ ] **Step 2: Document conversion and coverage rules**

Document the pinned source metadata, case fields, matcher forms, runtime merge,
fixture upstream, HTTP/HTTPS behavior, source-number pairing, explicit skip
policy, and these commands:

```bash
source .envrc && make test-integration
source .envrc && go test ./t/plugin -run 'TestPluginIntegration/redirect' -count=1 -v
```

- [ ] **Step 3: Verify source coverage and focused suite**

```bash
source .envrc && make test-integration
```

Expected: 48/48 redirect, 57/57 proxy-rewrite, and 27/27 response-rewrite source
numbers accounted for; 83 executable cases pass; three named cases skip with
four explicit source blocks.

---

### Task 7: Review, verify, commit, push, and open the PR

**Files:**
- Review all task-owned files from Tasks 1-6.

**Interfaces:**
- Consumes: complete local diff.
- Produces: reviewed commit(s), pushed branch, and one PR to `master`.

- [ ] **Step 1: Run code-review-and-quality and repair required findings**

Review correctness, simplicity, process cleanup, timeout handling, assertion
diagnostics, source coverage, plugin parity fixes, test isolation, and dependency
discipline. Fix every valid Critical or Important finding.

- [ ] **Step 2: Run fresh final verification**

```bash
source .envrc && make test-integration
source .envrc && go test ./... -count=1
source .envrc && make lint
source .envrc && make build
git diff --check
git status --short
```

Remove only the generated `./apisix` binary if present, then re-run
`git status --short`. All commands must exit zero and the status must contain
only task-owned source/docs files.

- [ ] **Step 3: Stage and inspect the exact diff**

Stage only the design, plan, runner, manifests, README, Makefile, and confirmed
plugin/runtime fixes. Inspect `git diff --cached --stat` and
`git diff --cached` before committing.

- [ ] **Step 4: Commit and push**

Use a Conventional Commit describing the integration suite, for example:

```text
test(plugin): add standalone APISIX integration suite
```

Push `codex/plugin-integration-tests` to `origin`.

- [ ] **Step 5: Create the PR**

Create a ready PR with base `master`, head `codex/plugin-integration-tests`, and
sections for Summary, Source Coverage, Verification, Review, and Explicit
Skips. The PR must state the pinned APISIX commit and enumerate the four skipped
source blocks with reasons.
