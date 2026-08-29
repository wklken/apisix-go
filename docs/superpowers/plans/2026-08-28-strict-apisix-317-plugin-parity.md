# Strict APISIX 3.17 Plugin Parity Plan

> **Execution rule:** This plan targets literal plugin-behavior parity for all
> 111 Go-applicable APISIX 3.17 capabilities. It must not turn representative
> tests, package tests, or declared divergences into blanket differential
> evidence. Production readiness remains a later, separate goal.

**Goal:** Produce a first-attempt immutable qualification artifact in which all
111 required plugins are behavior-full and have plugin-specific APISIX 3.17
differential evidence.

**Frozen target:** Apache APISIX source commit
`9ef2ecab67f652d38365049613610ef649bb4ad0`, image
`docker.io/apache/apisix@sha256:5a8d7dfd8382aebfc0cab7bf9d24edf8dd73a6f0eed0b789d25578a373e86f64`.

## Current evidence baseline

- Source corpus accounting is complete with zero pending or blocked
  plugin-owned source blocks.
- All 119 manifest capabilities classify plugin-owned real dependencies and
  failure behavior with no missing, stale, flaky, or deferred claim.
- The immutable six-case oracle passes, but covers only five capabilities:
  `proxy-rewrite`, `cors`, `key-auth`, `request-validation`, and
  `response-rewrite`.
- The strict profile selects 111 plugins. Eighty-five are currently
  `behavior: full`; 26 are `behavior: partial`; all 111 still have
  `differential: missing`.
- The fail-closed gate correctly reports `0/111`. That result must remain until
  both behavior and per-plugin differential evidence are complete.

## Non-negotiable acceptance rules

1. Every selected plugin has at least one candidate-versus-APISIX execution on
   the same semantic input. Authentication, mutation, rejection, dependency
   wire shape, and phase-sensitive behavior require separate obligations where
   one happy-path case cannot prove them.
2. The catalog, APISIX source/image, candidate source/tree/binary, fixtures,
   normalization policy, and output observations are content-addressed.
3. No skip, xfail, expected divergence, retry-only pass, broad field ignore, or
   blanket `not_applicable` can satisfy the strict profile.
4. A package, dependency, failure, or converted-upstream test may supply the
   scenario and assertion design, but it becomes differential evidence only
   after both candidate and official APISIX execute it.
5. A selected plugin remains unqualified while any known behavior gap is real,
   even if its other differential cases pass.
6. Host-native development evidence cannot become final qualification proof.
   The final artifact compares the pinned APISIX linux/amd64 image with the
   exact candidate linux/amd64 image digest intended for release qualification.

## Phase 0 — Correct qualification semantics before adding cases

**Files:**

- Create `qualification/differential-cases.yaml`.
- Modify `t/plugin/oracle.go`, `t/plugin/oracle_test.go`, and
  `t/plugin/differential_test.go`.
- Modify `t/plugin/case.go` and `t/plugin/coverage_test.go` only for stable case
  lookup and catalog validation.
- Modify `scripts/qualification/plugin_differential.sh` and
  `scripts/qualification/plugin_behavior_gate.sh`.
- Modify `pkg/capability/load.go`, `cmd/capability-gen`, and generated
  projections so static contract readiness is not presented as a completed
  runtime qualification.

**Work:**

1. Rename the current six-case artifact/profile to an honest differential
   smoke for five capabilities.
2. Add a catalog keyed by plugin and obligation. Catalog rows reference existing
   integration cases rather than copying request/config/fixture data.
3. Upgrade the artifact to schema v2 with exact required/covered plugin sets,
   per-plugin obligations, raw observations, normalized hashes, first-attempt
   status, and catalog hash.
4. Make the strict gate compare the catalog set exactly with all 111 required
   plugins and reject duplicates, extras, filters, missing obligations, and
   mutable identities.
5. Separate `ContractReadyPlugins` from candidate qualification. A candidate is
   qualified only by a matching immutable runtime artifact.

