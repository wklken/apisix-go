# All Supported Plugin Integration Tests Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. The repository forbids subagents unless the user explicitly authorizes them, so execution stays inline.

**Goal:** Convert the Apache APISIX `t/plugin` suites for every source-backed plugin marked `Supported` in `docs/plugins.md` into executable APISIX-Go standalone integration cases, with no placeholder skips.

**Architecture:** `docs/plugins.md` selects the plugin set and Apache APISIX commit `c3d7d5ec69774121f53d2e20d29d09c816795dd7` supplies the source cases. Strict YAML manifests map every upstream `TEST` block exactly once, but setup and assertion blocks may be grouped into one executable scenario. Every scenario writes isolated `config.yaml` and `apisix.yaml`, starts the real APISIX-Go command, sends one or more requests, and asserts client responses plus fixture observations.

**Tech Stack:** Go 1.26.4, `go.yaml.in/yaml/v3`, Go `testing`, `httptest`, `os/exec`, standalone YAML configuration, bbolt, Make, GitHub CLI.

## Global Constraints

- Work in `/Users/wklken/workspace/tx/wklken/apisix-go` on branch `codex/plugin-integration-tests` and keep PR #4 draft until every final gate passes.
- Run every Go command from Bash after `source .envrc`.
- Do not add dependencies or run `make dep`.
- The selected set is the 100 rows whose `Registered` value is `yes` and whose `Current status` begins with `Supported` in `docs/plugins.md`: 89 `Supported (monitor)` plus 11 `Supported (README)`.
- At the pinned Apache APISIX commit, 98 selected plugins have upstream `t/plugin` sources. `GM` and `proxy-buffering` have no upstream `.t` source and must be reported as source-absence exceptions, not represented by skipped cases.
- A source `TEST` block is covered only when its number belongs to an executable scenario. Setup-only blocks must be grouped with the behavior block that consumes their resources. Schema blocks must become valid-startup or invalid-startup cases. Generic skip text, source enumeration without execution, and a `skip` field do not count.
- Every executable scenario must write standalone runtime/resources, start APISIX-Go, send at least one HTTP, HTTPS, gRPC, TCP, UDP, or plugin control request, and assert the resulting response or fixture observation.
- External behavior must use deterministic local fixtures owned by the test. Real cloud credentials and public network services are forbidden.
- Fix production code only for a mismatch reproduced by a converted integration case; add a focused regression test before the fix.
- Final gates are `make test-integration`, `go test ./... -count=1`, `make lint`, `make build`, and `git diff --check`.
- The generated `./apisix`, `apisix-go-store.db`, fixture logs, certificates, and temporary data must not be committed.

## Authoritative Plugin Set

| Wave | Plugins |
|---|---|
| Core HTTP and transformation | `redirect`, `echo`, `gzip`, `brotli`, `real-ip`, `error-page`, `exit-transformer`, `attach-consumer-label`, `response-rewrite`, `proxy-rewrite`, `fault-injection`, `mocking`, `degraphql`, `body-transformer` |
| Authentication and authorization | `key-auth`, `basic-auth`, `jwt-auth`, `hmac-auth`, `ldap-auth`, `openid-connect`, `forward-auth`, `multi-auth`, `jwe-decrypt`, `wolf-rbac`, `authz-keycloak`, `authz-casdoor`, `cas-auth`, `saml-auth`, `dingtalk-auth`, `feishu-auth`, `authz-casbin`, `opa` |
| Security and validation | `acl`, `ip-restriction`, `ua-restriction`, `referer-restriction`, `uri-blocker`, `consumer-restriction`, `cors`, `csrf`, `public-api`, `chaitin-waf`, `traffic-label`, `request-validation`, `oas-validator`, `data-mask` |
| Traffic and control | `limit-conn`, `limit-count`, `limit-req`, `proxy-cache`, `graphql-proxy-cache`, `proxy-mirror`, `api-breaker`, `traffic-split`, `request-id`, `proxy-control`, `client-control`, `workflow`, `batch-requests`, `graphql-limit-count`, `node-status`, `server-info`, `GM`, `proxy-buffering` |
| Function and protocol adapters | `azure-functions`, `openfunction`, `openwhisk`, `aws-lambda`, `http-dubbo`, `kafka-proxy` |
| Metrics, tracing, and logging | `opentelemetry`, `skywalking`, `http-logger`, `tcp-logger`, `udp-logger`, `syslog`, `kafka-logger`, `rocketmq-logger`, `clickhouse-logger`, `sls-logger`, `google-cloud-logging`, `error-log-logger`, `splunk-hec-logging`, `file-logger`, `loggly`, `elasticsearch-logger`, `tencent-cloud-cls`, `loki-logger`, `lago`, `datadog`, `skywalking-logger`, `log-rotate` |
| AI | `ai-aws-content-moderation`, `ai-rag`, `ai-prompt-decorator`, `ai-prompt-guard`, `ai-request-rewrite`, `ai-rate-limiting`, `ai-proxy` |
| Development | `example-plugin` |

