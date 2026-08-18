#requires -Version 5.1

<#
.SYNOPSIS
Runs a bounded smoke test against the OpenAI-compatible image generation API.

.DESCRIPTION
Reads the bearer token from a local file, calls /v1/images/generations, and
handles HTTP 429 responses using Retry-After when supplied. URL responses are
reported after validation. Base64 responses are decoded to a local file and
are never written to the console.

.EXAMPLE
.\image_api_smoke_test.ps1 -KeyPath C:\Secrets\m365-api-key.txt

.EXAMPLE
.\image_api_smoke_test.ps1 -KeyPath C:\Secrets\m365-api-key.txt -ResponseFormat b64_json -OutputPath C:\Temp\image.png
#>

[CmdletBinding()]
param(
    [ValidateNotNullOrEmpty()]
    [string]$BaseUrl = 'https://m365.hanlaomo.dpdns.org',

    [ValidateNotNullOrEmpty()]
    [string]$KeyPath = $(
        if (-not [string]::IsNullOrWhiteSpace($env:M365_TEST_KEY_FILE)) {
            $env:M365_TEST_KEY_FILE
        }
        else {
            Join-Path ([Environment]::GetFolderPath('UserProfile')) '.m365copilot-api-key'
        }
    ),

    [ValidateNotNullOrEmpty()]
    [string]$Prompt = 'A simple blue circle centered on a white background.',

    [ValidateSet('auto', 'gpt-image-2')]
    [string]$Model = 'auto',

    [ValidateNotNullOrEmpty()]
    [string]$Size = '1024x1024',

    [ValidateSet('url', 'b64_json')]
    [string]$ResponseFormat = 'url',

    [string]$OutputPath,

    [ValidateRange(0, 10)]
    [int]$MaxRetries = 3,

    [ValidateRange(1, 300)]
    [int]$DefaultRetrySeconds = 5,

    [ValidateRange(1, 300)]
    [int]$MaxRetryAfterSeconds = 60,

    [ValidateRange(1, 600)]
    [int]$RequestTimeoutSeconds = 180,

    [switch]$Force
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Add-Type -AssemblyName System.Net.Http

function Get-RetryDelaySeconds {
    param(
        [Parameter(Mandatory = $true)]
        $Response,

        [Parameter(Mandatory = $true)]
        [int]$FallbackSeconds,

        [Parameter(Mandatory = $true)]
        [int]$MaximumSeconds,

        [Parameter(Mandatory = $true)]
        [int]$RetryNumber
    )

    $seconds = $null
    try {
        $retryAfter = $Response.Headers.RetryAfter
        if ($null -ne $retryAfter) {
            if ($null -ne $retryAfter.Delta) {
                $seconds = [Math]::Ceiling($retryAfter.Delta.TotalSeconds)
            }
            elseif ($null -ne $retryAfter.Date) {
                $seconds = [Math]::Ceiling(($retryAfter.Date.UtcDateTime - [DateTime]::UtcNow).TotalSeconds)
            }
        }
    }
    catch {
        $seconds = $null
    }

    if ($null -eq $seconds) {
        $exponent = [Math]::Max(0, $RetryNumber - 1)
        $seconds = $FallbackSeconds * [Math]::Pow(2, $exponent)
    }

    return [int][Math]::Min($MaximumSeconds, [Math]::Max(1, $seconds))
}

function Get-ImageFileExtension {
    param(
        [Parameter(Mandatory = $true)]
        [byte[]]$Bytes
    )

    if ($Bytes.Length -ge 8 -and
        $Bytes[0] -eq 0x89 -and $Bytes[1] -eq 0x50 -and $Bytes[2] -eq 0x4E -and $Bytes[3] -eq 0x47 -and
        $Bytes[4] -eq 0x0D -and $Bytes[5] -eq 0x0A -and $Bytes[6] -eq 0x1A -and $Bytes[7] -eq 0x0A) {
        return '.png'
    }

    if ($Bytes.Length -ge 3 -and $Bytes[0] -eq 0xFF -and $Bytes[1] -eq 0xD8 -and $Bytes[2] -eq 0xFF) {
        return '.jpg'
    }

    if ($Bytes.Length -ge 6) {
        $gifHeader = [Text.Encoding]::ASCII.GetString($Bytes, 0, 6)
        if ($gifHeader -eq 'GIF87a' -or $gifHeader -eq 'GIF89a') {
            return '.gif'
        }
    }

    if ($Bytes.Length -ge 12) {
        $riffHeader = [Text.Encoding]::ASCII.GetString($Bytes, 0, 4)
        $webpHeader = [Text.Encoding]::ASCII.GetString($Bytes, 8, 4)
        if ($riffHeader -eq 'RIFF' -and $webpHeader -eq 'WEBP') {
            return '.webp'
        }
    }

    return '.bin'
}

function Get-SafeOutputPath {
    param(
        [string]$RequestedPath,

        [Parameter(Mandatory = $true)]
        [string]$Extension
    )

    $fileName = 'image-smoke-{0}{1}' -f [DateTime]::UtcNow.ToString('yyyyMMdd-HHmmss'), $Extension

    if ([string]::IsNullOrWhiteSpace($RequestedPath)) {
        $candidate = Join-Path ([IO.Path]::GetTempPath()) $fileName
    }
    elseif (Test-Path -LiteralPath $RequestedPath -PathType Container) {
        $candidate = Join-Path $RequestedPath $fileName
    }
    else {
        $candidate = $RequestedPath
    }

    $fullPath = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($candidate)
    $parentPath = [IO.Path]::GetDirectoryName($fullPath)
    if ([string]::IsNullOrWhiteSpace($parentPath) -or -not (Test-Path -LiteralPath $parentPath -PathType Container)) {
        throw "Output directory does not exist: '$parentPath'."
    }

    return $fullPath
}

if ([string]::IsNullOrWhiteSpace($Prompt)) {
    throw 'Prompt cannot be empty or whitespace.'
}

try {
    $keyFile = Get-Item -LiteralPath $KeyPath -ErrorAction Stop
}
catch {
    throw "API key file was not found or is not accessible: '$KeyPath'."
}

if ($keyFile.PSIsContainer) {
    throw "API key path must identify a file: '$KeyPath'."
}

$apiKey = [IO.File]::ReadAllText($keyFile.FullName).Trim()
if ([string]::IsNullOrWhiteSpace($apiKey)) {
    throw "API key file is empty: '$KeyPath'."
}

if ($apiKey.IndexOf("`r") -ge 0 -or $apiKey.IndexOf("`n") -ge 0) {
    throw "API key file must contain exactly one key: '$KeyPath'."
}

$endpointText = $BaseUrl.TrimEnd('/') + '/v1/images/generations'
$endpointUri = $null
if (-not [Uri]::TryCreate($endpointText, [UriKind]::Absolute, [ref]$endpointUri) -or
    ($endpointUri.Scheme -ne [Uri]::UriSchemeHttps -and $endpointUri.Scheme -ne [Uri]::UriSchemeHttp)) {
    throw "BaseUrl must be an absolute HTTP or HTTPS URL: '$BaseUrl'."
}

$requestBody = [ordered]@{
    model           = $Model
    prompt          = $Prompt
    n               = 1
    size            = $Size
    response_format = $ResponseFormat
} | ConvertTo-Json -Compress

$client = [System.Net.Http.HttpClient]::new()
$client.Timeout = [TimeSpan]::FromSeconds($RequestTimeoutSeconds)
$client.DefaultRequestHeaders.Accept.ParseAdd('application/json')
if (-not $client.DefaultRequestHeaders.TryAddWithoutValidation('Authorization', "Bearer $apiKey")) {
    $client.Dispose()
    $apiKey = $null
    throw 'Could not set the Authorization header.'
}

$responsePayload = $null
try {
    for ($attempt = 1; $attempt -le ($MaxRetries + 1); $attempt++) {
        $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::Post, $endpointUri)
        $request.Content = [System.Net.Http.StringContent]::new($requestBody, [Text.Encoding]::UTF8, 'application/json')
        $response = $null
        $retryDelaySeconds = $null

        try {
            try {
                $response = $client.SendAsync($request).GetAwaiter().GetResult()
            }
            catch {
                $safeMessage = $_.Exception.Message.Replace($apiKey, '[REDACTED]')
                throw "Image API request failed before receiving a response: $safeMessage"
            }

            $responseText = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
            $statusCode = [int]$response.StatusCode

            if ($statusCode -eq 429) {
                if ($attempt -gt $MaxRetries) {
                    throw "Image API returned HTTP 429 after $attempt attempts."
                }

                $retryDelaySeconds = Get-RetryDelaySeconds `
                    -Response $response `
                    -FallbackSeconds $DefaultRetrySeconds `
                    -MaximumSeconds $MaxRetryAfterSeconds `
                    -RetryNumber $attempt
            }
            elseif (-not $response.IsSuccessStatusCode) {
                throw "Image API returned HTTP $statusCode."
            }
            else {
                try {
                    $responsePayload = $responseText | ConvertFrom-Json
                }
                catch {
                    throw 'Image API returned success status with invalid JSON.'
                }
            }
        }
        finally {
            if ($null -ne $response) {
                $response.Dispose()
            }
            $request.Dispose()
        }

        if ($null -ne $retryDelaySeconds) {
            Write-Warning "Image API returned HTTP 429. Retrying in $retryDelaySeconds seconds (retry $attempt of $MaxRetries)."
            Start-Sleep -Seconds $retryDelaySeconds
            continue
        }

        break
    }
}
finally {
    $client.Dispose()
}

