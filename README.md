# apisix-go

[English](README.md) | [简体中文](README.zh-CN.md)

[![CI](https://github.com/wklken/apisix-go/actions/workflows/unit-test.yml/badge.svg)](https://github.com/wklken/apisix-go/actions/workflows/unit-test.yml)
[![License](https://img.shields.io/github/license/wklken/apisix-go)](LICENSE)
[![GitHub stars](https://img.shields.io/github/stars/wklken/apisix-go?style=social)](https://github.com/wklken/apisix-go)

**apisix-go is an open-source, Go-native implementation of the [Apache APISIX](https://github.com/apache/apisix) data plane.** It targets APISIX 3.17 compatibility and is designed for straightforward distribution, operation, and extension across API and edge gateway deployments.

> [!WARNING]
> apisix-go is under active development and is not ready for production use.

## Why apisix-go?

- **Simple delivery:** build and distribute a single Go binary or container image.
- **Flexible configuration:** use etcd for traditional deployments or local YAML/JSON files for standalone data-plane deployments.
- **Familiar APISIX model:** configure routes, services, upstreams, consumers, SSL, stream routes, and plugins with APISIX resource shapes.
- **Go-native ecosystem:** extend traffic handling with Go middleware and plugins, with built-in logging, metrics, and tracing integrations.

## Quick start

From the repository root, start `apisix-go` in standalone mode with the included example configuration:

```bash
source .envrc && go run . -c conf/config-example.yaml
```

In another terminal, send a request through the gateway:

```bash
curl http://127.0.0.1:9080/hello
```

The example route in [`conf/apisix.yaml`](conf/apisix.yaml) proxies the request to the public [httpbingo.org](https://httpbingo.org) echo service and rewrites the upstream path to `/anything/hello`. A successful request returns HTTP 200 with the echoed request details. Stop the gateway with `Ctrl-C`.

## Compatibility

apisix-go currently registers 100 of the 104 APISIX 3.17 default plugins (96.2%). Of those defaults, 89 are supported at the project's documented Go-native level; remaining gaps are deferred to separate protocol work or depend on NGINX, OpenResty, Lua, or external plugin runtimes.

The exact status definitions, supported behavior, and remaining gaps are maintained in [`docs/plugins.md`](docs/plugins.md), the authoritative plugin-status document.

## Documentation

- [Plugin support and remaining gaps](docs/plugins.md)
- [Configuration compatibility](docs/configuration.md)
- [Candidate HTTP data-plane profile](docs/production-profile.md) (awaiting release and operations qualification)
- [Production release runbook](docs/runbooks/production-release.md) (qualification evidence required)
- [Proxy runtime performance acceptance](docs/performance/proxy-runtime-acceptance.md)
- [Design notes](docs/design.md)
- [Standalone plugin integration tests](t/plugin/README.md)

## Open source

apisix-go is released under the [Apache License 2.0](LICENSE) and developed in public. Bug reports, documentation improvements, compatibility work, tests, and focused pull requests are welcome.

- [Report an issue](https://github.com/wklken/apisix-go/issues)
- [Open a pull request](https://github.com/wklken/apisix-go/pulls)
- [Explore the documentation](docs/)

If apisix-go is useful to you, [star the repository](https://github.com/wklken/apisix-go) and share it with teams exploring Go-native API and edge gateways.