The table contains all 100 selected plugins exactly once. The implementation gate must parse the live matrix and reject drift instead of trusting this prose list.

---

### Task 1: Replace skip-oriented coverage with executable coverage

**Files:**
- Modify: `t/plugin/case.go`
- Modify: `t/plugin/case_test.go`
- Create: `t/plugin/coverage_test.go`

**Interfaces:**
- Consumes: all strict YAML manifests and `docs/plugins.md`.
- Produces: multi-source manifest validation and a final matrix-to-manifest coverage gate.

- [x] **Step 1: Write failing tests for multi-source coverage**

Add tests proving that one manifest can declare multiple `sources`, that each case must name its source file when more than one exists, and that missing/duplicated numbers fail per file. Keep the singular `source` form for one-file manifests so the verified initial corpus does not require a mechanical rewrite.

Run:

```bash
source .envrc && go test ./t/plugin -run 'TestManifest(MultipleSources|RejectsMissingSourceFile|RejectsDuplicateSourceNumber)' -count=1
```

Expected: RED because the current manifest owns only one `source`.

- [x] **Step 2: Implement the multi-source manifest contract**

Extend the contract to:

```go
type Manifest struct {
    Source  SourceSpec   `yaml:"source,omitempty"`
    Sources []SourceSpec `yaml:"sources,omitempty"`
    Cases   []Case       `yaml:"cases"`
}

type CaseSource struct {
    File  string `yaml:"file,omitempty"`
    Tests []int  `yaml:"tests"`
}
```

Require exactly one of singular `source` or plural `sources`, at least one case, exact per-file `1..tests` coverage, unique source paths, and a pinned commit/repository on every source. Preserve the currently verified case and variant execution fields. Keep the three existing concrete `skip` entries only until Tasks 2-3 provide their missing fixtures; do not add new skips.

- [x] **Step 3: Add and unit-test docs-matrix selection helpers**

Parse `docs/plugins.md` exactly as `pkg/plugin/docs_matrix_test.go` does. Unit-test the parser against an in-memory complete manifest-name set: it must select 100 rows, require one manifest per source-backed selected plugin, reject extra manifests, and allow exactly these two source-absence exceptions:

```go
map[string]string{
    "GM":              "no Apache APISIX t/plugin source at the pinned commit",
    "proxy-buffering": "no Apache APISIX t/plugin source at the pinned commit",
}
```

Do not run the complete-set assertion against the incomplete working tree in this task and do not add a disabled guard. Task 8 wires the already-tested helper to the real manifest catalog after all 98 manifests exist.

- [x] **Step 4: Run focused tests GREEN**

```bash
source .envrc && go test ./t/plugin -run 'Test(LoadManifest|Manifest|DocumentedPluginManifests)' -count=1
```

Expected: all focused contract and parser-helper tests pass.

### Task 2: Support executable multi-step standalone scenarios

**Files:**
- Modify: `t/plugin/case.go`
- Modify: `t/plugin/case_test.go`
- Modify: `t/plugin/runner_test.go`

**Interfaces:**
- Consumes: a case with ordered `steps` and named fixtures.
- Produces: deterministic request sequences and assertions in one APISIX-Go process.

- [x] **Step 1: Write failing request-sequence tests**

Add an inline case that sends two requests through one route and proves state is retained between requests. Add a case with two named HTTP fixtures and placeholders `{{FIXTURE.primary.ADDR}}` and `{{FIXTURE.audit.URL}}`.

Run:

```bash
source .envrc && go test ./t/plugin -run 'TestHarness(RunsRequestSequence|RunsNamedFixtures)' -count=1 -v
```

Expected: RED because the current runner supports one input/output and one upstream.

- [x] **Step 2: Implement ordered steps**

Use this strict shape:

```go
type Step struct {
    Name   string        `yaml:"name"`
    Input  InputSpec     `yaml:"input"`
    Output OutputSpec    `yaml:"output"`
    Wait   time.Duration `yaml:"wait,omitempty"`
}
```

