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
        $release = Invoke-RestMethod -UseBasicParsing -Headers $headers -Uri "$ApiBase/repos/$Repo/releases/latest"
        if (-not $release.tag_name) {
            throw "GitHub response did not contain tag_name"
        }
        return [string]$release.tag_name
    }

    $handler = [System.Net.Http.HttpClientHandler]::new()
    $handler.AllowAutoRedirect = $true
    $client = [System.Net.Http.HttpClient]::new($handler)
    try {
        $client.DefaultRequestHeaders.UserAgent.ParseAdd("metis-installer")
        $response = $client.GetAsync(
            "$WebBase/$Repo/releases/latest",
            [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead
        ).GetAwaiter().GetResult()
        try {
            $response.EnsureSuccessStatusCode() | Out-Null
            $tag = $response.RequestMessage.RequestUri.Segments[-1].TrimEnd("/")
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

if ($PSVersionTable.PSEdition -eq "Core" -and -not $IsWindows) {
    throw "install.ps1 supports Windows only"
}

$architecture = Resolve-Architecture
$resolvedTag = Resolve-ReleaseTag
$artifact = "metis-windows-$architecture.zip"
$checksumAsset = "$artifact.sha256"
$downloadBase = "$WebBase/$Repo/releases/download/$resolvedTag"
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("metis-install-" + [guid]::NewGuid().ToString("N"))

New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
try {
    $zipPath = Join-Path $tempDir $artifact
    $checksumPath = Join-Path $tempDir $checksumAsset
    $downloadHeaders = @{}
    if ($Token) {
        $downloadHeaders.Authorization = "Bearer $Token"
    }

    Write-Step "installing metis $resolvedTag for windows-$architecture"
    Invoke-WebRequest -UseBasicParsing -Headers $downloadHeaders -Uri "$downloadBase/$artifact" -OutFile $zipPath
    Invoke-WebRequest -UseBasicParsing -Headers $downloadHeaders -Uri "$downloadBase/$checksumAsset" -OutFile $checksumPath

    $checksumFields = (Get-Content -Raw $checksumPath).Trim() -split "\s+"
    if ($checksumFields.Count -lt 2 -or $checksumFields[-1].TrimStart([char]'*') -ne $artifact) {
        throw "Checksum sidecar does not describe $artifact"
    }
    $expected = $checksumFields[0].ToLowerInvariant()
    $actual = (Get-FileHash -Algorithm SHA256 $zipPath).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        throw "SHA-256 mismatch: got $actual, expected $expected"
    }

    $zip = [System.IO.Compression.ZipFile]::OpenRead($zipPath)
    try {
        if ($zip.Entries.Count -ne 1 -or $zip.Entries[0].FullName -ne "metis.exe" -or $zip.Entries[0].Name -ne "metis.exe") {
            throw "Release archive must contain exactly one root entry named metis.exe"
        }
    }
    finally {
        $zip.Dispose()
    }

    Expand-Archive -Path $zipPath -DestinationPath $tempDir -Force
    $source = Join-Path $tempDir "metis.exe"
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "Release archive is missing metis.exe"
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $destination = Join-Path $InstallDir "metis.exe"
    $staged = Join-Path $InstallDir "metis.new.exe"
    $backup = Join-Path $InstallDir "metis.old.exe"
    Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
    Copy-Item -LiteralPath $source -Destination $staged -Force

    if (-not $SkipVersionCheck) {
        & $staged version
        if ($LASTEXITCODE -ne 0) {
            throw "Downloaded metis.exe failed its version check"
        }
    }

    $hadExisting = Test-Path -LiteralPath $destination -PathType Leaf
    if ($hadExisting) {
        Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
        Move-Item -LiteralPath $destination -Destination $backup -Force
    }
    try {
        Move-Item -LiteralPath $staged -Destination $destination -Force
        Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
    }
    catch {
        if ($hadExisting -and (Test-Path -LiteralPath $backup -PathType Leaf)) {
            Move-Item -LiteralPath $backup -Destination $destination -Force
        }
        throw
    }
    Write-Step "installed: $destination"

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $onPath = $false
    if ($userPath) {
        $onPath = @($userPath -split ";") -contains $InstallDir
    }
    if (-not $onPath) {
        Write-Host ""
        Write-Host "$InstallDir is not on your user PATH. Add it, then open a new terminal:" -ForegroundColor Yellow
        Write-Host ('  [Environment]::SetEnvironmentVariable("Path", "{0};" + [Environment]::GetEnvironmentVariable("Path", "User"), "User")' -f $InstallDir)
    }

    if (-not $Token) {
        Write-Host ""
        Write-Host "No GitHub token is required for public releases. A token is optional for higher API rate limits."
    }
}
finally {
    if (Test-Path -LiteralPath $tempDir) {
        Remove-Item -LiteralPath $tempDir -Recurse -Force
    }
}
