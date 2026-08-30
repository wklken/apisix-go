# AGENTS.md

## Project Overview

`apisix-go` is a drop-in Go replacement for the Apache APISIX 3.17 HTTP data
plane. Observable APISIX 3.17 behavior is authoritative. Do not add
apisix-go-specific configuration, publication, security, or qualification
policy when APISIX already defines the behavior.

This is a single Go module: `github.com/wklken/apisix-go`.

Key runtime pieces:

- `main.go` enters the Cobra CLI in `cmd/root.go`.
- Static configuration is loaded by the presence-aware `pkg/config` loader from builtins, `conf/config-default.yaml`, an optional `-c/--config` file, APISIX file-template expansion, and APISIX 3.17 reserved environment overrides.
- `pkg/plugin/registry.go` directly owns implemented plugin factories, aliases, and execution metadata. `pkg/capability/declarations.go` owns encrypted-field declarations.
- Providers submit desired snapshots to the single-writer `pkg/generation` coordinator, which owns in-memory desired and published state.
- `pkg/compiler` plans and materializes immutable HTTP/TLS and stream snapshots. `pkg/route` and `pkg/stream` contain detached snapshot compilers; `pkg/server` atomically activates them and owns generation leases.
- HTTP and TLS listeners come from effective configuration; the default HTTP listener is `0.0.0.0:9080`.
- APISIX plugins live under `pkg/plugin/<plugin_name>` and are instantiated through the Go registry consumed by `pkg/plugin/init.go`.
- Proxying, load balancing, and transport behavior live under `pkg/proxy`.
- Kafka PubSub, Dubbo/http-Dubbo, and MQTT stream handling have explicit protocol owners under `pkg/plugin` and `pkg/stream`. Stream currently supports raw TCP and at most one `mqtt-proxy` protocol binding; general stream-plugin chaining, UDP, PROXY protocol, discovery, and stream TLS remain deferred.

## Architecture Sources of Truth

- [`docs/design.md`](docs/design.md) is the canonical human architecture record; unavoidable compatibility differences live in `docs/architecture/adr/`.
- `pkg/plugin/registry.go` is the editable runtime registration source. Plugin name, priority, and schemas remain owned by each implementation's `Init` method.
- Current source and focused tests override stale prose. If a process document, design paragraph, generated projection, and code disagree, verify the current call chain and then update the owning source rather than averaging them.
- The current runtime is a single process with an in-process `Server + GenerationEngine`. External supervisor/worker, IPC activation, listener inheritance, and worker probation/restart are not implemented runtime contracts.

### Documentation lifecycle

- Plans, review reports, remediation ledgers, and dated snapshots are temporary process evidence. Before closing a development stage, move each still-current fact to its durable owner and remove the process artifact.
- Route cross-package architecture to `docs/design.md`; unavoidable compatibility differences to an ADR; plugin behavior to source and focused tests; operator procedures to maintained runbooks; future-agent invariants to the closest `AGENTS.md`.
- Branch names, commit hashes, checked TODOs, review verdicts, and old pass counts are point-in-time evidence. Do not copy them into `AGENTS.md` or use them as current qualification proof.
- Keep durable specialized contracts such as configuration, HTTP scope, release, and performance acceptance documents when they still govern current behavior. Filename dates alone do not determine whether a document is disposable.

Directory-scoped instructions inherit this file. Read the closest `AGENTS.md` before changing a core module:

| Area | Local instructions |
|---|---|
| Capability/configuration | `pkg/capability/AGENTS.md`, `pkg/config/AGENTS.md` |
| Desired/published state | `pkg/generation/AGENTS.md` |
| Compiler/runtime/secrets | `pkg/compiler/AGENTS.md`, `pkg/runtime/AGENTS.md`, `pkg/secret/AGENTS.md` |
| Serving/protocol | `pkg/server/AGENTS.md`, `pkg/route/AGENTS.md`, `pkg/stream/AGENTS.md` |
| Proxy/plugins/metrics | `pkg/proxy/AGENTS.md`, `pkg/plugin/AGENTS.md`, `pkg/plugin/logger_batch/AGENTS.md`, `pkg/observability/metrics/AGENTS.md` |

## Cross-package Runtime Invariants

