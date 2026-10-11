# ARCHER 1

Archer 0.1.0-dev. User Commands. 11 October 2026.

This reference includes unreleased protocol extensions in the current source.
The existing local v0.1.0 release binaries predate those commands.

## NAME

archer - resolve project requirements, reuse services, and install verified artifacts

## SYNOPSIS

```text
archer init [--project DIR]
archer plan [CAPABILITY ...] [OPTIONS]
archer install [CAPABILITY ...] [OPTIONS]
archer i [CAPABILITY ...] [OPTIONS]
archer doctor [CAPABILITY ...] [OPTIONS]
archer capabilities [--project DIR] [--json]
archer resolve [CAPABILITY ...] [OPTIONS]
archer resolve --stdin [OPTIONS]
archer install --stdin [--preflight] [OPTIONS]
archer context [--project DIR] [--home DIR]
archer version
archer help
archer --version
archer --help
```

## DESCRIPTION

Archer prepares project capabilities by checking existing services and local software before obtaining new artifacts. Requirements come from `archer.json` in the selected project directory. Positional capability names replace that file's requirement list for the current invocation; they do not add to it. Matching selectors retain the declared delivery and version policy.

For each compatible requirement, Archer prefers a configured service with a successful current health check, then an explicitly probed local capability, then a hash-verified managed executable or pinned executable recipe. A catalog advertisement alone does not establish configured access. An advertised service that needs configuration or verification blocks duplicate artifact installation. Missing, stale, or unavailable configured discovery blocks unrestricted fallback. Use `delivery: local` for an explicitly local-only artifact requirement.

This version installs individual raw executable artifacts. Composer/npm package requirements and non-package version constraints remain unresolved. It makes no model calls. Worker mode returns Parallax handoff results; the caller owns transport, task claims, and callbacks.

## COMMANDS

### init

Create `archer.json` in the selected project directory. Use the directory name as the initial scope. Read `require` and `require-dev` from `composer.json`, and `dependencies`, `devDependencies`, and `optionalDependencies` from `package.json`. Record package names and constraints as requirements prefixed with `composer:` or `npm:`.

Existing configuration is never overwritten. Missing manifests are allowed; malformed declarations are errors. The import does not consume lockfile graphs or retain dependency-group distinctions. Imported package requirements need a future package resolver before installation.

### plan

Evaluate requirements and print a plan with current evidence and remaining work. A pinned artifact proposed for installation is still remaining work. Planning may refresh catalog cache, call service health endpoints, and execute configured local probes. It installs no artifacts and records no learned outcomes.

### install

Compute a fresh plan, obtain the selected pinned artifacts, verify their SHA-256 digests, and publish them into the project-scoped Archer bin directory. Existing matching managed files are reused after verification; conflicting files are preserved and cause an error. Installed binaries are not executed by the artifact installer.

Installation is sequential. Earlier successful publications remain if a later step fails. After the installation loop completes, record verified service, local, and installed outcomes in scoped context. Unresolved requirements are reported and produce exit status 2.

### i

Alias for `install`.

### doctor

Alias for `plan`. It evaluates the selected requirements rather than inspecting the entire machine.

### context

Print project-scoped provider bindings, context revision, and recent verified outcomes as JSON. An `archer.json` configuration is required. No probes or catalog requests run. This command does not change context.

### capabilities

Print configured capability advertisements as a JSON array without probes or catalog requests. A native project configuration is required. Descriptors advertise potential routes rather than current installed state.

### resolve

Print `parallax.resolution/v1` JSON, including verified satisfaction, installable work, unresolved requirements, and a portable native approval. With `--stdin`, read an object containing `requirements` and optional `context`. Otherwise use selected project requirements. Parallax capability names use the lowercase machine-slug grammar; local package identifiers such as `npm:react` are not v1 protocol requirements.

Resolution status counts installable routes as covered. It does not claim installation. `context.probe_installed: false` suppresses planning-host local probes and installed-state claims.

### install with --stdin

Read a `parallax.handoff/v1` request addressed to and owned by `archer`. Require its complete `context.approved_resolution` object with schema `archer.approval/v1` and native `recipe_schema: archer.project/v1`. Validate integrity, approved scope, definitions, and routes before execution. Return an evidence-backed `parallax.handoff-result/v1` partition, including remaining work and the next owner.

PHP `archer/v1` installer approvals describe a different executor and are rejected before probes. The current adapter handles native service health, local probes, and raw executable recipes. See `docs/PROTOCOL.md` for the compatibility boundary.

### version

Print the executable's version and exit. Also available as `--version`.

### help

