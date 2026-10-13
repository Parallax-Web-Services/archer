# ARCHER 1

Archer 0.3.0-alpha.4. User Commands. 12 October 2026.

This reference covers the v0.3.0-alpha.4 runtime and execution slice, including native worker protocol and frozen npm/Composer replay.

## NAME

archer - resolve project requirements, reuse services, and install verified artifacts

## SYNOPSIS

```text
archer init [--project DIR]
archer plan [CAPABILITY ...] [OPTIONS]
archer install [CAPABILITY ...] [OPTIONS]
archer i [CAPABILITY ...] [OPTIONS]
archer run [--project DIR] [--home DIR] [--offline] -- COMMAND [ARG ...]
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

Archer prepares project capabilities by checking existing services and local software before obtaining new artifacts. Requirements come from `archer.json` in the selected project directory or, without it, from the project's Composer/npm manifests and environment (see CONFIGURATION). Positional capability names replace that requirement list for the current invocation; they do not add to it. Matching selectors retain the declared delivery and version policy.

For each compatible requirement, Archer prefers a configured service with a successful current health check, then an explicitly probed local capability, then a hash-verified managed executable or pinned executable recipe. A catalog advertisement alone does not establish configured access. An advertised service that needs configuration or verification blocks duplicate artifact installation. Missing, stale, or unavailable configured discovery blocks unrestricted fallback. Use `delivery: local` for an explicitly local-only artifact requirement.

This version installs raw executable artifacts and explicitly pinned PHP/Node runtime bundles, replays supported npm v3 and Composer locks, and launches project commands. Runtime versions must be exact stable pins; other non-package version constraints remain unresolved. It makes no model calls. Worker mode returns Parallax handoff results; the caller owns transport, task claims, and callbacks.

The prebuilt Archer executable requires no Go toolchain. Go is optional and needed only when compiling or developing Archer from source. Project runtime declarations choose local reuse or a verified bundle; Composer checks PHP and extensions, and declared Node pins are checked against npm engine requirements.

## COMMANDS

### init

Create `archer.json` in the selected project directory. Use the directory name as the initial scope. Read `require` and `require-dev` from `composer.json`, and `dependencies`, `devDependencies`, and `optionalDependencies` from `package.json`. Record package names and constraints as requirements prefixed with `composer:` or `npm:`.

Existing configuration is never overwritten. Missing manifests are allowed; malformed declarations are errors. The import does not consume lockfile graphs or retain dependency-group distinctions. Package planning subsequently reads each complete manifest and supported lock.

### plan

Evaluate requirements and print a plan with current evidence and remaining work. A pinned artifact proposed for installation is still remaining work. Planning may refresh catalog cache, call service health endpoints, and execute configured local probes. It installs no artifacts and records no learned outcomes.

### install

Compute a fresh plan, obtain the selected pinned artifacts, verify their SHA-256 digests, and publish them into the project-scoped Archer bin directory. Existing matching managed files are reused after verification; conflicting files are preserved and cause an error. Installed binaries are not executed by the artifact installer.

For a direct npm requirement, replay the whole supported v3 lock into project `node_modules`, including development dependencies and nested placements. Verify SHA-512 archives and stage the complete tree before publication. Existing managed trees are freshly verified; unmanaged, changed, and updated-lock trees are refused. Package code and lifecycle scripts are not executed. A declared Node pin is checked against the root and complete selected lock's supported engine ranges. Without a Node runtime declaration, engines are reported without a runtime compatibility claim. See `docs/NPM.md`.

For a direct Composer library requirement, replay the complete supported lock into `vendor`, including development dependencies, and generate PHP autoloading, installed metadata, and PHP binary proxies. Check the selected PHP interpreter and extensions against the complete graph. A directly selected PHP/extension requirement verifies runtime readiness without installing the library tree. Composer is not invoked. Default source policy requires locked SHA-1 distribution checksums; missing checksums need explicit `composer.source_policy` with `mode: source-reference` and trusted hosts. First-download SHA-256 receipts record local provenance and rely on those HTTPS hosts and local receipt trust. Scripts and plugins are skipped. See `docs/COMPOSER.md`.

Prepare selected declared runtimes before dependent package trees. Without selectors, include every runtime declaration. A `composer:` selector includes PHP, an `npm:` selector includes Node, and `runtime:php` or `runtime:node` selects the declared runtime directly. Unrelated capability selectors do not prepare all project runtimes. Reuse requires an exact local version and applicable graph readiness; a configured PHP bundle can replace local PHP that lacks a required extension.

Verify runtime archive SHA-256, extract and inventory a complete bounded tree, probe its version, and publish it under the project scope. Every managed reuse derives expected files, links, and POSIX permissions from the verified cached archive; the runtime receipt alone is insufficient. Modified or unmanaged paths are preserved and rejected. See `docs/RUNTIMES.md`.

Raw artifact publication is sequential; package downloads use bounded concurrency. Earlier successful publications remain if a later backend fails. After installation completes, record verified service, local, and installed outcomes in scoped context. Unresolved requirements are reported and produce exit status 2.

### i

Alias for `install`.

### run

Execute the explicit command after `--` in the project directory with freshly
verified declared runtimes. Check relevant frozen Composer PHP/extension and npm
Node engine requirements before starting the child. Obtain no artifacts,
prepare no runtimes, and run no catalog or service requests. Prepare the project
with `archer i` first. This command does not verify the installed package trees;
`archer i` performs their integrity checks.

Prepend selected runtime bin directories, the scoped Archer bin directory, and
project `node_modules/.bin` to inherited PATH. A literal `php` or `node` command
uses the selected interpreter even when its filename differs. Pass argv directly,
without a shell, and connect standard input, output, and error. Return the child
exit code. An explicitly named shell is required for shell expressions.

If the resulting PATH would resolve `php` or `node` to a different interpreter
than its selected runtime, refuse execution before starting the child. Use a
runtime bundle with its own bin directory to avoid conflicting interpreter paths.

Managed PHP uses its pinned ini or an empty Archer-owned ini through `PHPRC`
with `PHP_INI_SCAN_DIR` empty. Selected local PHP without an ini declaration keeps
ambient configuration. Node probes clear `NODE_OPTIONS`; launched commands with
a declared Node runtime receive it empty too.
The child receives `ARCHER_HOME`; the parent shell is unchanged. Accept only
`--project`, `--home`, and `--offline` before the separator. Offline mode does not
restrict the child's network access or application effects.

### doctor

Alias for `plan`. It evaluates the selected requirements rather than inspecting the entire machine.

### context

Print project-scoped provider bindings, context revision, and recent verified outcomes as JSON. No probes or catalog requests run. This command does not change context.

### capabilities

Print configured capability advertisements as a JSON array without probes or catalog requests. A native project configuration is required. Descriptors advertise potential routes rather than current installed state.

### resolve

Print `parallax.resolution/v1` JSON, including verified satisfaction, installable work, unresolved requirements, and a portable native approval. With `--stdin`, read an object containing `requirements` and optional `context`. Otherwise use selected project requirements. Parallax capability names use the lowercase machine-slug grammar; local package identifiers such as `npm:react` are not v1 protocol requirements.

Resolution status counts installable routes as covered. It does not claim installation. `context.probe_installed: false` suppresses planning-host local probes and installed-state claims.

### install with --stdin

Read a `parallax.handoff/v1` request addressed to and owned by `archer`. Require its complete `context.approved_resolution` object with schema `archer.approval/v1` and native `recipe_schema: archer.project/v1`. Validate integrity, approved scope, definitions, and routes before execution. Return an evidence-backed `parallax.handoff-result/v1` partition, including remaining work and the next owner.

PHP `archer/v1` installer approvals describe a different executor and are rejected before probes. The current adapter handles native service health, local probes, and raw executable recipes. Runtime preparation and routes dependent on configured runtimes are rejected before effects; use the local project commands. See `docs/PROTOCOL.md` for the compatibility boundary.

### version

Print the executable's version and exit. Also available as `--version`.

### help

Print command usage and exit. With no arguments, Archer prints help. Also available as `--help` or `-h`.

## OPTIONS

Options may appear before or after the command and capability names. Supply option values as separate arguments; `--option=value` is unsupported. The `--` separator is supported only for `run`, where all following arguments belong to the child command.

### --project DIR

Read project configuration and manifests from DIR. Default: the current directory. Archer does not search parent directories for configuration.

### --home DIR

Use DIR for Archer cache, managed binaries and runtime trees, and scoped context. Overrides `ARCHER_HOME`. Default: `ARCHER_HOME` if set, otherwise `.archer` in the user's home directory.

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

Runtime installation likewise requires cached archive bytes. Managed runtime reuse
still verifies those archives and the installed tree and executes current probes.
`run` obtains no artifacts in either mode, and `--offline` does not sandbox its child.

### --approval PLAN_ID

For an actual `install` or `i` operation, require the freshly computed plan ID to match PLAN_ID before obtaining or publishing artifacts. A mismatch returns an error and the current plan. This check is optional; ordinary installs do not prompt for approval. It is ignored by planning-only operations.

The ID binds project sources, project identity and scope, context revision, platform, selected routes, and service descriptors. It is a drift guard, not a publisher signature, package lockfile, or portable Slipstream approval. Successful installations can change context revision and therefore the next plan ID.

### --help, -h

Print usage. Use the standalone `archer --help` form.

### --version

Print the executable's version. Use the standalone `archer --version` form.

## CONFIGURATION

Without `archer.json`, Archer follows Laravel convention instead of requiring another root file. Requirements are the direct `require`/`require-dev` and npm dependency declarations, as `init` would record them, and Archer settings come from `ARCHER_*` variables. Process environment variables take precedence over `.env`, which takes precedence over the committed `.env.example`. Recognized keys are `ARCHER_SCOPE` (default: the directory name), `ARCHER_COMPOSER_SOURCE_POLICY` (`checksum` or `source-reference`), `ARCHER_COMPOSER_TRUSTED_HOSTS` (comma-separated hostnames), and `ARCHER_PHP` (equivalent to `composer.php`). `PARALLAX_SAAS_INDEX_URL` is read the same way. Services, probes, recipes, and runtime pins still require `archer.json`; when that file exists, it is used instead of `ARCHER_*` project settings.

`archer.json` must be one JSON object with schema `archer.project/v1` and a nonempty `scope`. Requirements are declared in a `requirements` array; omitted or null requirements are treated as empty. A requirement is a capability-name string or an object containing `capability`, optional `delivery`, and optional `constraint`. Delivery may be `service`, `local`, or `package`. Declare constraints in configuration; positional arguments are capability names, not free-form intent or version expressions.

`services` contains explicit bindings with `id`, `provides`, `endpoint`, and `health_url`; optional `token_env` names a local environment variable containing a bearer token. Health and service endpoints must share an origin. HTTP 200 establishes health-endpoint availability, not full functional integration or tenant entitlement.

`local` contains capabilities with `id`, `provides`, and a `probe` object. `probe.argv` is an explicit argument array; optional `probe.contains` requires matching output. Probes run in the project directory with the inherited environment, a five-second timeout, and at most 16 KiB of retained combined output. They bypass a shell unless a shell is explicitly named. Read-only behavior is the configuration author's responsibility.

`registries` lists configured catalog URLs using HTTPS or directly dialed loopback HTTP. Accepted catalogs are JSON objects containing a `services` array with schema `archer.catalog/v1`, a Parallax `products` array, or a normalized `capabilities` descriptor array. Top-level arrays are rejected. Freshness is 300 seconds. Remote catalog data cannot select local credential variables or probe commands. No service index is configured by default.

`recipes` contains raw executable installation recipes with `id`, `provides`, `filename`, optional `platforms`, and `artifact`. The artifact requires `url` and a 64-character SHA-256 digest in `sha256`. Platform names use OS/architecture pairs such as `linux/amd64`. Sources may be HTTPS URLs, absolute regular local files, local file URLs, or HTTP on directly dialed loopback. Artifacts are limited to 256 MiB. Redirects must retain their origin. Cache hits are rehashed before reuse.

`runtimes` is an optional map keyed by `php` and/or `node`. Each entry requires
an exact stable `version` in `x.y.z` form and accepts a local `binary` candidate,
an `artifacts` array, and PHP-only `php_ini`. The candidate defaults to its runtime
name on PATH; paths may be absolute or safe project-relative paths. Each bundle
declares `platform`, `format` (`raw`, `tar.gz`, or `zip`), `artifact` with `url` and
`sha256`, `executable` as a safe relative path, and optional exact `strip_prefix`.
At most one bundle is allowed per platform. Raw bundles cannot strip a prefix.

Managed PHP's `php_ini` names a regular file inside its bundle. Local PHP's ini
may be absolute or project-relative. Managed PHP without one uses an empty owned
ini. The bundle must supply compatible extensions and libraries; copying PHP does
not provision OS libraries. An explicit `composer.php` and `runtimes.php.binary`
must agree. `init` does not choose runtime pins or sources.

Managed PHP receives `ARCHER_PHP_ROOT` as the owned bundle root. Its ini can use
`extension_dir="${ARCHER_PHP_ROOT}/ext"` instead of hard-coding a scoped path.
A selected local ini receives the local executable's directory as that value.

Runtime archives support bounded regular files, directories, and safe internal
relative symlinks. Traversal, escaping links, hardlinks, devices, case collisions,
and special files are rejected. Compressed artifacts are limited to 256 MiB,
expanded runtime trees to 512 MiB, and archives to 50,000 entries. There is no
runtime catalog, automatic version selection, or source compilation.

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

### Runtime child environment

For managed PHP and local PHP with a declared ini, Archer supplies `PHPRC`,
empties `PHP_INI_SCAN_DIR`, and sets `ARCHER_PHP_ROOT` as described above. A declared
Node runtime supplies an empty `NODE_OPTIONS`. `run` prepends project runtime and
executable directories to PATH and sets the selected `ARCHER_HOME` for its child.
Other environment variables are inherited; Archer does not modify the parent shell.

### Bootstrap variables

The bootstrap scripts additionally use `ARCHER_VERSION` for the pinned release tag, defaulting to `v0.3.0-alpha.4`, and `ARCHER_DIST_DIR` for a local release directory. These variables do not change CLI version or project recipe selection. Without a local directory, bootstrap obtains the selected binary and checksum manifest from the tagged GitHub release. Neither path requires a Go toolchain.

## FILES

Paths below HOME refer to the selected Archer state directory, not the user's ordinary home directory.

```text
PROJECT/archer.json
    Optional project requirements, bindings, probes, registries, and recipes.

