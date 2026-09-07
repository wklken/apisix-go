---
id: ADR-0007
title: Kafka logger producers remain owned by their generation
status: accepted
target: apisix-3.17
owner: wklken
date: 2026-09-07
---

# Context

APISIX 3.17's Lua Kafka client caches asynchronous producers process-wide by
`cluster_name`. Reusing that number can reuse the first producer's brokers,
credentials, buffering options, and timers even when later plugin configurations
differ. This is a Lua runtime cache behavior, outside the required native-runtime
parity scope.

# Decision

Kafka logger producers retain their configuration and generation-scoped
credentials. The Go implementation accepts `cluster_name` but does not reproduce
the Lua process-wide producer cache. It applies `producer_max_buffering` to the
queued message count, excluding a batch already removed for delivery, and uses
`meta_refresh_interval` for the producer transport's metadata lifetime.

# Consequences

Configurations that rely on sharing a producer through `cluster_name` can select
different brokers or use different queues in the Go runtime. Closing one
generation does not close another generation's writer or retain its credentials.

# Evidence required to retire

A shared-producer implementation must preserve generation-scoped credential
access, configuration identity, queue ownership, and retirement. Reusing a
credential-bearing writer solely by a numeric cluster name does not meet those
contracts.