Print command usage and exit. With no arguments, Archer prints help. Also available as `--help` or `-h`.

## OPTIONS

Options may appear before or after the command and capability names. Supply option values as separate arguments; `--option=value` and the `--` end-of-options marker are unsupported.

### --project DIR

Read project configuration and manifests from DIR. Default: the current directory. Archer does not search parent directories for configuration.

### --home DIR

Use DIR for Archer cache, managed binaries, and scoped context. Overrides `ARCHER_HOME`. Default: `ARCHER_HOME` if set, otherwise `.archer` in the user's home directory.

### --json

Print plan, doctor, or installation output as `archer.plan/v1` JSON instead of the human summary. Diagnostics remain on standard error. `context` always prints JSON. This option does not change `init`, `help`, or `version` output.

### --plan

For `install` or `i`, perform the same evaluation as `plan` without obtaining or publishing artifacts. This is not a prohibition on health requests, catalog-cache updates, or configured probes.

### --stdin

For `resolve`, read a resolution request. For `install` or `i`, read a Parallax handoff. Input must be one UTF-8 JSON object, at most 2 MiB, with bounded nesting and no duplicate keys or trailing second object. Protocol responses always use JSON. Positional selectors, `--plan`, and local `--approval PLAN_ID` cannot be combined with stdin requests.

### --preflight

Require `install --stdin` or `i --stdin`. Verify frozen native routes without downloading or publishing artifacts, refreshing catalog cache, or writing context. Missing readiness returns pending work with exit status 2. Caller assertions about installed software do not establish readiness.

### --offline

Skip Archer's catalog refreshes and service health requests. Services remain unverified. Configured local probes still run and are not network-sandboxed. Artifact installation requires bytes already present in the verified content-addressed cache; an uncached recipe pointing to a local source file also remains unresolved. Offline mode does not assume that a remote service is ready.

### --approval PLAN_ID

For an actual `install` or `i` operation, require the freshly computed plan ID to match PLAN_ID before obtaining or publishing artifacts. A mismatch returns an error and the current plan. This check is optional; ordinary installs do not prompt for approval. It is ignored by planning-only operations.

The ID binds project sources, project identity and scope, context revision, platform, selected routes, and service descriptors. It is a drift guard, not a publisher signature, package lockfile, or portable Slipstream approval. Successful installations can change context revision and therefore the next plan ID.

### --help, -h

Print usage. Use the standalone `archer --help` form.

### --version

Print the executable's version. Use the standalone `archer --version` form.

## CONFIGURATION

`archer.json` must be one JSON object with schema `archer.project/v1` and a nonempty `scope`. Requirements are declared in a `requirements` array; omitted or null requirements are treated as empty. A requirement is a capability-name string or an object containing `capability`, optional `delivery`, and optional `constraint`. Delivery may be `service`, `local`, or `package`. Declare constraints in configuration; positional arguments are capability names, not free-form intent or version expressions.

`services` contains explicit bindings with `id`, `provides`, `endpoint`, and `health_url`; optional `token_env` names a local environment variable containing a bearer token. Health and service endpoints must share an origin. HTTP 200 establishes health-endpoint availability, not full functional integration or tenant entitlement.

`local` contains capabilities with `id`, `provides`, and a `probe` object. `probe.argv` is an explicit argument array; optional `probe.contains` requires matching output. Probes run in the project directory with the inherited environment, a five-second timeout, and at most 16 KiB of retained combined output. They bypass a shell unless a shell is explicitly named. Read-only behavior is the configuration author's responsibility.

`registries` lists configured catalog URLs using HTTPS or directly dialed loopback HTTP. Accepted catalogs are JSON objects containing a `services` array with schema `archer.catalog/v1`, a Parallax `products` array, or a normalized `capabilities` descriptor array. Top-level arrays are rejected. Freshness is 300 seconds. Remote catalog data cannot select local credential variables or probe commands. No service index is configured by default.

`recipes` contains raw executable installation recipes with `id`, `provides`, `filename`, optional `platforms`, and `artifact`. The artifact requires `url` and a 64-character SHA-256 digest in `sha256`. Platform names use OS/architecture pairs such as `linux/amd64`. Sources may be HTTPS URLs, absolute regular local files, local file URLs, or HTTP on directly dialed loopback. Artifacts are limited to 256 MiB. Redirects must retain their origin. Cache hits are rehashed before reuse.

## ENVIRONMENT

### ARCHER_HOME

Default Archer state directory. Overridden by `--home`.

### PARALLAX_SAAS_INDEX_URL

Add this catalog URL to the project's configured registries if not already present.

