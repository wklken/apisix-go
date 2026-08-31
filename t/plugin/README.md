# Standalone Plugin Integration Tests

This directory converts the pinned APISIX 3.17.0 `t/plugin/*.t` behavior into
declarative APISIX-Go standalone tests. Later-version upstream regression
blocks belong in focused plugin unit tests or a separately authorized future
compatibility target, not this corpus. Each executable case starts the real
CLI in a fresh child process, writes temporary `conf/config.yaml` and
`conf/apisix.yaml` files, creates isolated runtime directories, and uses a fresh
loopback upstream fixture.

The checked-in catalog is discovered directly from `t/plugin/*.yaml`. Each
manifest is an independent behavior test, not one row in a global compatibility
or upstream-source ledger. Every manifest contains at least one standalone
scenario that activates its target plugin and assertions produced by the real
APISIX-Go process. An intentional negative scenario may keep the target plugin
disabled only when it declares a nonblank `target_plugin_exempt_reason`; the
harness rejects missing, blank, or stale exemptions. No generated placeholder
manifest is counted as a behavior test.

The schema rejects `skip` fields and upstream source-accounting fields. Generic
fixture proxies that never configure the named plugin do not count as behavior
tests. Cases that need a local dependency use a fixture declared in the same
manifest; they are not represented as skips. Network fixtures cover TCP,
TLS-over-TCP, UDP, gRPC, Redis, Kafka, Dubbo, and LDAP protocol surfaces, and
`{{WORK_DIR}}` keeps file assertions inside the per-case temporary directory.

## Run

From the repository root, activate the checkout-local Go environment first:

```bash
source .envrc
make test-integration
```

Run one manifest or case with Go's subtest pattern:

```bash
go test ./t/plugin -run 'TestPluginIntegration/redirect' -count=1 -v
go test ./t/plugin -run 'TestPluginIntegration/proxy-rewrite/rewrite-host' -count=1 -v
```

The package has no build tag, so `go test ./... -count=1` also runs it.

## Manifest contract

Each `<plugin>.yaml` contains only executable APISIX-Go scenarios. Repository,
commit, upstream filename, and `TEST` numbering are qualification history rather
than integration-test inputs and are deliberately excluded from this contract.
When an upstream setup depends on the Admin API, Lua, or an external service,
the manifest provides an equivalent standalone resource or local fixture so the
behavior remains executable.

An HTTP case contains:

- optional `target_plugin_exempt_reason`: required only for an intentional
  negative case that does not activate the manifest's target plugin; variants
  declare this independently rather than on their parent case;
- optional `runtime`: recursive overrides written to generated
  `conf/config.yaml`;
- `config`: standalone resources written to generated `conf/apisix.yaml`;
- `input`: client method, path, headers, and body;
- optional `upstream`: HTTP/HTTPS fixture expectations and response;
- `output`: expected status plus optional header, body, and log assertions.

Configuration-rejection cases send their declared request to the rejected route;
they require an `output.status` assertion and an `output.logs` matcher proving
the intended route/plugin initialization failure. This keeps invalid
configuration cases observable at the APISIX-Go HTTP boundary.

When one upstream block contains multiple independent inputs, `variants`
declares one complete standalone scenario for each input. Every variant gets
its own files, process, request/assertion cycle, and temporary store.

`{{UPSTREAM_ADDR}}` inside `config` is replaced with the current fixture's
loopback address and is valid only when the case declares `upstream`.
`{{APISIX_URL}}` resolves to the isolated instance's frontend URL.

An ordered step may capture one regular-expression group from a response
header and reuse it in a later request path, body, or header:

```yaml
output:
  captures:
    state:
      header: Location
      matches: 'state=([^&]+)'
input:
  path: /callback?state={{CAPTURE.state}}
```

Set `without_cookies: true` on an input to deliberately omit the shared client
cookie jar for that request while retaining it for later ordered steps.

## Matchers

Every matcher configures exactly one operation:

```yaml
equals: literal value
matches: '^Go regular expression$'
not_matches: 'forbidden regular expression'
absent: true
```

`equals` and `matches` work for response bodies, logs, fixture paths, Host,
headers, and fixture bodies. `absent` is valid only for headers.

## Adding a plugin

1. Create `t/plugin/<plugin>.yaml`; pair setup blocks with their behavior block.
2. Convert relevant behavior into executable standalone scenarios; upstream
   repository, commit, filename, and `TEST` numbering do not belong in the
   manifest.
3. Do not add `skip` fields or placeholder cases; both are rejected.
4. Prefer fixture request assertions for request-mutating plugins and response
   assertions for response plugins.
5. Run the focused manifest. If it exposes a parity bug, add a focused failing
   unit test at the owning package before changing production code.
6. Run the focused case, then `make test-plugin-harness`.

The upstream expectation is authoritative. Do not weaken an assertion to match
an incompatible current implementation.
