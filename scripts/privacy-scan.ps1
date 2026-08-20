#requires -Version 5.1

[CmdletBinding()]
param(
    [string]$Root = (Split-Path -Parent $PSScriptRoot)
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$rootPath = (Resolve-Path -LiteralPath $Root).Path
$findings = [System.Collections.Generic.List[string]]::new()

$deniedPathPatterns = @(
    '(?i)(^|/)(\.env|\.envrc|accounts.*\.json|api-keys\.json|credentials.*\.json|oauth.*\.json|tokens.*\.json|token-cache\.json|sessions.*\.json|conversations.*\.json|settings\.json|usage\.jsonl|conversation-index\.json|admin-password)$',
    '(?i)\.(pem|key|p12|pfx|sqlite|sqlite3|db|har|log|prof|bak|exe|dll|zip|tgz|tar|tar\.gz)$',
    '(?i)(^|/)(id_rsa.*|id_ed25519.*|docs/screenshots/.*)$'
)

$contentPatterns = [ordered]@{
    'private-key'       = '-----BEGIN [A-Z ]*PRIVATE KEY-----'
    'github-token'      = '(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})'
    'm365-api-key'      = '\bm365_[A-Za-z0-9_-]{32,}\b'
    'long-bearer'       = '(?i)\bBearer\s+[A-Za-z0-9._~-]{40,}'
    'oauth-code'        = '(?i)oauth2/nativeclient\?code=[A-Za-z0-9._~-]{20,}'
    'token-json'        = '(?i)"(?:access_token|refresh_token)"\s*:\s*"[^"\r\n]{12,}"'
    'credentialed-url' = '(?i)https?://[^\s/:]+:[^\s/@]+@'
    'local-user-path'   = '(?i)\b[A-Z]:\\(?:Users|xiaoshuo3)\\'
}

$files = Get-ChildItem -LiteralPath $rootPath -Recurse -File -Force | Where-Object {
    $_.FullName -notlike (Join-Path $rootPath '.git\*')
}

foreach ($file in $files) {
    $relative = $file.FullName.Substring($rootPath.Length).TrimStart('\', '/').Replace('\', '/')

    foreach ($pattern in $deniedPathPatterns) {
        if ($relative -match $pattern) {
            $findings.Add("[denied-path] $relative")
            break
        }
    }

    $stream = [IO.File]::OpenRead($file.FullName)
    try {
        $probeLength = [Math]::Min(8192, [int]$stream.Length)
        $probe = [byte[]]::new($probeLength)
        [void]$stream.Read($probe, 0, $probeLength)
        if ($probe -contains 0) {
            continue
        }
    }
    finally {
        $stream.Dispose()
    }

    $lineNumber = 0
    foreach ($line in [IO.File]::ReadLines($file.FullName)) {
        $lineNumber++
        foreach ($entry in $contentPatterns.GetEnumerator()) {
            if ($line -match $entry.Value) {
                $findings.Add("[$($entry.Key)] ${relative}:$lineNumber")
            }
        }

        $emailMatches = [regex]::Matches($line, '\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b', 'IgnoreCase')
        foreach ($match in $emailMatches) {
            if ($match.Value -notmatch '(?i)@(example\.(com|org|net)|example\.test|[A-Z0-9.-]+\.invalid|users\.noreply\.github\.com)$') {
                $findings.Add("[email] ${relative}:$lineNumber")
            }
        }
    }
}

if ($findings.Count -gt 0) {
    $findings | Sort-Object -Unique | Write-Error
    throw "Privacy scan failed with $($findings.Count) finding(s). Values were not printed."
}

Write-Output "Privacy scan passed: $($files.Count) files checked; no denied paths or secret/PII patterns found."
