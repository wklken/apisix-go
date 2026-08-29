---
id: ADR-0001
title: Keep APISIX compatibility separate from Go-native extensions
status: accepted
target: apisix-3.17
divergence_ids: [DIV-001-go-native-extension-identity]
owner: wklken
owner_approval_ref: "decisions 1-42"
date: 2026-08-23
---

# Context

The project targets the observable behavior of the pinned Apache APISIX 3.17
source and image. A Go implementation may use Go-native mechanisms internally,
but placing project-specific facilities in the APISIX namespace would make the
compatibility claim ambiguous.

# Decision

The `apisix` namespace remains compatibility-pure: entries in it describe the
pinned APISIX contract and must record gaps and evidence against that target.
Go-native implementation techniques are allowed when they preserve that
contract. Project-specific extensions are named only in the versioned
`apisix-go/v1` namespace and cannot be counted as APISIX parity.

# Consequences

New extensions must declare `apisix-go/v1`; moving one into `apisix` requires
pinned compatibility evidence and an owner-reviewed manifest change. Existing
partial or deferred APISIX facilities remain gaps, not extensions and not
divergences. Rollback removes the extension or restores its prior namespace;
it must not relax the APISIX namespace contract.

# Evidence required to retire

Retirement requires a newly pinned compatibility target in which the same
facility has an upstream APISIX identity, differential evidence for its
observable behavior, a manifest migration, and an exact owner-approved ADR
that replaces this namespace rule.
