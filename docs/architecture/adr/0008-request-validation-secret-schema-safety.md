---
id: ADR-0008
title: Bound secret-backed request-validation schema execution
status: proposed
compatibility_target: apisix-3.17
divergence_ids: [DIV-008-request-validation-secret-schema-safety]
owner: wklken
owner_approval_ref: ""
date: 2026-08-29
---

# Context

APISIX 3.17 recursively resolves terminal secret values inside
`header_schema` and `body_schema`. The Go implementation must preserve that
behavior without retaining decrypted literals in a generation-long compiled
schema. `jsonschema/v5` retains constants, enums, regular expressions,
annotations, references, and diagnostic strings, and its default reference
loader can read external resources during compilation.

Compiling each secret-backed schema only inside `secret.Value.Use` removes the
generation-long plaintext copy, but makes schema complexity and compile
concurrency part of the request attack surface. Detailed library validation
errors may also reproduce a decrypted literal after the callback returns.

# Decision

Every request-validation schema is rejected before publication when either its
resolved source or its APISIX-normalized form exceeds 256 KiB encoded JSON,
depth 64, or 8192 value nodes. At most 256 semantic `$ref`, `$recursiveRef`, and
`$dynamicRef` occurrences are admitted. Resource counting covers the complete
value tree, while reference scanning follows only the subschema positions used
by the selected bundled JSON Schema draft, so reference-shaped keys inside
`const`, `enum`, `default`, or other literal data remain ordinary data.

References may target fragments or resources identified by `$id` within the
same document. References that would require file, HTTP, HTTPS, URN, or relative
external loading, and non-bundled `$schema` locations, fail before compilation.

Secret-backed schemas compile and validate only inside `secret.Value.Use`.
Their default mismatch diagnostic is the fixed `request does not match schema`
message. One generation attempt owns a four-slot compile limiter shared by all
request-validation bindings. Request cancellation, binding Stop, and attempt
closure wake queued waiters; stopping one binding does not close the shared
attempt gate.

# Consequences

Schemas that depend on external reference loading or exceed a hard budget are
rejected even if APISIX 3.17 or the underlying library could process them.
Default sensitive-schema failures provide less detail than ordinary schema
failures. Secret-backed validation still pays a bounded per-request compilation
cost, with at most four concurrent compilations across the exact attempt.

The pinned converted corpus contains 36 schemas with measured maxima of 276
encoded bytes, depth 5, 19 nodes, and no references. It therefore remains far
below every proposed resource limit. This proposal does not become an accepted
production divergence until the owner explicitly approves it.

# Evidence required to retire

Retirement requires a pinned APISIX target or replacement validator that
provides equivalent secret-lifetime isolation, bounded compilation, controlled
same-document reference resolution, cancellation, and non-retaining diagnostics,
or an owner-approved replacement contract with migration and rollback evidence.