if ($null -eq $responsePayload) {
    throw 'Image API did not return a successful response.'
}

$dataProperty = $responsePayload.PSObject.Properties['data']
if ($null -eq $dataProperty) {
    throw 'Image API response did not contain a data array.'
}

$dataItems = @($dataProperty.Value)
if ($dataItems.Count -eq 0 -or $null -eq $dataItems[0]) {
    throw 'Image API response contained an empty data array.'
}

$result = $dataItems[0]
$urlProperty = $result.PSObject.Properties['url']
if ($null -ne $urlProperty -and -not [string]::IsNullOrWhiteSpace([string]$urlProperty.Value)) {
    $urlText = [string]$urlProperty.Value
    $escapedApiKey = [Uri]::EscapeDataString($apiKey)
    if ($urlText.Contains($apiKey) -or $urlText.Contains($escapedApiKey)) {
        $apiKey = $null
        throw 'Image API returned a URL containing the API credential; the URL was not displayed.'
    }

    $imageUri = $null
    if (-not [Uri]::TryCreate($urlText, [UriKind]::Absolute, [ref]$imageUri) -or
        ($imageUri.Scheme -ne [Uri]::UriSchemeHttps -and $imageUri.Scheme -ne [Uri]::UriSchemeHttp)) {
        throw 'Image API returned an invalid image URL.'
    }

    $apiKey = $null
    Write-Output ('Image URL: {0}' -f $imageUri.AbsoluteUri)
    return
}