Add `Steps []Step` and `Fixtures []FixtureSpec` to `Case` and `CaseVariant`. Keep the current singular `input`, `upstream`, and `output` fields as a one-step shorthand. Reject a case that mixes shorthand fields with `steps` or named `fixtures`.

Execute steps in declaration order against the same APISIX process. Require unique step names, explicit protocol/method/path, an exact output status for HTTP-family requests, and at least one response or fixture assertion.

- [x] **Step 3: Implement named fixtures**

Support local `http` and `https` fixture kinds here; Task 3 adds `tcp`, `udp`, and `grpc`. Each fixture owns a sequence of responses and captured requests. Replace only declared placeholders in runtime/resources/steps, fail on an unknown placeholder, and fail when expected fixture requests are missing or extra.

- [x] **Step 4: Run focused tests GREEN**

```bash
source .envrc && go test ./t/plugin -run 'TestHarness(RunsStandaloneRoute|RunsRequestSequence|RunsNamedFixtures)' -count=1 -v
```

### Task 3: Support protocol and transport assertions required by upstream cases

**Files:**
- Modify: `t/plugin/case.go`
- Modify: `t/plugin/case_test.go`
- Modify: `t/plugin/runner_test.go`
- Create: `t/plugin/testdata/certs/README.md`

**Interfaces:**
- Consumes: raw/multi-value headers, TLS settings, streaming bodies, TCP/UDP payloads, and fixture callbacks.
- Produces: assertions that preserve upstream wire behavior.

- [ ] Add failing tests for duplicate headers, ordered response chunks, HTTP/2, frontend TLS, gRPC unary frames/trailers, TCP payloads, UDP datagrams, client disconnect, and timeout behavior.
- [ ] Extend header fields from `map[string]string` to `http.Header`-equivalent YAML lists while retaining scalar shorthand.
- [ ] Generate deterministic test certificates during the test from fixed fixture inputs; do not invoke OpenSSL or commit private keys.
- [ ] Add raw TCP/UDP clients and gRPC frame/trailer matchers using the Go standard library and the repository's existing gRPC dependencies.
- [ ] Convert the three remaining `redirect`, `proxy-rewrite`, and `response-rewrite` gaps to executable standalone cases, then remove `skip` from `Case` and prove strict decoding rejects any future `skip:` key.
- [ ] Run `source .envrc && go test ./t/plugin -run 'TestHarness' -count=1 -v` and require GREEN.

### Task 4: Convert core HTTP, transformation, security, and validation suites

**Files:**
- Create/modify: `t/plugin/<plugin>.yaml` for every plugin in the first three rows of the authoritative table.
- Modify only when a converted RED case proves a defect: `pkg/plugin/<plugin_package>/*.go` and its focused tests.

**Interfaces:**
- Consumes: Tasks 1-3 manifest and fixture contracts.
- Produces: complete executable source coverage for 46 plugins.

- [ ] For each upstream source file, record its repository, pinned commit, path, and exact `TEST` count from `rg -c '^=== TEST ' <file>`.
- [ ] Group Admin API/setup blocks with every behavior block that consumes the resulting route, service, consumer, consumer-group, plugin-config, global-rule, or proto resource.
- [ ] Convert schema sanity blocks into startup-success cases and invalid schema blocks into startup-rejection cases that still issue a request and assert the unavailable/rejected route plus the plugin validation log.
- [ ] Convert `request`, `more_headers`, `request_body`, `error_code`, `response_body`, `response_body_like`, `response_headers`, `raw_response_headers_like`, `error_log`, and `no_error_log` sections into direct step assertions.
- [ ] Run each plugin manifest alone with `source .envrc && go test ./t/plugin -run 'TestPluginIntegration/<plugin>/' -count=1 -v` before moving to the next plugin.
- [ ] For each production mismatch, add a focused failing package test, make the smallest fix, and rerun both the package and manifest.

### Task 5: Convert traffic, stateful, function, and protocol suites

**Files:**
- Create/modify: `t/plugin/<plugin>.yaml` for every plugin in the Traffic/control and Function/protocol rows, excluding the two source-absence entries.
- Modify only for reproduced defects: matching `pkg/plugin`, `pkg/route`, `pkg/proxy`, or `pkg/stream` files plus focused tests.

**Interfaces:**
- Consumes: ordered steps, protocol clients, and named fixtures.
- Produces: complete executable source coverage for 22 source-backed plugins.

