[CmdletBinding()]
param(
    [string]$Version = $(if ($env:METIS_VERSION) { $env:METIS_VERSION } else { "latest" }),
    [string]$InstallDir = $(if ($env:METIS_INSTALL_DIR) { $env:METIS_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\Metis\bin" }),
    [string]$Repo = $(if ($env:METIS_REPO) { $env:METIS_REPO } else { "Ricardo-M-L/metis" }),
    [string]$Token = $(if ($env:METIS_GITHUB_TOKEN) { $env:METIS_GITHUB_TOKEN } else { $env:GITHUB_TOKEN }),
    [string]$ApiBase = $(if ($env:METIS_GITHUB_API_BASE) { $env:METIS_GITHUB_API_BASE } else { "https://api.github.com" }),
    [string]$WebBase = $(if ($env:METIS_GITHUB_WEB_BASE) { $env:METIS_GITHUB_WEB_BASE } else { "https://github.com" }),
    [switch]$SkipVersionCheck
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
Add-Type -AssemblyName System.Net.Http
Add-Type -AssemblyName System.IO.Compression.FileSystem

$VersionRetentionCount = 2
$StaleArtifactAge = [TimeSpan]::FromHours(1)
$MetadataTimeout = [TimeSpan]::FromSeconds(30)
$AssetTimeout = [TimeSpan]::FromSeconds(300)
$MaxArchiveBytes = 128MB
$MaxExpandedBytes = 128MB
$MaxChecksumBytes = 64KB

function Write-Step([string]$Message) {
    Write-Host "==> $Message" -ForegroundColor Cyan
}

function Resolve-Architecture {
    try {
        $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
    }
    catch {
        $arch = $(if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }).ToLowerInvariant()
    }
    switch ($arch) {
        "x64"   { return "amd64" }
        "amd64" { return "amd64" }
        "arm64" { return "arm64" }
        default  { throw "Unsupported Windows architecture: $arch" }
    }
}

function Resolve-ReleaseTag {
    if ($Version -ne "latest") {
        return $Version
    }

    if ($Token) {
        $headers = @{
            Authorization = "Bearer $Token"
            Accept = "application/vnd.github+json"
            "X-GitHub-Api-Version" = "2022-11-28"
            "User-Agent" = "metis-installer"
        }
        $release = Invoke-RestMethod -UseBasicParsing -Headers $headers -TimeoutSec ([int]$MetadataTimeout.TotalSeconds) -Uri "$ApiBase/repos/$Repo/releases/latest"
        if (-not $release.tag_name) {
            throw "GitHub response did not contain tag_name"
        }
        return [string]$release.tag_name
    }

    # Avoid the anonymous GitHub REST rate limit. The public web redirect is
    # enough to resolve the latest tag and requires no credentials.
    $handler = [System.Net.Http.HttpClientHandler]::new()
    $handler.AllowAutoRedirect = $true
    $client = [System.Net.Http.HttpClient]::new($handler)
    try {
        $client.Timeout = $MetadataTimeout
        $client.DefaultRequestHeaders.UserAgent.ParseAdd("metis-installer")
        $response = $client.GetAsync(
            "$WebBase/$Repo/releases/latest",
            [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead
        ).GetAwaiter().GetResult()
        try {
            $response.EnsureSuccessStatusCode() | Out-Null
            $tag = [Uri]::UnescapeDataString($response.RequestMessage.RequestUri.Segments[-1].TrimEnd("/"))
            if (-not $tag -or $tag -eq "latest") {
                throw "Could not resolve the latest public release tag"
            }
            return $tag
        }
        finally {
            $response.Dispose()
        }
    }
    finally {
        $client.Dispose()
        $handler.Dispose()
    }
}

function Download-FileWithLimit(
    [string]$Uri,
    [string]$Destination,
    [long]$MaxBytes,
    [TimeSpan]$Timeout,
    [string]$BearerToken
) {
    $handler = [System.Net.Http.HttpClientHandler]::new()
    $handler.AllowAutoRedirect = $true
    $client = [System.Net.Http.HttpClient]::new($handler)
    $cancellation = [System.Threading.CancellationTokenSource]::new()
    try {
        $client.Timeout = $Timeout
        $client.DefaultRequestHeaders.UserAgent.ParseAdd("metis-installer")
        if ($BearerToken) {
            $client.DefaultRequestHeaders.Authorization = [System.Net.Http.Headers.AuthenticationHeaderValue]::new("Bearer", $BearerToken)
        }
        $cancellation.CancelAfter($Timeout)
        $response = $client.GetAsync(
            $Uri,
            [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead,
            $cancellation.Token
        ).GetAwaiter().GetResult()
        try {
            $response.EnsureSuccessStatusCode() | Out-Null
            $contentLength = $response.Content.Headers.ContentLength
            if ($null -ne $contentLength -and [long]$contentLength -gt $MaxBytes) {
                throw "Download exceeds the $MaxBytes byte limit: $Uri"
            }

            $input = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
            try {
                $output = [System.IO.File]::Open(
                    $Destination,
                    [System.IO.FileMode]::CreateNew,
                    [System.IO.FileAccess]::Write,
                    [System.IO.FileShare]::None
                )
                try {
                    $buffer = New-Object byte[] 65536
                    [long]$total = 0
                    while (($read = $input.ReadAsync($buffer, 0, $buffer.Length, $cancellation.Token).GetAwaiter().GetResult()) -gt 0) {
                        $total += $read
                        if ($total -gt $MaxBytes) {
                            throw "Download exceeded the $MaxBytes byte limit while streaming: $Uri"
                        }
                        $output.Write($buffer, 0, $read)
                    }
                    $output.Flush($true)
                }
                finally {
                    $output.Dispose()
                }
            }
            finally {
                $input.Dispose()
            }
        }
        finally {
            $response.Dispose()
        }
    }
    catch {
        Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
        if ($cancellation.IsCancellationRequested) {
            throw "Download timed out after $([int]$Timeout.TotalSeconds) seconds: $Uri"
        }
        throw
    }
    finally {
        $cancellation.Dispose()
        $client.Dispose()
        $handler.Dispose()
    }
}

function Extract-MetisZipWithLimit([string]$ZipPath, [string]$DestinationDir) {
    $zip = [System.IO.Compression.ZipFile]::OpenRead($ZipPath)
    try {
        if ($zip.Entries.Count -ne 1) {
            throw "Release archive must contain exactly one root entry named metis.exe"
        }
        $entry = $zip.Entries[0]
        if ($entry.FullName -ne "metis.exe" -or $entry.Name -ne "metis.exe") {
            throw "Release archive must contain exactly one root entry named metis.exe"
        }
        if ($entry.Length -lt 0 -or $entry.Length -gt $MaxExpandedBytes) {
            throw "Expanded metis.exe exceeds the $MaxExpandedBytes byte limit"
        }

        New-Item -ItemType Directory -Path $DestinationDir -Force | Out-Null
        $destination = Join-Path $DestinationDir "metis.exe"
        $input = $entry.Open()
        try {
            $output = [System.IO.File]::Open(
                $destination,
                [System.IO.FileMode]::CreateNew,
                [System.IO.FileAccess]::Write,
                [System.IO.FileShare]::None
            )
            try {
                $buffer = New-Object byte[] 65536
                [long]$total = 0
                while (($read = $input.Read($buffer, 0, $buffer.Length)) -gt 0) {
                    $total += $read
                    if ($total -gt $MaxExpandedBytes) {
                        throw "Expanded metis.exe exceeded the $MaxExpandedBytes byte limit while extracting"
                    }
                    $output.Write($buffer, 0, $read)
                }
                if ($total -ne $entry.Length) {
                    throw "Expanded metis.exe length mismatch: got $total, expected $($entry.Length)"
                }
                $output.Flush($true)
            }
            finally {
                $output.Dispose()
            }
        }
        catch {
            Remove-Item -LiteralPath $destination -Force -ErrorAction SilentlyContinue
            throw
        }
        finally {
            $input.Dispose()
        }
        return $destination
    }
    finally {
        $zip.Dispose()
    }
}

function ConvertTo-VersionName([string]$Tag) {
    $name = $Tag.Trim()
    if ($name.StartsWith("v", [StringComparison]::OrdinalIgnoreCase)) {
        $name = $name.Substring(1)
    }
    if (-not $name -or $name.Length -gt 128 -or $name -eq "." -or $name -eq ".." -or $name -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]*$') {
        throw "Unsafe release tag: $Tag"
    }
    return $name
}

function Test-IsReparsePoint([System.IO.FileSystemInfo]$Item) {
    return (($Item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
}

function Get-SHA256Hex([string]$Path) {
    $info = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if ($info.PSIsContainer -or (Test-IsReparsePoint $info)) {
        throw "SHA-256 input must be a direct non-reparse regular file: $Path"
    }

    $stream = $null
    $sha256 = $null
    try {
        $stream = [System.IO.File]::Open(
            $Path,
            [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::Read,
            [System.IO.FileShare]::Read
        )
        $sha256 = [System.Security.Cryptography.SHA256]::Create()
        $digest = $sha256.ComputeHash($stream)
        return ([System.BitConverter]::ToString($digest)).Replace("-", "")
    }
    finally {
        if ($null -ne $sha256) {
            $sha256.Dispose()
        }
        if ($null -ne $stream) {
            $stream.Dispose()
        }
    }
}

function Assert-ManagedDirectory([string]$Path) {
    try {
        $info = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    }
    catch {
        throw "Managed directory cannot be inspected: $Path ($($_.Exception.Message))"
    }
    if (-not $info.PSIsContainer -or (Test-IsReparsePoint $info)) {
        throw "Managed directory must be a direct non-reparse directory: $Path"
    }
}

function Ensure-ManagedDirectory([string]$Path) {
    $existing = Get-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
    if ($null -eq $existing) {
        New-Item -ItemType Directory -Path $Path -Force -ErrorAction Stop | Out-Null
    }
    Assert-ManagedDirectory $Path
}

function Get-NormalizedPath([string]$Path) {
    return [System.IO.Path]::GetFullPath($Path).TrimEnd([char[]]@('\', '/'))
}

function Test-ProcessMatchesLock([int]$ProcessId, [long]$CreatedAt) {
    if ($ProcessId -le 0) {
        return "unknown"
    }
    try {
        $process = Get-Process -Id $ProcessId -ErrorAction Stop
    }
    catch [Microsoft.PowerShell.Commands.ProcessCommandException] {
        return "dead"
    }
    catch {
        return "unknown"
    }

    if ($CreatedAt -le 0) {
        return "unknown"
    }
    try {
        $created = [DateTimeOffset]::FromUnixTimeSeconds($CreatedAt).UtcDateTime
        $started = $process.StartTime.ToUniversalTime()
        # If the process began after this lock was created, the PID has been
        # reused and the old lock no longer protects a live Metis process.
        if ($started -gt $created.AddSeconds(5)) {
            return "dead"
        }
        return "alive"
    }
    catch {
        # Access to StartTime can be denied. Uncertainty must protect data.
        return "unknown"
    }
}

function Test-ValidLockNonce([string]$Nonce) {
    return ($Nonce -and $Nonce.Length -le 128 -and $Nonce -ne "." -and $Nonce -ne ".." -and
        $Nonce -match '^[A-Za-z0-9._-]+$')
}

function Read-JSONOwner([string]$Path) {
    $info = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if ($info.PSIsContainer -or (Test-IsReparsePoint $info) -or $info.Length -gt $MaxChecksumBytes) {
        throw "Lock owner must be a small direct non-reparse regular file: $Path"
    }
    return Get-Content -Raw -LiteralPath $Path -ErrorAction Stop | ConvertFrom-Json
}

function Read-InstallLockOwner([string]$LockDir) {
    $lockInfo = Get-Item -LiteralPath $LockDir -Force -ErrorAction Stop
    if (-not $lockInfo.PSIsContainer -or (Test-IsReparsePoint $lockInfo)) {
        throw "Install lock must be a direct non-reparse directory: $LockDir"
    }
    $owner = Read-JSONOwner (Join-Path $LockDir "owner.json")
    if ([int]$owner.pid -le 0 -or [long]$owner.created_at -le 0 -or
        -not (Test-ValidLockNonce ([string]$owner.nonce))) {
        throw "Invalid install lock owner"
    }
    return $owner
}

function Read-ReclaimGuardOwner([string]$GuardDir) {
    $guardInfo = Get-Item -LiteralPath $GuardDir -Force -ErrorAction Stop
    if (-not $guardInfo.PSIsContainer -or (Test-IsReparsePoint $guardInfo)) {
        throw "Reclaim guard must be a direct non-reparse directory: $GuardDir"
    }
    $owner = Read-JSONOwner (Join-Path $GuardDir "owner.json")
    if ([int]$owner.pid -le 0 -or [int]$owner.target_pid -le 0 -or [long]$owner.created_at -le 0 -or
        -not (Test-ValidLockNonce ([string]$owner.nonce)) -or
        -not (Test-ValidLockNonce ([string]$owner.target_nonce))) {
        throw "Invalid reclaim guard owner"
    }
    return $owner
}

function Release-ReclaimGuard($Guard) {
    if (-not $Guard) {
        return
    }
    try {
        $owner = Read-ReclaimGuardOwner $Guard.Directory
        if ([int]$owner.pid -ne $PID -or [string]$owner.nonce -ne [string]$Guard.Nonce -or
            [int]$owner.target_pid -ne [int]$Guard.TargetPID -or
            [string]$owner.target_nonce -ne [string]$Guard.TargetNonce) {
            return
        }
        $released = $Guard.Directory + ".release." + $Guard.Nonce
        [System.IO.Directory]::Move($Guard.Directory, $released)
        Remove-Item -LiteralPath $released -Recurse -Force -ErrorAction SilentlyContinue
    }
    catch {
        Write-Verbose "Could not release reclaim guard safely: $($_.Exception.Message)"
    }
}

function Acquire-ReclaimGuard([string]$LocksDir, [int]$TargetPID, [string]$TargetNonce) {
    if ($TargetPID -le 0 -or -not (Test-ValidLockNonce $TargetNonce)) {
        throw "Invalid reclaim target owner"
    }
    $guardDir = Join-Path $LocksDir ("install.reclaim.$TargetNonce.d")
    for ($attempt = 0; $attempt -lt 3; $attempt++) {
        $existingGuard = Get-Item -LiteralPath $guardDir -Force -ErrorAction SilentlyContinue
        if ($null -ne $existingGuard) {
            if (-not $existingGuard.PSIsContainer -or (Test-IsReparsePoint $existingGuard)) {
                throw "Reclaim guard is not a direct non-reparse directory: $guardDir"
            }
            try {
                $owner = Read-ReclaimGuardOwner $guardDir
                if ([int]$owner.target_pid -ne $TargetPID -or [string]$owner.target_nonce -ne $TargetNonce) {
                    throw "reclaim guard targets a different install lock"
                }
                if ((Test-ProcessMatchesLock ([int]$owner.pid) ([long]$owner.created_at)) -ne "dead") {
                    throw "another process is reclaiming the install lock"
                }
                $current = Read-ReclaimGuardOwner $guardDir
                if ([int]$current.pid -ne [int]$owner.pid -or [string]$current.nonce -ne [string]$owner.nonce -or
                    [int]$current.target_pid -ne $TargetPID -or [string]$current.target_nonce -ne $TargetNonce) {
                    throw "reclaim guard owner changed"
                }
                # Retired guards are permanent ABA tombstones. Every stale
                # observer of this owner contends for the same no-replace path.
                $retired = $guardDir + ".retired." + [string]$owner.nonce
                [System.IO.Directory]::Move($guardDir, $retired)
                continue
            }
            catch {
                throw "Reclaim guard cannot be safely recovered: $guardDir ($($_.Exception.Message))"
            }
        }

        $guardNonce = [guid]::NewGuid().ToString("N")
        $pendingDir = $guardDir + ".pending." + $guardNonce
        try {
            New-Item -ItemType Directory -Path $pendingDir -ErrorAction Stop | Out-Null
            $guardOwner = [ordered]@{
                pid = $PID
                nonce = $guardNonce
                target_pid = $TargetPID
                target_nonce = $TargetNonce
                created_at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
            }
            [System.IO.File]::WriteAllText((Join-Path $pendingDir "owner.json"), ($guardOwner | ConvertTo-Json -Compress))
            try {
                [System.IO.Directory]::Move($pendingDir, $guardDir)
                return [pscustomobject]@{
                    Directory = $guardDir
                    Nonce = $guardNonce
                    TargetPID = $TargetPID
                    TargetNonce = $TargetNonce
                }
            }
            catch [System.IO.IOException] {
                if ($null -eq (Get-Item -LiteralPath $guardDir -Force -ErrorAction SilentlyContinue)) {
                    throw
                }
            }
        }
        finally {
            if (Test-Path -LiteralPath $pendingDir) {
                Remove-Item -LiteralPath $pendingDir -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }
    throw "Could not acquire reclaim guard: $guardDir"
}

function Acquire-InstallLock([string]$LocksDir, [string]$VersionName) {
    Assert-ManagedDirectory $LocksDir
    $lockDir = Join-Path $LocksDir "install.lock.d"
    $ownerPath = Join-Path $lockDir "owner.json"

    for ($attempt = 0; $attempt -lt 2; $attempt++) {
        $nonce = [guid]::NewGuid().ToString("N")
        $pendingDir = Join-Path $LocksDir ("install.lock.d.pending.$nonce")
        try {
            New-Item -ItemType Directory -Path $pendingDir -ErrorAction Stop | Out-Null
            $owner = [ordered]@{
                pid = $PID
                version = $VersionName
                created_at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
                nonce = $nonce
            }
            [System.IO.File]::WriteAllText((Join-Path $pendingDir "owner.json"), ($owner | ConvertTo-Json -Compress))
            try {
                [System.IO.Directory]::Move($pendingDir, $lockDir)
                return [pscustomobject]@{ Directory = $lockDir; Owner = $ownerPath; Nonce = $nonce }
            }
            catch [System.IO.IOException] {
                try {
                    $existing = Read-InstallLockOwner $lockDir
                    $existingPID = [int]$existing.pid
                    $existingNonce = [string]$existing.nonce
                    $existingCreatedAt = [long]$existing.created_at
                    if ((Test-ProcessMatchesLock $existingPID $existingCreatedAt) -ne "dead") {
                        throw "install lock owner is alive or cannot be verified"
                    }
                }
                catch {
                    throw "Another Metis install/update lock exists and its owner cannot be verified: $lockDir ($($_.Exception.Message))"
                }

                $reclaimGuard = Acquire-ReclaimGuard $LocksDir $existingPID $existingNonce
                try {
                    $current = Read-InstallLockOwner $lockDir
                    if ([int]$current.pid -ne $existingPID -or [string]$current.nonce -ne $existingNonce -or
                        [long]$current.created_at -ne $existingCreatedAt -or
                        (Test-ProcessMatchesLock ([int]$current.pid) ([long]$current.created_at)) -ne "dead") {
                        throw "install lock owner changed or is no longer verifiably dead"
                    }
                    $quarantine = $lockDir + ".stale." + [guid]::NewGuid().ToString("N")
                    [System.IO.Directory]::Move($lockDir, $quarantine)
                    Remove-Item -LiteralPath $quarantine -Recurse -Force -ErrorAction SilentlyContinue
                }
                finally {
                    Release-ReclaimGuard $reclaimGuard
                }
            }
        }
        finally {
            if (Test-Path -LiteralPath $pendingDir) {
                Remove-Item -LiteralPath $pendingDir -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }
    throw "Could not acquire Metis install/update lock: $lockDir"
}

function Release-InstallLock($Lock) {
    if (-not $Lock) {
        return
    }
    try {
        $owner = Read-InstallLockOwner $Lock.Directory
        if ([string]$owner.nonce -eq [string]$Lock.Nonce -and [int]$owner.pid -eq $PID) {
            $quarantine = $Lock.Directory + ".release." + $Lock.Nonce
            Move-Item -LiteralPath $Lock.Directory -Destination $quarantine -ErrorAction Stop
            Remove-Item -LiteralPath $quarantine -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
    catch {
        # Never delete a lock whose ownership cannot be proven.
        Write-Verbose "Could not release install lock safely: $($_.Exception.Message)"
    }
}

function Get-ManagedVersionBinary([string]$VersionsDir, [string]$Version) {
    $normalizedVersion = ConvertTo-VersionName $Version
    if ($normalizedVersion -ne $Version) {
        throw "Managed version is not normalized: $Version"
    }
    $versionDir = Join-Path $VersionsDir $normalizedVersion
    $versionBinary = Join-Path $versionDir "metis.exe"
    $versionInfo = Get-Item -LiteralPath $versionDir -Force -ErrorAction Stop
    $binaryInfo = Get-Item -LiteralPath $versionBinary -Force -ErrorAction Stop
    if (-not $versionInfo.PSIsContainer -or (Test-IsReparsePoint $versionInfo) -or
        $binaryInfo.PSIsContainer -or (Test-IsReparsePoint $binaryInfo)) {
        throw "Managed version is not a direct regular binary: $normalizedVersion"
    }
    return $versionBinary
}

function Get-ManagedVersionMatches([string]$VersionsDir, [string]$FilePath) {
    Assert-ManagedDirectory $VersionsDir
    $fileInfo = Get-Item -LiteralPath $FilePath -Force -ErrorAction Stop
    if ($fileInfo.PSIsContainer -or (Test-IsReparsePoint $fileInfo)) {
        throw "Candidate launcher is not a direct regular file: $FilePath"
    }
    $candidateHash = Get-SHA256Hex $FilePath
    foreach ($entry in @(Get-ChildItem -LiteralPath $VersionsDir -Force -ErrorAction Stop)) {
        if ($entry.Name -like ".migrate-*") {
            if (-not $entry.PSIsContainer -or (Test-IsReparsePoint $entry)) {
                throw "Legacy migration staging path is not a direct directory: $($entry.FullName)"
            }
            continue
        }
        if (-not $entry.PSIsContainer -or (Test-IsReparsePoint $entry)) {
            throw "Unexpected entry in managed versions root: $($entry.FullName)"
        }
        $normalized = ConvertTo-VersionName $entry.Name
        if ($normalized -ne $entry.Name) {
            throw "Unexpected version directory name: $($entry.Name)"
        }
        $binary = Get-ManagedVersionBinary $VersionsDir $entry.Name
        $managedHash = Get-SHA256Hex $binary
        if ($managedHash.Equals($candidateHash, [StringComparison]::OrdinalIgnoreCase)) {
            Write-Output $entry.Name
        }
    }
}

function Read-CurrentVersion([string]$CurrentVersionFile) {
    $markerInfo = Get-Item -LiteralPath $CurrentVersionFile -Force -ErrorAction Stop
    if ($markerInfo.PSIsContainer -or (Test-IsReparsePoint $markerInfo)) {
        throw "current-version must be a direct non-reparse regular file: $CurrentVersionFile"
    }
    $version = (Get-Content -Raw -LiteralPath $CurrentVersionFile -ErrorAction Stop).Trim()
    $normalized = ConvertTo-VersionName $version
    if ($normalized -ne $version) {
        throw "current-version is not normalized: $version"
    }
    return $version
}

function Write-VersionMarkerAtomically([string]$CurrentVersionFile, [string]$Version) {
    $managedRoot = Split-Path -Parent $CurrentVersionFile
    Assert-ManagedDirectory $managedRoot
    if (Test-Path -LiteralPath $CurrentVersionFile) {
        $null = Read-CurrentVersion $CurrentVersionFile
    }
    $temporary = Join-Path $managedRoot ("current-version.tmp.$PID." + [guid]::NewGuid().ToString("N"))
    try {
        [System.IO.File]::WriteAllText($temporary, $Version + [Environment]::NewLine)
        Replace-FileAtomically $temporary $CurrentVersionFile
    }
    finally {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }
}

function Invoke-ActivationCrashForTest([string]$Point) {
    if ($env:METIS_INSTALL_TEST_CRASH_AFTER -and
        $env:METIS_INSTALL_TEST_CRASH_AFTER.Equals($Point, [StringComparison]::Ordinal)) {
        # An abrupt self-termination deliberately bypasses catch/finally so
        # Windows integration tests exercise real crash-recovery states.
        Stop-Process -Id $PID -Force
        [Environment]::FailFast("Metis installer fault injection: $Point")
    }
}

function Repair-InterruptedLauncherSwap([string]$LauncherPath, [string]$VersionsDir, [string]$CurrentVersionFile) {
    $launcherDir = Split-Path -Parent $LauncherPath
    if (-not (Test-Path -LiteralPath $launcherDir -PathType Container)) {
        return
    }
    Assert-ManagedDirectory $launcherDir
    Assert-ManagedDirectory $VersionsDir
    $backups = @(Get-ChildItem -LiteralPath $launcherDir -File -Force -ErrorAction SilentlyContinue |
        Where-Object {
            -not (Test-IsReparsePoint $_) -and
            ($_.Name -match '^\.metis\.old\..+\.exe$' -or $_.Name -eq "metis.old.exe")
        } |
        Sort-Object -Property LastWriteTimeUtc -Descending)

    $markerExists = Test-Path -LiteralPath $CurrentVersionFile
    $launcherExists = Test-Path -LiteralPath $LauncherPath
    if ($markerExists) {
        $currentVersion = Read-CurrentVersion $CurrentVersionFile
        $currentBinary = Get-ManagedVersionBinary $VersionsDir $currentVersion
        $currentHash = Get-SHA256Hex $currentBinary
        $launcherMatchesCurrent = $false
        if ($launcherExists) {
            $launcherInfo = Get-Item -LiteralPath $LauncherPath -Force -ErrorAction Stop
            if ($launcherInfo.PSIsContainer -or (Test-IsReparsePoint $launcherInfo)) {
                throw "Refusing to repair a non-regular launcher: $LauncherPath"
            }
            $launcherHash = Get-SHA256Hex $LauncherPath
            $launcherMatchesCurrent = $launcherHash.Equals($currentHash, [StringComparison]::OrdinalIgnoreCase)
            if (-not $launcherMatchesCurrent) {
                $knownMatches = @(Get-ManagedVersionMatches $VersionsDir $LauncherPath)
                if ($knownMatches.Count -eq 0) {
                    throw "Refusing to overwrite an unknown launcher while repairing managed activation: $LauncherPath"
                }
            }
        }

        # current-version is the commit record. If the launcher is missing or
        # contains another known immutable Metis version, restore the committed
        # binary rather than guessing from backup names or timestamps.
        if (-not $launcherMatchesCurrent) {
            $repairNonce = [guid]::NewGuid().ToString("N")
            $repairTemp = Join-Path $launcherDir (".metis.new.$repairNonce.exe")
            $displaced = Join-Path $launcherDir (".metis.old." + [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() + ".$repairNonce.exe")
            $displacedLauncher = $false
            try {
                Copy-Item -LiteralPath $currentBinary -Destination $repairTemp -ErrorAction Stop
                $repairHash = Get-SHA256Hex $repairTemp
                if (-not $repairHash.Equals($currentHash, [StringComparison]::OrdinalIgnoreCase)) {
                    throw "Repair launcher hash does not match current-version"
                }
                if ($launcherExists) {
                    Move-Item -LiteralPath $LauncherPath -Destination $displaced -ErrorAction Stop
                    $displacedLauncher = $true
                }
                Move-Item -LiteralPath $repairTemp -Destination $LauncherPath -ErrorAction Stop
            }
            catch {
                if ($displacedLauncher -and -not (Test-Path -LiteralPath $LauncherPath) -and
                    (Test-Path -LiteralPath $displaced -PathType Leaf)) {
                    Move-Item -LiteralPath $displaced -Destination $LauncherPath -ErrorAction SilentlyContinue
                }
                throw
            }
            finally {
                Remove-Item -LiteralPath $repairTemp -Force -ErrorAction SilentlyContinue
            }
            if ($displacedLauncher) {
                $backups += Get-Item -LiteralPath $displaced -Force
            }
        }
    }
    elseif ($launcherExists) {
        # This is either a first-install crash after publishing the launcher or
        # a legacy/custom launcher. Only a unique immutable hash match proves
        # the former; otherwise leave it untouched for strict legacy handling.
        $knownMatches = @(Get-ManagedVersionMatches $VersionsDir $LauncherPath)
        if ($knownMatches.Count -gt 1) {
            throw "Cannot recover current-version because launcher matches multiple managed versions"
        }
        if ($knownMatches.Count -eq 1) {
            Write-VersionMarkerAtomically $CurrentVersionFile $knownMatches[0]
        }
    }

    foreach ($backup in $backups) {
        try {
            $knownBackupMatches = @(Get-ManagedVersionMatches $VersionsDir $backup.FullName)
        }
        catch {
            Write-Verbose "Preserving unreadable launcher backup $($backup.FullName)"
            continue
        }
        if ($knownBackupMatches.Count -eq 0) {
            Write-Verbose "Preserving unrecognized launcher backup $($backup.FullName)"
            continue
        }
        try {
            Remove-Item -LiteralPath $backup.FullName -Force -ErrorAction Stop
        }
        catch {
            # Sharing violations are expected while an older Metis process is
            # still running. Unique backup names keep future updates working.
            Write-Verbose "Deferring cleanup of running launcher $($backup.FullName)"
        }
    }
}

function Migrate-FlatLauncher([string]$LauncherPath, [string]$VersionsDir, [string]$CurrentVersionFile) {
    Assert-ManagedDirectory $VersionsDir
    if (-not (Test-Path -LiteralPath $LauncherPath)) {
        return
    }

    $launcherInfo = Get-Item -LiteralPath $LauncherPath -Force
    if ($launcherInfo.PSIsContainer -or (Test-IsReparsePoint $launcherInfo)) {
        throw "Refusing to overwrite unverified existing launcher: $LauncherPath"
    }

    # A marker bypasses flat migration only when the marker, immutable version,
    # and visible launcher form a verifiable managed installation.
    if (Test-Path -LiteralPath $CurrentVersionFile) {
        try {
            $markerInfo = Get-Item -LiteralPath $CurrentVersionFile -Force
            if ($markerInfo.PSIsContainer -or (Test-IsReparsePoint $markerInfo)) {
                throw "invalid current-version marker"
            }
            $markedVersion = (Get-Content -Raw -LiteralPath $CurrentVersionFile).Trim()
            $normalizedMarkedVersion = ConvertTo-VersionName $markedVersion
            if ($normalizedMarkedVersion -ne $markedVersion) {
                throw "current-version marker is not normalized"
            }
            $markedDir = Join-Path $VersionsDir $markedVersion
            $markedBinary = Join-Path $markedDir "metis.exe"
            $markedDirInfo = Get-Item -LiteralPath $markedDir -Force
            $markedBinaryInfo = Get-Item -LiteralPath $markedBinary -Force
            if (-not $markedDirInfo.PSIsContainer -or (Test-IsReparsePoint $markedDirInfo) -or
                $markedBinaryInfo.PSIsContainer -or (Test-IsReparsePoint $markedBinaryInfo)) {
                throw "managed current version is not regular"
            }
            $launcherHash = Get-SHA256Hex $LauncherPath
            $markedHash = Get-SHA256Hex $markedBinary
            if (-not $launcherHash.Equals($markedHash, [StringComparison]::OrdinalIgnoreCase)) {
                throw "visible launcher does not match current-version"
            }
            return
        }
        catch {
            throw "Refusing to overwrite unverified existing launcher: invalid managed current state ($($_.Exception.Message))"
        }
    }

    try {
        $legacyOutput = @(& $LauncherPath version 2>&1)
    }
    catch {
        throw "Refusing to overwrite unverified existing launcher: version command failed ($($_.Exception.Message))"
    }
    if ($LASTEXITCODE -ne 0) {
        throw "Refusing to overwrite unverified existing launcher: version command exited with $LASTEXITCODE"
    }
    $legacyText = (($legacyOutput | ForEach-Object { $_.ToString() }) -join " ").Trim()
    $legacyFields = @($legacyText -split '\s+')
    if ($legacyFields.Count -eq 0 -or -not $legacyFields[0]) {
        throw "Refusing to overwrite unverified existing launcher: empty version output"
    }
    if ($legacyText -notmatch '(?i)\bMetis\b') {
        throw "Refusing to overwrite unverified existing launcher: version output has no Metis product marker"
    }
    try {
        $legacyVersion = ConvertTo-VersionName $legacyFields[0]
    }
    catch {
        throw "Refusing to overwrite unverified existing launcher: unsafe version output"
    }

    $legacyVersionDir = Join-Path $VersionsDir $legacyVersion
    $legacyVersionBinary = Join-Path $legacyVersionDir "metis.exe"
    $launcherHash = Get-SHA256Hex $LauncherPath
    if (Test-Path -LiteralPath $legacyVersionDir) {
        $legacyInfo = Get-Item -LiteralPath $legacyVersionDir -Force
        if (-not $legacyInfo.PSIsContainer -or (Test-IsReparsePoint $legacyInfo) -or
            -not (Test-Path -LiteralPath $legacyVersionBinary -PathType Leaf)) {
            throw "Legacy version path is not a regular managed directory: $legacyVersionDir"
        }
        $managedHash = Get-SHA256Hex $legacyVersionBinary
        if (-not $managedHash.Equals($launcherHash, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Refusing to overwrite managed version $legacyVersion with a different flat launcher"
        }
        Write-VersionMarkerAtomically $CurrentVersionFile $legacyVersion
        return
    }

    $migrationDir = Join-Path $VersionsDir (".migrate-" + [guid]::NewGuid().ToString("N"))
    try {
        New-Item -ItemType Directory -Path $migrationDir -ErrorAction Stop | Out-Null
        $migrationBinary = Join-Path $migrationDir "metis.exe"
        Copy-Item -LiteralPath $LauncherPath -Destination $migrationBinary -ErrorAction Stop
        $migratedHash = Get-SHA256Hex $migrationBinary
        if (-not $migratedHash.Equals($launcherHash, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Migrated legacy launcher hash does not match its source"
        }
        Move-Item -LiteralPath $migrationDir -Destination $legacyVersionDir -ErrorAction Stop
        Write-VersionMarkerAtomically $CurrentVersionFile $legacyVersion
    }
    finally {
        Remove-Item -LiteralPath $migrationDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Cleanup-StaleArtifacts([string]$StagingDir, [string]$LauncherDir, [string]$ManagedRoot) {
    Assert-ManagedDirectory $ManagedRoot
    Assert-ManagedDirectory $StagingDir
    Assert-ManagedDirectory $LauncherDir
    $cutoff = [DateTime]::UtcNow.Subtract($StaleArtifactAge)
    foreach ($entry in @(Get-ChildItem -LiteralPath $StagingDir -Force -ErrorAction SilentlyContinue)) {
        if (Test-IsReparsePoint $entry) {
            continue
        }
        if ($entry.Name -like "install-*" -and $entry.LastWriteTimeUtc -lt $cutoff) {
            Remove-Item -LiteralPath $entry.FullName -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
    foreach ($entry in @(Get-ChildItem -LiteralPath $LauncherDir -File -Force -ErrorAction SilentlyContinue)) {
        if (Test-IsReparsePoint $entry) {
            continue
        }
        if ($entry.Name -like ".metis.new.*.exe" -and $entry.LastWriteTimeUtc -lt $cutoff) {
            Remove-Item -LiteralPath $entry.FullName -Force -ErrorAction SilentlyContinue
        }
    }
    foreach ($entry in @(Get-ChildItem -LiteralPath $ManagedRoot -File -Force -ErrorAction SilentlyContinue)) {
        if (-not (Test-IsReparsePoint $entry) -and
            ($entry.Name -like "current-version.tmp.*" -or $entry.Name -like "current-version.replace.*") -and
            $entry.LastWriteTimeUtc -lt $cutoff) {
            Remove-Item -LiteralPath $entry.FullName -Force -ErrorAction SilentlyContinue
        }
    }
}

function Get-RunningVersionProtection([string]$VersionsDir) {
    $protected = @{}
    $prefix = (Get-NormalizedPath $VersionsDir) + [System.IO.Path]::DirectorySeparatorChar
    foreach ($process in @(Get-Process -Name "metis" -ErrorAction SilentlyContinue)) {
        try {
            $processPath = [string]$process.Path
            if (-not $processPath) {
                return [pscustomobject]@{ Reliable = $false; Versions = $protected }
            }
            $fullPath = [System.IO.Path]::GetFullPath($processPath)
            if (-not $fullPath.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
                continue
            }
            $relative = $fullPath.Substring($prefix.Length)
            $parts = @($relative -split '[\\/]')
            if ($parts.Count -eq 2 -and $parts[1].Equals("metis.exe", [StringComparison]::OrdinalIgnoreCase)) {
                $protected[$parts[0].ToLowerInvariant()] = $true
            }
            else {
                return [pscustomobject]@{ Reliable = $false; Versions = $protected }
            }
        }
        catch {
            return [pscustomobject]@{ Reliable = $false; Versions = $protected }
        }
    }
    return [pscustomobject]@{ Reliable = $true; Versions = $protected }
}

function Get-LockedVersionProtection([string]$RunningLocksDir) {
    $protected = @{}
    if (-not (Test-Path -LiteralPath $RunningLocksDir)) {
        return [pscustomobject]@{ Reliable = $true; Versions = $protected }
    }
    try {
        $rootInfo = Get-Item -LiteralPath $RunningLocksDir -Force
        if (-not $rootInfo.PSIsContainer -or (Test-IsReparsePoint $rootInfo)) {
            return [pscustomobject]@{ Reliable = $false; Versions = $protected }
        }
        $rootEntries = @(Get-ChildItem -LiteralPath $RunningLocksDir -Force -ErrorAction Stop)
    }
    catch {
        return [pscustomobject]@{ Reliable = $false; Versions = $protected }
    }

    $versionLocks = @()
    foreach ($entry in $rootEntries) {
        if (-not $entry.PSIsContainer -or (Test-IsReparsePoint $entry)) {
            return [pscustomobject]@{ Reliable = $false; Versions = $protected }
        }
        $versionName = $entry.Name
        if ($versionName -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]*$') {
            return [pscustomobject]@{ Reliable = $false; Versions = $protected }
        }
        $versionLocks += $entry
    }

    foreach ($versionLock in $versionLocks) {
        $versionName = $versionLock.Name
        try {
            $lockEntries = @(Get-ChildItem -LiteralPath $versionLock.FullName -Force -ErrorAction Stop)
        }
        catch {
            return [pscustomobject]@{ Reliable = $false; Versions = $protected }
        }

        foreach ($lockFile in $lockEntries) {
            if ($lockFile.PSIsContainer -or (Test-IsReparsePoint $lockFile) -or
                $lockFile.Name -notmatch '^[1-9][0-9]*\.json$') {
                return [pscustomobject]@{ Reliable = $false; Versions = $protected }
            }
            try {
                $owner = Read-JSONOwner $lockFile.FullName
                $declaredVersion = [string]$owner.version
                $ownerPID = [int]$owner.pid
                $ownerNonce = [string]$owner.nonce
                if (-not $declaredVersion -or $declaredVersion.TrimStart("v") -ne $versionName -or
                    $ownerPID -le 0 -or -not (Test-ValidLockNonce $ownerNonce) -or
                    [long]$owner.created_at -le 0 -or $lockFile.Name -ne "$ownerPID.json") {
                    return [pscustomobject]@{ Reliable = $false; Versions = $protected }
                }
                $status = Test-ProcessMatchesLock $ownerPID ([long]$owner.created_at)
                if ($status -ne "dead") {
                    $protected[$versionName.ToLowerInvariant()] = $true
                    continue
                }
                Remove-Item -LiteralPath $lockFile.FullName -Force -ErrorAction SilentlyContinue
            }
            catch {
                # An unknown entry makes the whole running set uncertain, so
                # pruning must retain every managed version.
                return [pscustomobject]@{ Reliable = $false; Versions = $protected }
            }
        }
        try {
            if (@(Get-ChildItem -LiteralPath $versionLock.FullName -Force -ErrorAction Stop).Count -eq 0) {
                Remove-Item -LiteralPath $versionLock.FullName -Force -ErrorAction SilentlyContinue
            }
        }
        catch {
            $protected[$versionName.ToLowerInvariant()] = $true
        }
    }
    return [pscustomobject]@{ Reliable = $true; Versions = $protected }
}

function Cleanup-ManagedVersions([string]$VersionsDir, [string]$CurrentVersionFile, [string]$RunningLocksDir) {
    Assert-ManagedDirectory $VersionsDir
    Assert-ManagedDirectory $RunningLocksDir
    # If current resolution is uncertain, deleting nothing is safer than
    # guessing. Staging and launcher leftovers are cleaned independently.
    try {
        $currentVersion = Read-CurrentVersion $CurrentVersionFile
        $null = Get-ManagedVersionBinary $VersionsDir $currentVersion
        $versionDirs = @(Get-ChildItem -LiteralPath $VersionsDir -Directory -Force -ErrorAction Stop |
            Where-Object { -not (Test-IsReparsePoint $_) })
        $currentDir = @($versionDirs | Where-Object { $_.Name.Equals($currentVersion, [StringComparison]::OrdinalIgnoreCase) })
        if ($currentDir.Count -ne 1 -or -not (Test-Path -LiteralPath (Join-Path $currentDir[0].FullName "metis.exe") -PathType Leaf)) {
            return
        }
    }
    catch {
        return
    }

    $protected = @{}
    $protected[$currentVersion.ToLowerInvariant()] = $true

    $running = Get-RunningVersionProtection $VersionsDir
    if (-not $running.Reliable) {
        return
    }
    foreach ($name in $running.Versions.Keys) {
        $protected[$name] = $true
    }

    $locked = Get-LockedVersionProtection $RunningLocksDir
    if (-not $locked.Reliable) {
        return
    }
    foreach ($name in $locked.Versions.Keys) {
        $protected[$name] = $true
    }

    $eligible = @($versionDirs |
        Where-Object { -not $protected.ContainsKey($_.Name.ToLowerInvariant()) } |
        Sort-Object -Property LastWriteTimeUtc -Descending)
    $versionsToDelete = @($eligible | Select-Object -Skip $VersionRetentionCount)
    foreach ($versionDir in $versionsToDelete) {
        try {
            # Only managed, direct, non-reparse children are ever removed.
            Remove-Item -LiteralPath $versionDir.FullName -Recurse -Force -ErrorAction Stop
        }
        catch {
            # A sharing violation means the version is still in use. Leave it
            # for the next successful update/startup cleanup.
            Write-Verbose "Deferring cleanup of version $($versionDir.Name): $($_.Exception.Message)"
        }
    }
}

function Replace-FileAtomically([string]$TemporaryPath, [string]$DestinationPath) {
    $destinationInfo = Get-Item -LiteralPath $DestinationPath -Force -ErrorAction SilentlyContinue
    if ($null -ne $destinationInfo) {
        if ($destinationInfo.PSIsContainer -or (Test-IsReparsePoint $destinationInfo)) {
            throw "Atomic destination must be a direct regular file: $DestinationPath"
        }
        $replacementBackup = $DestinationPath + ".replace." + [guid]::NewGuid().ToString("N")
        try {
            [System.IO.File]::Replace($TemporaryPath, $DestinationPath, $replacementBackup, $true)
        }
        finally {
            Remove-Item -LiteralPath $replacementBackup -Force -ErrorAction SilentlyContinue
        }
    }
    else {
        [System.IO.File]::Move($TemporaryPath, $DestinationPath)
    }
}

if ($PSVersionTable.PSEdition -eq "Core" -and -not $IsWindows) {
    throw "install.ps1 supports Windows only"
}

$architecture = Resolve-Architecture
$resolvedTag = $null
$versionName = $null
$artifact = $null
$checksumAsset = $null
$downloadBase = $null

$normalizedInstallDir = Get-NormalizedPath $InstallDir
$installRoot = Split-Path -Parent $normalizedInstallDir
if (-not $installRoot) {
    throw "InstallDir must have a parent directory: $InstallDir"
}
$versionsDir = Join-Path $installRoot "versions"
$stagingDir = Join-Path $installRoot "staging"
$locksDir = Join-Path $installRoot "locks"
$runningLocksDir = Join-Path $locksDir "running"
$currentVersionFile = Join-Path $installRoot "current-version"
$destination = Join-Path $normalizedInstallDir "metis.exe"
$stageDir = Join-Path $stagingDir ("install-" + [guid]::NewGuid().ToString("N"))
$stagedLauncher = Join-Path $normalizedInstallDir (".metis.new." + [guid]::NewGuid().ToString("N") + ".exe")
$temporaryMarker = Join-Path $installRoot ("current-version.tmp.$PID." + [guid]::NewGuid().ToString("N"))
$installLock = $null

# Validate every pre-existing managed root before creating any missing child,
# so one reparse sentinel cannot redirect partial installer state elsewhere.
Ensure-ManagedDirectory $installRoot
foreach ($managedPath in @($normalizedInstallDir, $versionsDir, $stagingDir, $locksDir, $runningLocksDir)) {
    $existing = Get-Item -LiteralPath $managedPath -Force -ErrorAction SilentlyContinue
    if ($null -ne $existing) {
        Assert-ManagedDirectory $managedPath
    }
}
foreach ($managedPath in @($normalizedInstallDir, $versionsDir, $stagingDir, $locksDir, $runningLocksDir)) {
    Ensure-ManagedDirectory $managedPath
}

try {
    # Serialize the complete install, including staging cleanup and network
    # work, so an installer never mistakes another installer's files for
    # abandoned state.
    $installLock = Acquire-InstallLock $locksDir $Version
    Repair-InterruptedLauncherSwap $destination $versionsDir $currentVersionFile
    Migrate-FlatLauncher $destination $versionsDir $currentVersionFile
    Cleanup-StaleArtifacts $stagingDir $normalizedInstallDir $installRoot

    # Local recovery and legacy adoption deliberately happen before release
    # resolution so an interrupted install can be repaired while offline.
    $resolvedTag = Resolve-ReleaseTag
    $versionName = ConvertTo-VersionName $resolvedTag
    $artifact = "metis-windows-$architecture.zip"
    $checksumAsset = "$artifact.sha256"
    $downloadBase = "$WebBase/$Repo/releases/download/$resolvedTag"
    New-Item -ItemType Directory -Path $stageDir -Force | Out-Null

    $zipPath = Join-Path $stageDir $artifact
    $checksumPath = Join-Path $stageDir $checksumAsset
    Write-Step "installing metis $resolvedTag for windows-$architecture"
    Download-FileWithLimit "$downloadBase/$artifact" $zipPath $MaxArchiveBytes $AssetTimeout $Token
    Download-FileWithLimit "$downloadBase/$checksumAsset" $checksumPath $MaxChecksumBytes $AssetTimeout $Token

    $checksumFields = @((Get-Content -Raw -LiteralPath $checksumPath).Trim() -split "\s+")
    if ($checksumFields.Count -lt 2 -or $checksumFields[-1].TrimStart([char]'*') -ne $artifact) {
        throw "Checksum sidecar does not describe $artifact"
    }
    $expected = $checksumFields[0].ToLowerInvariant()
    if ($expected -notmatch '^[0-9a-f]{64}$') {
        throw "Checksum sidecar contains an invalid SHA-256 value"
    }
    $actual = (Get-SHA256Hex $zipPath).ToLowerInvariant()
    if ($actual -ne $expected) {
        throw "SHA-256 mismatch: got $actual, expected $expected"
    }

    $extractedDir = Join-Path $stageDir "version"
    $extractedBinary = Extract-MetisZipWithLimit $zipPath $extractedDir

    Assert-ManagedDirectory $versionsDir
    $versionDir = Join-Path $versionsDir $versionName
    $versionBinary = Join-Path $versionDir "metis.exe"
    if (Test-Path -LiteralPath $versionDir) {
        $versionInfo = Get-Item -LiteralPath $versionDir -Force
        if (-not $versionInfo.PSIsContainer -or (Test-IsReparsePoint $versionInfo) -or -not (Test-Path -LiteralPath $versionBinary -PathType Leaf)) {
            throw "Managed version path is not a regular version directory: $versionDir"
        }
        $installedHash = Get-SHA256Hex $versionBinary
        $downloadedHash = Get-SHA256Hex $extractedBinary
        if (-not $installedHash.Equals($downloadedHash, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Existing managed version $versionName differs from the verified release asset"
        }
    }
    else {
        Move-Item -LiteralPath $extractedDir -Destination $versionDir
    }

    Assert-ManagedDirectory $normalizedInstallDir
    Copy-Item -LiteralPath $versionBinary -Destination $stagedLauncher -Force
    $versionHash = Get-SHA256Hex $versionBinary
    $launcherHash = Get-SHA256Hex $stagedLauncher
    if (-not $versionHash.Equals($launcherHash, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Staged launcher hash does not match the managed version"
    }

    if (-not $SkipVersionCheck) {
        $versionOutput = @(& $stagedLauncher version 2>&1)
        if ($LASTEXITCODE -ne 0) {
            throw "Downloaded metis.exe failed its version check: $($versionOutput -join ' ')"
        }
        $versionFields = @((($versionOutput | ForEach-Object { $_.ToString() }) -join " ").Trim() -split '\s+')
        if ($versionFields.Count -eq 0 -or -not $versionFields[0].Equals($resolvedTag, [StringComparison]::Ordinal)) {
            throw "Downloaded metis.exe reported version '$($versionFields[0])', expected '$resolvedTag'"
        }
    }

    Assert-ManagedDirectory $installRoot
    [System.IO.File]::WriteAllText($temporaryMarker, $versionName + [Environment]::NewLine)
    Invoke-ActivationCrashForTest "marker-staged"
    $hadExisting = Test-Path -LiteralPath $destination
    if ($hadExisting -and -not (Test-Path -LiteralPath $destination -PathType Leaf)) {
        throw "Launcher path exists but is not a regular file: $destination"
    }
    $backup = Join-Path $normalizedInstallDir (".metis.old." + [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() + "." + [guid]::NewGuid().ToString("N") + ".exe")
    if ($hadExisting) {
        Move-Item -LiteralPath $destination -Destination $backup
        Invoke-ActivationCrashForTest "launcher-backed-up"
    }

    try {
        Move-Item -LiteralPath $stagedLauncher -Destination $destination
        Invoke-ActivationCrashForTest "launcher-replaced"
        Replace-FileAtomically $temporaryMarker $currentVersionFile
        Invoke-ActivationCrashForTest "marker-replaced"
    }
    catch {
        $installError = $_
        if ($hadExisting -and (Test-Path -LiteralPath $backup -PathType Leaf)) {
            try {
                Remove-Item -LiteralPath $destination -Force -ErrorAction SilentlyContinue
                Move-Item -LiteralPath $backup -Destination $destination
            }
            catch {
                throw "Metis install failed and launcher rollback also failed. Recoverable backup: $backup. Original error: $($installError.Exception.Message)"
            }
        }
        elseif (-not $hadExisting) {
            Remove-Item -LiteralPath $destination -Force -ErrorAction SilentlyContinue
        }
        throw $installError
    }

    # Activation time determines which non-current versions are the newest
    # rollback candidates.
    (Get-Item -LiteralPath $versionDir).LastWriteTimeUtc = [DateTime]::UtcNow
    Repair-InterruptedLauncherSwap $destination $versionsDir $currentVersionFile
    Cleanup-ManagedVersions $versionsDir $currentVersionFile $runningLocksDir
    Write-Step "installed: $destination"

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $onPath = $false
    if ($userPath) {
        $onPath = @($userPath -split ";") -contains $normalizedInstallDir
    }
    if (-not $onPath) {
        Write-Host ""
        Write-Host "$normalizedInstallDir is not on your user PATH. Add it, then open a new terminal:" -ForegroundColor Yellow
        Write-Host ('  [Environment]::SetEnvironmentVariable("Path", "{0};" + [Environment]::GetEnvironmentVariable("Path", "User"), "User")' -f $normalizedInstallDir)
    }

    if (-not $Token) {
        Write-Host ""
        Write-Host "No GitHub token is required for public releases. A token is optional for private repositories or higher API rate limits."
    }
}
finally {
    Release-InstallLock $installLock
    foreach ($path in @($stagedLauncher, $temporaryMarker, $stageDir)) {
        if ($path -and (Test-Path -LiteralPath $path)) {
            Remove-Item -LiteralPath $path -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
