# Pinned, per-user Archer bootstrap. Downloaded binaries are never executed here.
$ErrorActionPreference = 'Stop'
$version = if ($env:ARCHER_VERSION) { $env:ARCHER_VERSION } else { 'v0.2.0-alpha.1' }
if ($version -notmatch '^v\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?$' -or $version.Contains('..')) { throw 'ARCHER_VERSION must be a safe pinned version, for example v0.2.0-alpha.1.' }
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'This installer supports Windows. Use install.sh on Linux or macOS.' }
$machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($machine.ToLowerInvariant()) { 'amd64' { 'amd64' } 'arm64' { 'arm64' } default { throw 'Supported architectures: amd64 and arm64.' } }
$artifact = "archer_windows_${arch}.exe"
$inputRoot = if ($env:ARCHER_HOME) { $env:ARCHER_HOME } else { Join-Path $env:USERPROFILE '.archer' }
if (-not [IO.Path]::IsPathRooted($inputRoot) -or $inputRoot -match '(?:^|[\\/])\.{1,2}(?:[\\/]|$)') { throw 'ARCHER_HOME must be absolute without dot components.' }
$root = [IO.Path]::GetFullPath($inputRoot).TrimEnd([char[]]'\/')
if ($root -eq [IO.Path]::GetPathRoot($root).TrimEnd([char[]]'\/')) { throw 'ARCHER_HOME cannot be a filesystem root.' }

