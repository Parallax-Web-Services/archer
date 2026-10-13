# ARCHER

Straight & steady. One installer for Zefram and the Parallax ecosystem.

Install and run Archer as a standalone executable. **Go is optional**: it is
needed only to build Archer from source or develop it. A prebuilt Archer needs
no Go, PHP, Node, Composer, npm, Docker, Kubernetes or system package manager
to start. Project declarations can now pin PHP and Node runtimes for local reuse
or verified bundle installation. `archer run` launches application commands with
those selected runtimes.

Read the [product and technical design](docs/DESIGN.md) for the complete product
contract, current boundaries, Slipstream integration, caching architecture, and
the acceptance gates for a working Zefram environment.

The [command manual](docs/MANPAGE.md) documents the shipped CLI. Its Unix
manpage source is [archer.1](docs/archer.1).

On a Unix system with `man` available, read it with `man -l docs/archer.1`.

The current source includes the [Parallax worker adapter](docs/PROTOCOL.md):
`capabilities`, `resolve --stdin`, approved `install --stdin`, and read-only
`install --preflight --stdin`. It uses native `archer.project/v1` definitions;
PHP installer approvals cannot authorize different Go recipes.

The current source also includes [frozen npm v3 replay](docs/NPM.md). Selecting
a direct `npm:` requirement verifies and installs its whole locked dependency
tree with SHA-512 integrity. Lifecycle scripts and version solving are unsupported.
The [Zefram acceptance record](docs/VALIDATION.md) reports the real fixture's
47 passing PHP tests, 18 passing JavaScript tests, three frontend builds, and
offline replay of both dependency trees.

[Frozen Composer replay](docs/COMPOSER.md) now prepares the locked PHP `vendor`
tree, autoloader, runtime metadata, and PHP executable proxies in Archer.
PHP and extensions are checked before installation. Locks without archive
checksums require an explicit source-reference policy. Laravel projects can set it
in `.env.example` and `.env` instead of adding `archer.json`; VCS repositories and
branch versions from explicitly trusted GitLab hosts are supported.

[Managed runtimes and project execution](docs/RUNTIMES.md) complete the next
local preparation slice: exact PHP/Node pins, verified runtime archives,
project-scoped trees, current lock-graph compatibility checks, and direct command
execution. Runtime sources remain explicit project policy; OS libraries,
automatic builds, and service readiness need further adapters.

Archer is a clean-sheet CLI implemented in Go, with no third-party module
dependencies. Its implementation language adds no toolchain requirement for users.

The first milestone implements an explicit decision order:

1. Reuse a configured, freshly verified service.
2. Reuse a freshly verified local capability.
3. Reuse hash-verified artifact bytes from the shared content-addressed cache.
4. Install a pinned standalone artifact into a project-scoped Archer bin directory.
5. Return unresolved requirements, with a reason and an AI eligibility flag.

A known service that needs configuration or fails verification blocks a duplicate
installation. Stale or unavailable service discovery does not justify installation
or generation. Missing configured discovery also blocks unrestricted installation;
use an explicit `delivery: local` requirement for a local-only artifact route.
Package requirements cannot be satisfied by a similarly named
remote API. **There are no model calls in this version.**

## Install Archer

Following the nvm/Composer distribution approach, the POSIX and PowerShell scripts
install a pinned version under a per-user `ARCHER_HOME`, verify SHA-256 before
placement, retain old versions, and print PATH setup. They do not modify shell
startup files, use sudo, or execute downloaded binaries.

