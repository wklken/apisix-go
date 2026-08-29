---
id: ADR-0005
title: Redact credential material from authentication diagnostics
status: accepted
target: apisix-3.17
divergence_ids: [DIV-005-credential-log-redaction]
owner: wklken
owner_approval_ref: "six-plugin rollout owner decision, 2026-08-27"
date: 2026-08-27
---

# Context

APISIX 3.17 basic-auth diagnostics include the invalid base64 payload or the
decoded value when an Authorization header cannot be parsed. Those values are
credential material supplied by an untrusted client. Copying them into the Go
runtime logs would violate the project rule that plugins do not log plaintext
credentials.

# Decision

Basic-auth preserves the APISIX-compatible authentication decision, HTTP
status, response body, and challenge header. Its diagnostics instead emit
stable classifications for an invalid authorization format, invalid Basic
base64 encoding, or an invalid decoded Basic value. The raw Authorization
value, encoded credential payload, decoded username, and decoded password are
not included in logs in any runtime configuration.

# Consequences

Exact basic-auth error-log text differs from APISIX 3.17 for malformed
credentials. Converted upstream evidence must assert both the stable redacted
classification and the absence of the source credential material. This
divergence does not permit different authentication outcomes or response
contracts.

# Evidence required to retire

Retirement requires a pinned APISIX target that no longer logs credential
material, or another owner-approved diagnostic contract that proves equivalent
redaction while preserving the authentication and response behavior.
