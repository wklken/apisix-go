# Plugin Testing

Plugin verification has two owners with different purposes.

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

Cases are declarative `<plugin>.yaml` manifests. Each one must activate and
observe its target plugin, or declare the narrow reason an intentional negative
case does not. Source labels retained in a manifest are local traceability, not
a repository-wide coverage ledger.

When official APISIX 3.17 behavior is unclear, compare the implementation with
the official source and tests during development. Any discovered difference
must become a focused unit test or standalone integration case. Investigation
scripts, copied oracle implementations, aggregate pass counts, and generated
reports are temporary development material and do not live in the product
repository.
