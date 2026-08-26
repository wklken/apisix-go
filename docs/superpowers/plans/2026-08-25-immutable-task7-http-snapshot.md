# Immutable Task 7 HTTP/TLS Snapshot Execution Plan

**Goal:** compile one detached, immutable HTTP handler and TLS selector from the
accepted Task 6 publication without reading Store, exposing runtime authority to
`pkg/route`, or activating a second production path.

**Baseline:** local `master` and this worktree start at accepted Task 6 commit
`990deb01`. The legacy `Store -> route.Builder -> routeHandler.Replace` and
package-global TLS selector remain the only production path until the joint
Task 9 cutover.

**Architecture:** `pkg/compiler` owns candidate authority, exact occurrences,
plugin materialization, runtime acquisition, cleanup and snapshot attachment.
`pkg/route` owns pure HTTP planning and handler assembly from cloned resources,
materialized bindings and already-acquired cluster handles. A lower-level TLS
compiler is the canonical protocol/SNI implementation used by both the legacy
server wrapper and the detached compiler path.

## Non-negotiable boundary

- No Task 7 production caller of `PrepareGeneration`, `PrepareRecovery`,
  `CompileHTTP`, or `PreparedGeneration.HTTP`.
- No Store getter, global consumer lookup, global SNI lookup, plugin factory,
  `runtime.Acquire`, lease, secret capability or cleanup authority in the new
  detached `pkg/route` path.
- No `pkg/compiler -> pkg/server` or `pkg/route -> pkg/compiler` dependency.
- Do not delete or bypass `route.Builder`, `proxy.ClusterRegistry`, server
  reload, `routeHandler.Replace`, or legacy TLS callbacks before Task 9.
- Every plugin binding comes from one final precedence winner paired with the
  exact admitted Task 6 occurrence. Losers never materialize.
- Every resource acquired after pure planning is immediately registered in the
  candidate cleanup stack. Failure releases in reverse acquisition order.
- Candidate and recovery preparation use the same HTTP compiler. Stream-only
  generations have no HTTP snapshot.

## Canonical-source ledger

| Contract | Canonical owner during Task 7 | Derived/compatibility surface | Proof |
| --- | --- | --- | --- |
| HTTP route, plugin phase and precedence behavior | existing Builder tests plus extracted pure helpers | detached route compiler and legacy Builder adapter | shared helper tests and parity cases |
| Plugin lifecycle and secret authority | compiler-private Task 6 materializer | route receives only `plugin.Binding` values | AST/import guard and cleanup tests |
| TLS protocols, ciphers and immutable SNI selection | new lower-level TLS compiler | legacy server wrapper and `compiler.HTTPSnapshot` | shared TLS corpus against both paths |
| Runtime resource identity and lifetime | compiler plus `runtime.ResourceRegistry` | route receives cluster handles only | acquire/reuse/reverse-release tests |
| Production activation | joint Task 9 | Task 7 detached snapshot only | zero production caller scan |

Confidence is high for the authority split. Exact route compiler input structs
may evolve during RED/GREEN work, but they may not violate this table.

## Dependency order

```text
T7-0 contract + RED tests
  |---- T7-1 router extraction
  `---- T7-2 pure HTTP planning
             |
             v
       T7-3 materialization context
             |
             v
       T7-4 cluster preparation
             |
             v
       T7-5 detached handler assembly
             |
             v
       T7-6 TLS + PreparedGeneration snapshot
             |
             v
       T7-7 parity/guards/review/integration
```

T7-1 may run in parallel with the RED/planning work after T7-0 freezes the
interfaces. Conflict-sensitive files `pkg/route/builder.go`,
`pkg/compiler/prepared_generation.go`, and
`pkg/compiler/effective_binding_materializer.go` have one serial integration
owner.

## T7-0: Freeze the contract and prove RED

**Create/modify:** this plan, the parent immutable plan, Task 6 handoff status,
`pkg/route/compiler_test.go`, `pkg/compiler/http_test.go`.

Add failing coverage for:

- input mutation after `route.CompileHTTP`;
- exact winner/occurrence pairing and no loser materialization;
- nil/incomplete input rejection before side effects;
- TLS config defensive clone;
- attach-versus-close serialization;
- no Store/global getter use from detached compilation.

Record RED output before implementation. Existing focused baseline is:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler ./pkg/route \
  -run '^(TestPreparedGeneration|TestWorkerCompilerFactory|TestBuildStrict|TestRoute|TestConsumer|TestPublicAPI|TestScoped)' \
  -count=1
```

## T7-1: Extract router matching without semantic change

**Create:** `pkg/route/router.go` and focused router tests.

Move the route registrar, wildcard dispatcher, URI conversion, host matching
and method selection out of `builder.go`. The legacy Builder calls the same
helpers. Preserve stable route ordering and add an equal-priority tie case.

No plugin, upstream, lifecycle or Store behavior changes in this slice.

## T7-2: Build a pure HTTP plan

**Create:** `pkg/route/plugin_compile.go`, planning tests.

Decode/accept cloned routes, services, upstreams, plugin-config rules, global
rules, SSL resources and the dynamic enabled-plugin set. Compute, before
side effects:

- route > plugin-config > service winners;
- deduplicated global winners and system/404 bindings;
- consumer-over-group winners, excluding credential-only auth configs;
- `_meta.disable`, priority, filter and error-response identity;
- exact route/service resource context and provenance;
- route upstream precedence and traffic-split referenced upstream closure;
- quarantine decisions for route-scoped failures without weakening
  generation-wide failures.

The plan carries declarative binding requests, not plugin instances or runtime
authority. A generation-local public API registry is created during preparation
and is published only through the closed handler.

## T7-3: Complete compiler-private materialization context