PROJECT/.env, PROJECT/.env.example
    ARCHER_* settings used when archer.json is absent.

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

HOME/projects/SCOPE_KEY/runtimes/KIND/RUNTIME_ID
    Project-scoped pinned runtime tree; KIND is php or node.

HOME/projects/SCOPE_KEY/runtimes/KIND/RUNTIME_ID/.archer-runtime.json
    Runtime receipt and inventory; reuse also derives expectations from the archive.

HOME/projects/SCOPE_KEY/runtimes/KIND/RUNTIME_ID.lock
    Runtime publication lock directory.

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

Command completed successfully. For plan, doctor, and installation, all selected requirements and included runtime declarations are satisfied. A project with no selected requirements or runtime declarations is satisfied.

For `run`, the child exited successfully. Other child exit codes are preserved.

For `resolve`, status `satisfied` or `partial` returns 0 because installable routes count as coverage. Its `satisfied` array still contains only verified state.

### 1

Configuration, integrity, approval-drift, execution, or output error. If a plan exists, it may be printed before the diagnostic. Previously published artifacts remain; there is no transaction-wide rollback.

### 2

Evaluation completed with remaining requirements. In a plan, this includes artifacts proposed for installation. In an installation, inspect each decision to distinguish verified success from unresolved work.

For `resolve`, 2 means no requirement is covered. For handoff/preflight, 2 means pending or partial verified work.

