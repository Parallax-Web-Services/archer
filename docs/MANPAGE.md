# ARCHER 1

Archer 0.2.0-alpha.2. User Commands. 12 October 2026.

This reference covers the v0.2.0-alpha.2 prerelease, including native worker protocol and frozen npm/Composer replay.

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

This version installs individual raw executable artifacts and replays supported npm v3 and Composer locks. Non-package version constraints remain unresolved. It makes no model calls. Worker mode returns Parallax handoff results; the caller owns transport, task claims, and callbacks.

The prebuilt Archer executable requires no Go toolchain. Go is optional and needed only when compiling or developing Archer from source. Application runtime requirements remain separate: Composer replay verifies PHP and its extensions, and application execution requires compatible PHP or Node as declared by the project.

## COMMANDS

### init

Create `archer.json` in the selected project directory. Use the directory name as the initial scope. Read `require` and `require-dev` from `composer.json`, and `dependencies`, `devDependencies`, and `optionalDependencies` from `package.json`. Record package names and constraints as requirements prefixed with `composer:` or `npm:`.

Existing configuration is never overwritten. Missing manifests are allowed; malformed declarations are errors. The import does not consume lockfile graphs or retain dependency-group distinctions. Package planning subsequently reads each complete manifest and supported lock.

### plan

Evaluate requirements and print a plan with current evidence and remaining work. A pinned artifact proposed for installation is still remaining work. Planning may refresh catalog cache, call service health endpoints, and execute configured local probes. It installs no artifacts and records no learned outcomes.

### install

Compute a fresh plan, obtain the selected pinned artifacts, verify their SHA-256 digests, and publish them into the project-scoped Archer bin directory. Existing matching managed files are reused after verification; conflicting files are preserved and cause an error. Installed binaries are not executed by the artifact installer.

For a direct npm requirement, replay the whole supported v3 lock into project `node_modules`, including development dependencies and nested placements. Verify SHA-512 archives and stage the complete tree before publication. Existing managed trees are freshly verified; unmanaged, changed, and updated-lock trees are refused. Package code and lifecycle scripts are not executed. Recorded Node engine requirements are reported without runtime provisioning or independent range solving. See `docs/NPM.md`.

For a direct Composer library requirement, replay the complete supported lock into `vendor`, including development dependencies, and generate PHP autoloading, installed metadata, and PHP binary proxies. Check the selected PHP interpreter and extensions against the complete graph. A directly selected PHP/extension requirement verifies runtime readiness without installing the library tree. Composer is not invoked. Default source policy requires locked SHA-1 distribution checksums; missing checksums need explicit `composer.source_policy` with `mode: source-reference` and trusted hosts. First-download SHA-256 receipts record local provenance and rely on those HTTPS hosts and local receipt trust. Scripts and plugins are skipped. See `docs/COMPOSER.md`.

Raw artifact publication is sequential; package downloads use bounded concurrency. Earlier successful publications remain if a later backend fails. After installation completes, record verified service, local, and installed outcomes in scoped context. Unresolved requirements are reported and produce exit status 2.

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

The bootstrap scripts additionally use `ARCHER_VERSION` for the pinned release tag, defaulting to `v0.2.0-alpha.2`, and `ARCHER_DIST_DIR` for a local release directory. These variables do not change CLI version or project recipe selection. Without a local directory, bootstrap obtains the selected binary and checksum manifest from the tagged GitHub release. Neither path requires a Go toolchain.

## FILES

Paths below HOME refer to the selected Archer state directory, not the user's ordinary home directory.

```text
PROJECT/archer.json
    Project requirements, bindings, probes, registries, and recipes.

HOME/cache/sha256/PREFIX/DIGEST
    Verified artifact objects; PREFIX is the digest's first two characters.

HOME/cache/locks/DIGEST
    Per-artifact download lock directories.

HOME/cache/sha512/PREFIX/DIGEST
    Verified npm archives; PREFIX is the digest's first two characters.

PROJECT/node_modules/.archer-install.json
    Frozen npm tree identity and inventory; verification also checks pinned archives.

PROJECT/.archer-npm-install.lock
    Project-level npm publication lock directory.

HOME/cache/composer/sources/PREFIX/IDENTITY
    Composer ZIP archive and first-download provenance receipt for a source identity.

PROJECT/vendor/.archer-install.json
    Frozen Composer tree inventory; verification also checks cached ZIPs and receipts.

PROJECT/vendor/autoload.php
    Generated PHP autoloader entry point.

PROJECT/.archer-composer-install.lock
    Project-level Composer publication lock directory.

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

General dependency solving, lifecycle hooks, managed runtimes, Docker/Kubernetes/apt adapters, SaaS business actions, legacy PHP installer manifest execution, and automatic worker callbacks are not implemented. npm replay requires a supported v3 registry lock; workspaces, native builds, updated-lock replacement, and Windows executable shims are unsupported. Composer replay requires stable GitHub ZIP distributions and a supported autoload profile; plugins, custom installers, platform overrides, broad root classmaps, and non-PHP executable proxies are unsupported. The JSON `ai_eligible` field is an annotation; no model is invoked. It does not prove complete ecosystem discovery.

Remembered context affects service ordering but never replaces a current health, probe, or hash check. Early installation failures report a partial plan, including prior verified successes, without recording those outcomes into context. Abandoned artifact and context locks require inspection; Archer reports a bounded error rather than automatically deleting them.

## SEE ALSO

Project README, `docs/DESIGN.md`, `docs/PROTOCOL.md`, `docs/NPM.md`, `docs/COMPOSER.md`, and `examples/archer.json` in the Archer repository: https://github.com/Parallax-Web-Services/archer.