- The only production publication path is provider -> `generation.Coordinator` -> compiler -> `GenerationEngine`. Do not restore mutable route builders or provider-owned activation.
- Publication order is `apply desired in memory -> compile/prepare -> atomic active-bundle swap -> commit coordinator state -> return acknowledgement to provider`. Publication failure preserves the exact predecessor and does not advance coordinator state.
- First-generation invalid state fails closed. `last-good` requires an exact same-domain published predecessor; a tombstone is deletion and never falls back.
- A prepared generation owns its secret materialization, task registry, resource leases, and HTTP/stream snapshots. Cleanup is retryable and ordered `quiesce -> resource finalize -> secret release`; a residual or deadline retains ownership.
- HTTP requests, hijacked connections, TLS callbacks, and stream connections pin the exact generation they use. A generation retires only after it owns no active domain and all leases drain.
- Background work in `pkg/plugin`, `pkg/proxy`, `pkg/route`, and `pkg/stream` must use the owned runtime task APIs; the AST/type gate for raw goroutines covers exactly those roots, not the whole repository.
- Secret materialization is allowed only through catalog-declared, exact generation/domain/resource scope. Never expose plaintext, ciphertext, or backend references in diagnostics.

## Setup Commands

- Use Go 1.26 as the project target from `go.mod`. Run `source .envrc` before Go commands; it shares downloaded toolchains, modules, build cache, and installed tools across this repository's worktrees while keeping mutable task outputs inside the active worktree. It does not depend on GVM or a user-level Go environment file.
- Download dependencies after sourcing `.envrc`: `source .envrc && scripts/go_cache.sh run -- go mod download`.
- Install the golangci-lint linter and formatter: `make init`.
- Do not run `make dep` casually. It runs `go mod tidy` and `go mod vendor`; use it only when dependency or vendoring changes are intentional.

### Worktree-aware Go cache

Run these commands from the repository root in every new shell:

```bash
source .envrc
go version
make cache-status
```

`.envrc` derives the shared root from Git's common directory, so every linked
worktree for this repository resolves the same `<main-checkout>/.cache/shared`
path. Content-addressed/download state is shared; task outputs remain isolated:

| Scope | Purpose | Environment variable | Path |
|---|---|---|---|
| Shared | Go toolchain/module downloads | `GOMODCACHE` | `<main-checkout>/.cache/shared/go-mod` |
| Shared | Build cache | `GOCACHE` | `<main-checkout>/.cache/shared/go-build` |
| Shared | Go workspace/bin directory | `GOPATH` / `GOBIN` | `<main-checkout>/.cache/shared/go` / `<main-checkout>/.cache/shared/bin` |
| Shared | golangci-lint cache | `GOLANGCI_LINT_CACHE` | `<main-checkout>/.cache/shared/golangci-lint` |
| Worktree-local | Temporary build/test files | `GOTMPDIR` / `TMPDIR` | `.cache/tmp` |
| Worktree-local | Test telemetry | `TEST_TELEMETRY_DIR` | `.cache/telemetry` |
| Worktree-local | Application binary | `BINARY_PATH` | `.cache/out/apisix` |
| Worktree-local | Benchmarks and coverage | `BENCH_DIR` / test flags | `.cache/bench` / `.cache/coverage` |

`source .envrc` is required for the current shell; `direnv allow` is not required. Do not run `go`, `go test`, `go build`, or `make` in a fresh shell before sourcing it, otherwise Go may fall back to user-level caches such as macOS `/private` paths and trigger unnecessary permission prompts. Verify the active paths with `make cache-status` or `env | rg '^(APISIX_GO_ROOT|APISIX_GO_SHARED_CACHE|GOPATH|GOBIN|GOCACHE|GOMODCACHE|GOLANGCI_LINT_CACHE|GOTMPDIR|TMPDIR|TEST_TELEMETRY_DIR)='`.

Make targets that use the shared Go build cache run through `scripts/go_cache.sh`, which registers an active lease for the command. Use the same runner for direct build, test, lint, install, and benchmark commands, for example `scripts/go_cache.sh run -- go test ./pkg/route -count=1`. A direct cache-using command outside the runner is invisible to coordinated cleanup.

The runner checks cache pressure at most once per hour after the final active lease exits. It requests cleanup when `GOCACHE` reaches 50 GiB or filesystem free space falls to 40 GiB, and it limits automatic cleanup to once every 12 hours. Override `CACHE_GC_MAX_GIB`, `CACHE_GC_MIN_FREE_GIB`, `CACHE_GC_CHECK_INTERVAL`, or `CACHE_GC_COOLDOWN` only for a measured operational need. `make cache-gc` applies the thresholds immediately; `make cache-clean-shared` ignores thresholds but refuses to clean while a participating command is active. Test the protocol with `make cache-gc-test`.

