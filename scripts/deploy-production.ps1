#requires -Version 7.2

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Za-z0-9.-]+$')]
    [string]$HostName,

    [Parameter(Mandatory = $true)]
    [string]$KnownHostsPath,

    [string]$IdentityFile,

    [ValidatePattern('^[A-Za-z0-9._-]+$')]
    [string]$SshUser = 'root',

    [ValidateRange(1, 65535)]
    [int]$Port = 22,

    [ValidatePattern('^[A-Za-z0-9._-]+$')]
    [string]$ReleaseId,

    [ValidatePattern('^[A-Za-z0-9@._-]+$')]
    [string]$ServiceName = 'm365-copilot2api',

    [ValidatePattern('^/[A-Za-z0-9._/-]+$')]
    [string]$InstallRoot = '/opt/m365-copilot2api',

    [ValidatePattern('^/[A-Za-z0-9._/-]+$')]
    [string]$StateDir = '/var/lib/m365-copilot2api',

    [ValidateRange(2, 50)]
    [int]$Retain = 3,

    [ValidateSet('amd64', 'arm64')]
    [string]$Architecture = 'amd64',

    [switch]$SkipTests
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path -LiteralPath (Split-Path -Parent $PSScriptRoot)).Path
$archive = $null

if (-not (Test-Path -LiteralPath $KnownHostsPath -PathType Leaf)) {
    throw "Known-hosts file not found: '$KnownHostsPath'."
}
if ($IdentityFile -and -not (Test-Path -LiteralPath $IdentityFile -PathType Leaf)) {
    throw "SSH identity file not found: '$IdentityFile'."
}

$dirty = @(& git -C $repo status --porcelain)
if ($LASTEXITCODE -ne 0 -or $dirty.Count -ne 0) {
    throw 'Production deployment requires a clean Git worktree.'
}

$commit = (& git -C $repo rev-parse --short=12 HEAD).Trim()
if (-not $ReleaseId) {
    $tag = (& git -C $repo describe --tags --exact-match 2>$null).Trim()
    if (-not $tag) {
        throw 'Specify -ReleaseId or deploy from an exact Git tag.'
    }
    $ReleaseId = $tag
}

$temp = Join-Path ([IO.Path]::GetTempPath()) ("m365-release-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $temp | Out-Null
try {
    Push-Location $repo
    try {
        if (-not $SkipTests) {
            & go test -count=1 ./...
            if ($LASTEXITCODE -ne 0) { throw 'go test failed.' }
            & go vet ./...
            if ($LASTEXITCODE -ne 0) { throw 'go vet failed.' }
        }

        & (Join-Path $PSScriptRoot 'privacy-scan.ps1') -Root $repo
        if ($LASTEXITCODE -ne 0) { throw 'Privacy scan failed.' }

        $binary = Join-Path $temp 'm365-copilot2api'
        $buildTime = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
        $version = $ReleaseId -replace '^stable-v', ''
        $ldflags = "-s -w -X m365-copilot2api/internal/web.Version=$version -X m365-copilot2api/internal/web.Commit=$commit -X m365-copilot2api/internal/web.BuildTime=$buildTime"

        $oldGoos = $env:GOOS
        $oldGoarch = $env:GOARCH
        $oldCgo = $env:CGO_ENABLED
        try {
            $env:GOOS = 'linux'
            $env:GOARCH = $Architecture
            $env:CGO_ENABLED = '0'
            & go build -trimpath "-ldflags=$ldflags" -o $binary ./cmd/server
            if ($LASTEXITCODE -ne 0) { throw 'Linux build failed.' }
        }
        finally {
            $env:GOOS = $oldGoos
            $env:GOARCH = $oldGoarch
            $env:CGO_ENABLED = $oldCgo
        }
    }
    finally {
        Pop-Location
    }

    $binarySha = (Get-FileHash -Algorithm SHA256 -LiteralPath $binary).Hash.ToLowerInvariant()
    "$binarySha  m365-copilot2api" | Set-Content -LiteralPath (Join-Path $temp 'SHA256SUMS') -Encoding ascii
    [ordered]@{
        release_id = $ReleaseId
        commit = $commit
        build_time = $buildTime
        goos = 'linux'
        goarch = $Architecture
        binary_sha256 = $binarySha
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $temp 'manifest.json') -Encoding utf8NoBOM

    $archive = Join-Path ([IO.Path]::GetTempPath()) ("$ReleaseId.tar.gz")
    & tar -czf $archive -C $temp m365-copilot2api SHA256SUMS manifest.json
    if ($LASTEXITCODE -ne 0) { throw 'Could not create release archive.' }
    $archiveSha = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()

    $remoteArchive = "/tmp/m365-$ReleaseId.tar.gz"
    $remoteScript = "/tmp/m365-remote-release-$ReleaseId.sh"
    $common = @('-o', 'StrictHostKeyChecking=yes', '-o', "UserKnownHostsFile=$KnownHostsPath")
    if ($IdentityFile) { $common += @('-i', $IdentityFile) }

    & scp @common -P $Port $archive "${SshUser}@${HostName}:$remoteArchive"
    if ($LASTEXITCODE -ne 0) { throw 'Archive upload failed.' }
    & scp @common -P $Port (Join-Path $repo 'deploy/remote-release.sh') "${SshUser}@${HostName}:$remoteScript"
    if ($LASTEXITCODE -ne 0) { throw 'Release helper upload failed.' }

    $remoteCommand = "bash '$remoteScript' '$ReleaseId' '$remoteArchive' '$archiveSha' '$ServiceName' '$InstallRoot' '$StateDir' '$Retain'"
    & ssh @common -p $Port "${SshUser}@${HostName}" $remoteCommand
    if ($LASTEXITCODE -ne 0) { throw 'Remote release failed or rolled back.' }
}
finally {
    if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Recurse -Force }
    if ($null -ne $archive -and (Test-Path -LiteralPath $archive)) { Remove-Item -LiteralPath $archive -Force }
}
