---
id: ADR-0008
title: Preserve the Go regular-expression boundary in data-mask
status: accepted
target: apisix-3.17
owner: wklken
date: 2026-09-07
---

# Context

APISIX data-mask uses OpenResty's PCRE substitution. The Go implementation uses
`regexp`, which does not support all PCRE syntax. For example, the valid PCRE
pattern `(?=secret)secret` replaces `secret` in APISIX but cannot be compiled by
Go's regular-expression engine.

# Decision

Full PCRE/OpenResty execution remains outside the native-runtime parity scope.
The existing skip-on-regex-error behavior must not be described as equivalent
masking for valid PCRE expressions that Go cannot compile. Such a rule leaves
the corresponding log value unchanged; other applicable rules still run.
This is an unresolved masking difference, not evidence that the rule was
applied successfully. Forwarded requests remain unchanged by data-mask.

# Consequences

Configurations relying on unsupported PCRE features do not provide equivalent
log masking. Supported expressions and replace/remove rules retain their
normal behavior. No additional admission policy or fallback redaction is
introduced by this boundary.

# Evidence required to retire

A compatible substitution implementation must demonstrate the supported PCRE
semantics, replacement expansion, first-match behavior, and error handling
against the pinned APISIX target, with focused tests and real log delivery.