$base64Property = $result.PSObject.Properties['b64_json']
if ($null -eq $base64Property -or [string]::IsNullOrWhiteSpace([string]$base64Property.Value)) {
    throw 'Image API response contained neither a URL nor b64_json image data.'
}

try {
    $imageBytes = [Convert]::FromBase64String([string]$base64Property.Value)
}
catch {
    throw 'Image API returned invalid b64_json image data.'
}

if ($imageBytes.Length -eq 0) {
    throw 'Image API returned empty b64_json image data.'
}

$apiKey = $null
$extension = Get-ImageFileExtension -Bytes $imageBytes
$outputFile = Get-SafeOutputPath -RequestedPath $OutputPath -Extension $extension

if ($Force) {
    $fileMode = [IO.FileMode]::Create
}
else {
    $fileMode = [IO.FileMode]::CreateNew
}

$fileStream = $null
try {
    $fileStream = [IO.File]::Open($outputFile, $fileMode, [IO.FileAccess]::Write, [IO.FileShare]::None)
    $fileStream.Write($imageBytes, 0, $imageBytes.Length)
}
catch [IO.IOException] {
    throw "Could not save image to '$outputFile'. The file may already exist; use -Force to overwrite it."
}
finally {
    if ($null -ne $fileStream) {
        $fileStream.Dispose()
    }
}

Write-Output "Saved image: $outputFile"
