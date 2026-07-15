# Standalone Plugin Integration Tests Design

## Goal

Add a self-contained integration-test surface under `t/plugin` that converts
selected Apache APISIX `t/plugin/*.t` cases into declarative standalone
configuration, client input, fixture-upstream behavior, and output assertions.
The tests must exercise the real APISIX-Go command bootstrap, standalone YAML
loader, store, route builder, plugin chain, proxy, and HTTP response.

The first implementation covers every upstream `TEST` block in `redirect.t`,
`proxy-rewrite.t`, and `response-rewrite.t` at Apache APISIX commit
`c3d7d5ec69774121f53d2e20d29d09c816795dd7`. It establishes the format and
runner needed to convert more plugin files later without expanding the first
version into a Test::Nginx compatibility layer.

## Scope

Version 1 includes:

- the `t/plugin` directory and its test runner;
- one YAML file for each converted upstream plugin test file;
- exact response status assertions;
- exact, regular-expression, and absence assertions for headers and bodies;
- optional assertions for the request received by a local fixture upstream;
- isolated standalone-mode APISIX-Go processes with temporary configuration and
  storage;
- a `make test-integration` entry point;
- automatic participation in `go test ./...` without a build tag;
- source file and upstream `TEST` number traceability for every converted case.
- a mechanical coverage check that rejects missing or duplicated source test
  numbers.

The standalone contract deliberately does not include:

- automatic parsing or execution of upstream `.t` files;
- arbitrary Lua, NGINX directives, Admin API setup, or Test::Nginx semantics;
- live external services such as etcd, Redis, Kafka, OpenID providers, or cloud APIs;
- multi-worker integration fixtures;
- parallel case execution.

Upstream `.t` files are converted manually because their setup sections can
contain arbitrary Lua, Admin API calls, server directives, concurrency, and
external dependencies. The declarative format records the behavior being
ported, while deterministic loopback fixtures provide the external protocol
boundaries needed by the standalone process.

## Directory Layout

```text
t/plugin/
├── README.md
├── case.go
├── case_test.go
├── runner_test.go
├── <plugin>.yaml
└── ...
```

- `case.go` defines strict YAML decoding, validation, matching, and fixture
  types. It belongs only to the integration-test package; it is not a public
  runtime API.
- `case_test.go` verifies invalid manifests and matcher behavior before the
  process runner is implemented.
- `runner_test.go` discovers embedded YAML manifests, owns process lifecycle,
  renders standalone resources, sends requests, and reports failures.
- `<plugin>.yaml` mirrors the corresponding upstream `<plugin>.t` filename and
  contains independently runnable cases.
- `README.md` documents the format, conversion rules, and commands.

## Case Format

Each manifest starts with pinned source metadata and contains a `cases`
sequence. A case has six responsibilities:

```yaml
source:
  repository: https://github.com/apache/apisix
  commit: c3d7d5ec69774121f53d2e20d29d09c816795dd7
  file: t/plugin/redirect.t
  tests: 48
```

The manifest validator requires the union of every `case.source.tests` list to
equal the integer range `1..source.tests` exactly. Missing and duplicated
numbers fail before integration processes start. Setup-only upstream blocks are
paired with the behavior block that consumes their configuration. A source
block is not counted as covered unless its case activates the target plugin and
runs a request/assertion against the standalone process; external setup is
represented by a local fixture rather than a skip.

1. identify the source upstream tests;
2. provide optional APISIX runtime-configuration overrides;
3. provide an APISIX standalone resource document;
4. describe the client request;
5. define the local upstream fixture when the route can proxy;
6. describe the expected client response.

Example:

```yaml
cases:
  - name: fixed-uri-redirect
    source:
      tests: [3, 4]

    runtime: {}

    config:
      routes:
        - id: redirect-fixed-uri
          uri: /hello
          plugins:
            redirect:
              uri: /test/add
              ret_code: 301
          upstream:
            type: roundrobin
            nodes:
              "{{UPSTREAM_ADDR}}": 1

    input:
      method: GET
      path: /hello
      headers: {}
      body: ""

    upstream:
      respond:
        status: 200
        headers: {}
        body: hello world

    output:
      status: 301
      headers:
        Location:
          equals: /test/add
      body:
        equals: ""
```

`runtime` is an optional mapping recursively merged into the generated minimal
`conf/config.yaml`. The runner sets `apisix.node_listen`, `deployment.role`, and
`deployment.role_data_plane.config_provider` after the merge so cases cannot
escape the isolated loopback standalone process. Runtime overrides represent
upstream `.t` sections such as `yaml_config` and `extra_yaml_config`.

`config` is an arbitrary standalone resource mapping. The runner marshals it as
`conf/apisix.yaml`, replaces fixture placeholders such as
`{{FIXTURE.sink.ADDR}}`, and appends the required `#END` marker. A case may
declare HTTP, TCP/TLS-TCP, UDP, gRPC, Redis, Kafka, Dubbo, or LDAP fixtures;
fixture request assertions are evaluated against the real bytes or HTTP
request received by the local listener.

`input.method` defaults to `GET`. For HTTP cases, `input.path` is required and
must begin with `/`. Request headers and body are optional. Configuration-
rejection cases send the declared request to the rejected route and assert both
the client status and the target-plugin startup log; the runner does not
silently count a route that was never exercised.

`upstream.respond` defaults to status `200`, empty headers, and an empty body.
`upstream.expect`, when present, can assert the received method, path, Host,
headers, and body. This is how request-mutating plugins such as `proxy-rewrite` are
tested without relying on a special echo-server response format. An optional
`upstream.tls: true` starts an HTTPS fixture for scheme-rewrite cases; the
existing proxy transport intentionally skips certificate verification for
route upstreams.

