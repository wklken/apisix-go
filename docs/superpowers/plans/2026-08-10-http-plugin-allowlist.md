# HTTP Plugin Allowlist Implementation Plan

> **Execution:** Use `fast-plan-impl` with at most three bounded implementation
> workers. Workers may implement and run focused verification only; they may
> not commit, push, open a PR, merge, or delegate.

**Goal:** Close production-readiness finding PR-013 by making
`config.plugins` the immutable capability boundary for every reachable HTTP
plugin factory, including nested factories, while retaining last-good routes
and exposing route-build failure through the existing config-apply readiness
metrics.

**Baseline:** `origin/master@c4133032e06102a61de88731916581b673ffa1b4`

**Architecture:** `plugin.EnabledSet` clones the configured exact names. Every
`route.Builder.BuildStrict` generation installs a strict set, validates each
reachable source map before precedence merging, and checks membership again at
the final factory boundary. System-injected `request-context` is the only
bypass. Consumer and consumer-group plugins remain lazy under the current
architecture and fail the authenticated request closed. Route reload returns
its build error so standalone and etcd owners can retain the last-good handler
and update the existing fixed-cardinality config-apply metrics.

**Tech stack:** Go 1.26, Viper config loading, chi route builder, bbolt-backed
resources, existing Prometheus config-apply metrics.

## Confirmed current facts

- `Config.Plugins` is parsed, but `plugin.New(name)` exposes every registered
  HTTP factory and ordinary route materialization never checks the configured
  list.
- Special endpoints independently check the list, so operator-visible
  enablement and ordinary route execution currently disagree.
- A reachable disabled `mcp-bridge` route can install a handler that starts a
  local process on request.
- `BuildStrict` and server startup already propagate initial route-build
  failures; dynamic reload already retains the installed last-good handler.
  This plan must reuse those mechanisms.
- Consumers and consumer groups are materialized lazily on authenticated
  requests and do not participate in `ConfigSnapshot` or route-reload bucket
  selection. Orphan services and plugin-config resources are not materialized
  until a route references them.
- `multi-auth` directly constructs child authentication plugins and `workflow`
  directly constructs limiter actions. Guarding only `plugin.New` leaves both
  bypasses open.
- Plugin metadata is inert until an enabled resource creates that plugin. It
  does not expand the enabled set and must not be rejected merely because its
  plugin is disabled.
- `request-context` is always injected by the Builder. `client-control` may be
  auto-generated for the global body limit but is not a system capability
  bypass.

## Fixed behavior contract

### Config and immutable set

- `plugins` is an exact-name allowlist. A configured empty list enables no
  user-controlled HTTP plugin.
- Config load rejects an empty name, a name with leading/trailing whitespace,
  and an exact duplicate. Names are never trimmed or case-normalized.
- Unknown names remain valid in the static list because the default config
  intentionally contains native/deferred APISIX names. If a reachable resource
  uses an unknown enabled name, existing factory validation returns “not
  supported”.
- `plugin.NewEnabledSet(names)` clones the input. Later mutation of the source
  slice cannot change membership.
- `BuildStrict` always installs a non-nil strict set, including a strict empty
  set. A nil set is allowed only for legacy package-private helper tests that
  call initialization functions without `BuildStrict`.

### Reachable resource enforcement

- Check original source maps before precedence merging so the error retains
  provenance and an overridden disabled entry cannot disappear silently.
- Error text includes the disabled plugin name, resource kind, and resource
  identifier for:
  - direct route plugins (`route`, route ID);
  - referenced plugin configs (`plugin_config`, plugin-config ID);
  - referenced services (`service`, service ID);
  - global rules (`global_rule`, global-rule ID);
  - consumers (`consumer`, username);
  - consumer groups (`consumer_group`, group ID).
- Orphan service/plugin-config records are inert. They are checked when a route
  materializes them; this PR does not add provider/store-wide validation.
- Consumer and consumer-group plugins are checked when the authenticated
  request builds its lazy chain. A disabled name returns HTTP 500 and no child
  factory or plugin handler executes. This PR does not add consumer-triggered
  route reloads or claim `BuildStrict` rejection for lazy consumer data.
- A Builder-created `request-context` instance bypasses membership. A
  user-supplied `request-context` entry is ordinary resource config and must be
  enabled.
- Auto-generated `client-control` must be enabled. `_meta.disable=true` does
  not bypass membership because the capability boundary is checked before
  metadata parsing or factory creation.
- Disabled plugin metadata may remain stored and inert. The existing special
  global error-log path retains its static-list check.

### Nested factories

- `multi-auth` and `workflow` expose
  `SetPluginEnabledChecker(func(string) bool)`. A nil checker preserves direct
  legacy unit construction; the strict Builder always injects its immutable
  checker before `PostInit`.
- `multi-auth` rejects a disabled child auth name before `newAuthPlugin`.
- `workflow` rejects disabled `limit-req`, `limit-conn`, and `limit-count`
  actions before constructing their plugin. The built-in `return` action is
  not a plugin capability and remains available.

