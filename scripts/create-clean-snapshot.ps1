#requires -Version 5.1

[CmdletBinding()]
param(
    [string]$SourceRoot = (Split-Path -Parent $PSScriptRoot),

    [Parameter(Mandatory = $true)]
    [string]$OutputRoot,

    [string]$Branch = 'clean-production'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$sourcePath = (Resolve-Path -LiteralPath $SourceRoot).Path
$outputPath = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($OutputRoot)
if (Test-Path -LiteralPath $outputPath) {
    throw "Output path already exists: '$outputPath'."
}

$files = @(& git -C $sourcePath ls-files -co --exclude-standard)
if ($LASTEXITCODE -ne 0 -or $files.Count -eq 0) {
    throw 'Could not enumerate repository files.'
}

New-Item -ItemType Directory -Path $outputPath | Out-Null
try {
    foreach ($relative in $files) {
        $sourceFile = Join-Path $sourcePath $relative
        if (-not (Test-Path -LiteralPath $sourceFile -PathType Leaf)) {
            continue
        }
        $targetFile = Join-Path $outputPath $relative
        $targetDirectory = Split-Path -Parent $targetFile
        if (-not (Test-Path -LiteralPath $targetDirectory)) {
            New-Item -ItemType Directory -Path $targetDirectory -Force | Out-Null
        }
        Copy-Item -LiteralPath $sourceFile -Destination $targetFile
    }

    & (Join-Path $outputPath 'scripts/privacy-scan.ps1') -Root $outputPath
    if ($LASTEXITCODE -ne 0) {
        throw 'Privacy scan rejected the snapshot.'
    }

    & git -C $outputPath init "--initial-branch=$Branch"
    if ($LASTEXITCODE -ne 0) {
        throw 'Could not initialize the clean repository.'
    }
    & git -C $outputPath add --all
    if ($LASTEXITCODE -ne 0) {
        throw 'Could not stage the clean snapshot.'
    }
}
catch {
    if (Test-Path -LiteralPath $outputPath) {
        $resolvedOutput = (Resolve-Path -LiteralPath $outputPath).Path
        if ($resolvedOutput.StartsWith((Split-Path -Parent $sourcePath), [StringComparison]::OrdinalIgnoreCase)) {
            Remove-Item -LiteralPath $resolvedOutput -Recurse -Force
        }
    }
    throw
}

Write-Output "Clean snapshot staged at: $outputPath"
Write-Output 'Review git status and git diff --cached before committing.'
