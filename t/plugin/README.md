# Standalone Plugin Integration Tests

This directory converts pinned Apache APISIX `t/plugin/*.t` behavior into
declarative APISIX-Go standalone tests. Each executable case starts the real
CLI in a fresh child process, writes temporary `conf/config.yaml` and
`conf/apisix.yaml` files, creates its own temporary `apisix-go-store.db`, and
uses a fresh loopback upstream fixture.

The initial corpus is pinned to Apache APISIX commit
`c3d7d5ec69774121f53d2e20d29d09c816795dd7`:

| Manifest | Upstream blocks | Local groups | Executable | Skipped groups |
| --- | ---: | ---: | ---: | ---: |
| `redirect.yaml` | 48 | 30 | 29 | 1 |
| `proxy-rewrite.yaml` | 57 | 36 | 35 | 1 |
| `response-rewrite.yaml` | 27 | 20 | 19 | 1 |
| Total | 132 | 86 | 83 | 3 |

The three skipped groups remain in the manifests with concrete reasons. They
cover redirect frontend TLS tests 28-29 and the Admin API/etcd serialization
tests in proxy-rewrite 35 and response-rewrite 15.

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
setup. Unsupported blocks must use a non-empty `skip` reason; they must not be
deleted from the mapping.

An HTTP case contains:

- `source.tests`: source `TEST` numbers represented by the case;
- optional `runtime`: recursive overrides for the generated runtime config;
- `config`: standalone resources written to `conf/apisix.yaml`;
- `input`: client method, path, headers, and body;
- optional `upstream`: HTTP/HTTPS fixture expectations and response;
- `output`: expected status plus optional header, body, and log assertions.

Configuration-rejection cases omit `input` and `output.status`; they require an
`output.logs` matcher proving the intended route/plugin initialization failure.
The runner then checks startup logs without sending a request through the
intentionally rejected route.

`{{UPSTREAM_ADDR}}` inside `config` is replaced with the current fixture's
loopback address and is valid only when the case declares `upstream`.

## Matchers

Every matcher configures exactly one operation:

```yaml
equals: literal value
matches: '^Go regular expression$'
absent: true
```

`equals` and `matches` work for response bodies, logs, fixture paths, Host,
headers, and fixture bodies. `absent` is valid only for headers.

## Adding a plugin

1. Pin the exact upstream repository commit and count every `=== TEST` block.
2. Create `t/plugin/<plugin>.yaml`; pair setup blocks with their behavior block.
3. Convert all blocks, including explicit skips for behavior that cannot run in
   standalone mode.
4. Prefer fixture request assertions for request-mutating plugins and response
   assertions for response plugins.
5. Run the focused manifest. If it exposes a parity bug, add a focused failing
   unit test at the owning package before changing production code.
6. Run `make test-integration` and the repository verification gates.

The upstream expectation is authoritative. Do not weaken an assertion to match
an incompatible current implementation.
