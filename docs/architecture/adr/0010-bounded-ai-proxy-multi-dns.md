---
id: ADR-0010
title: Bound ai-proxy-multi DNS and active-health work
status: proposed
compatibility_target: apisix-3.17
divergence_ids: [DIV-010-bounded-ai-proxy-multi-dns]
owner: wklken
owner_approval_ref: ""
date: 2026-08-29
---

# Context

APISIX 3.17 expands domain endpoints into address-level upstream nodes and
applies active health checks to those nodes. A Go implementation must preserve
the configured DNS resolver, logical Host authority, TLS ServerName, immutable
request selection, health transitions, and fallback ordering without allowing
configuration preparation or a large DNS response to create unbounded work.

Synchronous initial resolution can block publication for the sum of every
instance timeout. Falling back to the system resolver when configured DNS has
no address can also send health credentials outside the configured split-DNS
boundary. Spawning one owned task per returned address makes the task count
depend on untrusted DNS data even when network concurrency is separately
limited.

# Decision

Generation preparation publishes domain requirements without network I/O,
starts one generation-owned refresh loop, and wakes it immediately. Requests
remain fail closed until the configured resolver publishes an immutable node
snapshot. An unresolved instance is ineligible in both healthy and fallback
passes, so selection continues through the same and lower priorities. Active
checks never probe an unresolved domain through the system resolver.

Each instance accepts at most 64 unique resolved addresses. Resolution uses at
most four owned lookup workers per pass, and active checks use at most 32 owned
probe workers per pass while retaining the lower per-instance
`checks.active.concurrency` limit. A response over the address ceiling is
rejected; an existing last-good node set is retained, while a first resolution
remains unavailable. Resolver and probe cancellation are tied to the exact
generation task registry.

After the immediate first wake, the refresh loop schedules the earliest current
node or instance healthy/unhealthy deadline and the earliest configured DNS
expiry. A single probe panic is contained as a failed probe result, its
per-instance concurrency token is always released, and the loop continues with
later scheduled passes without failing the generation task component. Plugin
Stop cancels and joins this loop before closing clients. Retired address clients
close their idle connections immediately. A request or health probe pinned
before DNS replacement performs a second, idempotent idle-connection close when
its response completes. This leaves the request itself as the only lifetime
owner and avoids a generation-wide retired-node registry that could grow under
continuous DNS churn.

# Consequences

The resolver, logical authority, TLS identity, per-address health state, and
fallback behavior match the declared APISIX 3.17 contract. The fixed address
and worker ceilings are Go-native resource-safety limits: a deployment that
requires more than 64 addresses for one AI instance must split that instance
or obtain an explicit owner decision to change the bound.

HTTP and HTTPS authorities omit explicit scheme-default ports (`80` and `443`)
while non-default ports and TCP dial ports remain explicit. This is a parity
correction, not an additional divergence.

Initial requests may receive a stable 503 while the first asynchronous lookup
is in progress instead of delaying generation publication. This proposal is
not owner-approved and does not itself make the project production ready.

# Evidence required to retire

Retirement requires an upstream APISIX target with equivalent explicit DNS
address and worker ceilings, or a replacement design that proves bounded
publication latency, task count, credential routing, fallback behavior,
generation cancellation, migration, and rollback under adversarial DNS
responses.
