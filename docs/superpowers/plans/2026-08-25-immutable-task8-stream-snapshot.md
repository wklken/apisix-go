# Immutable Task 8 Stream Snapshot Execution Plan

**Goal:** compile one detached, immutable stream router from the accepted Task 7
publication without reading Store, opening listeners, activating a second runtime
path, or bypassing the generation-scoped plugin materializer.

**Baseline:** local `master` and the integration worktree start at accepted Task 7
commit `9c46edda`. The legacy `Store -> resolveStreamRoutesWithServices ->
stream.NewRuntime/Router.Reload` path remains the only production stream path
until the joint Task 9 cutover.

**Architecture:** `pkg/compiler` owns publication decoding, route/service/upstream
resolution, enabled-plugin selection, final precedence, exact occurrence pairing,
plugin materialization, cleanup, artifact identity, and snapshot attachment.
`pkg/stream` owns pure route-entry assembly from cloned route data and an already
materialized optional stream binding. Task 8 produces a detached router only;
Task 9 installs it together with the HTTP/TLS snapshot and retires reload owners.

## Non-negotiable boundary

- No Task 8 production caller of `PrepareGeneration`, `PrepareRecovery`,
  `CompileRouter`, `PreparedGeneration.Stream`, or `StreamSnapshot.Router`.
- Do not modify `pkg/server`, stream listener creation, `Runtime.Reload`,
  `Router.Reload`, event acknowledgement, journal activation, or last-good Store
  publication. Task 9 owns those changes as one cutover.
- The detached `pkg/stream` path must not call a plugin factory, schema compiler,
  secret materializer, Store getter, `runtime.Acquire`, or listener API.
- The legacy `NewRouter` path remains buildable and behaviorally unchanged until
  Task 9, even though it still constructs `mqtt-proxy` itself.
- The detached path accepts only zero bindings for raw TCP or one already
  materialized `mqtt-proxy` binding. General stream-plugin chaining, stream mTLS,
  UDP, external plugin runners, and new protocol owners remain out of scope.
- Route/service/upstream merge and plugin enablement happen before any side
  effect. A disabled, shadowed, or non-stream factory never materializes.
- Every final plugin winner is paired with its exact admitted
  `FactoryOccurrence`; raw occurrence enumeration may not create bindings.
- Candidate and recovery preparation use the same stream compiler. HTTP-only
  generations have no stream snapshot; combined generations own both snapshots.
- Closing `PreparedGeneration` revokes both observation surfaces and releases
  the shared generation-owned binding lease exactly once.

## Canonical-source ledger

| Contract | Canonical owner during Task 8 | Derived/compatibility surface | Proof |
| --- | --- | --- | --- |
| Published stream bytes and artifact identity | owned stream `PublicationCandidate` | decoded resource set and `StreamSnapshot` | candidate/recovery and mutation tests |
| Route/service/upstream precedence | compiler stream planner | legacy server resolver retained until Task 9 | parity table against existing resolver tests |
| Enabled stream plugins | dynamic `/plugins` entries with `stream: true`, else static `Config.StreamPlugins` | detached plan only | absent-versus-empty and HTTP/stream split tests |
| Plugin lifecycle, secret and occurrence authority | compiler-private Task 6 materializer | stream receives one `plugin.Binding` value | exact occurrence/loser/guard tests |
| TCP matching, balancing, retry and protocol serving | `pkg/stream` route-entry compiler | legacy `NewRouter/Reload` adapter | shared router corpus plus detached tests |
| Listener, activation and retirement ownership | joint Task 9 | Task 8 detached router only | zero production caller and no-listener guards |

## Dependency order

```text
T8-0 contract + RED tests
  |---- T8-1 detached stream route-entry API
  `---- T8-2 pure stream publication planner
             |
             v
       T8-3 exact winner materialization
             |
             v
       T8-4 snapshot attachment and lifecycle
             |
             v
       T8-5 parity/guards/review/integration
```

T8-1 and T8-2 may run in parallel from the frozen plan commit because they own
disjoint packages. `pkg/compiler/prepared_generation.go`, `worker_factory.go`,
and the parent plan have one serial integration owner.

## T8-0: Freeze interfaces and prove RED

**Create/modify:** this plan, parent Task 8 status, `pkg/stream/snapshot_test.go`,
`pkg/compiler/stream_test.go`.

Freeze these additive interfaces before implementation:

```go
// pkg/stream
type PreparedRoute struct {
    Route    resource.StreamRoute
    Protocol plugin.Binding // zero value means raw TCP
}

type CompileInput struct {
    Revision uint64
    Routes   []PreparedRoute
    OnResult func(Result)
}

func CompileRouter(context.Context, CompileInput) (*Router, error)
func (r *Router) RouteIDs() []string

