# Plugin Testing and Compatibility Qualification

Plugin verification has three owners with different purposes.

## Unit tests

`pkg/plugin/<plugin>` tests own plugin-local schema, defaults, algorithms,
error handling, boundary conditions, and regression coverage. A compatibility
investigation that finds a product defect must add a regression here whenever
the behavior is testable without a real process.

## Standalone integration tests

`t/plugin` owns candidate-only real-process behavior that cannot be proved
credibly at package scope. This includes configuration publication, plugin
ordering, consumer/route/upstream collaboration, streaming and connection
lifecycle, and real protocol or logger fixtures.

Cases are declarative `<plugin>.yaml` manifests. Differential runners, oracle
configuration, and candidate-versus-oracle case builders do not belong in this
directory.

## Compatibility qualification

`validation/compatibility/<target>/cases.yaml` owns repeatable
candidate-versus-oracle cases for one pinned APISIX target. The generic,
opt-in suite in `t/compatibility` loads those cases only through explicit
compatibility or release-candidate gates.

The case configuration contains case identity, plugin, obligation, route,
standalone configuration, request or ordered steps, fixture, comparison policy,
file capture, and security decision. The loader fails closed unless the declared
plugin is bound to the target route; non-route activation such as a global rule,
public API, plugin metadata, stream route, or control API must be declared with
`plugin_binding`. A default exact-parity case must also declare at least one
independent `expect` assertion; a shared candidate/oracle no-op is not evidence
that the obligation ran. A registered semantic comparison policy may own this
assertion for cases that need protocol-aware comparison. Target-level files pin
oracle identity and normalization policy. Go code is limited to reusable runner
logic, comparators, protocol drivers, and fixtures that cannot be expressed as
declarative data.

Differential discovery follows a one-way lifecycle:

1. Reproduce the difference against the pinned oracle.
2. Add durable regression coverage to the owning unit or integration layer.
3. Retain the smallest repeatable compatibility case in versioned config when
   future target qualification needs it.
4. Delete investigation-only Go builders, duplicate assertions, and reports.

`make test` and `make test-integration` never run an APISIX oracle. A full
differential run is explicit and binds the source commit, oracle image digest,
catalog digest, candidate source commit, and candidate binary SHA-256 into its
artifact. That artifact is qualification evidence, not a substitute for unit
or integration regression coverage.
