param(
    [string]$Go = 'go',
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$SourceCommit,
    [ValidateSet('clean', 'modified')][string]$SourceTree = 'modified',
    [Parameter(Mandatory = $true)][string]$OutputDir
)
$ErrorActionPreference = 'Stop'
if ($Version -notmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?$') { throw 'Unsafe release version.' }
if ($SourceCommit -notmatch '^[0-9a-f]{40}$') { throw 'SourceCommit must be the full Git commit SHA.' }
$output = [IO.Path]::GetFullPath($OutputDir)
if (Test-Path -LiteralPath $output) {
    $item = Get-Item -LiteralPath $output -Force
    if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or @(Get-ChildItem -LiteralPath $output -Force).Count -ne 0) { throw 'Release output must be a new or empty regular directory.' }
}
$repo = Split-Path $PSScriptRoot -Parent
$goVersion = & $Go version
if ($LASTEXITCODE -ne 0) { throw 'Go toolchain unavailable; Go is only required to build release artifacts.' }
& (Join-Path $PSScriptRoot 'release.ps1') -Go $Go -Version $Version -OutputDir $output
foreach ($asset in @(
    @{ Source = 'scripts/install.sh'; Name = 'install.sh' },
    @{ Source = 'scripts/install.ps1'; Name = 'install.ps1' },
    @{ Source = 'docs/archer.1'; Name = 'archer.1' },
    @{ Source = 'docs/MANPAGE.md'; Name = 'MANPAGE.md' },
    @{ Source = 'README.md'; Name = 'README.md' },
    @{ Source = 'LICENSE'; Name = 'LICENSE' }
)) {
    [IO.File]::Copy((Join-Path $repo $asset.Source), (Join-Path $output $asset.Name), $false)
}
$record = [ordered]@{
    schema = 'archer.release-build/v1'
    version = $Version
    source_commit = $SourceCommit
    source_tree = $SourceTree
    go_toolchain = $goVersion.Trim()
    cgo_enabled = $false
    targets = @('linux/amd64', 'linux/arm64', 'darwin/amd64', 'darwin/arm64', 'windows/amd64', 'windows/arm64')
    flags = @('-trimpath', '-buildvcs=false', '-s', '-w', "-X main.version=$Version")
}
[IO.File]::WriteAllText((Join-Path $output 'BUILDINFO.json'), ($record | ConvertTo-Json -Depth 5) + "`n", [Text.UTF8Encoding]::new($false))
$entries = foreach ($asset in (Get-ChildItem -LiteralPath $output -File | Where-Object Name -ne 'SHA256SUMS' | Sort-Object Name)) {
    '{0}  {1}' -f (Get-FileHash -LiteralPath $asset.FullName -Algorithm SHA256).Hash.ToLowerInvariant(), $asset.Name
}
[IO.File]::WriteAllText((Join-Path $output 'SHA256SUMS'), ($entries -join "`n") + "`n", [Text.UTF8Encoding]::new($false))
Write-Output "Packaged Archer ${Version}: six native binaries, bootstrap files, documentation, and build record."
