# apisix-go

This is an [apache/apisix](https://github.com/apache/apisix) Data Plane(DP) implemented via Go

This project is still under development and NOT READY FOR PRODUCTION!

## Quick start

Start `apisix-go` in standalone mode with the included example configuration:

```bash
source .envrc && go run . -c conf/config-example.yaml
```

In another terminal, send a request through the gateway:

```bash
curl http://127.0.0.1:9080/hello
```

The example route in [`conf/apisix.yaml`](conf/apisix.yaml) proxies the request
to the public `httpbingo.org` echo service. Stop the gateway with `Ctrl-C`.

## APISIX 3.17 parity status

The current Go-native parity baseline registers 100 of the 104 APISIX 3.17 default plugins (96.2%). The checklist tracks 89 plugins at the current supported monitoring level and 9 explicit native/runtime or separate-subsystem deferrals. The four missing registrations—`ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, and `inspect`—depend on external plugin runners or Lua/OpenResty features.

The detailed plugin comparison is maintained in [docs/plugins.md](docs/plugins.md). Protocol-specific boundaries are documented in [docs/design.md](docs/design.md).

## Features

- Small binary size and image size (<100M)
- Easy to deploy and scale
- Better performance with io plugins like `*-logger`
- Easy to extend with Go http middlewares or Go Plugins(develop and test is much easier)

### Supported

- [x] Route
- [x] Service
- [x] Upstream
- [x] Plugin Metadata
- [x] Global Rules
- [x] Plugin Attr
- [x] Consumer
- [x] Consumer Group (store/resource support and `consumer-restriction` path)
- [x] Plugin Config
- [ ] Script
- [ ] Secret (generic secret resource is not complete; plugin-level APISIX data-encryption fields are supported)

### Local Go environment

Source `.envrc` before running Go commands (this is local to the checkout and does not require `direnv allow`):

```bash
source .envrc
go test ./...
```

The main checkout's `.cache/shared/` directory holds one copy of Go toolchains, modules, build cache, and installed development tools for all linked worktrees. Each worktree keeps temporary files, telemetry, benchmark/coverage evidence, and the `make build` output (`.cache/out/apisix`, set by `.envrc` via `BINARY_PATH`) in its own `.cache/`. Without `.envrc`, a plain `make build` writes `./apisix` at the repo root. All of these paths are ignored by Git, so normal tests and builds do not write to user-level `/private` or home-directory caches. Run `make cache-status` to inspect the resolved paths, `make clean` to remove the local application binary, or `make cache-clean-local` after stopping an agent to remove that worktree's pre-migration duplicate Go caches.

## Plugin support

The complete APISIX 3.17 comparison, category summary, per-plugin supported/unsupported behavior, execution backlog, and remaining gap catalog are maintained in [docs/plugins.md](docs/plugins.md).

## Runtime boundaries

- HTTP routes listen on `:8080` by default and are built from the bbolt store populated by the etcd watcher.
- A `data_plane` deployment with `role_data_plane.config_provider: yaml` or `json` loads `conf/apisix.yaml` or `conf/apisix.json` and hot-reloads resource changes without etcd.
- Traditional etcd deployments with `server-info` enabled periodically publish `<etcd-prefix>/data_plane/server_info/<apisix-id>` and renew the lease until shutdown.
- TCP stream routing is enabled through `apisix.proxy_mode` and `apisix.stream_proxy.tcp`; the current main-server stream owner is `mqtt-proxy`, with weighted/chash upstream selection, route reload, cancellation, and lifecycle tests.
- Kafka PubSub uses the dedicated WebSocket/protobuf owner; Dubbo and HTTP-Dubbo use route-terminal TCP owners. General stream-plugin chains, stream mTLS, active upstream probes, exact OpenResty phase timing, and full Lua runtime compatibility remain deferred.

## TODO

The APISIX 3.17 plugin parity backlog is maintained in [docs/plugins.md](docs/plugins.md). The legacy project TODOs below are separate repository-level work and are not plugin parity claims.

- [x] standalone mode
- [ ] handle etcd compact
- [x] github action: go releaser
- [ ] logforamt change didn't take effect immediately