### ARCHER_CATALOG_ORIGIN

Explicit origin to which catalog credentials may be sent. Scheme, host, and effective port must match the requested catalog origin.

### ARCHER_CATALOG_TOKEN

Bearer token for catalogs matching `ARCHER_CATALOG_ORIGIN`. The credential affects cache identity through a hash; the token value is not stored in the catalog cache.

### SLIPSTREAM_TEAM

Team scope sent as `X-Slipstream-Team` only to the configured catalog origin. It also contributes to catalog cache identity.

### Service credential variables

Local `services[].token_env` bindings name their own environment variables. A missing configured credential prevents verification; Archer does not retry that binding through an anonymous catalog duplicate. Probes inherit the invoking process's environment, including PATH.

### Bootstrap variables

The bootstrap scripts additionally use `ARCHER_VERSION` for the pinned release tag, defaulting to `v0.1.0`, and `ARCHER_DIST_DIR` for a local release directory. These variables do not change CLI version or project recipe selection. Remote bootstrap requires published release assets; none have been published for this baseline.

## FILES

Paths below HOME refer to the selected Archer state directory, not the user's ordinary home directory.

```text
PROJECT/archer.json
    Project requirements, bindings, probes, registries, and recipes.

HOME/cache/sha256/PREFIX/DIGEST
    Verified artifact objects; PREFIX is the digest's first two characters.

HOME/cache/locks/DIGEST
    Per-artifact download lock directories.

HOME/catalogs/catalog/KEY.json
    Catalog metadata keyed by source and credential/team identity.

HOME/contexts/SCOPE_KEY.json
    Scoped bindings, revision, and at most 100 outcome events.

HOME/contexts/SCOPE_KEY.json.lock
    Context update lock directory.

HOME/projects/SCOPE_KEY/bin/FILENAME
    Managed project executables.

HOME/versions/TAG/bin/archer
    POSIX bootstrap's versioned executable.

HOME/versions/TAG/bin/archer.exe
    Windows bootstrap's versioned executable.

HOME/bin/archer or HOME/bin/archer.cmd
    Bootstrap launcher; POSIX uses a managed symlink.
```

SCOPE_KEY is derived from the explicit project scope and canonical absolute project directory. Moving the project changes its local identity. Shared artifact bytes do not imply shared project context. Add the displayed project bin directory to PATH when direct execution is needed; installation does not change the parent shell's environment.

## EXIT STATUS

### 0

Command completed successfully. For plan, doctor, and installation, all selected requirements are satisfied. An empty requirement set is satisfied.

For `resolve`, status `satisfied` or `partial` returns 0 because installable routes count as coverage. Its `satisfied` array still contains only verified state.

### 1

Configuration, integrity, approval-drift, execution, or output error. If a plan exists, it may be printed before the diagnostic. Previously published artifacts remain; there is no transaction-wide rollback.

### 2

Evaluation completed with remaining requirements. In a plan, this includes artifacts proposed for installation. In an installation, inspect each decision to distinguish verified success from unresolved work.

For `resolve`, 2 means no requirement is covered. For handoff/preflight, 2 means pending or partial verified work.

## EXAMPLES

Import the current project's declarations, then inspect remaining work:

```sh
archer init
archer plan --json
```

Inspect another project without obtaining artifacts:

```sh
archer i --project /path/to/project --plan
```

Evaluate one capability instead of the configured requirement list:

```sh
archer doctor database --project /path/to/project
```

Install configured pinned executables and inspect context:

```sh
archer i
archer context
```

Require an unchanged reviewed plan:

```sh
archer plan --json
archer i --approval PLAN_ID
```

Replace PLAN_ID with the `id` from the reviewed plan. Do not proceed if the fresh plan differs.

Reuse already cached local artifact inputs:

```sh
archer i --offline --home /path/to/archer-state
```

## LIMITATIONS

Composer/npm dependency solving and installation, archive extraction, autoload generation, lifecycle hooks, managed runtimes, Docker/Kubernetes/apt adapters, SaaS business actions, PHP manifest execution, and automatic worker callbacks are not implemented. The JSON `ai_eligible` field is an annotation; no model is invoked. It does not prove complete ecosystem discovery.

Remembered context affects service ordering but never replaces a current health, probe, or hash check. Early installation failures report a partial plan, including prior verified successes, without recording those outcomes into context. Abandoned artifact and context locks require inspection; Archer reports a bounded error rather than automatically deleting them.

## SEE ALSO

Project README, `docs/DESIGN.md`, and `examples/archer.json` in the Archer repository: https://github.com/Parallax-Web-Services/archer.