Before starting a child, `run` returns 2 for unavailable declared runtimes or
unsatisfied supported runtime graph requirements, and 1 for configuration,
integrity, or execution errors. Once a child starts, its exit code is preserved.

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

Prepare a declared Node runtime and execute project code:

```sh
archer i runtime:node
archer run -- node --version
archer run -- node ./scripts/test.mjs
```

Run application checks with declared runtimes:

```sh
archer i
archer run -- php artisan test
archer run -- node node_modules/vite/bin/vite.js build
```

## LIMITATIONS

General dependency solving, lifecycle hooks, automatic build/readiness orchestration, runtime catalog selection and source compilation, OS libraries and PHP extension provisioning, Docker/Kubernetes/apt adapters, SaaS business actions, legacy PHP installer manifest execution, and automatic worker callbacks are not implemented. npm replay requires a supported v3 registry lock; workspaces, native builds, updated-lock replacement, and Windows executable shims are unsupported. Composer replay requires stable GitHub ZIP distributions and a supported autoload profile; plugins, custom installers, platform overrides, broad root classmaps, and non-PHP executable proxies are unsupported. The JSON `ai_eligible` field is an annotation; no model is invoked. It does not prove complete ecosystem discovery.

Remembered context affects service ordering but never replaces a current health, probe, or hash check. Early installation failures report a partial plan, including prior verified successes, without recording those outcomes into context. Abandoned artifact and context locks require inspection; Archer reports a bounded error rather than automatically deleting them.

## SEE ALSO

Project README, `docs/DESIGN.md`, `docs/PROTOCOL.md`, `docs/RUNTIMES.md`, `docs/NPM.md`, `docs/COMPOSER.md`, and `examples/archer.json` in the Archer repository: https://github.com/Parallax-Web-Services/archer.
