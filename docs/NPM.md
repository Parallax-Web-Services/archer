# Frozen npm replay

Archer can materialize the exact registry archive tree recorded in a version 3 `package-lock.json`. This is the first package installation backend. It uses Go directly and does not invoke npm, Composer, a shell, or package lifecycle code.

```sh
go build -trimpath -buildvcs=false -o dist/archer ./cmd/archer
dist/archer init --project /path/to/project
dist/archer plan npm:react --project /path/to/project --json
dist/archer i npm:react --project /path/to/project --json
dist/archer i npm:react --project /path/to/project --offline --json
```

Replace `npm:react` with a direct declaration in that project's `package.json`. Selecting any supported npm requirement replays the **whole locked tree**, including development dependencies, nested placements, and supported optional packages. It does not install just that selector. Selecting npm requirements leaves Composer requirements outside the invocation; a full `archer i` evaluates both supported backends under the project's source and runtime policy.

## What the backend proves

The manifest's direct declarations must match the lock's root declarations. A selected project requirement with a constraint must exactly match that declaration. Archer binds the original source bytes, normalized OS/CPU/libc target, recorded versions, package paths, and archive integrity into `archer.npm-plan/v1`. Required dependency and peer references must resolve through the recorded ancestor `node_modules` placements. Optional absence requires explicit optional metadata.

Archer replays an existing resolution. It does not independently evaluate npm version ranges, solve peers, select versions, or prove Node engine compatibility. Node engine requirements appear in the plan. Running the application requires a compatible runtime; this backend does not provision one.

Each selected archive requires a SHA-512 SRI digest. Downloads enter a shared content-addressed cache and are rehashed before extraction or reuse. HTTPS redirects stay on the original origin. HTTP is limited to literal loopback fixture hosts. URL credentials and fragments are rejected.

Extraction permits regular files and directories under the archive's `package/` prefix. Absolute paths, traversal, links, special files, bundled dependency trees, and conflicting duplicate entries fail closed. Redundant `./` components and duplicate files are accepted only when normalized paths, content, sizes, and permissions agree. Package name and version must match the lock. Both declared lifecycle hooks and archive lifecycle hooks, including implicit `binding.gyp` builds, are unsupported.

The installer stages a complete `node_modules` tree in the project filesystem, validates it, and publishes it by rename. POSIX executable links use the corresponding parent `.bin` directory. An existing tree must be an Archer-managed tree for the same frozen plan; unmanaged, changed, and updated-lock trees are preserved and produce an error.

Warm verification rehashes installed files and links, then derives their expected inventory again from the pinned cache archives. Editing the marker alongside the tree cannot establish integrity. Cache archives must therefore remain available for warm verification. An offline warm run downloads nothing.

Vite may leave an empty root `node_modules/.vite-temp` directory after removing its temporary configuration bundle. Verification permits only that exact safe empty directory. A file, link, or nonempty directory there causes verification to fail; finish the build before checking again.

## Current boundaries

Only `package-lock.json` version 3 with registry tarballs is supported. An existing `npm-shrinkwrap.json` is rejected because it takes precedence in npm. Workspaces, aliases, Git/file dependencies, bundled dependencies, lifecycle scripts, native builds, lock updates, production-only selection, and Windows executable shims need later implementations. Windows plans containing executable links fail explicitly.

Limits are 2 MiB for the manifest, 16 MiB for the lock, 256 MiB per compressed archive, 512 MiB expanded bytes and 100,000 entries per archive, 4 GiB and 200,000 entries per tree, and 32 MiB for its marker. Interrupted attempts clean their staging directories; verified cache bytes remain reusable. Abandoned lock directories require inspection.

Package replay is available through the local project commands. Parallax v1 handoff requirements cannot contain colon-prefixed package identifiers, so the existing native worker interface continues to handle service/local/raw-artifact capabilities.

The format follows npm's documented [lockfile tree](https://docs.npmjs.com/cli/v11/configuring-npm/package-lock-json/) and [package metadata](https://docs.npmjs.com/cli/v11/configuring-npm/package-json/). Those documents describe npm's broader behavior; the supported subset above defines Archer's current contract.

See the [Zefram acceptance record](VALIDATION.md) for the tested source, environment, commands, timings, and remaining product boundary.
