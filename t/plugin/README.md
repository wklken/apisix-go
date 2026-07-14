# Standalone Plugin Integration Tests

This directory converts the pinned Apache APISIX `t/plugin/*.t` corpus into
declarative APISIX-Go tests. Each executable case starts the real CLI in a
fresh child process, writes temporary runtime and standalone configuration,
creates an isolated `apisix-go-store.db`, and uses a loopback upstream
fixture.

The corpus is pinned to Apache APISIX commit
`c3d7d5ec69774121f53d2e20d29d09c816795dd7`:

- 100 `Supported` plugin rows from `docs/plugins.md` are tracked.
- 96 manifests cover 252 upstream `.t` files and 3,928 upstream `TEST`
  blocks.
- 198 local cases execute end-to-end; 3,684 are retained as explicit skips
  because they require Lua/OpenResty phase behavior, sequential Admin API
  setup, external services, or another unavailable standalone boundary.
- `GM`, `proxy-cache`, `graphql-proxy-cache`, and `proxy-buffering` have no
  matching upstream `t/plugin/*.t` file at the pinned commit. The coverage
  test keeps these four documented exceptions visible instead of inventing
  source cases.

Every upstream test block is mapped exactly once. A manifest can declare one
`source` or multiple `sources`; with multiple files, each case identifies its
`source.file`. The validator rejects missing, duplicated, or out-of-range
source numbers before starting a child process.

## Run

From the repository root, activate the checkout-local Go environment first:

```bash
source .envrc
make test-integration
```

Run one manifest or case with Go's subtest pattern:

```bash
go test ./t/plugin -run 'TestPluginIntegration/redirect' -count=1 -v
go test ./t/plugin -run 'TestPluginIntegration/proxy-rewrite/safe-configured-uri-query' -count=1 -v
```

The package has no build tag, so `go test ./... -count=1` also runs it.

## Manifest contract

An HTTP case contains:

- `source.file` and `source.tests`: the upstream test block(s) represented;
- optional `runtime`: recursive overrides for the generated runtime config;
- `config`: standalone resources written to `conf/apisix.yaml`;
- `input`: client method, path, headers, and body;
- optional `upstream`: HTTP/HTTPS fixture expectations and response;
- `output`: expected status plus optional header, body, and log assertions.

Setup-only blocks remain source-mapped and use a non-empty `skip` reason.
Unsupported blocks must not be deleted from the mapping. A case containing
`{{UPSTREAM_ADDR}}` must declare an upstream fixture.

Configuration-rejection cases omit `input` and `output.status`; they require an
`output.logs` matcher proving the intended route or plugin initialization
failure. The runner checks startup logs without sending a request through the
intentionally rejected route. `{{UPSTREAM_ADDR}}` is replaced with the current
fixture's loopback address when an upstream fixture is declared.

Matchers configure exactly one operation:

```yaml
equals: literal value
matches: '^Go regular expression$'
absent: true
```

`equals` and `matches` work for response bodies, logs, fixture paths, Host,
headers, and fixture bodies. `absent` is valid only for headers.

## Adding a plugin

1. Pin the exact upstream repository commit and count every `=== TEST` block.
2. Add or extend `t/plugin/<plugin>.yaml`; pair setup blocks with the behavior
   they configure and preserve every source block.
3. Convert standalone request-path behavior to `config`/`input`/`output`; use
   an explicit skip for Lua/Admin API or external-service boundaries.
4. Run the focused manifest. If it exposes a parity bug, add a focused failing
   unit test at the owning package before changing production code.
5. Run `make test-integration` and the repository verification gates.

The upstream expectation is authoritative. Do not weaken an assertion to match
an incompatible current implementation.
