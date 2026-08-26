---
id: ADR-0003
title: Publish artifacts by explicit platform support tier
status: accepted
compatibility_target: apisix-3.17
divergence_ids: [DIV-003-platform-artifact-policy]
owner: wklken
owner_approval_ref: "decisions 107, 118-130"
date: 2026-08-23
---

# Context

The Go source can compile on more systems than the data-plane runtime and its
release process can qualify. Treating compilation, native smoke evidence, and
production artifact support as the same claim would overstate the current
platform contract.

# Decision

Linux is the production artifact platform. macOS artifacts are published only
after native smoke execution on their target architecture. Windows remains
source-buildable and experimental; the project does not publish a Windows
artifact. These support tiers do not assert that the project as a whole is
production ready.

# Consequences

Release metadata and documentation must distinguish Linux, native-smoked
macOS, and experimental Windows source builds. Cross-compilation alone cannot
promote a platform tier. A failed native smoke blocks the affected artifact;
rollback stops publishing that artifact without weakening the other platform
or compatibility gates.

# Evidence required to retire

Changing a tier requires reproducible native build and smoke evidence for each
affected architecture, release-pipeline provenance for the exact artifact,
runtime dependency coverage, rollback evidence, manifest updates, and an exact
project-owner-approved replacement ADR.
