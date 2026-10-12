# Zefram preparation and execution acceptance record

On 11 October 2026, Archer's local project CLI installed a frozen Zefram frontend dependency tree, passed the application's JavaScript tests and production builds, and repeated installation offline without changing dependency files. The live Zefram checkout was read only; source, caches, dependencies, and build output lived in disposable directories under Archer's ignored QA workspace.

The subsequent combined run used one `archer i` to prepare both PHP and npm dependencies with existing runtimes. On 12 October 2026, a further run also prepared pinned PHP and Node bundles, then passed all 47 PHP tests with 333 assertions, all 18 JavaScript tests, and three frontend builds through `archer run`. The runtime sources and Composer source policy are explicit prerequisites, described below.

## Inputs and environment

The fixture is Zefram commit [c8edd7771a0be62b73014874f000e013fedd9d29](https://github.com/Parallax-Web-Services/zefram/commit/c8edd7771a0be62b73014874f000e013fedd9d29). Its original manifests and locks were retained. No installed `node_modules` or `vendor` tree was copied into the fixture.

| Input | SHA-256 |
| --- | --- |
| package.json | `1a7228fe4c395ecdefdd86f8ccb205d57f1edd292e97c7a33aaed8bc73207fb2` |
| package-lock.json | `7a57ced24fa15c20f4ee7e749d82d7271847301305395f844ee0cc07fb835013` |
| composer.json | `6f6a543a3e092929add9646035e32ac987599fdcf5276ca9e23d759ccdc07158` |
| composer.lock | `f9d6fae6195f8b4abb33534c4be503af39d2b14adcff65e956fc31bb6652879a` |

The execution host was Ubuntu under WSL2, kernel `6.18.33.2-microsoft-standard-WSL2`, Linux x64 with glibc. Archer was built with Go 1.27.2. Application checks used an already installed Node `v24.20.0`; Archer did not provision this runtime.

The npm v3 lock contains 158 non-root entries and 11 direct declarations. Target filtering selected 112 package placements and excluded 46 optional packages for other platforms. Nested dependency placements and POSIX executable links were preserved.

## CLI checks

The source-built CLI performed this sequence on fresh frozen source and an empty isolated Archer cache:

```sh
archer init --project FIXTURE --home CACHE
archer plan npm:react --json --project FIXTURE --home CACHE
archer i npm:react --json --approval PLAN_ID --project FIXTURE --home CACHE
archer i npm:react --json --offline --project FIXTURE --home CACHE
```

`FIXTURE` and `CACHE` are disposable directories. `PLAN_ID` is the ID from the preceding reviewable plan. Planning returned exit 2 for pending work and created no cache or dependency tree. Installation returned 0 and verified the complete locked tree, including development dependencies; the selected React requirement was not treated as a standalone install.

| Run | Installed placements | Optional exclusions | Archive downloads | Cache reuses | Elapsed |
| --- | ---: | ---: | ---: | ---: | ---: |
| Fresh project, empty Archer cache | 112 | 46 | 107 | 5 | 6.448 s |
| Same tree, offline | 112 | 46 | 0 | 112 | 1.331 s |

The five cold-run reuses were duplicate pinned content encountered within that run. Timings include CLI planning, verification, installation, and scoped outcome recording. They are single observations on this host and network, with no Composer/npm comparison or general speedup claim. No model was called.

## Application checks

Using the installed tree and the existing Node runtime, the fixture ran its declared JavaScript tests:

```sh
node --test packages/zefram-js/tests/runtime.test.js \
  resources/js/Modules/Notes/tests/browser.test.js \
  resources/js/tests/slipstream-kit.test.js
```

All 18 tests passed. Its entire frontend build sequence also passed without invoking npm:

```sh
node node_modules/vite/bin/vite.js build
node node_modules/vite/bin/vite.js build --config vite.browser.config.js
node node_modules/vite/bin/vite.js build --config vite.preview.config.js
node scripts/copy-preview.js
```

The three Vite builds transformed 578, 578, and 78 modules respectively. A further offline `archer i npm:react` returned 0 with zero downloads. Independent snapshots confirmed the dependency file bytes, modes, and executable-link targets were unchanged after repetition and builds. Vite's empty temporary cache directory is the narrowly documented exception to the frozen directory inventory.

## Implementation verification and remaining work

All seven Go packages passed native Windows and Linux tests. Coverage includes required/peer graph closure, platform filtering, source and plan drift, archive integrity, identical canonical duplicates, conflicting entries, traversal and link rejection, archive/tree limits, lifecycle hooks, native build requirements, unmanaged and changed trees, offline replay, executable links, and context concurrency. The PHP bridge and native Parallax worker also passed their integration checks.

This initial run demonstrated npm replay. The combined PHP/npm run below extends the same frozen fixture; its remaining boundary is managed runtimes and authorized application/service orchestration.

## Combined PHP/npm acceptance

A second fresh checkout of the same archived commit and an empty shared Archer home were used. The four source hashes above were checked again and remained identical. PHP was the existing `/usr/bin/php` 8.5.8 with the required extensions; Node was the existing 24.20.0 runtime. The Composer lock includes 77 production packages and 38 development packages, all materialized by this profile. The complete graph requires PHP at least 8.4.1 despite the root's `^8.3` declaration; the runtime check uses the full graph.

This fixture's Composer lock supplies no distribution SHA-1 checksums. Its `archer.json` explicitly selected `source-reference` with `api.github.com` and `codeload.github.com` as trusted hosts, as documented in [COMPOSER.md](COMPOSER.md). Downloads retain the locked repository and 40-digit commit reference. First-download SHA-256 receipts record local provenance; they are not publisher checksums or signatures. That trust boundary differs from the lock-provided npm SHA-512 expectations.

```sh
archer init --project FIXTURE --home CACHE
# Set the documented composer.php and composer.source_policy fields in archer.json.
archer plan --json --project FIXTURE --home CACHE
archer i --json --approval PLAN_ID --project FIXTURE --home CACHE
archer i --json --offline --project FIXTURE --home CACHE
```

The reviewed plan returned exit 2 for proposed work. It created neither dependency tree nor cache. The full installation returned 0 with no remaining requirements, generated PHP autoloading and installed-package metadata, and prepared PHP executable proxies. No Composer or npm command was executed.

| Run | PHP packages | npm placements | Archive downloads | Cache reuses | Elapsed |
| --- | ---: | ---: | ---: | ---: | ---: |
| Fresh project, empty combined cache | 115 | 112 | 115 PHP + 107 npm | 5 npm | 19.200 s |
| Both trees, offline | 115 | 112 | 0 | 115 PHP + 112 npm | 4.210 s |

These are single CLI observations, including planning, PHP readiness probes, source checks, publication or verification, and outcome recording. They are not comparative Composer/npm benchmarks.

The same JavaScript test and build commands above passed in the combined fixture. PHP application preparation and tests were separately executed:

```sh
php artisan package:discover --no-ansi
php vendor/bin/phpunit --no-progress --colors=never
```

Package discovery completed for the locked Laravel providers. PHPUnit 12.5.38 reported `OK (47 tests, 333 assertions)`. The test environment used an ephemeral application key and in-memory SQLite; Node was on PATH for the PHP tests that verify generated JavaScript. No live application database or environment file was copied or changed.

A final full offline `archer i` after the tests and builds returned 0 with zero archive downloads. Independent snapshots confirmed that vendor and node_modules file bytes, modes, and executable-link targets remained unchanged. No model was called.

All eight Go packages passed native Windows and Linux tests. Additional checks cover exact Composer content-hash, locked graph providers/replacements/conflicts, PHP and extension constraints, four-segment version boundaries, source policy/repository/reference binding, receipt and archive reuse, unsafe ZIP rejection, bounded reads, PSR loading/classmaps/file ordering, generated metadata and PHP proxies, source drift, modified/unmanaged trees, and core runtime barriers.

This established locked package installation and application compatibility for this fixture. At that stage, PHP/Node runtime provisioning, application execution through Archer, and functional Parallax service readiness remained pending. The installer skips package scripts and plugins; the application checks above were explicitly executed after installation.

## Managed runtime preparation and project execution

On 12 October 2026, a third fresh archive of the same Zefram commit retained all four source hashes above. Its empty Archer home and starting PATH contained no PHP, Node, Go, Composer, npm, apt, Docker, or kubectl executable. The PATH supplied only basic host utilities. Archer itself was a standalone Linux/amd64 executable, built with Go 1.27.2.

The project declared exact runtime versions and SHA-256-pinned bundles:

| Runtime | Bundle | SHA-256 |
| --- | --- | --- |
| Node 24.20.0 | Official `node-v24.20.0-linux-x64.tar.gz` | `855d581f8a4eb1a8117e3426de25fe02770592febcfb31369aee1ffbfee9e8ec` |
| PHP 8.5.8 | Host QA bundle `php-8.5.8-hostqa-linux-amd64.tar.gz` | `608e632373fcfef08dd4b05fbdac6b917870de03dab71e613ea7871abe841063` |

The Node digest was checked against the [official release checksum manifest](https://nodejs.org/dist/v24.20.0/SHASUMS256.txt). The PHP QA bundle copied the existing host binary and 37 dynamic extensions, reproduced its 56 loaded modules, and supplied a pinned configuration using `ARCHER_PHP_ROOT` for the extension directory. Its provenance record retained file hashes and host library dependencies. This PHP bundle depends on the host's existing shared libraries; it is not a published portable PHP distribution or evidence of provisioning a fresh OS. No host runtime or system package was installed or changed.

The same explicit Composer source-reference policy remained necessary. After `archer init`, the project configuration added the runtime declarations described in [RUNTIMES.md](RUNTIMES.md). Planning selected both runtime bundles and the complete frozen package graphs. One reviewed installation then prepared everything:

```sh
archer plan --json --project FIXTURE --home CACHE
archer i --json --approval PLAN_ID --project FIXTURE --home CACHE
```

| Run | Managed runtimes | PHP packages | npm placements | Package archive downloads | Elapsed |
| --- | --- | ---: | ---: | --- | ---: |
| Fresh project and cache | PHP 8.5.8, Node 24.20.0 | 115 | 112 | 115 PHP + 107 npm | 25.052 s |
| Same project, offline | Both verified and reused | 115 | 112 | 0 | 11.576 s |

Both runs returned exit 0 with no remaining requirements and no model calls. The cold run also obtained the two pinned runtime bundles. Five npm content reuses occurred within that run. Offline replay verified retained runtime archives, runtime inventories, and both dependency trees without downloads. These timings are single observations, include complete verification, and make no Composer/npm speed comparison.

Every application command used `archer run` with the selected managed runtimes. Package discovery succeeded. The JavaScript test command above passed all 18 tests. All three Vite builds succeeded, transforming 578, 578, and 78 modules, and the preview-copy command succeeded. Frontend builds preceded the final PHP tests so the HTTP tests could read the generated Vite manifest:

```sh
archer run --project FIXTURE --home CACHE -- php artisan package:discover --no-ansi
archer run --project FIXTURE --home CACHE -- node --test \
  packages/zefram-js/tests/runtime.test.js \
  resources/js/Modules/Notes/tests/browser.test.js \
  resources/js/tests/slipstream-kit.test.js
archer run --project FIXTURE --home CACHE -- node node_modules/vite/bin/vite.js build
archer run --project FIXTURE --home CACHE -- node node_modules/vite/bin/vite.js build --config vite.browser.config.js
archer run --project FIXTURE --home CACHE -- node node_modules/vite/bin/vite.js build --config vite.preview.config.js
archer run --project FIXTURE --home CACHE -- node scripts/copy-preview.js
archer run --project FIXTURE --home CACHE -- php vendor/bin/phpunit --no-progress --colors=never
archer i --offline --json --project FIXTURE --home CACHE
```

PHPUnit reported `OK (47 tests, 333 assertions)` on PHP 8.5.8. Tests used an ephemeral key, in-memory SQLite, array cache/session/mail, and a synchronous queue. No live database or environment file was copied. Independent before/after snapshots confirmed that vendor and node_modules file bytes, modes, and link targets remained unchanged after all tests, builds, and offline replay; empty Vite cache directories were excluded from that snapshot.

All nine Go packages passed native Windows and Linux tests; Go vet passed. The 18 Windows/POSIX bootstrap checks, nine native Linux bootstrap checks, and ten Linux PHP bridge checks also passed. Runtime checks cover exact version pins, full Composer PHP/extension requirements, all selected npm Node engine constraints, bounded archive extraction, link safety, source drift, local and managed evidence, post-probe integrity, offline reuse, and approval stability after runtime preparation. Execution checks cover literal argument forwarding, standard streams, project working directory, runtime environment, selected executable precedence, and child exit codes. Runtime effects remain local-CLI-only; the Parallax worker rejects unsupported runtime requests before planning or installation.

This completes the explicit runtime-bundle and project-command slice for the frozen fixture. Curated portable runtime distributions, OS-library provisioning, automatic build/readiness orchestration, and functional Parallax service readiness remain further acceptance gates.