// pkg/compiler
func (prepared *PreparedGeneration) Stream() *StreamSnapshot
func (snapshot *StreamSnapshot) Revision() uint64
func (snapshot *StreamSnapshot) Router() *stream.Router
```

`CompileRouter` defensively owns all route/upstream values and copies the
binding value before constructing entries. It returns a frozen router whose
legacy `Reload` method fails without mutation. It strips raw plugin documents from
the route entry after validating the supplied binding. The concrete plugin
instance remains generation-owned and is revoked through `PreparedGeneration`,
not by `Router`.

Add failing coverage for:

- mutation of route IDs, plugin maps, upstream nodes and binding fields after
  `CompileRouter` returns;
- raw TCP and exactly one prepared MQTT binding;
- rejection of nil/incomplete/mismatched bindings and more than one configured
  effective stream plugin;
- rejection of `Reload` on a detached frozen router without changing route IDs;
- no listener open during detached compilation;
- stream artifact identity and attach-versus-close serialization;
- stream-only, HTTP-only and combined candidate/recovery generations.

Record RED with exact focused commands. The accepted pre-change baseline is:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/stream -count=1
go test ./pkg/compiler \
  -run '^(TestWorkerCompilerFactoryPrepareGenerationTransfersBaseOwners|TestWorkerCompilerFactoryPrepareRecoveryTransfersBaseOwners|TestPreparedGenerationAccessorsAreInertAfterClose|TestPreparedGenerationPublicAPIExposesNoRuntimeHandles)$' \
  -count=1
```

## T8-1: Add detached stream route-entry compilation

**Create:** `pkg/stream/snapshot.go`, `pkg/stream/snapshot_test.go`.

**Modify:** `pkg/stream/router.go`, focused router tests only.

Extract the common pure validation/balancer/route-entry assembly used by both
legacy `NewRouter/Reload` and detached `CompileRouter`. The detached path:

- rejects conflicting listen matches before publishing a router;
- clones route/upstream/plugin-independent data and preserves route order;
- accepts no binding only when the effective route has no stream plugin;
- accepts one binding only when descriptor factory, scope, provenance and
  concrete owner identify the single `protocol`-phase `mqtt-proxy` owner for
  that route/service winner;
- creates the MQTT serving closure from the supplied plugin instance without
  calling `Init`, schema validation, decode, secret materialization or
  `PostInit`;
- exposes only `RouteIDs` and existing serving behavior, never a mutable route
  slice or binding handle.

Keep `Router.mu`, `enabledPlugins`, `NewRouter`, and `Reload` for the legacy
owner, but mark detached routers frozen so their publicly reachable `Reload`
surface cannot mutate a published snapshot. Task 9 removes the compatibility
state after atomic installation. Run the existing router
corpus against both common entry assembly paths where practical, including
order, conflicts, remote CIDR, retry, weighted RR/chash, MQTT preread and idle
timeout.

## T8-2: Decode and plan the final stream publication

**Create:** `pkg/compiler/stream.go`, `pkg/compiler/stream_plan.go`, focused
planner tests.

First extend the managed-resource taxonomy so singleton `plugins` changes affect
both HTTP and stream publications. Add focused `pkg/generation` tests for this
contract; do not extend `ssls`, `plugin_configs`, consumers, global rules or
plugin metadata into the stream domain.

Then decode only the normalized, dependency-closed stream candidate into owned:

- ordered `stream_routes`;
- keyed services and upstreams;
- the dynamic `/plugins` stream subset and a separate document-present bit.

Preserve the distinction between absent `/plugins` and a present empty list.
When absent, use cloned `EffectiveConfig.StreamPlugins`; when present, use only
entries whose descriptor has `stream: true`, including a non-nil empty set that
disables every stream plugin. HTTP-only entries never enable stream factories.

For each route, before materialization:

1. require a stable route ID and clone every nested value;
2. preserve legacy service lookup semantics exactly: read the referenced service
   only when `service_id` is set and the route does not set `upstream_id`;
3. when the service is read, merge service plugins first and route plugins second so route config,
   including `_meta.disable`, is authoritative;
4. preserve the exact source key of every surviving winner (`stream_routes/id`
   or `services/id`), rather than relabeling inherited config as route-owned;
5. preserve route inline/referenced upstream precedence, otherwise inherit service inline or
   referenced upstream, then resolve the exact upstream resource;
6. reject missing services/upstreams, unresolved discovery, every non-nil
   stream upstream TLS config, unsupported scheme and conflicting listens before
   plugin side effects so TLS is never silently downgraded to cleartext;
7. reject more than one enabled effective protocol plugin and unsupported
   stream factories; raw TCP remains valid with zero winners.

Planner output carries cloned resolved route/service context plus one optional
declarative binding request. It carries no plugin instance, lease, task,
materializer, Store getter or listener.

Add parity cases for every existing `resolveStreamRoutesWithServices` behavior,
plus route `_meta.disable` suppressing an inherited service plugin, route
`upstream_id` suppressing the legacy service merge, static-vs-dynamic enabled
sets, dynamic empty list, source provenance, deterministic route order and no
HTTP-domain leakage.

