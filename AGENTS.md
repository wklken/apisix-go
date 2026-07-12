# AGENTS.md

## Project Overview

`apisix-go` is a Go implementation of the Apache APISIX data plane. The README explicitly says the project is still under development and is not production ready.

This is a single Go module: `github.com/wklken/apisix-go`.

Key runtime pieces:

- `main.go` enters the Cobra CLI in `cmd/root.go`.
- Configuration is loaded with Viper from `conf/config-default.yaml` by default, or from `-c/--config`.
- The HTTP server is built in `pkg/server` and currently listens on `:8080`.
- Route building lives in `pkg/route` and uses `go-chi/chi`.
- Runtime resources are stored through `pkg/store` in bbolt and are fed by the etcd watcher in `pkg/etcd`.
- APISIX plugins live under `pkg/plugin/<plugin_name>` and are registered in `pkg/plugin/init.go`.
- Proxying, load balancing, and transport behavior live under `pkg/proxy`.

## Setup Commands

- Use Go 1.26 as the project target from `go.mod`. Run `source .envrc` before Go commands; it keeps the toolchain, caches, temporary files, and installed binaries under the ignored checkout-local `.cache/` directory and does not depend on GVM or a user-level Go environment file.
- Download dependencies after sourcing `.envrc`: `source .envrc && go mod download`.
- Install formatting tools: `make init`.
- Do not run `make dep` casually. It runs `go mod tidy` and `go mod vendor`; use it only when dependency or vendoring changes are intentional.

## Development Workflow

- Build the binary: `make build`. This writes `./apisix`, which is ignored by git and should not be committed.
- Run the server after building: `make serve`.
- Run with live rebuilds: `make live`. This uses `github.com/cosmtrek/air@v1.51.0`.
- Run a specific config manually: `go run . -c conf/config.yaml`.
- The default config path is `conf/config-default.yaml`; `conf/config.yaml` contains local overrides and an example admin key.
- `conf/config-default.yaml` says not to modify default configurations there. Prefer custom settings in `conf/config.yaml`.
- Running the server is not dependency-free: the current server starts an etcd watcher from `deployment.etcd` config and creates `apisix-go-store.db` in the working directory.

## Testing Instructions

- Run all package checks: `go test ./...`.
- Run a package subset: `go test ./pkg/plugin/redirect` or `go test ./pkg/...`.
- There are currently no committed `*_test.go` files, so `go test ./...` mainly acts as a compile and vet smoke check until focused tests are added.
- Run a build smoke check for code changes: `make build`.
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

- OpenResty-native, NGINX-native, and Lua-runtime-native parity is not required unless the user explicitly asks for a Go-native approximation.
- Treat missing/deferred official defaults and serverless function plugins as native/runtime features that are not required for normal parity work. Current out-of-scope defaults/features: `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, `inspect`, `serverless-pre-function`, and `serverless-post-function`; do not add them to normal implementation plans or TODO lists.
- `serverless-pre-function` and `serverless-post-function` execute Lua/OpenResty code and should stay documented as OpenResty-native and not required; do not add placeholder Go implementations for them.
- For native-only features, document the unsupported status in README/checklist/plan files instead of adding placeholder Go implementations.
- Examples of out-of-scope native behavior include OpenResty phase timing, `ngx_lua` APIs, Lua code execution, NGINX buffering internals, shared-dict/lrucache exactness, OCSP/TLS stapling internals, and external plugin runner protocol compatibility unless separately requested.
- The canonical remaining-plugin backlog is `docs/apisix-3.17-remaining-plugin-todo.md`, grouped into Logger, Auth, AI, Observability, and Others.

## Configuration Notes

- Cobra defines `--config` / `-c`; Viper also reads environment variables with prefix `APISIXGO` and maps dots to underscores.
- `deployment.role_traditional.config_provider` is currently `etcd` in `conf/config.yaml`.
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