Do not remove the main checkout's entire `.cache/`: it contains the shared module cache, downloaded toolchain, and installed tools. `make clean` removes only the active worktree's application binary. After stopping the agent using a worktree, `make cache-clean-local` removes that worktree's temp/output directories and pre-migration duplicated Go/linter caches while preserving benchmark evidence, coverage, fixtures, and the shared cache. Use the coordinated targets instead of manually removing the shared build cache. Never commit `.cache/`.

## Development Workflow

- Build the binary: `make build`. After sourcing `.envrc` (the agent/worktree workflow) this writes the worktree-local `.cache/out/apisix`; without `.envrc`, it writes `./apisix` at the repo root. Both are ignored by Git and can be removed with `make clean`.
- Run the server after building: `make serve`.
- Run with live rebuilds: `make live`. This uses `github.com/cosmtrek/air@v1.51.0`.
- Run a specific config manually: `scripts/go_cache.sh run -- go run . -c conf/config.yaml`.
- The default config path is `conf/config-default.yaml`; `conf/config.yaml` contains local overrides and an example admin key.
- `conf/config-default.yaml` says not to modify default configurations there. Prefer custom settings in `conf/config.yaml`.
- Running the server is not dependency-free in etcd mode. Etcd and standalone providers must complete their initial desired-state submission before listeners start.

## Testing Instructions

- Repository-specific rule: verification is impact-scoped. This overrides generic instructions to run all existing tests, `make test`, or `go test ./...` after every code change.
- Derive the smallest credible test set from the changed packages, imports, call sites, and behavior. Start with an exact test such as `source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/redirect -run '^TestHandlerRedirectsWithRegexURI$' -count=1`, then expand only to directly affected package tests when needed.
- Do not use `go test ./...`, `go test ./pkg/...`, or `make test` as routine validation. Run a repository-wide aggregation only when the user explicitly requests it or the change itself affects repository-wide test/build infrastructure and no narrower check can prove correctness.
- Run `t/plugin` only when the change affects a specific integration manifest, the integration harness, or behavior that lacks a narrower test seam. Select the exact affected case, for example `source .envrc && scripts/go_cache.sh run -- go test ./t/plugin -run '^TestPluginIntegration/request-id/default-uuid-v4$' -count=1`; do not run `make test-integration` or the whole `t/plugin` package by default.
- Run only one real-process `t/plugin` command at a time because its cases use shared resources and fixed ports. Do not interrupt a relevant active run solely because it is slow when the process and its output/profile are still making progress.
- For concurrency-sensitive changes, run the focused race gate as well, for example `source .envrc && scripts/go_cache.sh run -- go test -race ./pkg/etcd ./pkg/plugin/server_info ./pkg/server -count=1`.
- Run a build smoke check for code changes: `source .envrc && make build`.
- `make test` runs the broad `./cmd/... ./pkg/...` unit-test aggregation, while `make test-integration` runs the real-process `t/plugin` suite. Both are opt-in broad gates under the rule above. `make lint` runs golangci-lint with the repository configuration.
- If a check already fails before your change, record the exact package, file, line, and message. Do not report a skipped or failing check as passing.
- For docs-only changes, a markdown/diff review is enough unless the documented commands themselves changed.

## Performance Benchmarking

Benchmark evidence is produced only through `scripts/benchmark.sh` via the Makefile `benchmark-*` targets; raw results, metadata, and profiles are written to the ignored `BENCH_DIR` (default `.cache/bench`) and never committed. Direct `go test -bench` runs through `scripts/go_cache.sh run --` are exploratory, not baseline evidence.

Workflow:

- `make init-bench` installs the pinned `benchstat` once; it does not touch `go.mod` or `vendor`.
- `make benchmark-runner-test` runs the fail-closed runner regression tests.
- `make benchmark-baseline` records an immutable baseline on the intended base; rerunning the same label is rejected.
- `make benchmark-current` records the change under test, then `make benchmark-compare` runs the pinned benchstat.
- `make benchmark-profile-cpu` / `benchmark-profile-mem` write separate CPU/heap profiles for diagnosis.

Acceptance contract:

