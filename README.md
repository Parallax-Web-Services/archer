# ARCHER

Straight & steady. One installer for Zefram and the Parallax ecosystem.

Read the [product and technical design](docs/DESIGN.md) for the complete product
contract, current boundaries, Slipstream integration, caching architecture, and
the acceptance gates for a working Zefram environment.

The [command manual](docs/MANPAGE.md) documents the shipped CLI. Its Unix
manpage source is [archer.1](docs/archer.1).

On a Unix system with `man` available, read it with `man -l docs/archer.1`.

Archer is a clean-sheet, dependency-free Go CLI. The executable needs no PHP,
Node, Composer, npm, Docker, Kubernetes or system package manager to start.

The first milestone implements an explicit decision order:

1. Reuse a configured, freshly verified service.
2. Reuse a freshly verified local capability.
3. Reuse hash-verified artifact bytes from the shared content-addressed cache.
4. Install a pinned standalone artifact into a project-scoped Archer bin directory.
5. Return unresolved requirements, with a reason and an AI eligibility flag.

A known service that needs configuration or fails verification blocks a duplicate
installation. Stale or unavailable service discovery does not justify installation
or generation. Package requirements cannot be satisfied by a similarly named
remote API. **There are no model calls in this version.**

## Use the local build

The repository's first release artifacts are built locally, not published:

```sh
./dist/v0.1.0/archer_linux_amd64 version
./dist/v0.1.0/archer_linux_amd64 init
./dist/v0.1.0/archer_linux_amd64 plan --json
./dist/v0.1.0/archer_linux_amd64 i --plan
./dist/v0.1.0/archer_linux_amd64 i
./dist/v0.1.0/archer_linux_amd64 context
```

On Windows use `archer_windows_amd64.exe`. Flags can follow capabilities:
`archer plan database --project /path/to/project --json`.

Exit codes: **0** all requirements satisfied, **2** remaining work,
**1** configuration, integrity or execution failure. Plans are machine-readable
`archer.plan/v1` documents. A planned installation remains unsatisfied until its
verified bytes are published.

`archer init` reads Composer/npm manifests and preserves package names and version
constraints in `archer.json`. It refuses to overwrite an existing configuration.
Package dependency solving, archive extraction, autoload generation and lifecycle
hooks are not implemented yet. Such requirements stay unresolved.

## Direct bootstrap distribution

Following the nvm/Composer distribution approach, the POSIX and PowerShell scripts
install a pinned version under a per-user `ARCHER_HOME`, verify SHA-256 before
placement, retain old versions, and print PATH setup. They do not modify shell
startup files, use sudo, or execute downloaded binaries.

For the local release on Linux/macOS:

```sh
ARCHER_DIST_DIR="$PWD/dist/v0.1.0" ARCHER_VERSION=v0.1.0 sh scripts/install.sh
export PATH="$HOME/.archer/bin:$PATH"
```

For Windows:

```powershell
$env:ARCHER_DIST_DIR = "$PWD/dist/v0.1.0"
$env:ARCHER_VERSION = "v0.1.0"
./scripts/install.ps1
$env:PATH = "$HOME/.archer/bin;$env:PATH"
```

Omitting `ARCHER_DIST_DIR` selects the pinned GitHub release URL. No release has
been published yet. Checksums establish integrity against the release manifest;
they are not publisher signatures. Existing unmanaged binaries and directories
are preserved. System package-manager listings are a later distribution step.

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

## Develop and verify

Go 1.24+ is needed only to develop Archer. There are no module dependencies.

```sh
go test ./...
go build -trimpath -buildvcs=false -o dist/archer ./cmd/archer
python scripts/test-installers.py
```

`scripts/release.ps1` cross-builds Linux, macOS and Windows for amd64 and arm64,
then writes `SHA256SUMS`. Cross-compilation is not native-platform execution QA.

Current limits: Composer/npm package resolution and installation, managed runtime
provisioning, Docker/Kubernetes/apt adapters, SaaS business-action execution,
automatic Slipstream worker callbacks, and AI fallback are future slices.
Archer currently prepares and installs pinned raw artifacts, verifies available
capabilities, and returns the remaining work honestly.

MIT licensed.
