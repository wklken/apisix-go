# apisix-go

[English](README.md) | [简体中文](README.zh-CN.md)

[![CI](https://github.com/wklken/apisix-go/actions/workflows/unit-test.yml/badge.svg)](https://github.com/wklken/apisix-go/actions/workflows/unit-test.yml)
[![许可证](https://img.shields.io/github/license/wklken/apisix-go)](LICENSE)
[![GitHub stars](https://img.shields.io/github/stars/wklken/apisix-go?style=social)](https://github.com/wklken/apisix-go)

**apisix-go 是一个开源的、原生使用 Go 实现的 [Apache APISIX](https://github.com/apache/apisix) 数据面。** 它面向 APISIX 3.17 兼容性，旨在为 API 和边缘网关部署提供简单的分发、运行和扩展方式。

> [!WARNING]
> apisix-go 正在积极开发中，尚未达到生产可用状态。

## 为什么选择 apisix-go？

- **交付简单：** 构建并分发单个 Go 二进制文件或容器镜像。
- **构建产物紧凑：** 参考 Linux/arm64 构建生成约 45 MiB 的精简二进制文件和约 60 MiB 的 Alpine 运行时镜像。
- **配置灵活：** 传统部署使用 etcd，独立数据面部署使用本地 YAML/JSON 文件。
- **熟悉的 APISIX 模型：** 使用 APISIX 资源格式配置路由、服务、上游、消费者、SSL、流路由和插件。
- **Go 原生生态：** 使用 Go 中间件和插件扩展流量处理，并内置日志、指标和追踪集成。

## 快速开始

在仓库根目录中，使用附带的示例配置以独立模式启动 `apisix-go`：

```bash
source .envrc && go run . -c conf/config-example.yaml
```

在另一个终端中，通过网关发送请求：

```bash
curl http://127.0.0.1:9080/hello
```

示例路由 [`conf/apisix.yaml`](conf/apisix.yaml) 会将请求代理到公开的 [httpbingo.org](https://httpbingo.org) 回显服务，并将上游路径重写为 `/anything/hello`。成功请求会返回 HTTP 200 以及回显的请求详情。使用 `Ctrl-C` 停止网关。

## 兼容性

apisix-go 当前注册了 APISIX 3.17 默认插件中的 100 个（共 104 个，96.2%）。其中 89 个达到项目文档所定义的 Go 原生支持级别；其余差距将通过独立的协议工作解决，或依赖 NGINX、OpenResty、Lua 或外部插件运行时。

确切的状态定义、支持行为和剩余差距维护在 [`docs/plugins.md`](docs/plugins.md) 中，该文件是插件状态的权威文档。

## 文档

- [插件支持情况和剩余差距](docs/plugins.md)
- [配置兼容性](docs/configuration.md)
- [候选 HTTP 数据面配置](docs/production-profile.md)（等待发布和运维资格认证）
- [生产发布手册](docs/runbooks/production-release.md)（需要资格认证证据）
- [设计说明](docs/design.md)
- [独立插件集成测试](t/plugin/README.md)

## 开源

apisix-go 基于 [Apache License 2.0](LICENSE) 发布，并以开源方式开发。欢迎提交问题报告、文档改进、兼容性工作、测试和聚焦明确的拉取请求。

- [报告问题](https://github.com/wklken/apisix-go/issues)
- [发起拉取请求](https://github.com/wklken/apisix-go/pulls)
- [浏览文档](docs/)

如果 apisix-go 对你有帮助，欢迎为[仓库加星](https://github.com/wklken/apisix-go)，并与正在探索 Go 原生 API 和边缘网关的团队分享。