Use the [v0.3.0-alpha.4 prerelease](https://github.com/Parallax-Web-Services/archer/releases/tag/v0.3.0-alpha.4).
It supplies Linux, macOS and Windows binaries for amd64 and arm64, plus
`SHA256SUMS`. Windows/amd64 and Linux/amd64 have native execution checks;
macOS and arm64 artifacts are cross-built. The package backends have additional
platform limits documented in their respective guides.

On Linux/macOS, download the pinned bootstrap script and run it:

```sh
curl -fsSL https://raw.githubusercontent.com/Parallax-Web-Services/archer/v0.3.0-alpha.4/scripts/install.sh -o archer-install.sh
ARCHER_VERSION=v0.3.0-alpha.4 sh ./archer-install.sh
export PATH="$HOME/.archer/bin:$PATH"
```

For Windows:

```powershell
Invoke-WebRequest -Uri "https://raw.githubusercontent.com/Parallax-Web-Services/archer/v0.3.0-alpha.4/scripts/install.ps1" -OutFile archer-install.ps1
$env:ARCHER_VERSION = "v0.3.0-alpha.4"
powershell -NoProfile -ExecutionPolicy Bypass -File ./archer-install.ps1
$env:PATH = "$HOME/.archer/bin;$env:PATH"
```

The scripts obtain the selected binary and checksum manifest from that tagged
release. To use a prepared local release directory instead, set `ARCHER_DIST_DIR`
when running the same script:

```sh
ARCHER_DIST_DIR="/path/to/release" ARCHER_VERSION=v0.3.0-alpha.4 sh ./archer-install.sh
```

On Windows, set `$env:ARCHER_DIST_DIR = "C:/path/to/release"` before running
`./archer-install.ps1`. The directory must contain the selected platform binary
and `SHA256SUMS`; Go is not needed for this path either.

Checksums establish integrity against the release manifest; they are not
publisher signatures. Existing unmanaged binaries and directories are preserved.
System package-manager listings are a later distribution step.

## Use Archer

After installation and PATH setup:

Application commands assume a prepared project with the corresponding PHP/Node
runtime declarations; add those pins before `archer i` as shown below.

```sh
archer version
archer init
archer plan --json
archer i --plan
archer i
archer run -- php artisan test
archer run -- node ./scripts/test.mjs
archer context
```

Flags can follow capabilities:
`archer plan database --project /path/to/project --json`.

Project preparation exit codes: **0** all requirements satisfied, **2** remaining work,
**1** configuration, integrity or execution failure. Plans are machine-readable
`archer.plan/v1` documents. A planned installation remains unsatisfied until its
verified bytes are published. `archer run` preserves the child command's exit code.

`archer init` reads Composer/npm manifests and preserves package names and version
constraints in `archer.json`. It refuses to overwrite an existing configuration.
The file is optional: without it, Archer reads the same manifests directly and takes
`ARCHER_*` settings from the environment, `.env`, or `.env.example`, so a Laravel
application keeps its existing Composer and npm configuration unchanged.
Supported npm v3 and stable GitHub Composer locks can now be replayed directly.
Add exact runtime pins and bundles to `archer.json` when the project needs managed
PHP or Node; `init` does not choose runtime versions or sources. `archer i`
prepares those runtimes before packages. General dependency solving and lifecycle
execution remain subsequent work. See the backend documents for their supported profiles.

For example, a project can declare the official Node 24.20.0 Linux/amd64 archive:

```json
{
  "runtimes": {
    "node": {
      "version": "24.20.0",
      "artifacts": [{
        "platform": "linux/amd64",
        "format": "tar.gz",
        "artifact": {
          "url": "https://nodejs.org/dist/v24.20.0/node-v24.20.0-linux-x64.tar.gz",
          "sha256": "855d581f8a4eb1a8117e3426de25fe02770592febcfb31369aee1ffbfee9e8ec"
        },
        "strip_prefix": "node-v24.20.0-linux-x64",
        "executable": "bin/node"
      }]
    }
  }
}
```

This is the `runtimes` portion of an existing project configuration. An exact
compatible local Node is reused; otherwise Archer installs the pinned bundle.
The [runtime guide](docs/RUNTIMES.md) has a complete configuration and the PHP,
archive, offline, and `run` contracts. Commands after `run --` receive direct
argv, the project working directory, standard streams, and selected runtime PATH.
`run` verifies readiness and obtains no new artifacts; prepare with `archer i` first.

## Project context and services

Start from [examples/archer.json](examples/archer.json), replacing its explicitly
illustrative endpoint. No Parallax service URL is invented or configured by default.

```json
{
  "schema": "archer.project/v1",
  "scope": "zefram-maiden-voyage",
  "requirements": [{"capability": "database", "delivery": "service"}],
  "services": [{
    "id": "existing-database",
    "provides": ["database"],
    "endpoint": "https://your-configured-service.example",
    "health_url": "https://your-configured-service.example/health",
    "token_env": "MY_DATABASE_SERVICE_TOKEN"
  }]
}
```

Local explicit bindings may reference an environment variable for a bearer token.
The health endpoint must share the service origin. Health verification establishes
endpoint availability, not execution of business actions or a complete integration.
Remote catalog data cannot choose local credential variables or probe commands.

Optional `registries` accepts HTTPS catalog URLs. `PARALLAX_SAAS_INDEX_URL` adds the
configured Parallax index. Accepted shapes include native `archer.catalog/v1`,
Parallax product indexes and Slipstream's normalized capability descriptors.
Endpointless advertisements remain unverified candidates.

Authenticated catalog access uses `ARCHER_CATALOG_TOKEN` and `SLIPSTREAM_TEAM`
only when `ARCHER_CATALOG_ORIGIN` explicitly matches the origin. Catalog cache
identity includes credential/team scope, without storing credentials. Freshness
is 300 seconds; failed refreshes expose a stale diagnostic and block fallback.

Read-only `local` probes are explicit argv arrays in local project configuration.
They bypass a shell unless the operator deliberately declares one. They are
trusted instructions, not sandboxed scripts. Availability does not establish
version compatibility: version-constrained capabilities remain unresolved pending
the version resolver.

## Pinned artifacts

Raw executable recipes use an exact SHA-256 digest:

```json
{
  "id": "my-tool",
  "provides": ["my-tool"],
  "filename": "my-tool",
  "platforms": ["linux/amd64"],
  "artifact": {
    "url": "https://your-trusted-origin.example/my-tool",
    "sha256": "REPLACE_WITH_THE_64_CHARACTER_SHA256_DIGEST"
  }
}
```

Local absolute files and local file URLs support offline work. HTTP is restricted
to directly dialed loopback; normal remote transport uses HTTPS. Downloads are
bounded, redirects retain their origin, and partial bytes are never published.

The shared store rehashes every hit. Per-digest locks deduplicate concurrent
downloads. Atomic publication never overwrites different existing files or follows
symlinks. Abandoned locks cause a bounded error and require explicit inspection.

Managed binaries live at `ARCHER_HOME/projects/<scope-hash>/bin`. Add the displayed
directory to PATH as needed. `--offline` allows cached artifact installs; services
remain unverified offline. `--approval PLAN_ID` rejects changed source, scope,
context, platform or selected routes before artifact installation.

Successful `i` runs remember scoped bindings and a bounded outcome history.
Scope includes the project identity and its explicit scope name. Memory influences
selection but never replaces current service/probe/hash verification. Planning
does not persist learned outcomes. Cached bytes are shared; context is project-scoped.

## Optional source build and development

Use Go 1.24+ only if you choose to build or develop Archer. There are no module
dependencies. The installed executable does not locate or invoke `go`.

```sh
go build -trimpath -buildvcs=false -o dist/archer ./cmd/archer
./dist/archer version
```

To verify development changes:

```sh
go test ./...
python scripts/test-installers.py
```

`scripts/release.ps1` cross-builds Linux, macOS and Windows for amd64 and arm64,
then writes `SHA256SUMS`. Cross-compilation is not native-platform execution QA.

Current limits: general Composer/npm dependency solving, lifecycle hooks,
automatic build/readiness orchestration, runtime catalog selection or source
compilation, OS libraries and PHP extension provisioning, Docker/Kubernetes/apt
adapters, SaaS business-action execution, automatic Slipstream worker callbacks,
and AI fallback are future slices. Archer currently replays its supported locks,
prepares explicit pinned runtime bundles and raw artifacts, runs requested project
commands, verifies available capabilities, and reports remaining work.

MIT licensed.