### Startup, reload, and readiness

- Initial startup with a reachable disabled route/service/plugin-config/global
  plugin returns an error before serving.
- Dynamic route-build failure never installs a partial handler and retains the
  last-good handler.
- `Server.reload(ctx)` returns the `BuildStrict` error. Its etcd scheduler owner
  records config-apply failure on error and success only after replacement.
- Standalone `applyStandaloneSnapshot` accepts `reloadRoutes func() error` and
  propagates that error. The acknowledged watcher callback records exactly one
  failure or success and retains/replays its last-good snapshot state.
- Consumer request-time failures and inert orphan resources do not change
  config-apply readiness; they are outside the route-build generation.

## Fixed interfaces

```go
// pkg/plugin/enabled.go
type EnabledSet struct { /* unexported cloned membership */ }
func NewEnabledSet(names []string) EnabledSet
func (s EnabledSet) Contains(name string) bool

// Implemented by multi-auth and workflow; route owns the private interface.
type pluginEnabledCheckerSetter interface {
    SetPluginEnabledChecker(func(string) bool)
}

// pkg/server private integration.
func (s *Server) reload(ctx context.Context) error
func applyStandaloneSnapshot(
    result config.StandaloneReloadResult,
    err error,
    syncStore func() error,
    reloadRoutes func() error,
    reloadStreams func(),
) error
```

## Work Unit 01: Core set, config validation, nested capabilities

**Exclusive ownership:**

- `pkg/plugin/enabled.go` (new)
- `pkg/plugin/enabled_test.go` (new)
- `pkg/config/init.go`
- `pkg/config/init_test.go`
- `pkg/plugin/multi_auth/plugin.go`
- `pkg/plugin/multi_auth/plugin_test.go`
- `pkg/plugin/workflow/plugin.go`
- `pkg/plugin/workflow/plugin_test.go`

### Regression-first tests

- `TestEnabledSetClonesSourceAndSupportsStrictEmpty`:
  enabled membership works, strict empty denies, and source-slice mutation does
  not alter the set.
- `TestLoadRejectsInvalidHTTPPluginAllowlist`:
  empty, surrounding whitespace, and duplicate names fail with index/name
  context; an unknown unique name is accepted unchanged.
- `TestMultiAuthRejectsDisabledNestedPluginBeforeConstruction`:
  enabled `multi-auth` plus disabled child auth fails `PostInit`; enabled
  children retain existing behavior.
- `TestWorkflowRejectsDisabledNestedPluginBeforeConstruction`:
  disabled limiter actions fail, enabled limiter actions pass, and `return`
  remains independent of plugin membership.

Run before production edits and record the expected compile/behavior failures:

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/plugin ./pkg/plugin/multi_auth ./pkg/plugin/workflow -run "^(TestEnabledSet|TestLoadRejectsInvalidHTTPPluginAllowlist|TestMultiAuthRejectsDisabledNestedPluginBeforeConstruction|TestWorkflowRejectsDisabledNestedPluginBeforeConstruction)$" -count=1'
```

### Implementation

- Add the immutable set without changing `plugin.New` or deleting factories.
- Validate config names after unmarshal and before publishing `GlobalConfig`.
- Add optional checker fields/setters to the two nested-factory plugins.
- Check nested membership before every child constructor.

### Focused verification

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/plugin ./pkg/plugin/multi_auth ./pkg/plugin/workflow -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/config/... ./pkg/plugin/multi_auth/... ./pkg/plugin/workflow/...'
git diff --check
```

## Work Unit 02: Builder provenance and all materialization paths

**Depends on:** accepted Work Unit 01 interfaces.

**Exclusive ownership:**

- `pkg/route/builder.go`
- `pkg/route/plugin_allowlist_test.go` (new)
- `pkg/resource/route.go`

### Regression-first tests

- Table-test reachable disabled plugins from route, referenced
  plugin-config, referenced service, and global rule. Each `BuildStrict` call
  returns a nil handler and an error containing plugin/kind/ID.
- Enabled controls build successfully.
- An overridden disabled entry in a referenced source still fails with that
  source’s provenance.
- Strict empty denies all user plugins while system `request-context` builds.
- User-configured `request-context`, generated `client-control`, and
  `_meta.disable=true` do not bypass membership.
- Metadata for a disabled plugin is inert and does not fail a build that never
  materializes that plugin.
- Consumer and group cases authenticate a request, receive HTTP 500, and prove
  the disabled plugin handler/factory path did not run; enabled controls run.
- Nested `multi-auth`/`workflow` checkers are injected before `PostInit`.
- Disabled `mcp-bridge` makes `BuildStrict` return `(nil, error)`. The test must
  not issue a request unless a handler is unexpectedly installed; a marker
  command must remain unstarted.
- Mutating `config.GlobalConfig.Plugins` after `BuildStrict` starts cannot alter
  the Builder generation or its lazy consumer checks.

