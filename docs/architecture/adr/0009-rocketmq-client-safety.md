---
id: ADR-0009
title: Bound RocketMQ logger transport and client lifecycle
status: proposed
target: apisix-3.17
divergence_ids: [DIV-009-rocketmq-client-safety]
owner: wklken
owner_approval_ref: ""
date: 2026-08-29
---

# Context

The RocketMQ logger uses `rocketmq-client-go/v2` for both name-server route
lookups and broker sends. The pinned APISIX-compatible client defaults to TLS
verification off, and its remoting path can wait on connection locks or a
blocked socket write after the logger's request context has expired. Its
background client tasks also use uninterruptible initial sleeps and are not
joined by `Shutdown`.

The logger keeps APISIX 3.17 transport defaults. The local client copy is pinned to
`v2.1.3-0.20231106021916-c9e197c3af45`; it must not silently become a fork of a
different upstream revision.

# Decision

The RocketMQ client remains a documented local source copy of the pinned
module. The provenance gate compares it byte-for-byte with the cached upstream
archive, verifies the module hash against `go.sum`, preserves `LICENSE` and
`NOTICE`, and allows only the exact production and focused-test paths listed in
`third_party/rocketmq-client-go/APISIX_GO_PATCHES.md`.

Remoting calls carry the caller context through name-server route lookup,
interceptors, connection acquisition, per-connection write locking, and
blocked writes. Context cancellation sets an immediate write deadline and is
returned as the context error. If the cancellation callback has started, the
request joins it before releasing the per-connection write lock so a late
deadline cannot corrupt the next user of that pooled connection. Producer retry
loops stop rather than wrapping the cancellation and continuing.

TLS keeps the APISIX 3.17 default of `tls_verify = false`. When verification is
enabled, connections use the system root pool and derive
`ServerName` from each name-server or broker address. Tests may inject a
private root pool through the client option solely for local fixtures.

Client background tasks use cancelable initial delays and a `WaitGroup`.
`Shutdown` cancels the task context, waits for all tasks and their bounded
operations, and only then closes remoting resources and removes the client.
Producer start failure shuts down the partially created producer before the
error is returned.

# Consequences

The APISIX-compatible default can connect to deployments that do not verify
RocketMQ TLS certificates. Operators that enable verification must provision a
certificate chain whose SAN matches every configured RocketMQ endpoint.

The local patch adds lifecycle and cancellation safety but does not claim
validation against an external RocketMQ cluster. The protocol-faithful
local fixture proves both name-server and broker TLS handshakes; nested client
tests prove cancellation, retry, and task-join behavior. Container tests prove
the nested replacement source is present before the image build.

This proposal is not owner-approved and does not by itself make the whole
project production ready. It records the one RocketMQ client divergence needed
for the controlled HTTP data-plane slice.

# Evidence required to retire

Retirement requires an official RocketMQ client release that provides the same
context, blocked-write, TLS identity, and lifecycle guarantees, plus a pinned
source comparison and all focused tests passing against that release. The
replacement must preserve the documented transport behavior and provide rollback
evidence before this proposed divergence is removed.
