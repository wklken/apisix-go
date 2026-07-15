# Standalone Plugin Integration Tests

This directory converts pinned Apache APISIX `t/plugin/*.t` behavior into
declarative APISIX-Go standalone tests. Each executable case starts the real
CLI in a fresh child process, writes temporary `conf/config.yaml` and
`conf/apisix.yaml` files, creates its own temporary `apisix-go-store.db`, and
uses a fresh loopback upstream fixture.

The corpus is pinned to Apache APISIX commit
`c3d7d5ec69774121f53d2e20d29d09c816795dd7`. The catalog currently contains
99 manifests: the 98 source-backed plugins marked Supported in
`docs/plugins.md` plus the supplemental `redirect2` source file. Of those, 37
plugin manifests currently contain real target-plugin standalone scenarios;
61 generated placeholder manifests remain and are deliberately rejected by
`TestManifestCorpusExercisesTargetPlugins`. A manifest is complete only when
every case or variant activates its target plugin and asserts behavior produced
by the real APISIX-Go process.

The schema rejects `skip` fields. A source block counts as covered only when it
belongs to an executable standalone scenario with a request and an assertion;
placeholder reasons such as "requires setup or an external dependency" are not
accepted as coverage. The semantic corpus gate also rejects generic fixture
proxies that never configure the named plugin. Cases that need a local
dependency use a fixture declared in the same manifest; they are not represented
as skips.

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

Each `<plugin>.yaml` declares the pinned repository, commit, source file, total
number of upstream `TEST` blocks, and a list of local cases. Every source test
number from `1..source.tests` must occur exactly once. The validator fails on a
missing, duplicated, or out-of-range number before starting a child process.

Setup-only source blocks are grouped with the request block that exercises the
setup. When upstream setup depends on the Admin API, Lua, or an external
service, the manifest provides an equivalent standalone resource or local
fixture so the behavior remains executable.

An HTTP case contains:

- `source.tests`: source `TEST` numbers represented by the case;
- optional `runtime`: recursive overrides written to generated
  `conf/config.yaml`;
- `config`: standalone resources written to generated `conf/apisix.yaml`;
- `input`: client method, path, headers, and body;
- optional `upstream`: HTTP/HTTPS fixture expectations and response;
- `output`: expected status plus optional header, body, and log assertions.

Configuration-rejection cases omit `input` and `output.status`; they require an
`output.logs` matcher proving the intended route/plugin initialization failure.
The runner then checks startup logs without sending a request through the
intentionally rejected route.

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

1. Pin the exact upstream repository commit and count every `=== TEST` block.
2. Create `t/plugin/<plugin>.yaml`; pair setup blocks with their behavior block.
3. Convert all blocks into executable standalone scenarios; `skip` fields and
   placeholder cases are rejected.
4. Prefer fixture request assertions for request-mutating plugins and response
   assertions for response plugins.
5. Run the focused manifest. If it exposes a parity bug, add a focused failing
   unit test at the owning package before changing production code.
6. Run `make test-integration` and the repository verification gates.

The upstream expectation is authoritative. Do not weaken an assertion to match
an incompatible current implementation.
