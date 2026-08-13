# Frontend and Upstream TLS Implementation Plan

**Goal:** Close PR-016, PR-022, and P1 5.10 by enforcing the configured frontend TLS policy, enabling HTTPS/grpcs upstream client certificates, and rejecting the incorrect absolute `send_timeout` mapping.

**Refreshed baseline:** `origin/master@998b1fe9a609092ba62d70ea74423dcf55212d3a` after Plans 01-17 merged.

## Contract

- Frontend TLS accepts only `TLSv1.2` and `TLSv1.3`. Empty, duplicate, or unknown protocol tokens fail startup.
- `ssl_ciphers` configures TLS 1.2 only. OpenSSL names are mapped to Go-supported TLS 1.2 suites; unknown, unsupported DHE, and TLS 1.3 suite names fail startup. The repository default contains only suites Go can enforce.
- `ssl_session_tickets: false` sets `tls.Config.SessionTicketsDisabled`.
- `ssl_trusted_certificate`, when configured, loads a PEM CA pool and globally requires a verified frontend client certificate. Per-SNI client-auth policy remains outside this bounded global configuration contract; SNI still selects the server certificate from the published SSL index.
- HTTPS and grpcs upstreams accept exactly one inline `client_cert`/`client_key` pair or one `client_cert_id`. Missing, disabled, conflicting, partial, or invalid material fails route compilation. Client certificate configuration on a plaintext HTTP/grpc upstream also fails closed.
- The transport and cluster identity contains the leaf certificate SHA-256 fingerprint and existing verification policy. SSL resource changes trigger HTTP route rebuild, so certificate rotation creates a distinct transport; old transports close when their final route-generation lease stops.
- `nginx_config.http.send_timeout` is not mapped to `http.Server.WriteTimeout`. Any non-zero value fails configuration loading because the current progress-timeout owner cannot implement NGINX frontend write-idle semantics.

## Tasks

### Task 1: Frontend TLS and send-timeout validation

- Add strict protocol/cipher parsing and global frontend client-CA loading in `pkg/server/tls.go`.
- Build the TLS config before listeners are bound and return contextual startup errors.
- Remove the absolute `WriteTimeout` mapping and reject non-zero `nginx_config.http.send_timeout` during config load.
- Add table and real-handshake tests for TLS 1.2/1.3, selected TLS 1.2 ciphers, tickets, SNI, and required client certificates.
- Update the enforceable default cipher list and document the bounded frontend/send-timeout contract.

### Task 2: Cluster transport certificate identity

- Extend `proxy.TransportOptionBuilder` with an immutable client-certificate option.
- Clone certificate byte slices when the option and `tls.Config` are built.
- Include only the leaf certificate SHA-256 fingerprint in the deterministic transport key identity; never serialize private-key material.
- Add real mTLS transport and cluster reuse/separation/close tests.

### Task 3: HTTPS/grpcs materialization and rotation

- Resolve inline or SSL-resource client certificates before acquiring a cluster.
- Generalize the existing Kafka SSL ID normalization without changing Kafka behavior.
- Use the resolved certificate for HTTPS and grpcs transports and validate it even when an upstream has no nodes.
- Make `ssls` an HTTP-route reload bucket and test inline, ID, invalid, rotation, reuse, old-cluster close, HTTPS, and grpcs handshakes.

## Verification

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/proxy ./pkg/resource ./pkg/store ./pkg/route ./pkg/server -run "(TLS|MTLS|ClientCert|SendTimeout|Cluster|SSLReload)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/proxy ./pkg/store ./pkg/route ./pkg/server -run "(TLS|MTLS|ClientCert|Cluster|SSLReload)" -count=3'
bash -lc 'source .envrc && go test ./pkg/config ./pkg/proxy ./pkg/resource ./pkg/store ./pkg/route ./pkg/server -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/config/... ./pkg/proxy/... ./pkg/resource/... ./pkg/store/... ./pkg/route/... ./pkg/server/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

Do not run broad `go test ./...`, `make test`, or the full `t/plugin` suite for this plan.
