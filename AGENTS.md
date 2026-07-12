# AGENTS.md

## Project Overview

`apisix-go` is a Go implementation of the Apache APISIX data plane. It remains under development and is not production ready, but the current branch has a verified Go-native APISIX 3.17 parity baseline: 100 of 104 default plugins are registered, with the remaining four requiring external or native runtimes.

This is a single Go module: `github.com/wklken/apisix-go`.

Key runtime pieces:

- `main.go` enters the Cobra CLI in `cmd/root.go`.
- Configuration is loaded with Viper from `conf/config-default.yaml` by default, or from `-c/--config`.
- The HTTP server is built in `pkg/server` and currently listens on `:8080`.
- Route building lives in `pkg/route` and uses `go-chi/chi`.
- Runtime resources are stored through `pkg/store` in bbolt and are fed by the etcd watcher in `pkg/etcd`.
- APISIX plugins live under `pkg/plugin/<plugin_name>` and are registered in `pkg/plugin/init.go`.
- Proxying, load balancing, and transport behavior live under `pkg/proxy`.
- Kafka PubSub, Dubbo/http-Dubbo, and MQTT stream handling have explicit protocol owners under `pkg/plugin` and `pkg/stream`; general stream-plugin chaining and stream mTLS remain deferred.

## Setup Commands

- Use Go 1.26 as the project target from `go.mod`. Run `source .envrc` before Go commands; it keeps the toolchain, caches, temporary files, and installed binaries under the ignored checkout-local `.cache/` directory and does not depend on GVM or a user-level Go environment file.
- Download dependencies after sourcing `.envrc`: `source .envrc && go mod download`.
- Install formatting tools: `make init`.
- Do not run `make dep` casually. It runs `go mod tidy` and `go mod vendor`; use it only when dependency or vendoring changes are intentional.

### Checkout-local Go cache

Run these commands from the repository root in every new shell:

```bash
source .envrc
go version
go test ./... -count=1
```

`.envrc` creates and uses these ignored paths under `.cache/`:

| Purpose | Environment variable | Checkout path |
|---|---|---|
| Go toolchain/module downloads | `GOMODCACHE` | `.cache/go-mod` |
| Build cache | `GOCACHE` | `.cache/go-build` |
| Go workspace/bin directory | `GOPATH` / `GOBIN` | `.cache/go` / `.cache/bin` |
| Temporary build/test files | `GOTMPDIR` / `TMPDIR` | `.cache/tmp` |
| Test telemetry | `TEST_TELEMETRY_DIR` | `.cache/telemetry` |

`source .envrc` is required for the current shell; `direnv allow` is not required. Do not run `go`, `go test`, `go build`, or `make` in a fresh shell before sourcing it, otherwise Go may fall back to user-level caches such as macOS `/private` paths and trigger unnecessary permission prompts. Verify the active paths with `env | rg '^(GOPATH|GOBIN|GOCACHE|GOMODCACHE|GOTMPDIR|TMPDIR|TEST_TELEMETRY_DIR)='`. The cache is disposable: stop running Go processes, remove `.cache/` if a clean re-download is needed, then run `source .envrc` again. Never commit `.cache/`.

## Development Workflow

- Build the binary: `make build`. This writes `./apisix`, which is ignored by git and should not be committed.
- Run the server after building: `make serve`.
- Run with live rebuilds: `make live`. This uses `github.com/cosmtrek/air@v1.51.0`.
- Run a specific config manually: `go run . -c conf/config.yaml`.
- The default config path is `conf/config-default.yaml`; `conf/config.yaml` contains local overrides and an example admin key.
- `conf/config-default.yaml` says not to modify default configurations there. Prefer custom settings in `conf/config.yaml`.
- Running the server is not dependency-free: the current server starts an etcd watcher from `deployment.etcd` config and creates `apisix-go-store.db` in the working directory.

## Testing Instructions

- Run the full repository gate with the checkout-local runtime: `source .envrc && go test ./... -count=1`.
- Run a package subset: `go test ./pkg/plugin/redirect` or `go test ./pkg/...`.
- The repository contains focused unit, route-chain, protocol-fixture, and lifecycle tests across the supported plugin surface; `go test ./...` is the full repository gate, not only a compile smoke check.
- For concurrency-sensitive changes, run the focused race gate as well, for example `source .envrc && go test -race ./pkg/etcd ./pkg/plugin/server_info ./pkg/server -count=1`.
- Run a build smoke check for code changes: `source .envrc && make build`.
- The Makefile has no `test` or `lint` targets. Do not invent them in status reports.
- If a check already fails before your change, record the exact package, file, line, and message. Do not report a skipped or failing check as passing.
- For docs-only changes, a markdown/diff review is enough unless the documented commands themselves changed.

