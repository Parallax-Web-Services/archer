# Project runtimes and command execution

Archer prepares explicitly pinned PHP and Node runtimes before their package
trees, then launches project commands with those runtimes through `archer run`.
It first verifies a compatible local interpreter. When reuse cannot satisfy the
pin and the application's runtime requirements, it installs a configured bundle
whose SHA-256 digest is already declared by the project.

This is a deterministic route: there is no runtime catalog, `latest` lookup,
version solver, source compilation, or system package installation. Go remains
optional for users of the standalone Archer executable.

## Prepare Node and run a command

The following complete `archer.json` prepares Node 24.20.0 on Linux/amd64. It uses
the [official Node archive and checksum manifest](https://nodejs.org/dist/v24.20.0/).
For an existing application, add the `runtimes` member to its configuration and
retain its requirements, services, and source policies.

```json
{
  "schema": "archer.project/v1",
  "scope": "node-example",
  "requirements": [],
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

```sh
archer plan --json
archer i
archer run -- node --version
archer run -- node ./scripts/test.mjs
archer i --offline
```

The local Node candidate must report exactly `24.20.0`. If it does, Archer reuses
it. Otherwise the selected archive is verified, extracted into this project's
managed runtime directory, and probed before publication. A declared Node pin is
also checked against the root and complete selected npm lock's engine ranges.
An incompatible or unsupported range blocks package preparation and command
execution. Installing Node does not implicitly run npm or a package lifecycle hook.

The archive contains its upstream files, including npm; Archer's replay backend
does not invoke them. Linux runtime compatibility still depends on the host's
ABI and shared libraries. A successful version probe establishes that the
interpreter starts on this host; application readiness requires application checks.

## Runtime declarations

`runtimes` is an object keyed by `php` and/or `node`. Each declaration accepts:

| Field | Meaning |
| --- | --- |
| `version` | Required exact stable `x.y.z` version. Ranges, prereleases, and `latest` are rejected. |
| `binary` | Optional local candidate: an executable name on PATH, an absolute path, or a safe path relative to the project. The default is `php` or `node`. |
| `artifacts` | Optional array of pinned bundles, with at most one entry per `platform`. Without a matching bundle, unsuccessful local reuse remains unresolved. |
| `php_ini` | Optional PHP configuration file. For managed PHP it names a regular file inside the bundle; for local PHP it names an absolute file or safe project-relative file. Invalid for Node. |

Each bundle has `platform` (`linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`, `windows/amd64`, or `windows/arm64`), `format` (`raw`, `tar.gz`,
or `zip`), `artifact` containing `url` and a 64-character `sha256`, and
`executable` naming a safe relative path after extraction. `strip_prefix` may
remove an exact archive prefix. Every archive entry must be inside that prefix;
the installer does not guess or discard another root. Raw bundles cannot use it.
For a raw bundle, `executable` is the path Archer assigns to the downloaded file.

Choose the correct archive for the application and host, obtain its digest from
the selected publisher's trusted release record, and commit both values with
the exact runtime version. Archer checks consistency with that declared digest;
the digest does not establish publisher trust by itself. Local absolute files
and local file URLs are supported for prepared offline distributions. Remote
transport uses HTTPS; HTTP is restricted to directly dialed loopback.

`archer init` imports package requirements and does not choose runtime versions
or invent runtime download sources. Runtime pins are explicit project policy.
An existing `composer.php` setting and `runtimes.php.binary`, when both supplied,
must identify the same interpreter. If the runtime's `binary` is omitted,
`composer.php` supplies the PHP candidate when configured.

## PHP and extension readiness

An exact local PHP pin is reused only when it satisfies the complete selected
Composer graph, including extensions and conflicts. If that local interpreter
lacks a required extension and a pinned PHP bundle is configured, Archer can
select the bundle instead. The managed interpreter must pass the same graph
checks before Composer package installation. A standalone PHP version probe
does not establish extension readiness.

Prepare PHP from a bundle that contains the executable, compatible libraries,
required extensions, and any declared `php_ini`. PHP bundles follow the same
fields as the Node example. Archer does not compile PHP, obtain arbitrary
extension builds, install OS libraries, or assume that copying a system PHP
executable creates a portable distribution. Shared-library or extension failures
are reported by the actual host probes.

Managed PHP uses the pinned bundle's `php_ini` or an empty Archer-owned ini file.
Archer sets `PHPRC` to that selected file and empties `PHP_INI_SCAN_DIR`, preventing
ambient scan directories from silently adding configuration. A selected local
`php_ini` receives the same environment handling. Local PHP without `php_ini`
keeps its existing configuration behavior for compatibility. Managed PHP also
receives `ARCHER_PHP_ROOT` as its owned bundle root, so a bundled ini can use
`extension_dir="${ARCHER_PHP_ROOT}/ext"` and corresponding extension paths without
hard-coding the project-specific installation directory. A selected local ini
receives `ARCHER_PHP_ROOT` as the local executable's directory. Bundle extension
and library paths must remain valid in that selected environment.

```sh
archer i composer:laravel/framework
archer run -- php artisan test
archer run -- php vendor/bin/phpunit
```

These commands require a supported Composer project and its declared PHP runtime.
Package scripts remain unsupported; `run` is an explicit application command.

## Selection and installation

With no selectors, `archer plan` and `archer i` consider every declared runtime.
Selecting `npm:` work includes the declared Node runtime; selecting `composer:`
work includes the declared PHP runtime. An unrelated service or raw-artifact
selector does not prepare all project runtimes. `runtime:node` and `runtime:php`
select a declared runtime directly. Runtime requirements use local delivery;
an explicit requirement constraint must equal its configured exact version.

```sh
archer i runtime:node --project /path/to/project
archer i npm:react --project /path/to/project
archer run --project /path/to/project --home /path/to/state -- node --version
```

Runtime plans appear in the `runtimes` array of `archer.plan/v1`, with schema
`archer.runtime.v1`. States distinguish `local`, `managed`, `install`, and
`unresolved`. Planning runs current readiness probes and verifies existing
managed trees, but writes no runtime trees and downloads no runtime archives.
Installation prepares the selected runtimes before dependent Composer/npm work.
Earlier successful publication remains if a later package or runtime fails.

Project runtime trees live under
`HOME/projects/SCOPE_KEY/runtimes/KIND/RUNTIME_ID`. Their identity binds the
declaration, runtime kind, platform, and extraction policy. Archive bytes share
the SHA-256 content cache; mutable runtime trees are isolated by project scope.
Moving a project changes that scope. Runtime binaries and configuration files
are intended to remain unchanged after preparation.

Every managed reuse rehashes the cached archive, derives the expected whole-tree
inventory from it, checks installed files and links plus POSIX permissions, and probes the
exact runtime version again. Changing a tree and its receipt together does not
establish integrity. The cached archive must remain available even for an
already installed managed runtime. Modified, unmanaged, or conflicting paths
are preserved and rejected. Changing the pin chooses a new runtime identity;
Archer does not delete old runtime trees automatically.

Extraction rejects absolute paths, traversal, case collisions, hardlinks,
devices, and other special files. Safe internal relative symlinks are supported
for runtime archives such as the official Node bundle; link paths and targets
must remain within the verified tree. Compressed artifacts are limited to
256 MiB, expanded runtime trees to 512 MiB, and archives to 50,000 entries.
Staging and publication are bounded and use an owned receipt plus atomic rename.
An abandoned runtime lock requires inspection.

`--offline` preparation requires a complete verified cache. An uncached local
source file is still not a cache hit. A warm offline run obtains no new archive
bytes, but local runtime and graph checks still run.

## Execute project commands

```text
archer run [--project DIR] [--home DIR] [--offline] -- COMMAND [ARG ...]
```

`run` verifies every declared runtime and relevant frozen Composer/npm runtime
requirements on the current host. It does not prepare runtimes, download
artifacts, install dependencies, run service discovery, or establish package
tree integrity. Prepare the project with `archer i` first; use that command
again to verify its managed package trees.

The child runs in the project directory and receives the selected runtime bin
directories, the scoped Archer bin directory, and `node_modules/.bin` ahead of
the inherited PATH. A literal `php` or `node` command uses the selected
interpreter even when its filename differs. Other commands resolve through
those directories and the inherited PATH. The child inherits standard input,
output, and error, and Archer returns the child's exit code.

Archer refuses execution if that PATH would resolve `php` or `node` to a
different interpreter than the selected runtime. A local candidate with a custom
filename can therefore conflict with another interpreter in its shared bin
directory or inherited PATH. Use a runtime bundle with its own bin directory
when those paths cannot represent the selected runtimes consistently.

Everything after `--` is a direct argument array. Archer constructs no shell
expression. Shell expansion, pipelines, redirection, and compound commands
require an explicitly named shell. Use `node node_modules/vite/bin/vite.js`
or `php vendor/bin/phpunit` when an interpreter script needs explicit invocation.
An explicitly requested command can have ordinary application effects and
network access; `--offline` limits Archer's preparation behavior and does not
network-sandbox the child.

Node probes clear `NODE_OPTIONS`; commands with a declared Node runtime receive
it empty too. Managed PHP
uses the ini environment described above. Archer supplies `ARCHER_HOME` to the
child and does not change its parent shell's environment. Automatic activation,
build graphs, lifecycle execution, service readiness, and fresh-host OS/library
provisioning remain separate work.

The Parallax v1 worker rejects managed runtime preparation before effects.
Runtime selectors and dependent preparation use the local project interface;
unrelated approved service/local/raw-artifact work retains its existing behavior.
See [the worker protocol](PROTOCOL.md) and [the acceptance record](VALIDATION.md)
for the integration boundary and tested environments.