function Assert-DirectoryPath([string]$Path) {
    $probe = [IO.Path]::GetFullPath($Path)
    while ($probe) {
        $item = Get-Item -LiteralPath $probe -Force -ErrorAction SilentlyContinue
        if ($item) {
            if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or -not $item.PSIsContainer) { throw "Unsafe directory path: $probe" }
        }
        $parent = [IO.Path]::GetDirectoryName($probe)
        if ($parent -eq $probe) { break }
        $probe = $parent
    }
}
function Assert-RegularFile([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "Missing regular file: $Path" }
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw "Linked file refused: $Path" }
}
function Hash([string]$Path) {
    $stream = [IO.File]::OpenRead($Path)
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return [BitConverter]::ToString($sha.ComputeHash($stream)).Replace('-', '').ToLowerInvariant() }
    finally { $sha.Dispose(); $stream.Dispose() }
}
function Remove-TemporaryDirectory([string]$Path, [string]$Parent, [string]$Prefix) {
    $absolute = [IO.Path]::GetFullPath($Path).TrimEnd([char[]]'\/')
    $allowedParent = [IO.Path]::GetFullPath($Parent).TrimEnd([char[]]'\/')
    if ([IO.Path]::GetDirectoryName($absolute) -ne $allowedParent -or -not [IO.Path]::GetFileName($absolute).StartsWith($Prefix)) { throw 'Temporary cleanup escaped its intended directory.' }
    if (Test-Path -LiteralPath $absolute) {
        Assert-DirectoryPath $absolute
        Remove-Item -LiteralPath $absolute -Recurse -Force
    }
}
function Launcher([string]$Tag) { return "@echo off`r`nsetlocal`r`n`"%~dp0..\versions\$Tag\bin\archer.exe`" %*`r`nexit /b %errorlevel%`r`n" }
function Fetch([string]$Name, [long]$Limit, [string]$Target) {
    if ($env:ARCHER_DIST_DIR) {
        $source = Join-Path $env:ARCHER_DIST_DIR $Name
        Assert-RegularFile $source
        if ((Get-Item -LiteralPath $source).Length -gt $Limit) { throw "Distribution file exceeds size limit: $Name" }
        [IO.File]::Copy($source, $Target, $false)
        return
    }
    $uri = "https://github.com/Parallax-Web-Services/archer/releases/download/$version/$Name"
    $request = [Net.HttpWebRequest]::Create($uri)
    $request.Timeout = 30000
    $request.ReadWriteTimeout = 30000
    $request.MaximumAutomaticRedirections = 5
    $request.UserAgent = 'Archer-bootstrap/0.2.0'
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $response = $null; $stream = $null; $output = $null
    try {
        $response = $request.GetResponse()
        if ($response.ResponseUri.Scheme -ne 'https') { throw 'Non-HTTPS redirect refused.' }
        if ($response.ContentLength -gt $Limit) { throw "Download exceeds size limit: $Name" }
        $stream = $response.GetResponseStream()
        $output = [IO.File]::Open($Target, [IO.FileMode]::CreateNew)
        $buffer = New-Object byte[] 65536
        $total = 0L
        $deadline = [DateTime]::UtcNow.AddSeconds(120)
        while (($n = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
            $total += $n
            if ($total -gt $Limit -or [DateTime]::UtcNow -gt $deadline) { throw "Download limit exceeded: $Name" }
            $output.Write($buffer, 0, $n)
        }
    } finally {
        if ($output) { $output.Dispose() }
        if ($stream) { $stream.Dispose() }
        if ($response) { $response.Dispose() }
    }
}
foreach ($path in @($root, (Join-Path $root 'bin'), (Join-Path $root 'versions'), (Join-Path $root "versions/$version"))) { Assert-DirectoryPath $path }
$tempParent = [IO.Path]::GetTempPath()
Assert-DirectoryPath $tempParent
$temp = Join-Path $tempParent ('archer-download-' + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($temp) | Out-Null
$stage = $null
try {
    Fetch 'SHA256SUMS' 1048576 (Join-Path $temp 'SHA256SUMS')
    Fetch $artifact 134217728 (Join-Path $temp $artifact)
    $entries = @(Get-Content -LiteralPath (Join-Path $temp 'SHA256SUMS') | Where-Object { $_ -match ('^([0-9a-fA-F]{64})  ' + [regex]::Escape($artifact) + '$') })
    if ($entries.Count -ne 1) { throw "Missing or duplicate checksum for $artifact" }
    $expected = $entries[0].Substring(0, 64).ToLowerInvariant()
    if ((Hash (Join-Path $temp $artifact)) -ne $expected) { throw "Checksum mismatch for $artifact" }
    $marker = Join-Path $root '.archer-managed-v1'
    $bin = Join-Path $root 'bin'
    $versions = Join-Path $root 'versions'
    if (Get-Item -LiteralPath $marker -Force -ErrorAction SilentlyContinue) {
        Assert-RegularFile $marker
        if ([IO.File]::ReadAllText($marker).Trim() -ne 'archer-install-root/v1') { throw 'Invalid installation ownership marker.' }
    } else {
        if (Get-Item -LiteralPath $versions -Force -ErrorAction SilentlyContinue) { throw 'Unmanaged versions directory refused.' }
        foreach ($name in @('archer', 'archer.exe', 'archer.cmd', 'archer.ps1', 'archer.bat')) {
            if (Get-Item -LiteralPath (Join-Path $bin $name) -Force -ErrorAction SilentlyContinue) { throw "Unmanaged binary refused: $name" }
        }
    }
    foreach ($name in @('archer', 'archer.exe', 'archer.ps1', 'archer.bat')) {
        if (Get-Item -LiteralPath (Join-Path $bin $name) -Force -ErrorAction SilentlyContinue) { throw "Unmanaged binary refused: $name" }
    }
    $target = Join-Path $bin 'archer.cmd'
    $launcherMarker = Join-Path $bin '.archer-launcher-v1'
    if (Get-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue) {
        Assert-RegularFile $target
        Assert-RegularFile $launcherMarker
        $previous = [IO.File]::ReadAllText($launcherMarker).Trim()
        if ($previous -notmatch '^v\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?$' -or $previous.Contains('..')) { throw 'Invalid launcher ownership marker.' }
        if ([IO.File]::ReadAllText($target) -ne (Launcher $previous)) { throw 'Edited or unmanaged launcher refused.' }
        $previousDir = Join-Path $versions $previous
        Assert-DirectoryPath (Join-Path $previousDir 'bin')
        Assert-RegularFile (Join-Path $previousDir 'bin/archer.exe')
        Assert-RegularFile (Join-Path $previousDir '.archer-version-v1')
        if ([IO.File]::ReadAllText((Join-Path $previousDir '.archer-version-v1')).Trim() -ne $previous) { throw 'Invalid installed version marker.' }
    }
    $versionDir = Join-Path $versions $version
    if (Test-Path -LiteralPath $versionDir) {
        Assert-DirectoryPath (Join-Path $versionDir 'bin')
        Assert-RegularFile (Join-Path $versionDir '.archer-version-v1')
        Assert-RegularFile (Join-Path $versionDir 'bin/archer.exe')
        if ([IO.File]::ReadAllText((Join-Path $versionDir '.archer-version-v1')).Trim() -ne $version) { throw 'Invalid version marker.' }
        if ((Hash (Join-Path $versionDir 'bin/archer.exe')) -ne $expected) { throw 'Installed version differs from the pinned release.' }
    } else {
        [IO.Directory]::CreateDirectory($bin) | Out-Null
        [IO.Directory]::CreateDirectory($versions) | Out-Null
        if (-not (Test-Path -LiteralPath $marker)) { [IO.File]::WriteAllText($marker, 'archer-install-root/v1') }
        $stage = Join-Path $root ('.archer-install-' + [Guid]::NewGuid().ToString('N'))
        [IO.Directory]::CreateDirectory((Join-Path $stage 'bin')) | Out-Null
        [IO.File]::Copy((Join-Path $temp $artifact), (Join-Path $stage 'bin/archer.exe'), $false)
        [IO.File]::WriteAllText((Join-Path $stage '.archer-version-v1'), $version)
        [IO.Directory]::Move($stage, $versionDir)
        $stage = $null
    }
    # The cmd launcher avoids Windows symlink privileges and remains reviewable.
    $launcherTemp = Join-Path $bin ('.archer-launcher-' + [Guid]::NewGuid().ToString('N'))
    $launcherBackup = $launcherTemp + '.previous'
    [IO.File]::WriteAllText($launcherTemp, (Launcher $version), [Text.ASCIIEncoding]::new())
    try {
        if (Test-Path -LiteralPath $target) { [IO.File]::Replace($launcherTemp, $target, $launcherBackup) }
        else { [IO.File]::Move($launcherTemp, $target) }
    } finally {
        if (Test-Path -LiteralPath $launcherTemp) { Remove-Item -LiteralPath $launcherTemp -Force }
        if (Test-Path -LiteralPath $launcherBackup) { Remove-Item -LiteralPath $launcherBackup -Force }
    }
    [IO.File]::WriteAllText($launcherMarker, $version)
    Write-Output "Installed Archer $version to $target"
    Write-Output "Add this directory to your user PATH: $bin"
    Write-Output 'For the default location in this session: $env:PATH = "$env:USERPROFILE\.archer\bin;$env:PATH"'
} finally {
    # These unique paths are created by this invocation under verified parents.
    if ($stage) { Remove-TemporaryDirectory $stage $root '.archer-install-' }
    Remove-TemporaryDirectory $temp $tempParent 'archer-download-'
}