## T8-3: Materialize only the final stream winner

**Create:** `pkg/compiler/stream_binding_plan.go` and focused tests.

Convert the optional final request into one `effectiveBindingSpec` with:

- `domain: generation.DomainStream`;
- execution owner `stream_routes/<route-id>`;
- `effectiveBindingContextStream` containing the fully resolved cloned route
  and referenced service;
- route scope with provenance matching the exact route or service source;
- the exact `SecretPluginConfig` occurrence owned by the preparation attempt;
- no HTTP runtime context, public API registry, consumer binding, traffic-split
  runtime, system binding or composite-child injection performed by this caller.

Call the same package-private Task 6 materializer used by HTTP. Require exactly
one returned binding, validate it against the declarative plan, then pass it to
`stream.CompileRouter`. A missing, duplicate, foreign-domain, losing-source or
wrong-factory occurrence fails before publication. Do not quarantine a broken
stream route independently: the stream candidate fails closed and its prepared
owners unwind in reverse order.

Tests must prove route and inherited-service secrets use their exact occurrence,
shadowed/disabled losers never construct, candidate and recovery share the path,
and failure after a later route releases every earlier binding exactly once.

## T8-4: Attach and revoke one stream snapshot

**Create/modify:** `pkg/compiler/stream_snapshot.go`,
`prepared_generation.go`, `worker_factory.go`, and focused lifecycle tests.

`compileAndAttachStream` obtains only the attempt-owned stream candidate, plans
and materializes it, compiles the detached router, and attaches:

```go
type StreamSnapshot struct {
    artifact generation.GenerationArtifact
    router   *stream.Router
    closed   atomic.Bool
}
```

`Revision` and `Router` return inert values after revoke. Attachment serializes
with `PreparedGeneration.Close` on the existing preparation gate. Extend the
public API guard deliberately for `PreparedGeneration.Stream`; no lifecycle,
lease, task, capability, binding or cleanup handle becomes public.

Invoke stream compilation from the single candidate/recovery transfer path
after HTTP compilation and before factory ownership transfer. A stream failure
must revoke the already attached HTTP snapshot and clean all generation owners.
Add an explicit checkpoint so cancellation/failure windows are deterministic.

## T8-5: Compatibility, boundary and integration gate

Minimum behavior coverage:

- legacy stream package corpus remains green;
- detached raw TCP and MQTT route behavior matches legacy entry assembly;
- route/service/upstream precedence and exact provenance match the frozen table;
- absent dynamic list falls back to static stream plugins, present empty list
  disables them, and HTTP-only dynamic entries do not leak;
- route/service exact occurrence, loser non-materialization and reverse cleanup;
- candidate/recovery stream snapshots and HTTP-only/stream-only/combined cases;
- input isolation, attach/close race and inert accessors after close.

Static guards must prove:

- detached stream files do not import Store, compiler, secret, runtime lifecycle
  or configuration providers and do not call plugin factories or listeners;
- compiler stream files do not import `pkg/server` or read Store/global state;
- `pkg/server` and `pkg/stream/runtime.go` have no Task 8 diff;
- C6 ApplyTicket and no-production-prepare restrictions remain intact;
- Task 8 has zero production activation callers.

Run impact-scoped gates:

```bash
source .envrc
export GOFLAGS=-mod=readonly

go test ./pkg/generation ./pkg/compiler ./pkg/stream \
  -run '^(TestCompileRouter|TestDetachedStream|TestStreamPlan|TestCompileAndAttachStream|TestStreamSnapshot|TestPreparedGeneration|TestWorkerCompilerFactory)' \
  -count=1
go test ./pkg/stream -count=1
go test -race ./pkg/compiler ./pkg/stream \
  -run '^(TestCompileRouter|TestCompileAndAttachStream|TestStreamSnapshot|TestPreparedGeneration|TestRouter)' \
  -count=1
golangci-lint run ./pkg/generation/... ./pkg/compiler/... ./pkg/stream/...
go run ./cmd/capability-gen -repo-root . -check
make build
git diff --check
```

Run an independent merge-level review, repair every Critical/Important finding,
rerun invalidated gates, commit Task 8 locally, and only then fast-forward local
`master`. Do not push or create a PR.

## Task 9 handoff ledger

Task 8 intentionally leaves these owners alive:

- Store stream-route/service/upstream preparation and last-good commit;
- server `resolveStreamRoutesWithServices` and provider event reload path;
- `stream.Runtime` listener ownership and `Runtime.Reload`;
- `stream.Router.mu`, enabled-name compatibility state and `Router.Reload`;
- journal stage/activate/commit/rollback/finalize and acknowledgement;
- atomic HTTP/TLS/stream runtime installation and connection-generation
  retirement.

Task 9 must install the prepared HTTP/TLS/stream generation as one activation,
bind accepted connection lifetime to generation retirement, switch provider and
journal control, then delete the compatibility owners in the same reviewed unit.
