# Archer worker protocol

The Go CLI now has a native project adapter for Slipstream's Parallax envelopes. It advertises configured capabilities, resolves requests, executes approved raw-artifact installations, and returns evidence-backed results. This is the first integration slice. PHP `archer/v1` installer manifests and dependency graphs are not imported by this adapter.

Build current source before using these commands; the local `dist/v0.1.0` artifacts predate the adapter.

```sh
go build -trimpath -buildvcs=false -o dist/archer ./cmd/archer
./dist/archer capabilities --json --project /path/to/project
./dist/archer resolve --json --stdin --project /path/to/project < request.json
./dist/archer install --preflight --json --stdin --project /path/to/project < handoff.json
./dist/archer install --json --stdin --project /path/to/project < handoff.json
```

## Resolution and approval

Resolution input is one JSON object:

```json
{
  "requirements": ["example-tool"],
  "context": {"probe_installed": false}
}
```

The project must have `archer.json`. Requests select capabilities while retaining their declared delivery and version policy. Capability names follow Parallax's lowercase machine-slug grammar; package identifiers such as `npm:react` use the local project interface and cannot be v1 handoff requirements.

The output is `parallax.resolution/v1`. `satisfied` means verified capability; `installable` means planned work; `remaining` means unresolved work. Resolution status counts satisfied and installable requirements as covered. A complete plan can therefore have status `satisfied` before installation.

`context.probe_installed: false` suppresses local probes and installed-state claims on the planning host. Installation independently checks the worker. Caller assertions about installed software or available drivers do not establish readiness.

The returned `approval` uses `archer.approval/v1` with the explicit extension `recipe_schema: archer.project/v1`. Persist the whole object and copy it unchanged into the authorized handoff's `context.approved_resolution`. It binds native definitions, source inputs, effective registries, requested policy, and selected routes. Runtime directories, context revision, and probe output are excluded. A local plan ID cannot replace this object. The caller still owns task authorization.

## Handoff execution

```json
{
  "schema": "parallax.handoff/v1",
  "handoff_id": "install-example-tool",
  "mission_id": "example-mission",
  "step_id": "dependencies",
  "from": "slipstream",
  "to": "archer",
  "owner": "archer",
  "objective": "Prepare the approved local tool",
  "requires": ["example-tool"],
  "context_refs": [],
  "attempt": 1,
  "context": {
    "approved_resolution": "REPLACE_WITH_THE_COMPLETE_APPROVAL_OBJECT"
  }
}
```

The placeholder is explanatory; execution requires an object, not a string. Requests may select an approved subset. Empty requests remain empty and never mean install the entire project.

Before probes, the adapter validates ownership, approval integrity, operation, subset, dependency bindings, and native definitions. It compares live routes before publication. Verified existing software can skip an approved installer; a different installation route requires a new approval. PHP approvals fail before instructions run because their recipe dialect is different.

`parallax.handoff-result/v1` partitions every requested requirement exactly once into `satisfied` and `remaining`, preserving its original text. Satisfaction requires matching verification evidence. Failures retain prior verified successes and return remaining work to the sender. The result ends Archer's ownership of that remaining work.

Preflight checks frozen native routes without artifact download, publication, catalog-cache refresh, or context writes. Missing readiness is pending work rather than a failed installation attempt. Service verification remains an HTTP health check; it does not provision services or prove business integration.

The protocol library validates result history, shrinking remaining sets, bounded attempts, and approval dependency closure. The coordinator must persist handoffs/history and claim tasks atomically. The CLI supplies no distributed execution lock or callback transport.

## Discovery policy

Missing, unavailable, stale, fresh-empty, and inaccessible matching discovery are distinct states. Freshness covers configured sources only. Unconfigured or failed discovery cannot justify unrestricted installation or AI eligibility.

Explicit `delivery: local` allows a local pinned executable without a service catalog. Verified service bindings and live local reuse remain available. No protocol command calls a model.

## Slipstream transport

The existing `ArcherProvider` prefixes its configured binary with `PHP_BINARY`. Point that binary to `scripts/slipstream-archer.php`, set `ARCHER_BINARY` to the absolute Go executable, and select a working directory containing the native project configuration. The bridge forwards argument arrays and standard streams and preserves exit codes. It installs nothing and constructs no shell command.

Slipstream is not changed automatically. Its PHP manifest registry and the Go project registry remain different backends. Migration needs matching server/worker project configuration and deliberate provider selection.

## Compatibility and limits

Tests use Slipstream's recorded handoff, result, and resolution approval fixtures. The PHP fixture digest is reproduced exactly. Version-evidence boundaries, shared dependency constraints, expanding history, malformed input, and drift are tested.

Arbitrary PHP JSON canonical equivalence is not claimed: associative decode/encode can change empty objects into arrays and normalize decimal/exponent numbers. The native profile uses an explicit dialect and bound hashes; altered bindings fail closed. Native JSON extensions and object/list shapes are retained. Version-constrained native capabilities remain unresolved until the resolver exists.

Stdin accepts one UTF-8 JSON object of at most 2 MiB with bounded nesting, no duplicate keys, and no trailing object. Stdin requests cannot combine positional selectors, `--plan`, or local `--approval PLAN_ID`; use the frozen object and `--preflight`.

Resolution exits 0 for covered or partially covered plans and 2 when nothing is covered. Handoff/preflight exits 0 for verified completion, 2 for pending or partial work, and 1 for failure. Errors use one JSON response on stdout and diagnostics on stderr.

Package installation, runtimes, PHP manifest compatibility, functional service readiness, and callbacks remain subsequent work. This slice establishes their transport, evidence, and approval boundary.