**Modify:** `pkg/compiler/effective_binding_materializer.go` and tests.

Before `PostInit`, supply the Builder-equivalent context that Task 6 deliberately
deferred:

- enabled-plugin checker;
- public API registry;
- pre-materialization validation;
- route ID/server address and route/service resource context;
- immutable traffic-split upstream resolver and compiler-owned runtime
  acquirer.

Keep runtime-only fields out of instance identity; identity remains attempt,
factory, effective config, `_meta` filter/error, scope, provenance and immutable
resource context. Apply priority and metadata wrappers exactly once after
materialization. Add tests for disabled losers, wrapper behavior, public API
generation isolation and context-before-`PostInit` ordering.

## T7-4: Acquire canonical upstream resources during prepare

**Create/modify:** `pkg/route/upstream_compile.go`, pure upstream helpers and
tests; compiler HTTP preparation and cleanup tests.

`pkg/route` may derive/validate canonical `proxy.ClusterConfig` but may not call
`proxy.NewCluster`, `runtime.Acquire`, or own a lease. `pkg/compiler` acquires
ordinary and traffic-split clusters through `runtime.ResourceRegistry`, then
registers each lease immediately in `PreparedGeneration` cleanup.

Before implementation, lock whether `ClusterConfig.Name` is behavioral identity
or an observer label. Reuse only exact canonical configurations. Test a shared
ordinary/traffic-split configuration, TLS rotation, failure after the third
acquisition and exact reverse release.

## T7-5: Assemble the detached HTTP handler

**Create:** `pkg/route/compiler.go`, `pkg/route/upstream_compile.go`, compiler
and parity tests.

`route.CompileHTTP(context.Context, CompileInput)` receives only owned values,
materialized bindings, consumer binding tables, public API registry and
prepared cluster handles. It builds:

- route and not-found handlers;
- request/response/log/streaming phase plans;
- websocket and protocol terminals;
- immutable consumer resolution with no lazy initialization;
- reverse proxy and traffic-split dispatch from prepared handles.

It never creates plugins, reads Store, acquires resources or starts tasks.
Mutation of source slices/maps after return cannot affect the handler.

The legacy Builder remains buildable and may call extracted pure helpers. Do
not replace its production call sites in this task.

## T7-6: Compile TLS and attach one HTTP snapshot

**Create/modify:** a lower-level TLS compiler and tests,
`pkg/compiler/http.go`, `prepared_generation.go`, `worker_factory.go`.

Compile protocols, ciphers, fallback SNI, exact and one-label wildcard
certificates, per-resource client CA/depth and the complete immutable selector
from the HTTP candidate. The legacy server wrapper delegates protocol behavior
to the same lower-level owner while retaining Store lookup until Task 9.

`compiler.HTTPSnapshot` owns the artifact, handler and TLS config. Accessors
return no lifecycle authority; `TLSConfig()` returns `Clone()`. HTTP attachment
and `Close` serialize on the existing preparation gate so no snapshot is
published after terminal close.

## T7-7: Compatibility, boundary and integration gate

Minimum behavior coverage:

- route/plugin-config/service/global/system/404 and `_meta` equivalence;
- consumer/group immutable resolution and missing identity failure;
- public API, scoped rewrite, auth/CORS, response/log, transparent upgrade and
  protocol-terminal equivalence;
- ordinary/traffic-split upstream, retry, mTLS and cleanup equivalence;
- exact/wildcard/fallback SNI and per-resource client CA;
- candidate/recovery HTTP snapshots; no HTTP snapshot for stream-only input;
- input isolation, attach/close race and reverse cleanup.

Static guards must prove:

- detached route files do not import Store, compiler, runtime lifecycle,
  secrets or data encryption;
- detached route files do not call plugin factories, `runtime.Acquire` or
  `proxy.NewCluster`;
- C6 ApplyTicket and no-production-prepare restrictions remain intact;
- any C6 import allowance is exact file/purpose, not a root-wide exemption;
- Task 7 still has zero production activation callers.

Run impact-scoped gates:

```bash
source .envrc
export GOFLAGS=-mod=readonly

go test ./pkg/compiler ./pkg/route ./pkg/plugin ./pkg/proxy ./pkg/server \
  -run '^(TestCompileHTTP|TestCompilerHTTP|TestPrepareHTTP|TestPreparedGenerationHTTP|TestRoute|TestScoped|TestConsumer|TestPublicAPI|TestResponsePlan|TestRequestPipeline|TestFrontendTLS|TestTrafficSplit)' \
  -count=1
go test -race ./pkg/compiler ./pkg/route ./pkg/proxy ./pkg/plugin/traffic_split \
  -run '^(TestCompileHTTP|TestPrepareHTTP|TestPreparedGenerationHTTP|TestConsumer|TestTrafficSplit)' \
  -count=1
golangci-lint run ./pkg/compiler/... ./pkg/route/... ./pkg/proxy/... ./pkg/plugin/traffic_split/...
go run ./cmd/capability-gen -repo-root . -check
make build
git diff --check
```

Run an independent merge-level review, repair every Critical/Important finding,
rerun invalidated gates, commit Task 7 locally, and only then integrate it to
local `master`. Do not push or create a PR.

## Task 9 handoff ledger

Task 7 intentionally leaves these legacy owners alive:

- Store `ConfigSnapshot` route Builder input;
- request-time global consumer-group lookup in Builder;
- server package-global SNI lookup;
- server `proxy.ClusterRegistry` and `Builder.Stop` retirement;
- provider event path, coordinator ticket, journal activation, rollback,
  finalize and acknowledgement.

Task 9 must atomically install the prepared HTTP/TLS owner, switch provider and
journal control, bind request lifetime to generation retirement, then delete
the legacy owners in the same reviewed unit.
