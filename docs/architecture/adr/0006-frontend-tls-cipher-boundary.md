---
id: ADR-0006
title: Frontend TLS cipher support follows the Go TLS implementation
status: accepted
target: apisix-3.17
owner: wklken
date: 2026-09-06
---

# Context

APISIX 3.17 uses OpenSSL and includes finite-field DHE suites such as
`DHE-RSA-AES128-GCM-SHA256` in `apisix/cli/config.lua`. Go's `crypto/tls`
implements ECDHE suites but does not implement finite-field DHE cipher suites.
These are different key-exchange algorithms.

# Decision

Frontend TLS compilation rejects cipher names that the Go TLS implementation
cannot negotiate. It does not map DHE names to ECDHE or silently remove them.
A configured DHE suite therefore remains an observable compatibility difference
from APISIX 3.17. Replacing the TLS implementation is outside the supported
Go-native data-plane contract.

TLS 1.1 is supported when explicitly configured with a compatible cipher.
TLS 1.3 may retain a supported TLS 1.2 cipher list in configuration; that list
has no effect on TLS 1.3 negotiation.

# Consequences

Deployments using finite-field DHE must change their cipher configuration to
supported suites before using this data plane. Failed TLS compilation preserves
an existing published generation and does not silently weaken its policy.

# Evidence required to retire

Retirement requires a TLS implementation that negotiates the configured DHE
suites, with real handshake tests and generation-owned certificate, connection,
and shutdown behavior. Admission of a cipher name alone is insufficient.
