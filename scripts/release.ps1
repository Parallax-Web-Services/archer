param(
    [string]$Go = 'go',
    [string]$Version = '0.3.0-alpha.3',
    [string]$OutputDir = ''
)
$ErrorActionPreference = 'Stop'
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?$' -or $Version.Contains('..')) { throw 'Unsafe release version.' }
$repo = Split-Path $PSScriptRoot -Parent
if (-not $OutputDir) { $OutputDir = Join-Path $repo "dist/v$Version" }
$output = [IO.Path]::GetFullPath($OutputDir)
[IO.Directory]::CreateDirectory($output) | Out-Null
$old = @{}
function Release-Hash([string]$Path) {
    $stream = [IO.File]::OpenRead($Path)
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return [BitConverter]::ToString($sha.ComputeHash($stream)).Replace('-', '').ToLowerInvariant() }
    finally { $sha.Dispose(); $stream.Dispose() }
}
foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOTOOLCHAIN')) { $old[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
Push-Location -LiteralPath $repo
try {
    $env:CGO_ENABLED = '0'
    $env:GOTOOLCHAIN = 'local'
    $sums = @()
    foreach ($os in @('linux', 'darwin', 'windows')) {
        foreach ($arch in @('amd64', 'arm64')) {
            $env:GOOS = $os
            $env:GOARCH = $arch
            $name = "archer_${os}_${arch}"
            if ($os -eq 'windows') { $name += '.exe' }
            $path = Join-Path $output $name
            & $Go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$Version" -o $path ./cmd/archer
            if ($LASTEXITCODE -ne 0) { throw "Build failed for $os/$arch." }
            $sums += ('{0}  {1}' -f (Release-Hash $path), $name)
        }
    }
    [IO.File]::WriteAllText((Join-Path $output 'SHA256SUMS'), (($sums | Sort-Object) -join "`n") + "`n", [Text.UTF8Encoding]::new($false))
    Write-Output "Built six Archer $Version artifacts and SHA256SUMS in $output"
} finally {
    Pop-Location
    foreach ($name in $old.Keys) { [Environment]::SetEnvironmentVariable($name, $old[$name], 'Process') }
}