Run the new named cases before production edits and record the expected
failures:

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "^(TestHTTPPluginAllowlist|TestDisabledMCPBridge)$" -count=1'
```

### Implementation details

- Add `enabledPlugins *plugin.EnabledSet` to `Builder`; clone from
  `config.GlobalConfig.Plugins` at the start of every `BuildStrict`.
- Add resource-source validation helpers and preserve route/plugin-config/
  service provenance before merging.
- Add `ID` to `resource.GlobalRule` so existing JSON snapshots retain rule
  identity in strict errors.
- Use a small internal init-options helper for the system-owned
  `request-context` bypass; the ordinary/final factory boundary remains strict.
- Inject the nested checker after `Init`/parse and before `PostInit`.
- Validate consumer/group source maps before authentication-plugin filtering
  and lazy chain caching. Return errors to the existing HTTP 500 boundary.
- Do not validate the plugin-metadata bucket or introduce new Store scans.

### Focused verification

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "(HTTPPluginAllowlist|DisabledPlugin|DisabledMCPBridge|ConsumerPlugin)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route -run "(HTTPPluginAllowlist|DisabledPlugin|ConsumerPlugin)" -count=1'
bash -lc 'source .envrc && go test ./pkg/route -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/route/... ./pkg/resource/...'
git diff --check
```

## Work Unit 03: Reload result and config-apply readiness

**Depends on:** the existing `BuildStrict` error contract; combine with Work
Unit 02 before final acceptance.

**Exclusive ownership:**

- `pkg/server/reload.go`
- `pkg/server/reload_test.go`
- `pkg/server/server.go`
- `pkg/server/server_test.go`

### Regression-first tests

- `TestReloadRetainsLastGoodHandlerAndReportsDisabledPlugin` installs a known
  handler, makes the next strict build fail, and asserts the old handler/stoppper
  remains while `reload` returns the build error.
- `TestReloadSchedulerRecordsConfigApplyReadiness` asserts one failure + ready
  zero on route-build error and ready one only after a successful replacement.
- `TestApplyStandaloneSnapshotPropagatesRouteBuildFailure` asserts route-build
  error is returned, stream reload is not spuriously treated as total success,
  and the callback owner records exactly one failure rather than success.
- Context-cancelled scheduler shutdown does not record a config failure.

Run the named cases before production edits and record the expected failures:

```bash
bash -lc 'source .envrc && go test ./pkg/server -run "^(TestReloadRetainsLastGoodHandlerAndReportsDisabledPlugin|TestReloadSchedulerRecordsConfigApplyReadiness|TestApplyStandaloneSnapshotPropagatesRouteBuildFailure)$" -count=1'
```

### Implementation details

- Return contextual build/panic errors from `reload`; keep builder cleanup and
  handler replacement transactional.
- Keep `runReloadScheduler`’s `func()` boundary; its server closure owns
  success/failure metric updates after `reload` returns.
- Change only the private standalone callback/helper signature to
  `reloadRoutes func() error`; propagate the error through the existing
  acknowledged callback so its current rollback/replay behavior remains.
- Do not count consumer request-time rejection or inert orphan resources as a
  config-apply failure.

### Focused verification

```bash
bash -lc 'source .envrc && go test ./pkg/server -run "(DisabledPluginReload|ReloadRetains|ApplyStandalone.*RouteBuild|ReloadScheduler)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/server -run "(Reload|ApplyStandalone|Shutdown)" -count=1'
bash -lc 'source .envrc && go test ./pkg/server -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/server/...'
git diff --check
```

## Integration, review, and delivery gate

After all work units are accepted, the main agent inspects the combined diff
and runs:

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/plugin ./pkg/plugin/multi_auth ./pkg/plugin/workflow ./pkg/route ./pkg/server -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route ./pkg/server -run "(HTTPPluginAllowlist|DisabledPlugin|ConsumerPlugin|Reload|ApplyStandalone)" -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/config/... ./pkg/plugin/multi_auth/... ./pkg/plugin/workflow/... ./pkg/route/... ./pkg/resource/... ./pkg/server/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

Then:

1. Audit every production `plugin.New` and direct nested factory reachable from
   HTTP route materialization; document any intentional bypass.
2. Confirm no Store/provider scan, consumer reload trigger, schema surface, or
   registry deletion was added.
3. Request an independent read-only code review, remediate all High/Medium
   findings, and refresh the relevant gates.
4. Stage exactly the plan plus accepted implementation paths, commit
   `fix(plugin): enforce the configured HTTP allowlist`, push
   `codex/prod-ready-http-allowlist`, and open one ready PR against `master`.
5. Merge only after independent approval and all required GitHub checks pass.

## Explicit deferrals

- Provider/store-wide rejection of orphan service/plugin-config/consumer data.
- Route rebuilds on consumer or consumer-group updates.
- Removing disabled factories from the compiled registry.
- Rejecting native/deferred names that are listed but not implemented in Go.
- General APISIX phase execution (Plan 11).
- Stream plugin allowlist behavior; this plan owns HTTP plugins only.
