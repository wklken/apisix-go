---
id: ADR-0002
title: Add strict security without rewriting compatibility behavior
status: accepted
compatibility_target: apisix-3.17
divergence_ids: [DIV-002-strict-security-profile]
owner: wklken
owner_approval_ref: "decisions 23, 35, 63-68"
date: 2026-08-23
---

# Context

Some pinned APISIX 3.17 defaults and observable bugs are unsuitable as modern
security defaults. Silently correcting them in compatibility mode would make
differential results and migrations unpredictable, while preserving them in
every deployment would prevent operators from selecting stronger controls.

# Decision

`security_profile: compat` preserves the pinned observable defaults and bugs
that are part of the declared compatibility contract. `security_profile:
strict` adds explicit, versioned security controls. The security axis is
independent from `compatibility_target` and `qualification_profile`; selecting
strict security neither changes the APISIX target nor asserts qualification.

# Consequences

Every behavior that differs under strict mode must name that profile and have
focused tests for both modes. Compatibility regressions cannot be disguised as
security fixes, and strict-only controls cannot leak into compatibility mode.
Rollback selects `compat` or reverts the versioned strict control without
changing the target or evidence ledger.

# Evidence required to retire

Retirement requires a newly pinned APISIX target whose observable defaults
make the strict distinction unnecessary, differential tests for compatibility
mode, focused security tests for the replacement behavior, migration and
rollback evidence, and exact project-owner approval of the replacement ADR.