## Code Style

- Match existing Go style and package organization. Keep changes surgical.
- Format touched Go files with the same tools as `make fmt`: `golines` with max line length 120 and `gofumpt`.
- `make fmt` rewrites the tree. If you use it, inspect the diff and keep only changes related to the task.
- Prefer existing project dependencies and patterns before adding new packages.
- Plugin package directories use snake_case, while APISIX plugin names in config use hyphenated names such as `key-auth`.
- Plugin implementations usually embed `base.BasePlugin`, define `priority`, `name`, and `schema`, expose a config struct through `Config()`, and fill defaults in `PostInit()`.
- When adding or renaming a plugin, update `pkg/plugin/init.go` so `plugin.New()` can instantiate it.
- If feature support changes, update the relevant README plugin checklist entry.

## APISIX Plugin Parity Scope

- The current parity snapshot is 100/104 registered default plugins, 89 checklist entries at the documented Go-native monitoring level, and 9 explicit native/runtime or separate-subsystem deferrals. The four missing registrations are `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, and `inspect`.
- The authoritative status artifacts are [`README.md`](README.md), [`docs/apisix-3.17-plugin-parity-checklist.md`](docs/apisix-3.17-plugin-parity-checklist.md), [`docs/apisix-3.17-plugin-parity-execution-todo.md`](docs/apisix-3.17-plugin-parity-execution-todo.md), and [`docs/apisix-3.17-remaining-plugin-todo.md`](docs/apisix-3.17-remaining-plugin-todo.md). Keep all four aligned when parity behavior changes.
- OpenResty-native, NGINX-native, and Lua-runtime-native parity is not required unless the user explicitly asks for a Go-native approximation.
- Treat missing/deferred official defaults as native/runtime features that are not required for normal parity work. Current out-of-scope defaults are `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, and `inspect`; do not add placeholder Go implementations for them.
- `serverless-pre-function` and `serverless-post-function` have bounded compatibility implementations, but full Lua/OpenResty parity is intentionally out of scope. Do not expand them into a general Lua runtime or claim full phase/streaming fidelity.
- For native-only features, document the unsupported status in README/checklist/plan files instead of adding placeholder Go implementations.
- Examples of out-of-scope native behavior include OpenResty phase timing, `ngx_lua` APIs, Lua code execution, NGINX buffering internals, shared-dict/lrucache exactness, OCSP/TLS stapling internals, and external plugin runner protocol compatibility unless separately requested.
- The canonical remaining-plugin backlog is `docs/apisix-3.17-remaining-plugin-todo.md`, grouped into Logger, Auth, AI, Observability, and Others.

## Configuration Notes

- Cobra defines `--config` / `-c`; Viper also reads environment variables with prefix `APISIXGO` and maps dots to underscores.
- `deployment.role_traditional.config_provider` is currently `etcd` in `conf/config.yaml`.
- When `server-info` is enabled with traditional etcd configuration, the server reports under `<deployment.etcd.prefix>/data_plane/server_info/<apisix-id>` using `plugin_attr.server-info.report_ttl` and renews the lease until shutdown. Data-plane mode intentionally does not write this registration record.
- TCP stream routing is enabled through `apisix.proxy_mode` plus `apisix.stream_proxy.tcp`; stream routes are loaded from the `stream_routes` store bucket. The current main-server stream owner is `mqtt-proxy`.
- The local bbolt store file `apisix-go-store.db` is generated at runtime and ignored by git.
- Do not treat the example admin key in `conf/config.yaml` as a production secret.

## Build and Deployment

- Local build: `make build`.
- Docker build: `docker build -t apisix-go .`.
- The Dockerfile uses a Go 1.26.4 Alpine builder and an Alpine runtime image.
- The container entrypoint is `/usr/bin/apisix -c /usr/local/apisix/conf/config.yaml`.

## Pull Request Guidelines

- Before committing code changes, run `go test ./...` and `make build`, then clean generated artifacts such as `./apisix` unless they are intentionally part of the task.
- For docs-only changes, do not run broad mutating commands.
- Keep dependency changes explicit: explain why `go.mod`, `go.sum`, or vendored files changed.
- Report verification honestly, including pre-existing failures and commands not run.