- [ ] Preserve counter/rate/cache state within one process and use explicit waits only where the upstream case asserts time-window behavior.
- [ ] Use local HTTP function fixtures for Azure, OpenFunction, OpenWhisk, and AWS signatures; assert the exact authorization/request received by the fixture.
- [ ] Use existing in-process Dubbo and Kafka protocol fixtures; do not require a public broker.
- [ ] Cover nested `proxy-cache` and `graphql-proxy-cache` sources under `t/plugin/<plugin>/` by declaring their full paths in the corresponding manifest.
- [ ] Run each manifest and its touched package tests GREEN before proceeding.

### Task 6: Convert metrics, tracing, and logger suites

**Files:**
- Create/modify: `t/plugin/<plugin>.yaml` for all 22 plugins in the Metrics, tracing, and logging row.
- Modify only for reproduced defects: matching `pkg/plugin` logger/observability packages plus focused tests.

**Interfaces:**
- Consumes: HTTP/TCP/UDP fixtures, request sequences, waits, and captured payload matchers.
- Produces: deterministic delivery/batching/retry/flush coverage without external services.

- [ ] Replace Elasticsearch, Loki, ClickHouse, SLS, CLS, Splunk, Loggly, Datadog, SkyWalking, RocketMQ, and HTTP endpoints with local protocol fixtures that return the exact source status/body sequence.
- [ ] Assert emitted payloads structurally where field order is not contractual and byte-for-byte where the upstream test explicitly checks wire format or signatures.
- [ ] Drive batching, retry, inactive timeout, and shutdown flush with bounded waits and captured delivery counts.
- [ ] Use temporary paths for file/error-log/log-rotate cases and assert file content after APISIX shutdown.
- [ ] Run each manifest and its touched package tests GREEN before proceeding.

### Task 7: Convert AI and development suites

**Files:**
- Create/modify: `t/plugin/<plugin>.yaml` for the seven AI plugins and `example-plugin`.
- Modify only for reproduced defects: matching `pkg/plugin` AI/example packages plus focused tests.

**Interfaces:**
- Consumes: HTTP/HTTPS/SSE fixtures, streaming assertions, request sequences, and signature capture.
- Produces: complete source coverage for eight plugins.

- [ ] Replace provider endpoints with local fixtures for OpenAI-compatible, Anthropic, Bedrock, Vertex, AWS moderation, RAG, and sidecar calls.
- [ ] Assert request rewriting, provider authorization/signatures, SSE/EventStream conversion, token usage, limits, prompt policies, and response/log payloads covered by the source.
- [ ] Convert `example.t` to `example-plugin.yaml`; map the source filename explicitly instead of relying on the plugin name.
- [ ] Run each manifest and its touched package tests GREEN before proceeding.

### Task 8: Enable complete coverage and publish PR #4

**Files:**
- Modify: `t/plugin/coverage_test.go`
- Modify: `t/plugin/README.md`
- Modify: `docs/plugins.md`
- Modify: `docs/superpowers/specs/2026-07-14-plugin-integration-tests-design.md`
- Modify: this plan

**Interfaces:**
- Consumes: all completed manifests and verified fixes.
- Produces: a merge-ready, mechanically complete integration corpus.

- [ ] Remove the temporary incomplete-catalog guard so the matrix gate always requires all 98 source-backed manifests and the two exact source-absence entries.
- [ ] Add a test rejecting any YAML `skip` key and asserting every case has at least one executable step.
- [ ] Recount every declared source directly from the pinned Apache files and compare its declared `tests` value plus exact `1..N` mapping.
- [ ] Update docs with the final plugin, source-file, upstream-block, executable-scenario, request-step, and zero-skip counts.
- [ ] Run final verification:

```bash
source .envrc && make test-integration
source .envrc && go test ./... -count=1
source .envrc && make lint
source .envrc && make build
git diff --check
git status --short
```

- [ ] Review every production change against its failing integration/unit evidence and remove unrelated edits or generated artifacts.
- [ ] Commit with specific Conventional Commit messages, push `codex/plugin-integration-tests`, update PR #4 with exact evidence/counts, and mark it ready only after all checks pass.

## Self-Review

- The plan includes all 100 live `Supported` rows exactly once.
- The plan distinguishes the 98 source-backed plugins from the two verified source absences.
- No skipped YAML case is accepted as coverage.
- Setup/Admin blocks are mapped into executable standalone scenarios rather than discarded.
- External/cloud behavior is exercised through deterministic local fixtures.
- Every production fix requires a reproduced RED integration case and focused regression.
- The final gate validates both the docs matrix and every upstream source test number.
