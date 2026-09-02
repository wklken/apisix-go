# apisix-go

[English](README.md) | [简体中文](README.zh-CN.md)

[![CI](https://github.com/wklken/apisix-go/actions/workflows/unit-test.yml/badge.svg)](https://github.com/wklken/apisix-go/actions/workflows/unit-test.yml)
[![License](https://img.shields.io/github/license/wklken/apisix-go)](LICENSE)
[![GitHub stars](https://img.shields.io/github/stars/wklken/apisix-go?style=social)](https://github.com/wklken/apisix-go)

**apisix-go is an open-source, Go-native implementation of the [Apache APISIX](https://github.com/apache/apisix) data plane.** It targets APISIX 3.17 compatibility and is designed for straightforward distribution, operation, and extension across API and edge gateway deployments.

> [!WARNING]
> **Release candidate scope:** apisix-go is a release candidate for the
> documented Apache APISIX 3.17 HTTP data-plane scope. The APISIX stream
> subsystem (TCP/UDP) is excluded. Production deployment still requires
> environment-specific validation.

## Why apisix-go?

- **Simple delivery:** build and distribute a single Go binary or container image.
- **Compact artifacts:** reference Linux/arm64 builds produce a roughly 45 MiB stripped binary and a 60 MiB Alpine runtime image.
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

The compatibility target is the Apache APISIX 3.17 HTTP data plane. Registered
plugin inventory is an implementation detail and does not by itself prove
behavioral compatibility.

## Documentation

- [HTTP data-plane compatibility](docs/http-data-plane.md)
- [Configuration](docs/configuration.md)
- [Architecture and decisions](docs/design.md)
- [HTTP candidate qualification](docs/runbooks/http-candidate-qualification.md)
- [Proxy runtime performance acceptance](docs/performance/proxy-runtime-acceptance.md)
- [Standalone plugin integration tests](t/plugin/README.md)

## Open source

apisix-go is released under the [Apache License 2.0](LICENSE) and developed in public. Bug reports, documentation improvements, compatibility work, tests, and focused pull requests are welcome.

- [Report an issue](https://github.com/wklken/apisix-go/issues)
- [Open a pull request](https://github.com/wklken/apisix-go/pulls)
- [Explore the documentation](docs/)

If apisix-go is useful to you, [star the repository](https://github.com/wklken/apisix-go) and share it with teams exploring Go-native API and edge gateways.
