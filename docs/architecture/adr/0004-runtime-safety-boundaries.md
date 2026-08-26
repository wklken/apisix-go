---
id: ADR-0004
title: Bound ambiguous stream routing and embedded Lua execution
status: accepted
compatibility_target: apisix-3.17
divergence_ids: [DIV-004-runtime-safety-boundaries]
owner: wklken
owner_approval_ref: "PR #163 remediation decisions, 2026-08-26"
date: 2026-08-26
---

# Context

APISIX resource order can select the first matching stream route, and an
embedded Lua runtime can expose process APIs or execute indefinitely. In the Go
runtime, those behaviors make route selection depend on provider ordering and
can prevent a generation from retiring after its domains have moved forward.

# Decision

Stream predicates are treated as sets. Disjoint predicates coexist. If two
predicates overlap, the compiler accepts them only when one is provably a
strict subset of the other, then places the narrower route first. Equal or
incomparable overlaps fail generation compilation.

Serverless Lua uses an operator-controlled hard deadline for both configured
chunk evaluation and request execution. The deadline cannot be changed by a
route. A call returns only after GopherLua has observed context cancellation.
The embedded state opens the supported base/package/table/string/math/coroutine
libraries, removes `dofile`/`loadfile`, limits `require` to preloaded modules,
and excludes OS, IO, debug, and channel libraries.

# Consequences

The runtime intentionally differs from order-dependent or unbounded APISIX
behavior. Ambiguous route sets fail closed before activation. Infinite Lua
cannot indefinitely retain a generation lease, while supported bounded Lua
continues to use `cjson` and the implemented `apisix.core`/`ngx` surface.

# Evidence required to retire

Retirement requires a replacement that proves deterministic stream selection
for every overlapping predicate shape, demonstrates that timed-out Lua has
actually stopped before lease release, defines any expanded library authority,
and has explicit owner approval plus rollback evidence.