`output.status` is required for HTTP cases. `output.headers` and `output.body`
are assertions against the response returned to the client. The HTTP client
must not follow redirects. `output.logs`, when present, applies a matcher to the
captured child output after shutdown; log-only invalid-config cases use it to
prove rejection came from the intended plugin validation.

## Assertion Model

A string matcher supports exactly one of these operations:

```yaml
equals: literal value
matches: '^regular expression$'
absent: true
```

`equals` and `matches` are available for response bodies, fixture request paths,
and fixture request bodies. Header assertions additionally support `absent`.
An assertion with none or more than one operation is invalid. `absent` is valid
only when it is `true` and the matcher is attached to a header.

Header names use Go's canonical case-insensitive HTTP behavior. `equals`
compares the value returned by `Header.Get`. `matches`
applies a compiled Go regular expression to that value. `absent` requires that
the header have no values. Version 1 does not add ordered multi-value header
assertions because none of the initial converted cases require them.

HTTP status assertions are exact integers in the range 100 through 599. Missing
or out-of-range response status values fail manifest validation before a child
process is started. A status may be omitted only by a log-only configuration-
rejection case with no HTTP input.

## Runner Architecture

The integration package uses the Go helper-process pattern instead of invoking
`go build` or `go run` from a test:

1. The parent test starts the current test executable with
   `-test.run=TestAPISIXProcess` and a private environment marker.
2. The helper test replaces `os.Args` with `apisix -c conf/config.yaml` and calls
   the real `cmd.Execute()` entry point.
3. The child process runs the normal configuration loader and server startup.
4. The parent sends `os.Interrupt` after assertions; the existing server signal
   handler performs graceful shutdown.

This executes the real command, standalone provider, bbolt store, route builder,
plugin creation, proxy handler, and response path without compiling an extra
binary for every test run.

Every case gets:

- a fresh temporary working directory;
- a generated minimal `conf/config.yaml` with a loopback listener, data-plane
  role, YAML standalone provider, no Admin API, and the case's optional runtime
  overrides;
- its own `conf/apisix.yaml`;
- its own `apisix-go-store.db`;
- a fresh loopback APISIX listener;
- a fresh `httptest.Server` upstream when declared;
- a fresh child process.

Cases run sequentially. APISIX-Go currently has process-global Viper and store
state, so process isolation is the smallest reliable boundary and prevents one
case from affecting another.

The parent reserves a loopback port, writes it into the runtime configuration,
starts the child, and polls the TCP listener until ready. Startup and client
requests have bounded timeouts. On failure, the parent stops the process before
reading captured logs to avoid concurrent buffer access.

## Failure Behavior

Manifest decoding uses `yaml.Decoder.KnownFields(true)` for the test schema.
The arbitrary APISIX `config` mapping is passed through without imposing a
second test-only resource schema; the real standalone loader and route builder
remain authoritative for those fields.

The runner fails before startup for:

- unknown top-level case fields;
- empty or duplicate case names;
- missing source file or source test numbers;
- invalid request paths;
- invalid response status values;
- malformed or ambiguous matchers;
- `absent` used for a non-header assertion;
- invalid regular expressions;
- `{{UPSTREAM_ADDR}}` without an upstream fixture.
- any source test number missing, repeated, below 1, or above the pinned source
  count;
- a skipped case without a reason, or a case without standalone config,
  request input, and a response, fixture, file, or startup-log assertion.

Runtime failures report:

- manifest filename and case name;
- the failing assertion;
- child exit status;
- captured child stdout and stderr;
- the rendered runtime and standalone configuration when startup fails.

The runner must stop the child and fixture server on all exit paths. A graceful
shutdown timeout falls back to killing the child and is itself reported as a
test failure.

## Complete Manifest Corpus

The corpus contains 99 manifests: one for each of the 98 source-backed plugins
marked Supported in `docs/plugins.md`, plus the supplemental `redirect2` source
manifest. Every source test number is assigned exactly once, every executable
case configures its target plugin in a standalone resource, and every case
starts the real APISIX-Go process before sending its request and evaluating the
response or fixture assertion. Redis, Kafka, LDAP, cloud, tracing, and logger
dependencies are deterministic loopback fixtures owned by the case; no source
block is represented by a skip or a placeholder reason.

The upstream expected behavior remains authoritative for converted cases. If a
case exposes a Go implementation mismatch, the implementation is fixed within
that plugin's converted scope with a focused regression test. Native/OpenResty-
only semantics remain outside the Go-native contract and are documented rather
than approximated silently.

## Commands and Gate Integration

The Makefile adds:

```make
.PHONY: test-integration
test-integration:
	go test ./t/plugin -count=1 -v
```

The documented focused command is:

```bash
source .envrc && make test-integration
```

No build tag is used, so the same package also runs under the repository gate:

```bash
source .envrc && go test ./... -count=1
```

The integration package uses only existing module dependencies and the Go
standard library. It does not add a new dependency.

## Acceptance Criteria

The design is implemented when:

1. `t/plugin` contains the strict declarative runner and documented schema.
2. The complete manifest set traces every case to upstream `.t` test numbers.
3. The manifest coverage validator proves every pinned source test number is
   represented exactly once.
4. Each executable case launches the real APISIX-Go command in standalone YAML mode and
   uses isolated temporary state.
5. Client response and optional fixture-upstream request assertions fail with
   actionable diagnostics.
6. No source block is omitted or represented by a skip placeholder.
7. `source .envrc && make test-integration` passes.
8. `source .envrc && go test ./... -count=1` passes.
9. `source .envrc && make lint` passes.
10. `source .envrc && make build` passes and the generated `apisix` binary is not
   included in the change.
11. `git diff --check` passes and the diff contains no unrelated changes.