- Before: declare the hypothesis, the affected benchmark rows, the primary metric, and a practical threshold.
- After: run `benchmark-current` with identical settings, then `benchmark-compare`. A rejected metadata or corpus fingerprint means the comparison is invalid.
- Report all affected rows and regressions; do not cherry-pick one size.
- Statistical significance is evidence against noise, not production impact.
- Allocation work reports exact B/op and allocs/op deltas; architectural latency claims also need a profile or end-to-end measurement tied to the hypothesis.
- Race/profile runs are diagnostic, never normal benchmark evidence.
- There is no uniform percentage threshold; without a pre-declared practical threshold, report measurements only and do not announce an optimization as verified.

Correctness:

- Run the focused tests and relevant race tests for the changed benchmark or production path, plus `make lint` and `make build`. Do not add `go test ./...` or unrelated `t/plugin` cases to performance verification.

## Code Style

- Match existing Go style and package organization. Keep changes surgical.
- Format touched Go files with `scripts/go_cache.sh run -- golangci-lint fmt`.
- `golangci-lint fmt` rewrites the tree. If you use it, inspect the diff and keep only changes related to the task.
- Prefer existing project dependencies and patterns before adding new packages.
- Plugin package directories use snake_case, while APISIX plugin names in config use hyphenated names such as `key-auth`.
- Plugin implementations usually embed `base.BasePlugin`, define `priority`, `name`, and `schema`, expose a config struct through `Config()`, and fill defaults in `PostInit()`.
- When adding or renaming a plugin, update `pkg/plugin/registry.go`. Add encrypted fields separately in `pkg/capability/declarations.go` only when the plugin actually materializes them.
- Keep behavior and regression evidence in plugin source and focused tests. The registry contains only runtime construction and execution facts.

## APISIX Plugin Parity Scope

- The official APISIX 3.17 source and tests define plugin behavior. The Go registry is runtime input, not a compatibility or readiness ledger.
- Do not infer compatibility or readiness from registration or inventory counts.
- OpenResty-native, NGINX-native, and Lua-runtime-native parity is not required unless the user explicitly asks for a Go-native approximation.
- Do not add placeholder Go implementations merely to increase inventory.
- `serverless-pre-function` and `serverless-post-function` have bounded compatibility implementations, but full Lua/OpenResty parity is intentionally out of scope. Do not expand them into a general Lua runtime or claim full phase/streaming fidelity.
- Record unavoidable native/runtime gaps in concise architecture documentation, not in runtime registry state.
- Examples of out-of-scope native behavior include OpenResty phase timing, `ngx_lua` APIs, Lua code execution, NGINX buffering internals, shared-dict/lrucache exactness, OCSP/TLS stapling internals, and external plugin runner protocol compatibility unless separately requested.

## Configuration Notes

- Cobra defines `--config` / `-c`; the loader expands APISIX file templates and implements only APISIX 3.17 reserved environment overrides.
- `deployment.role_traditional.config_provider` is currently `etcd` in `conf/config.yaml`.
- When `server-info` is enabled with traditional etcd configuration, the server reports under `<deployment.etcd.prefix>/data_plane/server_info/<apisix-id>` using `plugin_attr.server-info.report_ttl` and renews the lease until shutdown. Data-plane mode intentionally does not write this registration record.
- TCP stream routing is enabled through `apisix.proxy_mode` plus `apisix.stream_proxy.tcp`. Provider desired state is compiled into an immutable stream router; each accepted connection leases that exact generation.
- Do not treat the example admin key in `conf/config.yaml` as a production secret.

## Build and Deployment

- Local build: `make build`.
- Docker build: `make docker-build` (passes `VERSION`/`COMMIT`/`BUILD_TIME`/`GO_VERSION` build args so `apisix version` reports build metadata); plain `docker build -t apisix-go .` works with default build args.
- The Dockerfile uses a Go 1.26.6 builder and an Alpine runtime image.
- The container entrypoint is `/usr/bin/apisix -c /usr/local/apisix/conf/config.yaml`.

## Pull Request Guidelines

- Before committing code changes, run the impact-scoped tests required by Testing Instructions and `make build`, then run `make clean` unless the worktree-local binary is intentionally needed. Do not substitute a full repository or `t/plugin` aggregation.
- For docs-only changes, do not run broad mutating commands.
- Keep dependency changes explicit: explain why `go.mod`, `go.sum`, or vendored files changed.
- Report verification honestly, including pre-existing failures and commands not run.
