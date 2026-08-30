# Generation Secret Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/secret`.

## Authority and access

- Declarations come from the capability manifest. Preserve full scope:
  generation, attempt, domain, factory, resource kind/id, source, and field.
- Resolver access is limited to defensive bytes in the exact publication
  closure. Do not add Store/global config lookup.
- Catalog digest mismatch fails compiler/factory construction closed.
- Reject cross-scope or undeclared access before backend use.

## Plaintext and lifecycle

- Plaintext is available only inside `Value.Use`; persistent observation is a
  redacted digest/descriptor. Never return plaintext from a status API.
- Errors, logs, metrics, status, and cleanup ledgers must not contain plaintext,
  ciphertext, references, or key material.
- Attempt cache bounds, TTL, eviction, expiry, and close must zero retained
  bytes. Close waits for in-flight use.
- Incomplete close retains the attempt identity and authority for retry; it must
  not permit a replacement attempt with the same identity.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/secret ./pkg/data_encryption -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/secret -count=1'
```
