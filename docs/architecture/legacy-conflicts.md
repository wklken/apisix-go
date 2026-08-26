# Legacy architecture conflict ledger

This ledger preserves conflicting historical claims while identifying the
governing replacement. Current work follows the
[program specification](../superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md)
and [compatibility contract](compatibility-contract.md). Plugin and evidence
facts come only from the capability manifest and its
[generated status](../plugins.md); the former hand-edited status is retained in
the [historical archive](../history/plugins-2026-08-23.md).

| Historical source | Preserved claim | Governing replacement | Disposition |
| --- | --- | --- | --- |
| `docs/design.md`, candidate profile section | One `deployment.profile` combines compatibility and strict qualification. | Three independent axes: `compatibility_target`, `security_profile`, and `qualification_profile`. | Superseded 2026-08-23. |
| `docs/design.md`, lifecycle section | Route retirement closes WebSockets and `SIGHUP` drains/exits. | The supervisor generation-handoff target preserves ordinary hijacked connections; the current implementation remains explicitly pre-convergence until that child plan lands. | Superseded 2026-08-23. |
| `docs/design.md`, route schema section | Retain only the current compatibility subset. | The pinned APISIX observable contract with explicit gap accounting. | Superseded 2026-08-23. |
| `docs/plugins.md` before governance | A hand-edited table is the source of plugin truth. | `pkg/capability/manifest.yaml` is editable truth and generates `docs/plugins.md`. | Archived in `docs/history/plugins-2026-08-23.md`. |
| `docs/reviews/convergence-decisions.md`, ARCH-03 | Do not import or cover the full pinned route contract. | The program specification's HTTP compatibility contract. | Historical remediation evidence; prospective decision superseded 2026-08-23. |
| `docs/configuration.md`, lifecycle/SIGHUP | Pre-convergence route retirement and drain/exit behavior is the active design target. | The governing supervisor-generation handoff target, while the current implementation is explicitly not yet converged. | Superseded as governing design 2026-08-23. |
| `docs/production-profile.md`, lifecycle/SIGHUP | Retirement closes hijacked connections and `SIGHUP` exits after drain. | The same governing supervisor-generation handoff target and current implementation boundary. | Superseded as governing design 2026-08-23. |
