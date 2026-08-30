---
id: ADR-0001
title: Keep APISIX compatibility separate from Go-native extensions
status: accepted
target: apisix-3.17
owner: wklken
date: 2026-08-23
---

# Context

The project targets the observable behavior of the pinned Apache APISIX 3.17
source and image. A Go implementation may use Go-native mechanisms internally,
but placing project-specific facilities in the APISIX namespace would make the
compatibility claim ambiguous.

# Decision

The `apisix` namespace describes the pinned APISIX contract. Go-native
implementation techniques are allowed when they preserve that contract.
Project-specific extensions use the versioned `apisix-go/v1` namespace and are
outside the drop-in compatibility claim.

# Consequences

New extensions must declare `apisix-go/v1`. Moving one into `apisix` requires
matching APISIX 3.17 behavior. Removing an extension must not relax the APISIX
namespace contract.

# Evidence required to retire

Retirement requires a newly pinned compatibility target in which the same
facility has an upstream APISIX identity and matching observable behavior.
