---
id: ADR-0003
title: Publish artifacts by explicit platform support tier
status: accepted
target: apisix-3.17
divergence_ids: [DIV-003-platform-artifact-policy]
owner: wklken
owner_approval_ref: "decisions 107, 118-130"
date: 2026-08-23
---

# Context

Source portability, published archives, and qualified container platforms are
different claims. The release configuration must state each one explicitly.

# Decision

GoReleaser publishes Linux amd64 and arm64 archives. The container qualification
and publication workflow currently targets Linux amd64. The project publishes
no macOS or Windows artifact.

# Consequences

Documentation must distinguish source portability, archive targets, and the
qualified container target. Cross-compilation alone cannot promote a platform.
Adding or removing one artifact must not weaken unrelated compatibility,
security, or runtime gates.

# Evidence required to retire

Changing a published or qualified platform requires reproducible native build
and smoke evidence, release provenance for the exact artifact, runtime
dependency coverage, rollback evidence, manifest updates, and an
owner-approved replacement ADR.