**Focused verification:**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestDifferentialCatalog|TestDifferentialArtifact|TestOracleIdentity)$" -count=1'
bash scripts/qualification/plugin_differential_test.sh
bash scripts/qualification/plugin_behavior_gate_test.sh
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/capability ./cmd/capability-gen -count=1'
```

**Checkpoint:** The smoke artifact reports exactly 5/111 covered and can never
be mistaken for `apisix-3.17-all-plugins-v1` qualification.

## Phase 1 — Repair current capability truth before product changes

**Files:** `pkg/capability/manifest.yaml`, exact package tests, corpus mappings,
and generated projections.

1. Prove APISIX 3.17 `grpc-web` multi-write plus trailer Base64 behavior on both
   sides. If observations match, remove the incorrect lifetime-stream gap and
   promote only that behavior claim.
2. Prove `grpc-transcode pb_option=enable_hooks` produces the same observable
   request/response behavior. The pinned APISIX tree has no protobuf hook
   registration; remove the gap only after the differential passes.
3. Map `workflow.t` TEST 14 and remove the incorrect statement that rejecting
   `limit-count.group` is a compatibility gap. Preserve the real expression
   variable/PCRE gap.
4. Expand the gap audit beyond existing manifest text before promoting any
   plugin. Known examples that must be recorded include redirect `regex_uri`
   PCRE and status-code range differences, plus uri-blocker raw
   `request_uri`/status-range differences.

**Checkpoint:** Manifest truth describes the target source precisely; no product
code is changed merely to match an incorrect gap description.

## Phase 2 — Close independent, portable Go gaps

Each item starts with a failing APISIX-pinned package or differential test and
ends with its own focused package, race when relevant, build, and diff check.

1. `mocking`: implement all no-example random defaults used by APISIX 3.17,
   including string, number, integer, boolean, and 1–3 element arrays. Assert
   type/range/shape, not a fixed random value.
2. `redirect`: implement the official `uri` grammar validator. Do not mark the
   plugin full until its separate PCRE and status-range differences are decided.
3. `traffic-label`: replace the sequential expanded cursor with a concurrency-
   safe weighted round-robin picker matching the pinned selection contract.
4. `request-validation`: materialize declared secret leaves inside
   `header_schema` and `body_schema` with exact generation authority, atomic
   failure, cleanup, and no diagnostic leakage.

## Phase 3 — Add shared Go capabilities once, then migrate consumers

1. **JSONPath evaluator:** implement the APISIX-used bracket, array, union,
   slice, and multi-node operations once. Migrate `acl` value lookup and
   `data-mask` mutable-node removal/replacement to it.
2. **Encrypted OAuth sessions:** add a versioned AEAD session codec with expiry,
   key rotation/fallback, tamper rejection, and plaintext-free cookies. Migrate
   `dingtalk-auth` and `feishu-auth` without losing secret cleanup.
3. **Generation-owned breaker state:** share `api-breaker` counters by host and
   URI across route-local plugin instances while isolating generations and
   draining ownership safely.
4. **Resolver snapshots:** add an injectable, generation-owned DNS snapshot
   contract preserving original Host/SNI. Use it for `ai-proxy-multi` per-IP
   health/failover and the resolver-owned part of `proxy-mirror`.

These are cross-package changes. Each gets a dedicated design note, import-
boundary tests, focused race coverage, and independent review before consumers
are migrated.

## Phase 4 — Mandatory regex and NGINX-variable architecture decision

Affected plugins include `ai-prompt-guard`, `fault-injection`,
`response-rewrite`, `traffic-split`, `ua-restriction`, `uri-blocker`,
`workflow`, and redirect `regex_uri`.

Before product implementation, write an ADR and a conformance spike comparing:

- a maintained PCRE2/native integration and its cgo/platform/security cost;
- a sandboxed portable compatibility engine and its actual PCRE coverage;
- retaining RE2 as a deliberate security divergence.

The strict profile cannot choose the third option and still claim exact parity.
The spike must cover lookaround, backreferences, named captures, replacement
semantics, catastrophic-pattern controls, cancellation, and all supported build
platforms. NGINX variables must be cataloged separately into computable Go
values and runtime-native values; adding a regex engine does not create missing
NGINX request state.

**Stop condition:** Do not implement eight plugin-specific regex workarounds. If
no acceptable shared engine passes the spike, strict 111/111 is blocked pending
an explicit scope or platform decision.

## Phase 5 — Mandatory Lua/OpenResty compatibility decision

Affected plugins include `body-transformer`, `exit-transformer`,
`serverless-pre-function`, and `serverless-post-function`.

Literal parity includes arbitrary Lua statements/functions, OpenResty phase
behavior, streaming `ngx.arg`, `ngx.ctx`, logging, module loading, cosockets, and
other `ngx_lua` APIs. Extending the current bounded GopherLua surface one helper
at a time does not establish this contract.

Prepare an architecture proposal for one of:

- a separately sandboxed Lua/OpenResty-compatible execution subsystem with
  explicit resource, filesystem, network, secret, and generation ownership;
- delegating these capabilities to an external OpenResty sidecar/runtime;
- excluding exact native parity and changing the qualification scope.

**Stop condition:** No existing bounded serverless or transformer implementation
may be promoted to full without the selected runtime executing target-pinned
conformance cases. If the security model forbids the first two options, literal
111/111 is impossible and the profile must remain unqualified.

## Phase 6 — Close protocol and core-lifecycle gaps

1. `rocketmq-logger`: upgrade, replace, or maintain a pinned client with TLS
   nameserver/broker support; test CA verification, SNI, handshake failure,
   cancellation, and shutdown ownership.
2. `zipkin`: add the APISIX 3.17 `span_version=1` topology and phase observations
   across request, upstream, header/body, and finalization hooks; compare payload
   semantics rather than exact timestamps.
3. `proxy-mirror`: after resolver work, add request-body tee/subrequest lifecycle
   support for long-lived streaming mirrors. This is a core request-path change,
   not an asynchronous plugin-only patch.
4. Re-audit `exit-transformer` early-stop callbacks and any remaining phase-
   lifecycle gaps after the shared runtime decision.

## Phase 7 — Build differential coverage in bounded batches

The catalog is migrated only after a case runs on both sides. Development may
filter a batch; the final strict run may not.

1. Existing five-plugin smoke.
2. Portable HTTP/full plugins, no more than ten plugins per reviewed batch.
3. HTTP/TLS plugins.
4. Redis/state plugins.
5. Protocol bridges and logging/tracing sinks.
6. The 11 capabilities currently lacking their own process manifest:
   `ai-aliyun-content-moderation`, `dubbo-proxy`, `grpc-transcode`, `grpc-web`,
   `mcp-bridge`, `mqtt-proxy`, `proxy-buffering`, `server-info`, both serverless
   plugins, and `zipkin`.
7. The 26 partial plugins only after their gaps are closed.

Each batch updates the catalog, runs focused differential cases serially, updates
only evidence that passed on the first attempt, and receives an independent
review before the next batch.

## Phase 8 — Final strict gate

Run the final qualification only after all earlier checkpoints pass:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/plugin/... -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && APISIX_GO_REQUIRE_FULL_CORPUS=1 APISIX_SOURCE_DIR="$PWD/.cache/apache-apisix" scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestUpstreamCorpusAccounting|TestCorpusEvidenceMatchesCompatibilityTarget|TestUpstreamCorpusCompletion|TestDifferentialCatalogCoverage)$" -count=1 -v'
env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u NO_PROXY -u FTP_PROXY -u http_proxy -u https_proxy -u all_proxy -u no_proxy -u ftp_proxy CONTAINER_BIN=podman bash scripts/qualification/plugin_behavior_gate.sh
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make lint'
git diff --check
```

The final report must state the exact candidate image digest and 111/111
plugin qualification. It must also state that supervisor/worker operation,
strict security defaults, proxy isolation policy, stream TLS/UDP/general plugin
chains, capacity/soak, recovery, signing, canary, and release promotion remain
outside this plugin qualification and therefore production readiness is still
pending.

## Execution-size ruling

Literal strict parity is an XL program: it crosses more than 50 files, shared
runtime/compiler boundaries, dependency policy, protocol adapters, and possibly
the security model. Execute it as persistent SDD with one phase owner, bounded
exclusive file ownership, source-backed review after every batch, and no commit,
push, PR, image publication, or production action without separate authority.
