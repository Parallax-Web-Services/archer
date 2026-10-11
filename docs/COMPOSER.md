# Frozen Composer replay

Archer installs the complete package tree recorded in `composer.lock` using Go ZIP extraction and a generated PHP autoloader. Selecting a directly declared `composer:` requirement replays the whole lock, including development dependencies. The backend checks the installed PHP interpreter and required extensions before installation; it does not provision that interpreter yet.

```sh
archer plan composer:laravel/framework --project /path/to/project --json
archer i composer:laravel/framework --project /path/to/project --json
archer i composer:laravel/framework --project /path/to/project --offline --json
```

The selector must name a direct requirement in the project's `composer.json`. Its requested constraint must agree with that declaration. A service-delivery requirement cannot be satisfied by copying a PHP library into `vendor`.

## Source policy and receipts

The default `checksum` policy requires every package distribution to supply a valid SHA-1 `dist.shasum` in the lock. Archer verifies that checksum when downloading and reusing the archive. A lock without distribution checksums needs an explicit source policy. For the frozen Zefram fixture, the configuration is:

```json
{
  "composer": {
    "php": "/usr/bin/php",
    "source_policy": {
      "mode": "source-reference",
      "trusted_hosts": ["api.github.com", "codeload.github.com"]
    }
  }
}
```

This is the `composer` portion of `archer.json`; retain the rest of the project's configuration. Source-reference mode requires immutable 40-digit commit references and archive URLs that identify their repository and commit. Configured hosts must be explicit. When both GitHub hosts are trusted, Archer derives the corresponding immutable codeload route from an API archive URL, preserving the repository and commit while avoiding an API request for every package. Redirects must preserve that identity and remain inside the chosen source policy.

Each source identity receives an archive and a receipt published together in a private cache directory. The receipt binds the original distribution URL, any declared Git source URL, effective download URL, commit reference, any lock checksum, byte count, and the SHA-256 of the first downloaded bytes. A declared Git source must identify the same repository and commit as the distribution. Subsequent uses rehash the archive and compare that receipt. Archer preserves a corrupt or incomplete cache entry and reports an error instead of silently downloading replacement bytes.

**The receipt is local provenance, not a checksum supplied by the publisher or a signature.** Source-reference mode trusts the selected HTTPS hosts to serve the correct repository commit at first download. Continued integrity also relies on the receipt remaining trustworthy: an actor able to replace both the local archive and its receipt can replace that provenance. Read-only receipt permissions and publication without overwriting preserve normal installer behavior; they do not authenticate receipts against a malicious local account. npm replay has a different boundary because its SHA-512 expectations come from the frozen lock itself.

## Installation and verification

Archer binds manifest and lock bytes, Composer's root content hash, package metadata, and source policy into a frozen plan. Downloads have bounded concurrency and size. ZIP extraction strips one common archive root and accepts regular files and directories. It rejects traversal, absolute paths, symlinks, special files, multiple archive roots, and conflicting duplicate entries. A downloaded `composer.json` must identify the locked package when it declares a name.

The complete tree is built in a private directory on the project filesystem. Archer generates relative PHP autoload paths, installed-package metadata, and PHP executable proxies, checks the resulting file inventory, rechecks manifest and lock identity, and publishes `vendor` by rename. Composer, package plugins, lifecycle scripts, and application code are not executed by this backend.

An existing `vendor` directory must be managed by Archer for the same plan. Warm verification rehashes cached ZIPs, derives package file expectations from those archives, checks actual package bytes, regenerates expected autoload metadata from verified sources, and checks the complete installed inventory. Changing a vendor marker alongside a file does not establish integrity. Cached archives and receipts must remain available for warm verification; a warm offline run performs no downloads.

An unmanaged tree, modified file, changed lock, or unsupported profile produces a blocker and leaves the existing tree intact. Lock updates and replacement of an existing tree require a later explicit update implementation.

## Supported profile

The current backend supports frozen stable-version GitHub ZIP distributions for library and project package types, PSR-4 and PSR-0 loading, classmaps, file autoloading, installed-package metadata, and PHP binary proxies. The runtime must satisfy the PHP and extension requirements of the complete locked graph. Package requirements retain their recorded resolution; Archer does not run a dependency solver.

Custom repositories, Composer plugins, package scripts, metapackages, non-PHP executable proxies, custom vendor or binary directories, platform overrides, and partial production-only installs are outside this profile. Root classmaps that scan the whole project, vendor, or Archer's private staging directories are rejected because their output would depend on installation state. A project that needs a post-install script or readiness command must run it through a separately authorized execution step before claiming application readiness.

Limits are 2 MiB for the manifest, 16 MiB for the lock and each classmap/binary inspection source, 256 MiB per archive, 512 MiB expanded bytes and 100,000 entries per archive, 4 GiB and 200,000 entries per installed tree, and 32 MiB for the tree inventory. Failed attempts clean their private stages. Abandoned cache or project lock directories require inspection.

Composer content-hash generation preserves the supported PHP JSON encoding profile; decimal/exponent values in relevant content-hash fields are explicitly unsupported. Stable numeric dependency ranges and platform constraints are checked; prerelease/branch solving and general Composer semver compatibility are not claimed.

The compatibility contract follows Composer's [manifest schema](https://getcomposer.org/doc/04-schema.md), [runtime utilities](https://getcomposer.org/doc/07-runtime.md), and [vendor binary interface](https://getcomposer.org/doc/articles/vendor-binaries.md).

See the [Zefram acceptance record](VALIDATION.md) for application checks and the remaining installer work.
