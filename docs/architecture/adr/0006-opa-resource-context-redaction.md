---
id: ADR-0006
title: Redact credential-bearing resource maps from OPA input
status: accepted
target: apisix-3.17
owner: wklken
date: 2026-08-28
---

# Context

When `with_route`, `with_service`, or `with_consumer` is enabled, APISIX 3.17
sends the complete matching resource object to OPA. Those objects include
plugin configuration maps and may contain authentication credentials, upstream
client keys, or other secret-bearing configuration. OPA policies commonly need
stable resource identity, but do not require unrestricted credential material.

# Decision

The Go OPA plugin sends only policy-useful identity fields: route `id`, `name`,
and `uri`; service `id` and `name`; and consumer `username` and `group_id`. It
never serializes plugin maps, upstream definitions, credential fields, or other
resource configuration into the OPA request in every runtime configuration.

# Consequences

OPA policies that inspect APISIX plugin configuration maps must be rewritten to
use the stable identity fields or another operator-controlled data source.
Authorization decisions, request facts, decision responses, and
`send_headers_upstream` otherwise keep the APISIX 3.17 contract. Validation
must prove both the useful identity projection and the absence of representative
route, service, consumer, upstream, and TLS secrets.

# Evidence required to retire

Retirement requires a pinned APISIX target that no longer exposes complete
resource configuration, or an owner-approved replacement contract with explicit
secret classification, least-privilege projection, migration, differential,
and rollback evidence.
