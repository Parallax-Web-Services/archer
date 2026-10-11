# Zefram npm acceptance record

On 11 October 2026, Archer's local project CLI installed a frozen Zefram frontend dependency tree, passed the application's JavaScript tests and production builds, and repeated installation offline without changing dependency files. The live Zefram checkout was read only; source, caches, dependencies, and build output lived in disposable directories under Archer's ignored QA workspace.

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

This demonstrates npm lock replay and Zefram frontend compatibility for the specified inputs. It does not complete the full installer acceptance gate: Composer packages and PHP autoloading, managed PHP/Node runtimes, service contract readiness, and application-wide PHP tests remain pending. Locked PHP distributions also need an explicit provenance policy because this fixture's Composer lock does not supply distribution checksums.
